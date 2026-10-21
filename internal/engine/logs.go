package engine

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// LogPath is where the output of an engine baton started goes.
func LogPath(stateDir, name string) string {
	return filepath.Join(stateDir, "logs", name+".log")
}

// rotateLog keeps one previous log once the current one passes 10 MB.
func rotateLog(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() > 10<<20 {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
}

// ReadLog returns the log from byte offset from (or its last ~200 KB when
// from is negative or past the end), cleaned for display, and the offset to
// ask for next.
func ReadLog(path string, from int64) (text string, next, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", 0, 0, err
	}
	size = st.Size()
	start := from
	tail := from < 0 || from > size
	if tail {
		start = size - 200<<10
		if start < 0 {
			start = 0
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", 0, size, err
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return "", 0, size, err
	}
	if tail && start > 0 {
		if i := strings.IndexByte(string(b), '\n'); i >= 0 {
			b, start = b[i+1:], start+int64(i+1)
		}
	}
	// Hand back only whole lines; a half-written one comes next time.
	if i := strings.LastIndexByte(string(b), '\n'); i >= 0 {
		b = b[:i+1]
	} else if !tail {
		b = nil
	}
	return CleanLog(b), start + int64(len(b)), size, nil
}

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)
	keyRe  = regexp.MustCompile(`(?i)(key is:)\s*\S+`)
)

// cp1252 for the bytes older logs wrote before baton forced UTF-8.
var cp1252 = map[byte]rune{0x85: '…', 0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x96: '–', 0x97: '—', 0xB7: '·', 0xB0: '°'}

// CleanLog makes raw engine output readable: valid UTF-8, no terminal
// escapes, progress-bar carriage returns resolved, secrets redacted.
func CleanLog(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			if c, ok := cp1252[b[0]]; ok {
				r = c
			}
		}
		sb.WriteRune(r)
		b = b[n:]
	}
	s := ansiRe.ReplaceAllString(sb.String(), "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if j := strings.LastIndexByte(l, '\r'); j >= 0 {
			l = l[j+1:] // a redrawn progress bar: keep what was drawn last
		}
		lines[i] = keyRe.ReplaceAllString(l, "$1 [redacted]")
	}
	return strings.Join(lines, "\n")
}

// TabbyMetrics is TabbyAPI's summary of one finished request.
type TabbyMetrics struct {
	At         string  `json:"at"`
	Request    string  `json:"request"`
	Tokens     int     `json:"tokens"`
	Decode     float64 `json:"decode_tps"`
	Prompt     int     `json:"prompt_tokens"`
	CachedPct  int     `json:"cached_pct"`
	PromptTPS  float64 `json:"prompt_tps,omitempty"`
	FirstToken float64 `json:"first_token_s,omitempty"`
	Total      float64 `json:"total_s,omitempty"`
	DraftPct   int     `json:"draft_pct,omitempty"`
	DraftOK    int     `json:"draft_accepted,omitempty"`
	DraftAll   int     `json:"draft_total,omitempty"`
}

var (
	mGen    = regexp.MustCompile(`^(\d\d:\d\d:\d\d)\S*\s+INFO:\s+(#\d+ [^:]+):\s+([\d,]+) tokens generated(?: at ([\d.]+) T/s)?`)
	mPrompt = regexp.MustCompile(`prompt ([\d,]+) tokens, (?:(\d+)%|(none|all)) cached(?:, ([\d,]+) new in ([\d.]+) s(?: \(([\d,.]+) T/s\))?)?`)
	mFirst  = regexp.MustCompile(`first token ([\d.]+) s`)
	mTotal  = regexp.MustCompile(`total ([\d.]+) s`)
	mDraft  = regexp.MustCompile(`draft (\d+)/(\d+) accepted \((\d+)%\)`)
)

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return f
}

// ParseTabbyMetrics finds the last request summary in a TabbyAPI log.
func ParseTabbyMetrics(text string) *TabbyMetrics {
	// Rejoin records the console wrapped onto indented continuation lines.
	var recs []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "          ") && len(recs) > 0 {
			recs[len(recs)-1] += " " + strings.TrimSpace(l)
		} else {
			recs = append(recs, l)
		}
	}
	for i := len(recs) - 1; i >= 0; i-- {
		m := mGen.FindStringSubmatch(recs[i])
		if m == nil {
			continue
		}
		r := recs[i]
		t := &TabbyMetrics{At: m[1], Request: m[2], Tokens: atoi(m[3]), Decode: atof(m[4])}
		if p := mPrompt.FindStringSubmatch(r); p != nil {
			t.Prompt = atoi(p[1])
			switch {
			case p[2] != "":
				t.CachedPct = atoi(p[2])
			case p[3] == "all":
				t.CachedPct = 100
			}
			if p[6] != "" {
				t.PromptTPS = atof(p[6])
			} else if secs := atof(p[5]); secs > 0 {
				// Short prompts print no rate; work it out.
				t.PromptTPS = float64(int(float64(atoi(p[4]))/secs*10+0.5)) / 10
			}
		}
		if p := mFirst.FindStringSubmatch(r); p != nil {
			t.FirstToken = atof(p[1])
		}
		if p := mTotal.FindStringSubmatch(r); p != nil {
			t.Total = atof(p[1])
		}
		if p := mDraft.FindStringSubmatch(r); p != nil {
			t.DraftOK, t.DraftAll, t.DraftPct = atoi(p[1]), atoi(p[2]), atoi(p[3])
		}
		return t
	}
	return nil
}

// tabbyReqs follows the request lines of TabbyAPI's log to tell which
// generations are still running. Only what follows baton's last start marker
// counts: TabbyAPI numbers its requests from #1 again each time it starts.
type tabbyReqs struct {
	marker  time.Time            // when baton last started TabbyAPI; zero if not seen
	running map[string]time.Time // request tag ("12", or "12.1" for one of n choices) -> start
	prompts map[string]int       // request tag -> its prompt tokens
	done    map[string]bool      // ended: reading its start again must not revive it
	pending string               // the last record; wrapped lines may still extend it
	applied bool                 // pending has been applied as it stands

	window, pool int             // context window and cache size of the last load
	model        string          // the model of the last load
	clock        time.Time       // the log's time, dated from the marker
	fin          []tabbyFinished // finished requests not collected yet
}

// tabbyFinished is a request TabbyAPI summed up when it finished.
type tabbyFinished struct {
	Tag    string
	Run    time.Time // the start marker it follows; zero if none was read
	At     time.Time
	M      TabbyMetrics
	Window int
	Model  string
}

const markerPrefix = "===== baton started "

var (
	rqLine   = regexp.MustCompile(`^(\d\d):(\d\d):(\d\d)\S*\s+(INFO|WARNING|ERROR):\s+(.*)$`)
	rqStart  = regexp.MustCompile(`^#(\d+(?:\.\d+)?) [^:]*: ([\d,]+) prompt tokens`)
	rqFinish = regexp.MustCompile(`^#(\d+(?:\.\d+)?) [^:]*: [\d,]+ tokens generated`)
	// A client that went away (noticed by non-stream requests), a Kobold
	// abort, or an error sent back to the request. These name the request
	// without a choice index, so they end all of its choices.
	rqEnd = regexp.MustCompile(`^(?:Sent to request: )?#(\d+)(?:\.\d+)? .*(?:client disconnected|cancelled|aborted)`)
	// What a model load reports: its file and its context and cache sizes.
	rqLoad    = regexp.MustCompile(`^Loading model (.+?)(?: \([^)]*\))?\s*$`)
	rqContext = regexp.MustCompile(`^Context: max_seq_len ([\d,]+) tokens.*?cache_size ([\d,]+) tokens`)
)

func (r *tabbyReqs) reset() {
	r.marker, r.running, r.done = time.Time{}, map[string]time.Time{}, map[string]bool{}
	r.prompts, r.window, r.pool, r.model, r.clock = map[string]int{}, 0, 0, "", time.Time{}
}

// take hands over the finished requests read since the last call.
func (r *tabbyReqs) take() []tabbyFinished {
	f := r.fin
	r.fin = nil
	return f
}

// feed takes the next whole lines of the log.
func (r *tabbyReqs) feed(text string, now time.Time) {
	if r.running == nil {
		r.reset()
	}
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return
	}
	for _, l := range strings.Split(text, "\n") {
		// Records the console wrapped at 80 columns go on in indented lines.
		if strings.HasPrefix(l, "          ") && r.pending != "" {
			r.pending += " " + strings.TrimSpace(l)
			r.applied = false
			continue
		}
		r.flush(now)
		r.pending, r.applied = l, false
	}
	// The last record counts now; if wrapped lines follow it is applied
	// again, which changes nothing for a start or an end already seen.
	r.flush(now)
}

func (r *tabbyReqs) flush(now time.Time) {
	if !r.applied {
		r.apply(r.pending, now)
		r.applied = true
	}
}

func (r *tabbyReqs) apply(rec string, now time.Time) {
	if strings.HasPrefix(rec, markerPrefix) {
		r.reset()
		s := strings.TrimSuffix(strings.TrimSpace(rec), "=====")
		if i := strings.LastIndex(s, " at "); i >= 0 {
			r.marker, _ = time.Parse(time.RFC3339, strings.TrimSpace(s[i+4:]))
		}
		return
	}
	m := rqLine.FindStringSubmatch(rec)
	if m == nil {
		return
	}
	at := r.stamp(m[1], m[2], m[3], now)
	level, msg := m[4], m[5]
	if level == "INFO" {
		if s := rqStart.FindStringSubmatch(msg); s != nil {
			if _, ok := r.running[s[1]]; !ok && !r.done[s[1]] {
				r.running[s[1]] = logClock(m[1], m[2], m[3], now)
				r.prompts[s[1]] = atoi(s[2])
			}
			return
		}
		if s := rqFinish.FindStringSubmatch(msg); s != nil {
			r.end(s[1])
			// A record applied again (wrapped lines extended it) is handed
			// over again; the receiver keeps the fuller copy.
			if met := ParseTabbyMetrics(rec); met != nil {
				r.fin = append(r.fin, tabbyFinished{Tag: s[1], Run: r.marker, At: at, M: *met, Window: r.window, Model: r.model})
			}
			return
		}
		if s := rqContext.FindStringSubmatch(msg); s != nil {
			r.window, r.pool = atoi(s[1]), atoi(s[2])
			return
		}
		if s := rqLoad.FindStringSubmatch(msg); s != nil {
			p := strings.TrimRight(s[1], `\/`)
			r.model = p[strings.LastIndexAny(p, `\/`)+1:]
			return
		}
	}
	switch {
	case strings.HasPrefix(msg, "Immediately terminating all jobs"), strings.HasPrefix(msg, "Model unloaded"):
		r.endAll()
	case strings.HasPrefix(msg, "Generation failed; error was contained"):
		// The failed request is not named; with one in flight it is that one.
		if len(r.running) == 1 {
			r.endAll()
		}
	case level != "INFO":
		if s := rqEnd.FindStringSubmatch(msg); s != nil {
			r.end(s[1])
			for k := range r.running {
				if strings.HasPrefix(k, s[1]+".") {
					r.end(k)
				}
			}
		}
	}
}

func (r *tabbyReqs) end(tag string) {
	delete(r.running, tag)
	delete(r.prompts, tag)
	r.done[tag] = true
}

// stamp dates a time of day from the log. After baton's start marker the
// log's own clock runs on from the marker's date, so a log read days later
// still dates its lines right; without a marker it is the latest such moment
// not after now.
func (r *tabbyReqs) stamp(h, m, s string, now time.Time) time.Time {
	base := r.clock
	if base.IsZero() {
		base = r.marker
	}
	if base.IsZero() {
		return logClock(h, m, s, now)
	}
	base = base.In(now.Location())
	y, mo, d := base.Date()
	t := time.Date(y, mo, d, atoi(h), atoi(m), atoi(s), 0, now.Location())
	if t.Before(base.Add(-time.Minute)) {
		t = t.AddDate(0, 0, 1) // past midnight
	}
	if t.After(now.Add(time.Minute)) {
		return logClock(h, m, s, now)
	}
	r.clock = t
	return t
}

func (r *tabbyReqs) endAll() {
	for k := range r.running {
		r.end(k)
	}
}

// expire ends requests that started longer than age ago: their end was
// missed (TabbyAPI logs none when a stream's client goes away).
func (r *tabbyReqs) expire(now time.Time, age time.Duration) {
	for k, at := range r.running {
		if now.Sub(at) > age {
			r.end(k)
		}
	}
}

// logClock dates a time of day from the log: the latest such moment not
// after now.
func logClock(h, m, s string, now time.Time) time.Time {
	y, mo, d := now.Date()
	t := time.Date(y, mo, d, atoi(h), atoi(m), atoi(s), 0, now.Location())
	if t.After(now.Add(time.Minute)) {
		t = t.AddDate(0, 0, -1)
	}
	return t
}
