package engine

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	mGen    = regexp.MustCompile(`^(\d\d:\d\d:\d\d)\S*\s+INFO:\s+(#\d+ [^:]+):\s+([\d,]+) tokens generated at ([\d.]+) T/s`)
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
