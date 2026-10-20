// Package broker decides who holds the GPUs. Engines take turns: before one
// starts, every engine sharing a card with it finishes its work, gives the
// memory back, and the driver is checked to show it free. Games and streams
// override the engines.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"baton/internal/config"
	"baton/internal/detect"
	"baton/internal/engine"
	"baton/internal/events"
	"baton/internal/telemetry"
	"baton/internal/win"
)

type Mode string

const (
	Normal Mode = "normal" // engines take turns on demand
	Stream Mode = "stream" // a stream is live: ComfyUI held, LLM kept while VRAM allows
	Game   Mode = "game"   // a game runs: every engine is off the cards
	Hold   Mode = "hold"   // you asked for the cards (baton hold)
)

// HeldError says why an engine may not take the cards right now.
type HeldError struct {
	Mode Mode
	Why  string
}

func (e *HeldError) Error() string { return fmt.Sprintf("GPUs reserved (%s): %s", e.Mode, e.Why) }

// AgroundError says a released engine never gave its memory back, so nothing
// may start on those cards: it could run out of memory (strict_release).
type AgroundError struct {
	Engine string
	MiB    map[int]int // still held per GPU
	Waited time.Duration
	For    string // the engine that was to start, if any
}

func (e *AgroundError) Error() string {
	gpus := make([]int, 0, len(e.MiB))
	for g := range e.MiB {
		gpus = append(gpus, g)
	}
	sort.Ints(gpus)
	var held []string
	for _, g := range gpus {
		held = append(held, fmt.Sprintf("%.1f GiB on GPU %d", float64(e.MiB[g])/1024, g))
	}
	if len(held) == 0 {
		held = []string{"its memory"}
	}
	msg := fmt.Sprintf("%s still holds %s after %s", e.Engine, strings.Join(held, ", "), roughly(e.Waited))
	switch e.For {
	case "":
		msg += "; nothing else starts there until it lets go"
	case e.Engine:
		msg += "; not starting it again so it can't run out of memory"
	default:
		msg += fmt.Sprintf("; not starting %s so it can't run out of memory", e.For)
	}
	return msg + fmt.Sprintf(" (Force stop, or: baton reclaim %s)", e.Engine)
}

// roughly formats a wait for people: 45s, 3m, 1.5h.
func roughly(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	case d < 2*time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	default:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
}

// agroundRec is an engine that kept its memory after a release. It blocks
// its cards until the memory reads free or you reclaim it.
type agroundRec struct {
	since  time.Time
	waited time.Duration // the release wait that ran out
	mib    map[int]int   // still held per GPU, refreshed every cycle
	gpus   []int         // the cards it is stuck on
	procs  []win.Proc    // what it had when released: a killed process may linger
}

func (a *agroundRec) err(name, target string) *AgroundError {
	mib := make(map[int]int, len(a.mib))
	for g, v := range a.mib {
		mib[g] = v
	}
	return &AgroundError{Engine: name, MiB: mib, Waited: a.waited + time.Since(a.since), For: target}
}

type op struct {
	key    string
	name   string
	urgent bool
	fn     func(ctx context.Context) error
	done   chan struct{}
	err    error
	since  time.Time
}

type hookSig struct {
	kind  string
	since time.Time
}

// gateEntry is one ComfyUI job at the gate: when it arrived and when the node
// last asked about it (a node that stops asking is gone).
type gateEntry struct {
	first, last time.Time
}

type manualHold struct {
	Reason string    `json:"reason"`
	Until  time.Time `json:"until,omitempty"`
	Since  time.Time `json:"since"`
}

type Broker struct {
	cfg     *config.Config
	tel     *telemetry.Sampler
	ev      *events.Log
	det     *detect.Detector
	engines []engine.Engine
	byName  map[string]engine.Engine

	mu           sync.Mutex
	snap         *telemetry.Snapshot
	st           map[string]engine.Status
	mode         Mode
	modeWhy      string
	modeSince    time.Time
	hits         []detect.Hit
	lastGame     time.Time
	lastStream   time.Time
	manual       *manualHold
	hooks        map[string]hookSig
	holder       string
	restoreTo    string
	pendingRest  string
	headroomDone map[string]bool
	lowFree      int
	inflight     map[string]int
	gateWait     map[string]gateEntry
	gateRun      map[string]gateEntry
	comfyIdle    time.Time
	comfyTook    string
	cur          *op
	curStage     string
	curCancel    context.CancelFunc
	queue        []*op
	byKey        map[string]*op
	wake         chan struct{}
	noticed      map[string]time.Time
	since        map[string]time.Time // when each engine started occupying the cards
	aground      map[string]*agroundRec
	remote       func() (ip, errText string)
}

// SetRemote tells the view how to report the Tailscale address.
func (b *Broker) SetRemote(fn func() (string, string)) {
	b.mu.Lock()
	b.remote = fn
	b.mu.Unlock()
}

func New(cfg *config.Config, tel *telemetry.Sampler, ev *events.Log, det *detect.Detector, engines []engine.Engine) *Broker {
	b := &Broker{
		cfg: cfg, tel: tel, ev: ev, det: det, engines: engines,
		byName: map[string]engine.Engine{}, st: map[string]engine.Status{},
		mode: Normal, modeSince: time.Now(), hooks: map[string]hookSig{},
		headroomDone: map[string]bool{}, inflight: map[string]int{},
		gateWait: map[string]gateEntry{}, gateRun: map[string]gateEntry{},
		byKey: map[string]*op{}, wake: make(chan struct{}, 1), noticed: map[string]time.Time{},
		since: map[string]time.Time{}, aground: map[string]*agroundRec{},
	}
	for _, e := range engines {
		b.byName[e.Config().Name] = e
	}
	b.loadState()
	return b
}

// --- persisted state: what to restore, and a manual hold, survive restarts ---

type savedState struct {
	RestoreTo   string      `json:"restore_to,omitempty"`
	PendingRest string      `json:"pending_restore,omitempty"`
	Manual      *manualHold `json:"manual_hold,omitempty"`
}

func (b *Broker) statePath() string { return filepath.Join(b.cfg.Daemon.StateDir, "state.json") }

func (b *Broker) loadState() {
	raw, err := os.ReadFile(b.statePath())
	if err != nil {
		return
	}
	var s savedState
	if json.Unmarshal(raw, &s) == nil {
		b.restoreTo, b.pendingRest, b.manual = s.RestoreTo, s.PendingRest, s.Manual
	}
}

func (b *Broker) saveLocked() {
	_ = os.MkdirAll(b.cfg.Daemon.StateDir, 0o755)
	raw, _ := json.MarshalIndent(savedState{RestoreTo: b.restoreTo, PendingRest: b.pendingRest, Manual: b.manual}, "", "  ")
	_ = os.WriteFile(b.statePath(), raw, 0o644)
}

// --- the op worker: one change to the cards at a time ---

func (b *Broker) submit(key, name string, urgent bool, fn func(context.Context) error) *op {
	b.mu.Lock()
	defer b.mu.Unlock()
	if o, ok := b.byKey[key]; ok && key != "" {
		return o
	}
	o := &op{key: key, name: name, urgent: urgent, fn: fn, done: make(chan struct{}), since: time.Now()}
	if key != "" {
		b.byKey[key] = o
	}
	if urgent {
		// An urgent op (a game starting) cancels whatever is in progress and
		// jumps the queue.
		if b.cur != nil && !b.cur.urgent && b.curCancel != nil {
			b.curCancel()
		}
		b.queue = append([]*op{o}, b.queue...)
	} else {
		b.queue = append(b.queue, o)
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return o
}

func (b *Broker) worker(ctx context.Context) {
	for {
		b.mu.Lock()
		var o *op
		if len(b.queue) > 0 {
			o, b.queue = b.queue[0], b.queue[1:]
		}
		if o == nil {
			b.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-b.wake:
			}
			continue
		}
		octx, cancel := context.WithCancel(ctx)
		b.cur, b.curCancel, b.curStage = o, cancel, ""
		b.mu.Unlock()

		o.err = o.fn(octx)
		if errors.Is(o.err, context.Canceled) && ctx.Err() == nil {
			o.err = errors.New("cancelled: the GPUs were taken for something with higher priority")
		}
		cancel()

		b.mu.Lock()
		b.cur, b.curCancel, b.curStage = nil, nil, ""
		if b.byKey[o.key] == o {
			delete(b.byKey, o.key)
		}
		b.mu.Unlock()
		close(o.done)
	}
}

func (b *Broker) stage(format string, args ...any) {
	b.mu.Lock()
	b.curStage = fmt.Sprintf(format, args...)
	b.mu.Unlock()
}

func wait(ctx context.Context, o *op) error {
	select {
	case <-o.done:
		return o.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- the sense loop ---

// Run samples, detects and reconciles until ctx ends.
func (b *Broker) Run(ctx context.Context) {
	go b.worker(ctx)
	b.tick(ctx, true)
	t := time.NewTicker(b.cfg.Daemon.Poll.Duration)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.tick(ctx, false)
		}
	}
}

func (b *Broker) tick(ctx context.Context, first bool) {
	snap := b.tel.Sample()
	statuses := b.probeAll(ctx, snap)

	exclude := map[int]bool{}
	for _, s := range statuses {
		for _, pid := range s.PIDs {
			exclude[pid] = true
		}
	}
	hits := b.det.Scan(snap, exclude)
	now := time.Now()

	b.mu.Lock()
	b.snap, b.hits = snap, hits
	for _, s := range statuses {
		b.st[s.Name] = s
		b.settleAgroundLocked(s.Name, snap, s)
	}
	b.trackSinceLocked(statuses, first, now)
	if first {
		b.adoptHolderLocked()
	}
	b.pruneGateLocked(now)
	b.trackComfyLocked(now)
	want, why := b.desiredLocked(now)
	from := b.mode
	if want != from {
		b.mode, b.modeWhy, b.modeSince = want, why, now
	} else {
		b.modeWhy = why
	}
	b.mu.Unlock()

	if want != from {
		b.transition(from, want, why)
	} else if first && want == Normal && b.pendingRestore() {
		// A game or stream ended while baton was not running.
		b.transition(Game, Normal, "")
	}
	b.reconcile(now)
}

func (b *Broker) probeAll(ctx context.Context, snap *telemetry.Snapshot) []engine.Status {
	out := make([]engine.Status, len(b.engines))
	var wg sync.WaitGroup
	for i, e := range b.engines {
		wg.Add(1)
		go func(i int, e engine.Engine) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
			defer cancel()
			out[i] = e.Probe(pctx, snap)
		}(i, e)
	}
	wg.Wait()
	return out
}

// adoptHolderLocked works out who holds the cards when baton starts.
func (b *Broker) adoptHolderLocked() {
	for _, e := range b.engines {
		if s := b.st[e.Config().Name]; s.Loaded && e.IsLLM() {
			b.holder = e.Config().Name
			b.ev.Infof("start", b.holder, "found %s already holding the GPUs (%s)", b.holder, s.Model)
			return
		}
	}
	for _, e := range b.engines {
		if s := b.st[e.Config().Name]; s.Loaded {
			b.holder = e.Config().Name
			return
		}
	}
}

func (b *Broker) pendingRestore() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.restoreTo != ""
}

func (b *Broker) pruneGateLocked(now time.Time) {
	for id, g := range b.gateWait {
		if now.Sub(g.last) > 30*time.Second {
			delete(b.gateWait, id)
		}
	}
	for id, g := range b.gateRun {
		if now.Sub(g.first) > 6*time.Hour {
			delete(b.gateRun, id)
		}
	}
}

func (b *Broker) trackComfyLocked(now time.Time) {
	for _, e := range b.engines {
		if e.Config().Kind != "comfyui" {
			continue
		}
		s := b.st[e.Config().Name]
		if b.comfyBusyLocked(s) || len(b.gateRun) > 0 || len(b.gateWait) > 0 {
			b.comfyIdle = time.Time{}
		} else if b.comfyIdle.IsZero() {
			b.comfyIdle = now
		}
	}
}

func describe(hits []detect.Hit) string {
	seen := map[string]bool{}
	var parts []string
	for _, h := range hits {
		s := strings.TrimSuffix(h.Exe, ".exe")
		if h.Rule == "nvenc" {
			s += " (encoding)"
		}
		if !seen[s] {
			seen[s] = true
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, ", ")
}

func (b *Broker) desiredLocked(now time.Time) (Mode, string) {
	if b.manual != nil {
		if !b.manual.Until.IsZero() && now.After(b.manual.Until) {
			b.ev.Infof("mode", "", "hold expired: %s", b.manual.Reason)
			b.manual = nil
			b.saveLocked()
		} else {
			return Hold, b.manual.Reason
		}
	}
	var games, streams []detect.Hit
	nvenc := false
	for _, h := range b.hits {
		if h.Kind == "game" {
			games = append(games, h)
		} else {
			streams = append(streams, h)
			if h.Rule == "nvenc" {
				nvenc = true
			}
		}
	}
	for src, h := range b.hooks {
		switch {
		case h.kind == "game" && now.Sub(h.since) < 12*time.Hour:
			games = append(games, detect.Hit{Kind: "game", Rule: "hook", Exe: src})
		case h.kind == "stream" && (nvenc || now.Sub(h.since) < 3*time.Minute):
			// A hook whose stream never started encoding expires, so an
			// Apollo crash that skips the undo command cannot pin the mode.
			streams = append(streams, detect.Hit{Kind: "stream", Rule: "hook", Exe: src})
		case now.Sub(h.since) > 12*time.Hour || h.kind == "stream":
			delete(b.hooks, src)
		}
	}
	linger := b.cfg.Policy.Linger.Duration
	if len(games) > 0 {
		b.lastGame = now
		return Game, describe(games)
	}
	if b.mode == Game && now.Sub(b.lastGame) < linger {
		return Game, b.modeWhy
	}
	if len(streams) > 0 {
		b.lastStream = now
		return Stream, describe(streams)
	}
	if b.mode == Stream && now.Sub(b.lastStream) < linger {
		return Stream, b.modeWhy
	}
	return Normal, ""
}

func held(m Mode) bool { return m == Game || m == Hold }

func (b *Broker) transition(from, to Mode, why string) {
	switch {
	case held(to) && held(from):
		b.ev.Infof("mode", "", "%s -> %s: %s", from, to, why)
	case held(to):
		b.ev.Infof("mode", "", "%s: %s; freeing every GPU", strings.ToUpper(string(to)), why)
		b.submit("mode", "free GPUs for "+string(to), true, func(ctx context.Context) error {
			return b.freeFor(ctx, b.allGPUs(), b.cfg.Policy.Game.Drain.Duration, true, true)
		})
	case to == Stream && from == Normal:
		b.ev.Infof("mode", "", "STREAM: %s", why)
		b.submit("mode", "prepare for stream", true, func(ctx context.Context) error {
			return b.enterStream(ctx)
		})
	case to == Stream: // a game ended while the stream goes on
		b.ev.Infof("mode", "", "game over, still streaming (%s)", why)
		b.submit("mode", "restore after game", false, func(ctx context.Context) error {
			return b.restore(ctx, true)
		})
	case to == Normal:
		b.ev.Infof("mode", "", "back to normal after %s", from)
		b.submit("mode", "restore", false, func(ctx context.Context) error {
			return b.restore(ctx, false)
		})
	}
}

func (b *Broker) allGPUs() []int {
	set := map[int]bool{}
	if s := b.tel.Last(); s != nil {
		for _, g := range s.GPUs {
			set[g.Index] = true
		}
	}
	for _, e := range b.engines {
		for _, g := range e.Config().GPUs {
			set[g] = true
		}
	}
	var out []int
	for g := range set {
		out = append(out, g)
	}
	sort.Ints(out)
	return out
}

func overlap(a, b []int) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func (b *Broker) residual() int { return b.cfg.Policy.ResidualMiB }

// holds reports whether an engine occupies any of gpus.
func (b *Broker) holds(s engine.Status, gpus []int) bool {
	if s.Loaded || s.Loading {
		return true
	}
	for _, g := range gpus {
		if s.VRAMMiB[g] > b.residual() {
			return true
		}
	}
	return false
}

func (b *Broker) fresh(ctx context.Context, e engine.Engine) engine.Status {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	s := e.Probe(pctx, b.tel.Sample())
	b.mu.Lock()
	b.st[s.Name] = s
	b.mu.Unlock()
	return s
}

func (b *Broker) comfyBusyLocked(s engine.Status) bool {
	// A job parked at baton's gate shows as "running" in ComfyUI but is not
	// using the GPU.
	return s.Busy && len(b.gateWait) == 0
}

func (b *Broker) busy(e engine.Engine, s engine.Status) (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	name := e.Config().Name
	if n := b.inflight[name]; n > 0 {
		return true, fmt.Sprintf("%d request(s) through the gateway", n)
	}
	if e.Config().Kind == "comfyui" {
		if b.comfyBusyLocked(s) {
			return true, s.BusyWhy
		}
		return false, ""
	}
	if s.Busy {
		return true, s.BusyWhy
	}
	// GPU load stands in for requests that bypass the gateway, unless the
	// engine can see those requests itself (TabbyAPI, from its log).
	if e.IsLLM() && !s.BusyExact && s.Util >= b.cfg.Policy.BusyUtilPct {
		return true, fmt.Sprintf("GPU at %.0f%% (generating)", s.Util)
	}
	return false, ""
}

// drain waits until e has no work in flight, up to max. With interrupt, a
// ComfyUI job still running at the deadline is interrupted.
func (b *Broker) drain(ctx context.Context, e engine.Engine, max time.Duration, interrupt bool) error {
	name := e.Config().Name
	deadline := time.Now().Add(max)
	calm := 0
	for {
		s := b.fresh(ctx, e)
		busy, why := b.busy(e, s)
		if !busy {
			calm++
			if calm >= 2 {
				return nil
			}
		} else {
			calm = 0
			if time.Now().After(deadline) {
				if interrupt {
					if err := e.Interrupt(ctx); err == nil && e.Config().Kind == "comfyui" {
						_ = sleep(ctx, 2*time.Second)
					}
				}
				b.ev.Warnf("drain", name, "%s still busy after %s (%s); releasing anyway", name, max.Round(time.Second), why)
				return nil
			}
			b.stage("waiting for %s to finish: %s (gives up in %s)", name, why, time.Until(deadline).Round(time.Second))
		}
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// leftOn is the memory an engine still holds on gpus, per GPU above the
// residual. procs are processes it had when released: a killed process
// lingers while the driver tears it down, and by then it no longer looks like
// the engine (its port is closed), so it is tracked by PID and start time.
func (b *Broker) leftOn(snap *telemetry.Snapshot, s engine.Status, gpus []int, procs []win.Proc) map[int]int {
	counted := map[int]bool{}
	for _, pid := range s.PIDs {
		counted[pid] = true
	}
	var lingering []int
	if snap != nil && snap.Tree != nil {
		for _, p := range procs {
			if cur, ok := snap.Tree.ByPID[p.PID]; ok && !counted[p.PID] && (p.Start == 0 || cur.Start == p.Start) {
				counted[p.PID] = true
				lingering = append(lingering, p.PID)
			}
		}
	}
	var extra map[int]uint64
	if snap != nil {
		extra, _ = snap.VRAM(lingering)
	}
	out := map[int]int{}
	for _, g := range gpus {
		if v := s.VRAMMiB[g] + int(extra[g]>>20); v > b.residual() {
			out[g] = v
		}
	}
	return out
}

// waitFreed waits until the driver shows e's memory on gpus gone. before
// holds the processes the engine had when it was released. If the memory
// stays, strict_release marks the engine aground and returns *AgroundError.
func (b *Broker) waitFreed(ctx context.Context, e engine.Engine, gpus []int, before []win.Proc) error {
	name := e.Config().Name
	wait := b.cfg.Policy.ReleaseWait.Duration
	deadline := time.Now().Add(wait)
	start := time.Now()
	b.mu.Lock()
	if a := b.aground[name]; a != nil {
		// What it left behind last time still counts, or a reclaim that finds
		// no live engine would read as freed.
		before = append(append([]win.Proc(nil), before...), a.procs...)
	}
	b.mu.Unlock()
	for {
		snap := b.tel.Sample()
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		s := e.Probe(pctx, snap)
		cancel()
		b.mu.Lock()
		b.st[s.Name] = s
		b.mu.Unlock()
		held := b.leftOn(snap, s, gpus, before)
		left := 0
		for _, v := range held {
			left += v
		}
		if left == 0 {
			b.ev.Infof("release", name, "%s released its VRAM in %.1fs", name, time.Since(start).Seconds())
			b.mu.Lock()
			b.settleAgroundLocked(name, snap, s)
			b.mu.Unlock()
			return nil
		}
		if time.Now().After(deadline) {
			if !b.cfg.Policy.StrictRelease {
				b.ev.Warnf("release", name, "%s still holds %d MiB after %s; continuing", name, left, wait)
				return nil
			}
			cards := make([]int, 0, len(held))
			for g := range held {
				cards = append(cards, g)
			}
			sort.Ints(cards)
			b.mu.Lock()
			a := &agroundRec{since: time.Now(), waited: wait, mib: held, gpus: cards, procs: before}
			if old := b.aground[name]; old != nil {
				a.since, a.waited = old.since, old.waited
			}
			b.aground[name] = a
			err := a.err(name, "")
			b.mu.Unlock()
			b.ev.Warnf("release", name, "aground: %v", err)
			return err
		}
		b.stage("waiting for %s to free %d MiB", name, left)
		if err := sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// settleAgroundLocked drops name's aground mark once its memory reads free on
// the cards it was stuck on, and otherwise refreshes what it still holds.
func (b *Broker) settleAgroundLocked(name string, snap *telemetry.Snapshot, s engine.Status) {
	a := b.aground[name]
	if a == nil {
		return
	}
	if left := b.leftOn(snap, s, a.gpus, a.procs); len(left) > 0 {
		a.mib = left
		return
	}
	delete(b.aground, name)
	b.ev.Infof("release", name, "%s freed its memory; GPU(s) %v are usable again", name, a.gpus)
}

// agroundOn returns the error for an aground engine stuck on any of gpus,
// target included: starting it again beside its own leftovers can fail too.
func (b *Broker) agroundOn(gpus []int, target string) *AgroundError {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.engines {
		name := e.Config().Name
		if a := b.aground[name]; a != nil && overlap(a.gpus, gpus) {
			return a.err(name, target)
		}
	}
	return nil
}

func (b *Broker) releaseOne(ctx context.Context, e engine.Engine, mode string, gpus []int) error {
	name := e.Config().Name
	s := b.fresh(ctx, e)
	before := s.Procs()
	b.stage("releasing %s (%s)", name, mode)
	b.ev.Infof("release", name, "releasing %s (%s)", name, mode)
	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err := e.Release(rctx, mode, s)
	cancel()
	if err != nil {
		b.ev.Errorf("release", name, "release %s failed: %v", name, err)
		return fmt.Errorf("release %s: %w", name, err)
	}
	// It gave up the cards even if its memory is stuck (aground).
	werr := b.waitFreed(ctx, e, gpus, before)
	b.mu.Lock()
	delete(b.since, name)
	if b.holder == name {
		b.holder = ""
	}
	b.mu.Unlock()
	return werr
}

// freeFor releases every engine on gpus. remember records the LLM that held
// them so it can come back.
func (b *Broker) freeFor(ctx context.Context, gpus []int, drainMax time.Duration, game, remember bool) error {
	var firstErr error
	for _, e := range b.engines {
		if !overlap(e.Config().GPUs, gpus) {
			continue
		}
		s := b.fresh(ctx, e)
		if !b.holds(s, gpus) {
			continue
		}
		if remember && e.IsLLM() && s.Loaded {
			b.mu.Lock()
			if b.restoreTo == "" || b.restoreTo == e.Config().Name {
				b.restoreTo = e.Config().Name
				b.saveLocked()
			}
			b.mu.Unlock()
		}
		if err := b.drain(ctx, e, drainMax, true); err != nil {
			return err
		}
		mode := e.Config().Release
		if game {
			mode = e.Config().GameRelease
		}
		// An aground engine is recorded and reported, but the rest still
		// release: a game can't wait for it, and nothing starts here.
		if err := b.releaseOne(ctx, e, mode, gpus); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *Broker) enterStream(ctx context.Context) error {
	p := b.cfg.Policy.Stream
	if p.HoldComfy {
		for _, e := range b.engines {
			if e.Config().Kind != "comfyui" {
				continue
			}
			s := b.fresh(ctx, e)
			if !b.holds(s, e.Config().GPUs) {
				continue
			}
			if err := b.drain(ctx, e, p.ComfyDrain.Duration, true); err != nil {
				return err
			}
			if err := b.releaseOne(ctx, e, "free", e.Config().GPUs); err != nil {
				return err
			}
		}
	}
	return nil
}

// restore brings back the LLM a game or stream pushed out. During a stream
// only LLMs come back; ComfyUI stays held.
func (b *Broker) restore(ctx context.Context, streaming bool) error {
	b.mu.Lock()
	name := b.restoreTo
	b.headroomDone = map[string]bool{}
	b.lowFree = 0
	waiters := len(b.gateWait)
	if c := b.comfyEngine(); c == nil || !b.st[c.Config().Name].Up {
		waiters = 0 // nothing will run them
	}
	if !b.cfg.Policy.Restore || name == "" {
		b.restoreTo = ""
		b.saveLocked()
		b.mu.Unlock()
		return nil
	}
	if !streaming && waiters > 0 {
		// ComfyUI jobs waited out the game; let them run first.
		b.pendingRest, b.restoreTo = name, ""
		b.saveLocked()
		b.mu.Unlock()
		b.ev.Infof("restore", name, "%d ComfyUI job(s) waited; %s comes back once they finish", waiters, name)
		return nil
	}
	b.restoreTo = ""
	b.saveLocked()
	b.mu.Unlock()
	e := b.byName[name]
	if e == nil {
		return nil
	}
	b.ev.Infof("restore", name, "restoring %s", name)
	return b.doActivate(ctx, e, "", "restore")
}

// reconcile runs the per-cycle checks that are not mode changes.
func (b *Broker) reconcile(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	busyOp := b.cur != nil || len(b.queue) > 0

	// Stream headroom: drop the LLM when the encoding card runs short.
	if b.mode == Stream && !busyOp && b.snap != nil {
		p := b.cfg.Policy.Stream
		if g := b.snap.GPUByIndex(p.EncodeGPU); g != nil {
			if g.MemFree < uint64(p.MinFreeMiB)<<20 {
				b.lowFree++
			} else {
				b.lowFree = 0
			}
			if b.lowFree >= 2 {
				for _, e := range b.engines {
					name := e.Config().Name
					s := b.st[name]
					if !e.IsLLM() || !s.Loaded || b.headroomDone[name] || !overlap(e.Config().GPUs, []int{p.EncodeGPU}) {
						continue
					}
					b.headroomDone[name] = true
					if b.restoreTo == "" {
						b.restoreTo = name
						b.saveLocked()
					}
					free := int(g.MemFree >> 20)
					b.ev.Infof("stream", name, "GPU %d has %d MiB free (< %d) while streaming; releasing %s", p.EncodeGPU, free, p.MinFreeMiB, name)
					eng := e
					go b.submit("stream-headroom:"+name, "release "+name+" for stream headroom", false, func(ctx context.Context) error {
						if err := b.drain(ctx, eng, 30*time.Second, false); err != nil {
							return err
						}
						return b.releaseOne(ctx, eng, eng.Config().Release, eng.Config().GPUs)
					})
					break
				}
			}
		}
	}

	// A job ComfyUI ran without the gate while the cards were reserved.
	if b.mode != Normal {
		for _, e := range b.engines {
			if e.Config().Kind == "comfyui" && b.comfyBusyLocked(b.st[e.Config().Name]) && len(b.gateRun) == 0 {
				if (held(b.mode) || b.cfg.Policy.Stream.HoldComfy) && b.noticeLocked("comfy-nogate", 10*time.Minute) {
					b.ev.Warnf("gate", e.Config().Name, "ComfyUI is running a job during %s; install the baton gate node so jobs wait", b.mode)
				}
			}
		}
	}

	// Loads that happened outside baton: the newest wins, the rest release.
	if !busyOp {
		b.claimLocked()
	}

	if b.mode != Normal || busyOp || len(b.gateRun) > 0 || len(b.gateWait) > 0 || b.comfyIdle.IsZero() {
		return
	}
	idle := now.Sub(b.comfyIdle)
	// Restore deferred behind ComfyUI jobs that waited out a game or stream.
	if b.pendingRest != "" && (idle >= 30*time.Second || !b.comfyUpLocked()) {
		name := b.pendingRest
		b.pendingRest = ""
		b.saveLocked()
		go b.Activate(context.Background(), name, "", "restore after ComfyUI")
		return
	}
	// Give the cards back to the LLM ComfyUI took them from.
	if ret := b.cfg.Policy.ComfyIdleRet.Duration; ret > 0 && b.comfyTook != "" && b.holderIsComfyLocked() && idle >= ret {
		name := b.comfyTook
		b.comfyTook = ""
		b.ev.Infof("switch", name, "ComfyUI idle for %s; giving the GPUs back to %s", ret, name)
		go b.Activate(context.Background(), name, "", "ComfyUI idle")
	}
}

func (b *Broker) comfyUpLocked() bool {
	c := b.comfyEngine()
	return c != nil && b.st[c.Config().Name].Up
}

func (b *Broker) holderIsComfyLocked() bool {
	e := b.byName[b.holder]
	return e != nil && e.Config().Kind == "comfyui"
}

func (b *Broker) noticeLocked(key string, every time.Duration) bool {
	if t, ok := b.noticed[key]; ok && time.Since(t) < every {
		return false
	}
	b.noticed[key] = time.Now()
	return true
}

// --- what callers use ---

// heldFor says whether target may take the cards in the current mode.
func (b *Broker) heldFor(target engine.Engine) *HeldError {
	b.mu.Lock()
	defer b.mu.Unlock()
	if held(b.mode) {
		return &HeldError{b.mode, b.modeWhy}
	}
	if b.mode == Stream && target.Config().Kind == "comfyui" && b.cfg.Policy.Stream.HoldComfy {
		return &HeldError{b.mode, "ComfyUI waits while streaming (" + b.modeWhy + ")"}
	}
	return nil
}

func (b *Broker) doActivate(ctx context.Context, target engine.Engine, model, why string) error {
	name := target.Config().Name
	if h := b.heldFor(target); h != nil {
		return h
	}
	// Never push the holder out for an engine that cannot come up.
	if ts := b.fresh(ctx, target); !ts.Up && !ts.Loading && !ts.CanStart {
		err := fmt.Errorf("%s is not running and has no start command", name)
		if target.Config().Kind == "comfyui" {
			err = fmt.Errorf("%s is not running (open ComfyUI Desktop)", name)
		} else if target.Config().Kind == "unsloth" {
			err = fmt.Errorf("%s is not running (open Unsloth Studio)", name)
		}
		return err
	} else if pf, ok := target.(engine.Preflighter); ok {
		if err := pf.Preflight(model, ts); err != nil {
			return err
		}
	}
	gpus := target.Config().GPUs
	// Memory an engine kept after its release is still in use: refuse before
	// pushing anyone else out.
	if ag := b.agroundOn(gpus, name); ag != nil {
		return ag
	}
	for _, e := range b.engines {
		if e == target || !overlap(e.Config().GPUs, gpus) {
			continue
		}
		s := b.fresh(ctx, e)
		if !b.holds(s, gpus) {
			continue
		}
		b.ev.Infof("switch", name, "%s needs GPU(s) %v; %s holds them", name, gpus, e.Config().Name)
		if err := b.drain(ctx, e, b.cfg.Policy.SwitchDrain.Duration, false); err != nil {
			return err
		}
		if err := b.releaseOne(ctx, e, e.Config().Release, gpus); err != nil {
			var ag *AgroundError
			if errors.As(err, &ag) {
				ag.For = name
			}
			return err
		}
		if target.Config().Kind == "comfyui" && e.IsLLM() && s.Loaded {
			b.mu.Lock()
			b.comfyTook = e.Config().Name
			b.mu.Unlock()
		}
	}
	if h := b.heldFor(target); h != nil {
		return h
	}
	b.stage("starting %s", name)
	start := time.Now()
	err := target.Acquire(ctx, model, func(s string) { b.stage("%s", s) })
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			b.ev.Errorf("start", name, "%s failed to start: %v", name, err)
		}
		return err
	}
	b.mu.Lock()
	prev := b.holder
	b.holder = name
	b.since[name] = time.Now()
	if target.IsLLM() {
		b.comfyTook = ""
	}
	b.mu.Unlock()
	if prev != name || time.Since(start) > 2*time.Second {
		b.ev.Infof("switch", name, "%s holds the GPUs (%s, ready in %.1fs)", name, why, time.Since(start).Seconds())
	}
	return nil
}

// Activate gives the cards to an engine, releasing whatever shares them, and
// waits until it is ready or ctx ends.
func (b *Broker) Activate(ctx context.Context, name, model, why string) error {
	e := b.byName[name]
	if e == nil {
		return fmt.Errorf("no engine %q", name)
	}
	if h := b.heldFor(e); h != nil {
		return h
	}
	o := b.submit("activate:"+name+":"+model, "switch to "+name, false, func(ctx context.Context) error {
		return b.doActivate(ctx, e, model, why)
	})
	return wait(ctx, o)
}

// Ready reports whether name can serve right now without a switch.
func (b *Broker) Ready(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if held(b.mode) || b.cur != nil || len(b.queue) > 0 {
		return false
	}
	s := b.st[name]
	e := b.byName[name]
	if e == nil || !s.Up || (e.IsLLM() && !s.Loaded) {
		return false
	}
	for _, o := range b.engines {
		if o != e && overlap(o.Config().GPUs, e.Config().GPUs) && b.st[o.Config().Name].Loaded {
			return false
		}
	}
	return true
}

// ReleaseEngine frees one engine's GPUs now (no drain: you asked).
func (b *Broker) ReleaseEngine(ctx context.Context, name string) error {
	e := b.byName[name]
	if e == nil {
		return fmt.Errorf("no engine %q", name)
	}
	o := b.submit("release:"+name, "release "+name, false, func(ctx context.Context) error {
		return b.releaseOne(ctx, e, e.Config().Release, e.Config().GPUs)
	})
	return wait(ctx, o)
}

// Reclaim force-stops an engine whatever its release mode (Berth's reclaim),
// with no drain: you asked. Its aground mark goes once the memory reads free.
func (b *Broker) Reclaim(ctx context.Context, name string) error {
	e := b.byName[name]
	if e == nil {
		return fmt.Errorf("no engine %q", name)
	}
	o := b.submit("reclaim:"+name, "force stop "+name, false, func(ctx context.Context) error {
		return b.releaseOne(ctx, e, "stop", e.Config().GPUs)
	})
	return wait(ctx, o)
}

// FreeAll releases every engine without reserving the cards.
func (b *Broker) FreeAll(ctx context.Context) error {
	o := b.submit("free-all", "free all GPUs", false, func(ctx context.Context) error {
		return b.freeFor(ctx, b.allGPUs(), b.cfg.Policy.Game.Drain.Duration, false, false)
	})
	return wait(ctx, o)
}

// SetHold reserves the cards for you until ClearHold or d passes (0 = no limit).
func (b *Broker) SetHold(reason string, d time.Duration) {
	if reason == "" {
		reason = "manual hold"
	}
	b.mu.Lock()
	b.manual = &manualHold{Reason: reason, Since: time.Now()}
	if d > 0 {
		b.manual.Until = time.Now().Add(d)
	}
	b.saveLocked()
	b.mu.Unlock()
	b.ev.Infof("mode", "", "hold requested: %s", reason)
	go b.tick(context.Background(), false)
}

func (b *Broker) ClearHold() {
	b.mu.Lock()
	was := b.manual
	b.manual = nil
	b.saveLocked()
	b.mu.Unlock()
	if was != nil {
		b.ev.Infof("mode", "", "hold released: %s", was.Reason)
	}
	go b.tick(context.Background(), false)
}

// Refresh runs a detection cycle now instead of at the next poll.
func (b *Broker) Refresh() { go b.tick(context.Background(), false) }

// Hook records a start/end signal from Apollo's prep commands or a script.
func (b *Broker) Hook(event, source string) error {
	if source == "" {
		source = "hook"
	}
	kind, on := "", false
	switch event {
	case "stream-start":
		kind, on = "stream", true
	case "stream-end":
		kind = "stream"
	case "game-start":
		kind, on = "game", true
	case "game-end":
		kind = "game"
	default:
		return fmt.Errorf("unknown hook event %q", event)
	}
	b.mu.Lock()
	if on {
		b.hooks[source] = hookSig{kind: kind, since: time.Now()}
	} else {
		delete(b.hooks, source)
	}
	b.mu.Unlock()
	b.ev.Infof(kind, "", "hook %s from %s", event, source)
	go b.tick(context.Background(), false)
	return nil
}

// Begin and End count gateway requests in flight per engine.
func (b *Broker) Begin(name string) {
	b.mu.Lock()
	b.inflight[name]++
	b.mu.Unlock()
}

func (b *Broker) End(name string) {
	b.mu.Lock()
	if b.inflight[name] > 0 {
		b.inflight[name]--
	}
	b.mu.Unlock()
}

// Route finds the engine serving a model name.
func (b *Broker) Route(model string) (engine.Engine, string, error) {
	for _, e := range b.engines {
		if !e.IsLLM() || !e.Config().IsEnabled() {
			continue
		}
		if ok, up := e.Serves(model); ok {
			return e, up, nil
		}
	}
	if d := b.cfg.Gateway.DefaultEngine; d != "" {
		if e := b.byName[d]; e != nil {
			return e, model, nil
		}
	}
	return nil, "", fmt.Errorf("no engine serves model %q", model)
}

// Models lists every model name the gateway answers to.
func (b *Broker) Models() []ModelEntry {
	var out []ModelEntry
	seen := map[string]bool{}
	add := func(id, eng string) {
		if id != "" && !seen[strings.ToLower(id)] {
			seen[strings.ToLower(id)] = true
			out = append(out, ModelEntry{ID: id, Engine: eng})
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.engines {
		if !e.IsLLM() {
			continue
		}
		c := e.Config()
		for _, s := range c.Serves {
			add(s, c.Name)
		}
		for _, m := range c.Models {
			add(m.Name, c.Name)
		}
		if s := b.st[c.Name]; s.Model != "" {
			add(s.Model, c.Name)
		}
	}
	return out
}

type ModelEntry struct {
	ID     string `json:"id"`
	Engine string `json:"engine"`
}

// --- the ComfyUI gate ---

// GateAcquire is called by the ComfyUI node before each job runs. It returns
// within about wait: granted, or not yet and why.
func (b *Broker) GateAcquire(ctx context.Context, promptID string, maxWait time.Duration) (bool, string) {
	comfy := b.comfyEngine()
	if comfy == nil {
		return true, "no comfyui engine configured"
	}
	name := comfy.Config().Name
	b.mu.Lock()
	g, ok := b.gateWait[promptID]
	if !ok {
		g.first = time.Now()
	}
	g.last = time.Now()
	b.gateWait[promptID] = g
	b.comfyIdle = time.Time{}
	b.mu.Unlock()

	deadline := time.Now().Add(maxWait)
	for {
		if h := b.heldFor(comfy); h != nil {
			b.mu.Lock()
			first := b.noticeLocked("gate-held:"+promptID, time.Hour)
			b.mu.Unlock()
			if first {
				b.ev.Infof("gate", name, "ComfyUI job %s waits: %s", short(promptID), h.Why)
			}
			if time.Now().After(deadline) {
				return false, h.Error()
			}
			if sleep(ctx, 500*time.Millisecond) != nil {
				return false, "cancelled"
			}
			continue
		}
		// Checked before Ready: ComfyUI allocates per job, so even a ComfyUI
		// that is up could run out of memory beside an aground engine.
		if ag := b.agroundOn(comfy.Config().GPUs, name); ag != nil {
			b.mu.Lock()
			first := b.noticeLocked("gate-aground:"+promptID, time.Hour)
			b.mu.Unlock()
			if first {
				b.ev.Infof("gate", name, "ComfyUI job %s waits: %s", short(promptID), ag.Error())
			}
			if time.Now().After(deadline) {
				return false, ag.Error()
			}
			if sleep(ctx, 500*time.Millisecond) != nil {
				return false, "cancelled"
			}
			continue
		}
		if b.Ready(name) {
			b.grant(promptID)
			return true, ""
		}
		o := b.submit("activate:"+name+":", "switch to "+name, false, func(ctx context.Context) error {
			return b.doActivate(ctx, comfy, "", "ComfyUI job queued")
		})
		left := time.Until(deadline)
		if left <= 0 {
			return false, b.stageText()
		}
		wctx, cancel := context.WithTimeout(ctx, left)
		err := wait(wctx, o)
		cancel()
		if err == nil {
			b.grant(promptID)
			return true, ""
		}
		var h *HeldError
		var ag *AgroundError
		if errors.As(err, &h) || errors.As(err, &ag) {
			continue // the checks above keep the job waiting
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return false, b.stageText()
		}
		return false, "error: " + err.Error()
	}
}

func (b *Broker) grant(promptID string) {
	b.mu.Lock()
	delete(b.gateWait, promptID)
	b.gateRun[promptID] = gateEntry{first: time.Now(), last: time.Now()}
	b.mu.Unlock()
}

func (b *Broker) GateDone(promptID string) {
	b.mu.Lock()
	delete(b.gateRun, promptID)
	delete(b.gateWait, promptID)
	if len(b.gateRun) == 0 && len(b.gateWait) == 0 {
		b.comfyIdle = time.Now()
	}
	b.mu.Unlock()
}

func (b *Broker) comfyEngine() engine.Engine {
	for _, e := range b.engines {
		if e.Config().Kind == "comfyui" {
			return e
		}
	}
	return nil
}

func (b *Broker) stageText() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cur == nil {
		return "waiting for the GPUs"
	}
	if b.curStage != "" {
		return b.curStage
	}
	return b.cur.name
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
