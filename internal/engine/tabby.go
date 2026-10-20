package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"baton/internal/config"
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
}

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
	return st
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
		ID string `json:"id"`
	}
	err := doJSON(pctx, "GET", t.cfg.URL+"/v1/model", t.hdr(false), nil, &card)
	var he *httpError
	switch {
	case err == nil:
		st.Up, st.Loaded = true, true
		if card.ID != "" {
			st.Model = card.ID
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
