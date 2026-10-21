package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"baton/internal/config"
	"baton/internal/harbor"
	"baton/internal/telemetry"
)

// tabby drives TabbyAPI (ExLlamaV3). TabbyAPI loads its model before it opens
// its port, so "answers on the port" means "model loaded". The default release
// stops the server: a restart through start.bat reloads exactly what
// config.yml says, which an API reload does not (it ignores config.yml unless
// use_as_default lists the keys).
type tabby struct {
	base
	apiKey, adminKey string
	modelName        string

	mmu     sync.Mutex
	logMod  time.Time
	logSize int64
	lastReq *TabbyMetrics

	// Requests in flight, read from the log as it grows.
	rmu       sync.Mutex
	reqs      tabbyReqs
	reqOff    int64 // how far the log has been read
	reqMod    time.Time
	reqSize   int64
	idleSince time.Time // since when requests have been "in flight" on an idle GPU
}

const (
	tabbyStale     = 30 * time.Minute // a start this old lost its end line
	tabbyScanMax   = 8 << 20          // how much of a long log to read at first
	tabbyIdlePct   = 5                // GPU load below this is idle
	tabbyIdleGrace = 15 * time.Second // how long requests may sit on an idle GPU
)

func newTabby(c config.Engine, env *Env) *tabby {
	t := &tabby{base: base{cfg: c, env: env}}
	t.reloadFiles()
	return t
}

func (t *tabby) IsLLM() bool { return true }

// reloadFiles reads the tokens and the configured model name. Both are read
// again on every start so edits to TabbyAPI's files take effect.
func (t *tabby) reloadFiles() {
	tok := t.cfg.TokensFile
	if tok == "" && t.cfg.Dir != "" {
		tok = filepath.Join(t.cfg.Dir, "api_tokens.yml")
	}
	if kv := readYAMLTop(tok); kv != nil {
		t.apiKey, t.adminKey = kv["api_key"], kv["admin_key"]
	}
	if t.cfg.Dir != "" {
		t.modelName = readYAMLKey(filepath.Join(t.cfg.Dir, "config.yml"), "model", "model_name")
	}
}

func (t *tabby) hdr(admin bool) http.Header {
	h := http.Header{}
	k := t.apiKey
	if admin && t.adminKey != "" {
		k = t.adminKey
	}
	if k != "" {
		h.Set("Authorization", "Bearer "+k)
	}
	return h
}

func (t *tabby) Upstream() (string, http.Header) {
	return strings.TrimRight(t.cfg.URL, "/"), t.hdr(false)
}

func (t *tabby) Serves(model string) (bool, string) {
	var extra []string
	if t.modelName != "" {
		extra = append(extra, t.modelName)
	}
	return t.servesList(model, extra)
}

func (t *tabby) Probe(ctx context.Context, snap *telemetry.Snapshot) Status {
	st := t.probeServer(ctx, snap)
	if m := t.lastMetrics(); m != nil {
		if st.Extra == nil {
			st.Extra = map[string]any{}
		}
		st.Extra["last"] = m
	}
	t.trackRequests(&st, time.Now())
	return st
}

// trackRequests marks the status busy while TabbyAPI's log shows requests in
// flight, and exact, so the broker waits for those instead of guessing from
// GPU load. That needs a log that belongs to the TabbyAPI running now; a
// TabbyAPI started outside baton keeps the GPU-load guess.
func (t *tabby) trackRequests(st *Status, now time.Time) {
	t.rmu.Lock()
	defer t.rmu.Unlock()
	if !t.readRequests(LogPath(t.env.StateDir, t.cfg.Name), now) || !st.Up || !t.logTrusted(*st) {
		t.idleSince = time.Time{}
		return
	}
	t.reqs.expire(now, tabbyStale)
	// A stream whose client goes away usually ends without a line in the log
	// (its job is cancelled under TabbyAPI's feet), so requests that sit on
	// an idle GPU are let go after a while. Without per-process GPU data (no
	// VRAM seen) only the age limit applies.
	if len(t.reqs.running) > 0 && maxMiB(st.VRAMMiB) > 0 && st.Util < tabbyIdlePct {
		if t.idleSince.IsZero() {
			t.idleSince = now
		} else if now.Sub(t.idleSince) >= tabbyIdleGrace {
			t.reqs.endAll()
		}
	} else {
		t.idleSince = time.Time{}
	}
	st.BusyExact = true
	t.kvLocked(st, now)
	switch n := len(t.reqs.running); n {
	case 0:
	case 1:
		for tag, at := range t.reqs.running {
			st.Busy, st.BusyWhy = true, fmt.Sprintf("generating #%s (%s)", tag, max(now.Sub(at), 0).Round(time.Second))
		}
	default:
		st.Busy, st.BusyWhy = true, fmt.Sprintf("%d requests in flight", n)
	}
}

// readRequests feeds what the log gained since the last look to the
// tracker. A log that shrank was rotated or truncated: start over.
func (t *tabby) readRequests(path string, now time.Time) bool {
	fi, err := os.Stat(path)
	if err != nil {
		t.reqs, t.reqOff, t.reqSize, t.reqMod = tabbyReqs{}, 0, 0, time.Time{}
		return false
	}
	if fi.Size() == t.reqSize && fi.ModTime().Equal(t.reqMod) {
		return true
	}
	t.reqSize, t.reqMod = fi.Size(), fi.ModTime()
	skip := false // drop a partial first line
	if fi.Size() < t.reqOff || t.reqs.running == nil {
		t.reqs = tabbyReqs{}
		t.reqs.reset()
		t.reqOff = max(fi.Size()-tabbyScanMax, 0)
		skip = t.reqOff > 0
	}
	for t.reqOff < fi.Size() {
		text, next, _, err := ReadLog(path, t.reqOff)
		if err != nil {
			break
		}
		if next == t.reqOff {
			if fi.Size()-t.reqOff < 1<<20 {
				break // a half-written line: next time
			}
			t.reqOff += 1 << 20 // one line longer than a read: skip it
			skip = true
			continue
		}
		if skip {
			_, text, _ = strings.Cut(text, "\n")
			skip = false
		}
		t.reqs.feed(text, now)
		t.reqOff = next
		t.record(t.reqs.take())
	}
	return true
}

// record hands finished requests to the request history. The key makes a
// record read again (the log after a restart, or completed by wrapped lines)
// replace itself: TabbyAPI numbers requests from #1 each time it starts, so
// the key carries the start marker too.
func (t *tabby) record(fin []tabbyFinished) {
	if t.env.Record == nil {
		return
	}
	for _, f := range fin {
		run := f.At.Format("2006-01-02")
		if !f.Run.IsZero() {
			run = strconv.FormatInt(f.Run.Unix(), 10)
		}
		m := f.M
		model := f.Model
		if model == "" {
			model = t.modelName
		}
		_, route, _ := strings.Cut(m.Request, " ")
		t.env.Record(harbor.Request{
			Key: t.cfg.Name + "|" + run + "|" + f.Tag, At: f.At, Engine: t.cfg.Name, Model: model,
			Source: "log", Route: route, Status: 200,
			Prompt: m.Prompt, Cached: m.Prompt * m.CachedPct / 100, Output: m.Tokens,
			PromptTPS: m.PromptTPS, DecodeTPS: m.Decode, FirstTok: m.FirstToken, Total: m.Total,
			DraftOK: m.DraftOK, DraftAll: m.DraftAll, Window: f.Window,
		})
	}
}

// kvLocked adds what the log tells about the KV cache of the TabbyAPI running
// now: the window and pool of its load, if the server did not report them,
// and the prompt tokens of each request in flight.
func (t *tabby) kvLocked(st *Status, now time.Time) {
	kv, _ := st.Extra["kv"].(map[string]any)
	if kv == nil {
		kv = map[string]any{}
	}
	if _, ok := kv["window"]; !ok && t.reqs.window > 0 {
		kv["window"] = t.reqs.window
	}
	if _, ok := kv["pool"]; !ok && t.reqs.pool > 0 {
		kv["pool"] = t.reqs.pool
	}
	type flight struct {
		Tag    string  `json:"tag"`
		Prompt int     `json:"prompt"`
		Age    float64 `json:"age_s"`
	}
	fl := []flight{}
	for tag, at := range t.reqs.running {
		fl = append(fl, flight{Tag: tag, Prompt: t.reqs.prompts[tag], Age: max(now.Sub(at), 0).Round(time.Second).Seconds()})
	}
	sort.Slice(fl, func(i, j int) bool { return fl[i].Age > fl[j].Age })
	kv["inflight"] = fl
	if st.Extra == nil {
		st.Extra = map[string]any{}
	}
	st.Extra["kv"] = kv
}

// logTrusted reports whether the log shows the TabbyAPI running now: baton
// started it with its output going to the log, or (after a baton restart)
// it started right after baton's last marker.
func (t *tabby) logTrusted(st Status) bool {
	if t.cfg.ShowWindow {
		return false // its output goes to a console
	}
	if t.managedRunning() {
		return true
	}
	if t.reqs.marker.IsZero() {
		return false
	}
	var first time.Time
	for _, p := range st.procs {
		if at := fileTime(p.Start); !at.IsZero() && (first.IsZero() || at.Before(first)) {
			first = at
		}
	}
	d := first.Sub(t.reqs.marker)
	return !first.IsZero() && d > -2*time.Second && d < time.Minute
}

// fileTime converts a Windows FILETIME (100 ns ticks since 1601).
func fileTime(ft int64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	return time.Unix(0, (ft-116444736000000000)*100)
}

// lastMetrics reads TabbyAPI's summary of its latest request from the log,
// re-reading only when the log changed.
func (t *tabby) lastMetrics() *TabbyMetrics {
	path := LogPath(t.env.StateDir, t.cfg.Name)
	fi, err := os.Stat(path)
	if err != nil {
		return nil
	}
	t.mmu.Lock()
	defer t.mmu.Unlock()
	if fi.ModTime().Equal(t.logMod) && fi.Size() == t.logSize {
		return t.lastReq
	}
	t.logMod, t.logSize = fi.ModTime(), fi.Size()
	from := fi.Size() - 64<<10
	if from < 0 {
		from = 0
	}
	if text, _, _, err := ReadLog(path, from); err == nil {
		if m := ParseTabbyMetrics(text); m != nil {
			t.lastReq = m
		}
	}
	return t.lastReq
}

func (t *tabby) probeServer(ctx context.Context, snap *telemetry.Snapshot) Status {
	st := Status{URL: t.cfg.URL, Model: t.modelName}
	t.fill(&st, snap, t.attribute(snap, portOf(t.cfg.URL)))
	pctx, cancel := probeCtx(ctx)
	defer cancel()
	var card struct {
		ID         string         `json:"id"`
		Parameters map[string]any `json:"parameters"`
	}
	err := doJSON(pctx, "GET", t.cfg.URL+"/v1/model", t.hdr(false), nil, &card)
	var he *httpError
	switch {
	case err == nil:
		st.Up, st.Loaded = true, true
		if card.ID != "" {
			st.Model = card.ID
		}
		// The window and cache pool the server reports for the loaded model.
		kv := map[string]any{}
		for from, to := range map[string]string{"max_seq_len": "window", "cache_size": "pool"} {
			if v, ok := card.Parameters[from].(float64); ok && v > 0 {
				kv[to] = int(v)
			}
		}
		if v, ok := card.Parameters["cache_mode"].(string); ok && v != "" {
			kv["cache_mode"] = v
		}
		if len(kv) > 0 {
			st.Extra = map[string]any{"kv": kv}
		}
	case errors.As(err, &he) && he.Code == 503:
		st.Up = true // server up, no model loaded
	case errors.As(err, &he):
		st.Up = true
		st.Err = err.Error()
		st.Loaded = maxMiB(st.VRAMMiB) > 2048
	case snap.Listeners[portOf(t.cfg.URL)] != 0 && len(st.PIDs) > 0:
		// The port is open, so the model is loaded (TabbyAPI opens it only
		// after loading); the server is just too busy to answer right now.
		st.Up, st.Loaded, st.Busy, st.BusyWhy = true, true, true, "API not answering (busy)"
	default:
		// Not answering: either stopped, or still loading its model.
		if t.managedRunning() || len(st.PIDs) > 0 {
			st.Loading = true
			st.Detail = "loading model"
		}
		if gone, msg := t.managedExited(); gone {
			st.Err = "server exited (" + msg + ")"
			if tail := logTail(t.env.StateDir, t.cfg.Name, 3); tail != "" {
				st.Err += ": " + tail
			}
		}
	}
	return st
}

func (t *tabby) Release(ctx context.Context, mode string, st Status) error {
	switch mode {
	case "unload", "free":
		if !st.Up {
			return nil
		}
		if err := doJSON(ctx, "POST", t.cfg.URL+"/v1/model/unload", t.hdr(true), nil, nil); err != nil {
			var he *httpError
			if errors.As(err, &he) && he.Code == 503 {
				return nil // nothing loaded
			}
			return fmt.Errorf("unload: %w", err)
		}
		return nil
	default:
		return t.stop(st)
	}
}

func (t *tabby) Acquire(ctx context.Context, model string, progress func(string)) error {
	snap := t.env.Sample()
	st := t.Probe(ctx, snap)
	if st.Up && st.Loaded {
		return nil
	}
	t.reloadFiles()
	if st.Up && !st.Loaded {
		if t.modelName == "" {
			return errors.New("tabby is up with no model and config.yml names none")
		}
		progress("loading " + t.modelName)
		lctx, cancel := context.WithTimeout(ctx, t.cfg.ReadyTimeout.Duration)
		defer cancel()
		if err := doJSON(lctx, "POST", t.cfg.URL+"/v1/model/load", t.hdr(true), map[string]any{"model_name": t.modelName}, nil); err != nil {
			return fmt.Errorf("load %s: %w", t.modelName, err)
		}
	} else if !st.Loading {
		if err := t.launch(); err != nil {
			return err
		}
	}
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, t.cfg.ReadyTimeout.Duration)
	defer cancel()
	err := waitUntil(rctx, time.Second, func() (bool, error) {
		s := t.Probe(rctx, t.env.Sample())
		if s.Up && s.Loaded {
			return true, nil
		}
		if gone, msg := t.managedExited(); gone {
			e := "tabby exited while loading (" + msg + ")"
			if tail := logTail(t.env.StateDir, t.cfg.Name, 5); tail != "" {
				e += ":\n" + tail
			}
			return false, errors.New(e)
		}
		progress(fmt.Sprintf("loading %s, %ds", t.modelName, int(time.Since(start).Seconds())))
		return false, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("tabby did not answer within %s", t.cfg.ReadyTimeout.Duration)
	}
	return err
}

func maxMiB(m map[int]int) int {
	best := 0
	for _, v := range m {
		if v > best {
			best = v
		}
	}
	return best
}

// readYAMLTop reads "key: value" pairs at the top level of a flat YAML file.
func readYAMLTop(path string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.HasPrefix(line, " ") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[strings.TrimSpace(k)] = unquote(v)
		}
	}
	return out
}

// readYAMLKey reads one scalar key inside a top-level section.
func readYAMLKey(path, section, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	in := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			in = strings.TrimSuffix(trim, ":") == section
			continue
		}
		if in {
			if k, v, ok := strings.Cut(trim, ":"); ok && strings.TrimSpace(k) == key {
				return unquote(v)
			}
		}
	}
	return ""
}

func unquote(v string) string {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return strings.Trim(v, `"'`)
}
