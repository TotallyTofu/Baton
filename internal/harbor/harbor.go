// Package harbor keeps the history the dashboard draws, in bounded memory:
// memory over time (the tide), who held each card (the model timeline), the
// changes baton made to the cards (departures), what it caught on the way
// (the ticket counter) and the requests the engines served (the ledger).
// Counters, requests, spans and voyages are saved to the state dir now and
// then, so a restart keeps them; the tide starts over.
package harbor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	fineStep   = 5 * time.Second
	fineKeep   = time.Hour
	coarseStep = time.Minute
	coarseKeep = 24 * time.Hour
	maxPoints  = 480 // a longer range is averaged down to this many
	spanKeep   = 24 * time.Hour
	maxSpans   = 600
	spanGrace  = 8 * time.Second // an occupant missing this long has left
	maxVoyages = 60
	reqKeep    = 7 * 24 * time.Hour
	maxReqs    = 5000
	saveEvery  = time.Minute
)

// Counters are what the broker caught, since Since.
type Counters struct {
	Since     time.Time `json:"since"`
	Handoffs  int64     `json:"handoffs"`   // a card cleared before the next user got it
	HandedMiB int64     `json:"handed_mib"` // driver-measured memory those releases gave back
	Drains    int64     `json:"drains"`     // releases that first waited for work in flight
	Aground   int64     `json:"aground"`    // releases whose memory stayed (strict_release)
	Claims    int64     `json:"claims"`     // loads outside baton settled by releasing the rest
	Refused   int64     `json:"refused"`    // switches turned down: cards reserved or aground
	Yields    int64     `json:"yields"`     // games, holds and streams the cards were cleared for
	GateHeld  int64     `json:"gate_held"`  // ComfyUI jobs that waited at the gate
	Switches  int64     `json:"switches"`   // an engine took the cards
	Requests  int64     `json:"requests"`   // finished requests recorded
	Generated int64     `json:"generated"`  // tokens
	Fresh     int64     `json:"fresh"`      // prompt tokens read fresh
	Cached    int64     `json:"cached"`     // prompt tokens served from cache
}

// Level is one series' reading at a moment: a GPU's memory or system RAM.
type Level struct {
	Key   string         // "gpu0", "ram"
	Name  string         // what the legend calls it
	Total int            // MiB
	Used  int            // MiB
	By    map[string]int // holder -> MiB
}

// TidePoint is one reading of a series.
type TidePoint struct {
	T    int64          `json:"t"` // unix seconds
	Used int            `json:"used"`
	By   map[string]int `json:"by,omitempty"`
}

// TideSeries is a series over a range.
type TideSeries struct {
	Key    string      `json:"key"`
	Name   string      `json:"name"`
	Total  int         `json:"total"`
	Step   int         `json:"step_s"`
	Points []TidePoint `json:"points"`
}

// Occupant is something on the cards now: an engine with its model, a game,
// a stream, a manual hold.
type Occupant struct {
	Kind  string // engine kind, or "game", "stream", "hold"
	Owner string // engine name, or the kind again
	Label string // model, game title, hold reason
	GPUs  []int
}

// Span is one stay on the cards: a row of the model timeline.
type Span struct {
	Kind  string    `json:"kind"`
	Owner string    `json:"owner"`
	Label string    `json:"label"`
	GPUs  []int     `json:"gpus,omitempty"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end,omitzero"` // zero while it lasts
}

func (s Span) key() string { return s.Owner + "\x00" + s.Label }

// Voyage is one finished change to the cards (an op of the broker).
type Voyage struct {
	No       int64     `json:"no"`
	Name     string    `json:"name"`
	Start    time.Time `json:"start"`
	Dur      float64   `json:"dur_s"`
	Result   string    `json:"result"` // ok, refused, aground, failed, cancelled
	Detail   string    `json:"detail,omitempty"`
	FreedMiB int       `json:"freed_mib,omitempty"`
}

// Request is one finished request, as an engine's log or the gateway saw it.
type Request struct {
	Key       string    `json:"key,omitempty"` // a record read again (a log after a restart) replaces itself
	At        time.Time `json:"at"`            // when it finished
	Engine    string    `json:"engine"`
	Model     string    `json:"model,omitempty"`
	Source    string    `json:"source"` // log or gateway
	Route     string    `json:"route,omitempty"`
	Status    int       `json:"status"`
	Prompt    int       `json:"prompt"`
	Cached    int       `json:"cached"`
	Output    int       `json:"output"`
	PromptTPS float64   `json:"prompt_tps,omitempty"`
	DecodeTPS float64   `json:"decode_tps,omitempty"`
	FirstTok  float64   `json:"first_token_s,omitempty"`
	Total     float64   `json:"total_s,omitempty"`
	DraftOK   int       `json:"draft_ok,omitempty"`
	DraftAll  int       `json:"draft_all,omitempty"`
	Window    int       `json:"window,omitempty"` // the model's context window then
	Error     string    `json:"error,omitempty"`
}

// OK reports whether the request was served.
func (q Request) OK() bool { return q.Status == 0 || q.Status/100 == 2 }

type series struct {
	name   string
	total  int
	fine   []TidePoint
	coarse []TidePoint
	acc    accum // the minute being averaged
}

type accum struct {
	minute int64
	n      int
	used   int
	by     map[string]int
}

// Recorder holds the history. A nil *Recorder records nothing.
type Recorder struct {
	mu      sync.Mutex
	path    string
	c       Counters
	ticket  int64
	tide    map[string]*series
	order   []string
	spans   []Span
	missing map[string]time.Time
	voyages []Voyage
	reqs    []Request
	byKey   map[string]int
	dirty   bool
	saved   time.Time
}

type savedFile struct {
	Counters Counters  `json:"counters"`
	Ticket   int64     `json:"ticket"`
	Spans    []Span    `json:"spans"`
	Voyages  []Voyage  `json:"voyages"`
	Requests []Request `json:"requests"`
}

// Open loads what an earlier run saved in dir ("" keeps nothing on disk).
func Open(dir string) *Recorder {
	r := &Recorder{tide: map[string]*series{}, missing: map[string]time.Time{}, byKey: map[string]int{}}
	r.c.Since = time.Now()
	if dir == "" {
		return r
	}
	r.path = filepath.Join(dir, "harbor.json")
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return r
	}
	var s savedFile
	if json.Unmarshal(raw, &s) != nil {
		return r
	}
	if !s.Counters.Since.IsZero() {
		r.c = s.Counters
	}
	r.ticket, r.voyages, r.reqs = s.Ticket, s.Voyages, s.Requests
	// Stays still open when baton stopped ended by its last save.
	end := time.Now()
	if st, err := os.Stat(r.path); err == nil {
		end = st.ModTime()
	}
	for _, sp := range s.Spans {
		if sp.End.IsZero() {
			sp.End = end
		}
		r.spans = append(r.spans, sp)
	}
	r.reindex()
	r.saved = time.Now()
	return r
}

// --- counters ---

// Count changes the counters.
func (r *Recorder) Count(f func(c *Counters)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	f(&r.c)
	r.dirty = true
	r.mu.Unlock()
}

func (r *Recorder) Counters() Counters {
	if r == nil {
		return Counters{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.c
}

// --- the tide ---

// Tide records a reading of every series. Readings closer than the fine step
// to the last one are dropped.
func (r *Recorder) Tide(at time.Time, levels []Level) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	t := at.Unix()
	for _, l := range levels {
		s := r.tide[l.Key]
		if s == nil {
			s = &series{}
			r.tide[l.Key] = s
			r.order = append(r.order, l.Key)
		}
		s.name, s.total = l.Name, l.Total
		if n := len(s.fine); n > 0 && t-s.fine[n-1].T < int64(fineStep/time.Second) {
			continue
		}
		p := TidePoint{T: t, Used: l.Used, By: copyBy(l.By)}
		s.fine = trimPoints(append(s.fine, p), t-int64(fineKeep/time.Second))
		s.addCoarse(p)
	}
}

func (s *series) addCoarse(p TidePoint) {
	m := p.T / int64(coarseStep/time.Second)
	if s.acc.n > 0 && m != s.acc.minute {
		s.coarse = trimPoints(append(s.coarse, s.acc.point()), p.T-int64(coarseKeep/time.Second))
		s.acc = accum{}
	}
	if s.acc.n == 0 {
		s.acc = accum{minute: m, by: map[string]int{}}
	}
	s.acc.n++
	s.acc.used += p.Used
	for k, v := range p.By {
		s.acc.by[k] += v
	}
}

func (a accum) point() TidePoint {
	p := TidePoint{T: a.minute * int64(coarseStep/time.Second), Used: a.used / a.n}
	for k, v := range a.by {
		if p.By == nil {
			p.By = map[string]int{}
		}
		p.By[k] = v / a.n
	}
	return p
}

func copyBy(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

// trimPoints drops points before cutoff (unix seconds).
func trimPoints(ps []TidePoint, cutoff int64) []TidePoint {
	i := sort.Search(len(ps), func(i int) bool { return ps[i].T >= cutoff })
	if i == 0 {
		return ps
	}
	return append(ps[:0:0], ps[i:]...)
}

// TideView returns every series over the last rng, at most maxPoints each.
func (r *Recorder) TideView(rng time.Duration, now time.Time) []TideSeries {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := now.Add(-rng).Unix()
	out := make([]TideSeries, 0, len(r.order))
	for _, key := range r.order {
		s := r.tide[key]
		src, step := s.fine, int(fineStep/time.Second)
		if rng > fineKeep {
			src, step = s.coarse, int(coarseStep/time.Second)
			if s.acc.n > 0 {
				src = append(append([]TidePoint(nil), src...), s.acc.point())
			}
		}
		i := sort.Search(len(src), func(i int) bool { return src[i].T >= cutoff })
		pts, k := downsample(src[i:], maxPoints)
		out = append(out, TideSeries{Key: key, Name: s.name, Total: s.total, Step: step * k, Points: pts})
	}
	return out
}

// downsample averages runs of points so at most max remain; it returns the
// points and how many were averaged into each.
func downsample(ps []TidePoint, max int) ([]TidePoint, int) {
	if len(ps) <= max {
		return append([]TidePoint(nil), ps...), 1
	}
	k := (len(ps) + max - 1) / max
	out := make([]TidePoint, 0, len(ps)/k+1)
	for i := 0; i < len(ps); i += k {
		j := min(i+k, len(ps))
		a := accum{n: j - i, by: map[string]int{}}
		for _, p := range ps[i:j] {
			a.used += p.Used
			for key, v := range p.By {
				a.by[key] += v
			}
		}
		p := a.point()
		p.T = ps[i].T
		out = append(out, p)
	}
	return out, k
}

// --- the model timeline ---

// Occupants reconciles the open stays with what is on the cards now. A stay
// ends once its occupant has been gone for spanGrace, so a probe that misses
// once does not split it.
func (r *Recorder) Occupants(now time.Time, occ []Occupant) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	present := map[string]Occupant{}
	for _, o := range occ {
		present[Span{Owner: o.Owner, Label: o.Label}.key()] = o
	}
	open := map[string]int{}
	for i, s := range r.spans {
		if !s.End.IsZero() {
			continue
		}
		k := s.key()
		open[k] = i
		if o, ok := present[k]; ok {
			delete(r.missing, k)
			r.spans[i].GPUs = unionInts(s.GPUs, o.GPUs)
			continue
		}
		gone, ok := r.missing[k]
		if !ok {
			r.missing[k] = now
			continue
		}
		if now.Sub(gone) >= spanGrace {
			r.spans[i].End = gone
			delete(r.missing, k)
			r.dirty = true
		}
	}
	for k, o := range present {
		if _, ok := open[k]; ok {
			continue
		}
		r.spans = append(r.spans, Span{Kind: o.Kind, Owner: o.Owner, Label: o.Label, GPUs: unionInts(nil, o.GPUs), Start: now})
		r.dirty = true
	}
	r.pruneSpans(now)
}

func (r *Recorder) pruneSpans(now time.Time) {
	cut := now.Add(-spanKeep)
	keep := r.spans[:0]
	for _, s := range r.spans {
		if s.End.IsZero() || s.End.After(cut) {
			keep = append(keep, s)
		}
	}
	r.spans = keep
	if len(r.spans) > maxSpans {
		r.spans = append([]Span(nil), r.spans[len(r.spans)-maxSpans:]...)
	}
}

func unionInts(a, b []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, v := range append(append([]int(nil), a...), b...) {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out
}

// Timeline returns the stays that overlap the last rng, oldest first.
func (r *Recorder) Timeline(rng time.Duration, now time.Time) []Span {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := now.Add(-rng)
	var out []Span
	for _, s := range r.spans {
		if s.End.IsZero() || s.End.After(cut) {
			out = append(out, s)
		}
	}
	return out
}

// --- departures ---

// Ticket numbers the next change to the cards.
func (r *Recorder) Ticket() int64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ticket++
	r.dirty = true
	return r.ticket
}

// Voyage records a finished change to the cards.
func (r *Recorder) Voyage(v Voyage) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.voyages = append(r.voyages, v)
	if len(r.voyages) > maxVoyages {
		r.voyages = append([]Voyage(nil), r.voyages[len(r.voyages)-maxVoyages:]...)
	}
	r.dirty = true
}

// Voyages returns the last n, newest first.
func (r *Recorder) Voyages(n int) []Voyage {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Voyage, 0, min(n, len(r.voyages)))
	for i := len(r.voyages) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, r.voyages[i])
	}
	return out
}

// --- requests ---

// Request records a finished request. One with the Key of a record already
// kept replaces it: a log read again after a restart, or a record the log
// went on to complete.
func (r *Recorder) Request(q Request) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if q.At.Before(now.Add(-reqKeep)) {
		return
	}
	if i, ok := r.byKey[q.Key]; ok && q.Key != "" {
		r.tally(r.reqs[i], -1)
		r.reqs[i] = q
		r.tally(q, 1)
		r.dirty = true
		return
	}
	// Records arrive close to in order; keep the slice sorted by time.
	i := len(r.reqs)
	for i > 0 && r.reqs[i-1].At.After(q.At) {
		i--
	}
	r.reqs = append(r.reqs, Request{})
	copy(r.reqs[i+1:], r.reqs[i:])
	r.reqs[i] = q
	r.tally(q, 1)
	r.prune(now)
	r.reindex()
	r.dirty = true
}

// tally adds (sign 1) or takes back (-1) a request from the lifetime totals.
func (r *Recorder) tally(q Request, sign int64) {
	if !q.OK() {
		return
	}
	r.c.Requests += sign
	r.c.Generated += sign * int64(q.Output)
	r.c.Cached += sign * int64(q.Cached)
	r.c.Fresh += sign * int64(max(q.Prompt-q.Cached, 0))
}

func (r *Recorder) prune(now time.Time) {
	cut := now.Add(-reqKeep)
	i := sort.Search(len(r.reqs), func(i int) bool { return !r.reqs[i].At.Before(cut) })
	i = max(i, len(r.reqs)-maxReqs)
	if i > 0 {
		r.reqs = append([]Request(nil), r.reqs[i:]...)
	}
}

func (r *Recorder) reindex() {
	clear(r.byKey)
	for i, q := range r.reqs {
		if q.Key != "" {
			r.byKey[q.Key] = i
		}
	}
}

// Requests returns the requests that finished after since, oldest first.
func (r *Recorder) Requests(since time.Time) []Request {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i := sort.Search(len(r.reqs), func(i int) bool { return r.reqs[i].At.After(since) })
	return append([]Request(nil), r.reqs[i:]...)
}

// --- saving ---

// MaybeSave writes the history if it changed and the last save is a while ago.
func (r *Recorder) MaybeSave() {
	if r == nil {
		return
	}
	r.mu.Lock()
	due := r.dirty && time.Since(r.saved) >= saveEvery
	r.mu.Unlock()
	if due {
		_ = r.Save()
	}
}

// Save writes the history now (through a temporary file, so a crash mid-write
// keeps the last good copy).
func (r *Recorder) Save() error {
	if r == nil || r.path == "" {
		return nil
	}
	r.mu.Lock()
	raw, err := json.Marshal(savedFile{Counters: r.c, Ticket: r.ticket, Spans: r.spans, Voyages: r.voyages, Requests: r.reqs})
	r.dirty, r.saved = false, time.Now()
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}
