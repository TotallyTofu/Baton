package broker

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"baton/internal/config"
	"baton/internal/detect"
	"baton/internal/engine"
	"baton/internal/events"
	"baton/internal/telemetry"
)

// fake is an engine whose state the test controls.
type fake struct {
	cfg   config.Engine
	llm   bool
	mu    sync.Mutex
	up    bool
	load  bool
	busy  bool
	vram  int
	stuck bool // a release short of "stop" leaves its VRAM in place
	disk  bool // keeps a disk KV cache: FlushKV waits
	log   *actionLog
}

func (f *fake) FlushKV(_ context.Context, mode string, _ engine.Status, max time.Duration, _ func(string)) (engine.FlushResult, bool) {
	f.mu.Lock()
	disk := f.disk && f.load
	f.mu.Unlock()
	if !disk || mode != "stop" {
		return engine.FlushResult{}, false
	}
	f.log.add("flush " + f.cfg.Name + " " + max.String())
	return engine.FlushResult{Secs: 0.1, Files: 3, WroteMiB: 12}, true
}

type actionLog struct {
	mu   sync.Mutex
	acts []string
}

func (l *actionLog) add(s string) {
	l.mu.Lock()
	l.acts = append(l.acts, s)
	l.mu.Unlock()
}

func (l *actionLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.acts, ", ")
}

func (f *fake) Config() config.Engine           { return f.cfg }
func (f *fake) IsLLM() bool                     { return f.llm }
func (f *fake) Upstream() (string, http.Header) { return "", nil }
func (f *fake) Serves(m string) (bool, string)  { return m == f.cfg.Name, m }

func (f *fake) Probe(context.Context, *telemetry.Snapshot) engine.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := map[int]int{}
	for _, g := range f.cfg.GPUs {
		v[g] = f.vram
	}
	return engine.Status{Name: f.cfg.Name, Kind: f.cfg.Kind, GPUs: f.cfg.GPUs, Up: f.up, Loaded: f.load, Busy: f.busy, BusyWhy: "job", VRAMMiB: v, CanStart: true}
}

func (f *fake) Release(_ context.Context, mode string, _ engine.Status) error {
	f.mu.Lock()
	f.load, f.busy = false, false
	if !f.stuck || mode == "stop" {
		f.vram = 0
	}
	if mode == "stop" {
		f.up = false
	}
	f.mu.Unlock()
	f.log.add("release " + f.cfg.Name + " " + mode)
	return nil
}

func (f *fake) Acquire(context.Context, string, func(string)) error {
	f.mu.Lock()
	f.up = true
	if f.llm {
		f.load, f.vram = true, 9000
	}
	f.mu.Unlock()
	f.log.add("acquire " + f.cfg.Name)
	return nil
}

func (f *fake) Interrupt(context.Context) error {
	f.mu.Lock()
	f.busy = false
	f.mu.Unlock()
	f.log.add("interrupt " + f.cfg.Name)
	return nil
}

func (f *fake) set(fn func(f *fake)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

type rig struct {
	b                     *Broker
	tabby, unsloth, comfy *fake
	log                   *actionLog
	ctx                   context.Context
}

func newRig(t *testing.T) *rig {
	t.Helper()
	cfg := config.Default()
	cfg.Daemon.StateDir = t.TempDir()
	cfg.Policy.Linger = config.D(0)
	cfg.Policy.ReleaseWait = config.D(1 * time.Second)
	cfg.Policy.SwitchDrain = config.D(5 * time.Second)
	cfg.Policy.Game.Drain = config.D(1 * time.Second)
	cfg.Policy.Stream.ComfyDrain = config.D(1 * time.Second)
	cfg.Detect.Games = false
	cfg.Detect.StreamEncoders = nil
	log := &actionLog{}
	mk := func(name, kind string, llm bool, release, game string) *fake {
		return &fake{cfg: config.Engine{Name: name, Kind: kind, GPUs: []int{0, 1}, Release: release, GameRelease: game}, llm: llm, log: log}
	}
	r := &rig{
		tabby:   mk("tabby", "tabby", true, "stop", "stop"),
		unsloth: mk("unsloth", "unsloth", true, "unload", "unload"),
		comfy:   mk("comfyui", "comfyui", false, "free", "free"),
		log:     log,
	}
	r.comfy.up = true
	det, err := detect.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.b = New(cfg, telemetry.NewSampler(), events.Open(""), det, []engine.Engine{r.tabby, r.unsloth, r.comfy})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.ctx = ctx
	go r.b.worker(ctx)
	r.b.tick(ctx, true)
	return r
}

func (r *rig) settle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		r.b.tick(r.ctx, false)
		r.b.mu.Lock()
		idle := r.b.cur == nil && len(r.b.queue) == 0
		r.b.mu.Unlock()
		if idle {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("broker never settled")
}

func TestSwitchReleasesHolderFirst(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.b.tick(r.ctx, false)
	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log.String(), "release tabby stop, acquire unsloth"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if r.b.View(0).Holder != "unsloth" {
		t.Fatal("unsloth should hold the GPUs")
	}
}

// An engine with a disk KV cache gets kv_flush_wait to finish writing it
// before it is stopped; a game or hold allows only urgentFlushCap, and a
// force stop none.
func TestFlushKVBeforeStop(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram, f.disk = true, true, 12000, true })
	r.b.tick(r.ctx, false)
	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log.String(), "flush tabby 20s, release tabby stop, acquire unsloth"; got != want {
		t.Fatalf("switch: actions = %q, want %q", got, want)
	}

	r = newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram, f.disk = true, true, 12000, true })
	r.b.tick(r.ctx, false)
	r.b.SetHold("game", 0)
	r.settle(t)
	if got, want := r.log.String(), "flush tabby 5s, release tabby stop"; got != want {
		t.Fatalf("hold: actions = %q, want %q", got, want)
	}

	r = newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram, f.disk = true, true, 12000, true })
	r.b.tick(r.ctx, false)
	if err := r.b.Reclaim(r.ctx, "tabby"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log.String(), "release tabby stop"; got != want {
		t.Fatalf("force stop: actions = %q, want %q", got, want)
	}

	r = newRig(t)
	r.b.cfg.Policy.KVFlushWait = config.D(0)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram, f.disk = true, true, 12000, true })
	r.b.tick(r.ctx, false)
	if err := r.b.ReleaseEngine(r.ctx, "tabby"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log.String(), "release tabby stop"; got != want {
		t.Fatalf("kv_flush_wait 0: actions = %q, want %q", got, want)
	}
}

func TestSwitchWaitsForRunningComfyJob(t *testing.T) {
	r := newRig(t)
	r.comfy.set(func(f *fake) { f.load, f.busy, f.vram = true, true, 6000 })
	start := time.Now()
	go func() {
		time.Sleep(1500 * time.Millisecond)
		r.comfy.set(func(f *fake) { f.busy = false })
	}()
	if err := r.b.Activate(r.ctx, "tabby", "", "test"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 1500*time.Millisecond {
		t.Fatal("switch did not wait for the ComfyUI job")
	}
	if got, want := r.log.String(), "release comfyui free, acquire tabby"; got != want {
		t.Fatalf("actions = %q, want %q (a switch must not interrupt)", got, want)
	}
}

func TestHoldFreesEverythingAndRestores(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.b.tick(r.ctx, false)
	r.b.SetHold("game", 0)
	r.settle(t)
	if r.b.View(0).Mode != Hold {
		t.Fatal("mode should be hold")
	}
	if got := r.log.String(); got != "release tabby stop" {
		t.Fatalf("actions = %q", got)
	}
	var h *HeldError
	if err := r.b.Activate(r.ctx, "tabby", "", "test"); !errors.As(err, &h) {
		t.Fatalf("activate during hold = %v, want HeldError", err)
	}
	if r.b.View(0).RestoreTo != "tabby" {
		t.Fatal("tabby should be marked for restore")
	}
	r.b.ClearHold()
	r.settle(t)
	if got, want := r.log.String(), "release tabby stop, acquire tabby"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestGameInterruptsComfyAfterDrain(t *testing.T) {
	r := newRig(t)
	r.comfy.set(func(f *fake) { f.load, f.busy, f.vram = true, true, 6000 })
	r.b.tick(r.ctx, false)
	r.b.SetHold("game", 0)
	r.settle(t)
	if got, want := r.log.String(), "interrupt comfyui, release comfyui free"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestStreamHoldsComfyKeepsLLM(t *testing.T) {
	r := newRig(t)
	r.comfy.set(func(f *fake) { f.load, f.vram = true, 5000 })
	r.b.tick(r.ctx, false)
	if err := r.b.Hook("stream-start", "apollo"); err != nil {
		t.Fatal(err)
	}
	r.settle(t)
	if r.b.View(0).Mode != Stream {
		t.Fatalf("mode = %s, want stream", r.b.View(0).Mode)
	}
	if got := r.log.String(); got != "release comfyui free" {
		t.Fatalf("actions = %q", got)
	}
	ok, why := r.b.GateAcquire(r.ctx, "p1", 300*time.Millisecond)
	if ok || !strings.Contains(why, "streaming") {
		t.Fatalf("gate during stream = %v %q, want held", ok, why)
	}
	// The LLM may still start during a stream.
	if err := r.b.Activate(r.ctx, "tabby", "", "test"); err != nil {
		t.Fatalf("LLM during stream: %v", err)
	}
	_ = r.b.Hook("stream-end", "apollo")
	r.settle(t)
	ok, why = r.b.GateAcquire(r.ctx, "p1", 3*time.Second)
	if !ok {
		t.Fatalf("gate after stream = %q", why)
	}
	if got, want := r.log.String(), "release comfyui free, acquire tabby, release tabby stop, acquire comfyui"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func TestRestoreWaitsForQueuedComfyJobs(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.b.tick(r.ctx, false)
	r.b.SetHold("game", 0)
	r.settle(t)
	if ok, _ := r.b.GateAcquire(r.ctx, "job1", 200*time.Millisecond); ok {
		t.Fatal("gate granted during hold")
	}
	r.b.ClearHold()
	r.settle(t)
	v := r.b.View(0)
	if v.RestoreTo != "tabby" || strings.Contains(r.log.String(), "acquire tabby") {
		t.Fatalf("restore should wait for the ComfyUI job: restore=%q actions=%q", v.RestoreTo, r.log.String())
	}
	if ok, why := r.b.GateAcquire(r.ctx, "job1", 3*time.Second); !ok {
		t.Fatalf("job should run after the hold: %q", why)
	}
}

func TestNoReleaseForEngineThatCannotStart(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.comfy.set(func(f *fake) { f.up = false })
	r.b.tick(r.ctx, false)
	// fake reports CanStart; make comfy unstartable for this test.
	stuck := &noStart{r.comfy}
	r.b.engines[2], r.b.byName["comfyui"] = stuck, stuck
	if err := r.b.Activate(r.ctx, "comfyui", "", "test"); err == nil {
		t.Fatal("activating a stopped engine without a start command should fail")
	}
	if got := r.log.String(); got != "" {
		t.Fatalf("holder was touched: %q", got)
	}
}

type noStart struct{ *fake }

func (n *noStart) Probe(ctx context.Context, s *telemetry.Snapshot) engine.Status {
	st := n.fake.Probe(ctx, s)
	st.CanStart = false
	return st
}

// Your case: Tabby holds the GPUs, then a model is loaded from Unsloth
// Studio's own UI. Tabby must be released without anyone clicking anything.
func TestLoadOutsideBatonReleasesHolder(t *testing.T) {
	r := newRig(t)
	if err := r.b.Activate(r.ctx, "tabby", "", "test"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	r.unsloth.set(func(f *fake) { f.up, f.load, f.vram = true, true, 7000 })
	r.settle(t)
	r.settle(t)
	if got, want := r.log.String(), "acquire tabby, release tabby stop"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if h := r.b.View(0).Holder; h != "unsloth" {
		t.Fatalf("holder = %q, want unsloth", h)
	}
}

// And the reverse: Tabby started by hand while Unsloth has a model.
func TestTabbyStartedByHandReleasesUnsloth(t *testing.T) {
	r := newRig(t)
	r.unsloth.set(func(f *fake) { f.up, f.load, f.vram = true, true, 7000 })
	r.settle(t)
	time.Sleep(20 * time.Millisecond)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.settle(t)
	r.settle(t)
	if got, want := r.log.String(), "release unsloth unload"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

// Something loaded during a game is released again.
func TestLoadDuringGameIsReleased(t *testing.T) {
	r := newRig(t)
	r.b.SetHold("game", 0)
	r.settle(t)
	r.unsloth.set(func(f *fake) { f.up, f.load, f.vram = true, true, 7000 })
	r.settle(t)
	r.settle(t)
	if got, want := r.log.String(), "release unsloth unload"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

func engineView(v View, name string) EngineView {
	for _, e := range v.Engines {
		if e.Name == name {
			return e
		}
	}
	return EngineView{}
}

// stuckTabby holds both cards and keeps its memory through an unload.
func stuckTabby(r *rig) {
	r.tabby.cfg.Release, r.tabby.cfg.GameRelease = "unload", "unload"
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram, f.stuck = true, true, 12000, true })
}

// A release that leaves memory behind runs the engine aground: the next
// engine never starts, later tries are refused at once, and ComfyUI jobs wait.
func TestStuckReleaseRunsAground(t *testing.T) {
	r := newRig(t)
	stuckTabby(r)
	r.b.tick(r.ctx, false)
	err := r.b.Activate(r.ctx, "unsloth", "", "test")
	var ag *AgroundError
	if !errors.As(err, &ag) || ag.Engine != "tabby" || ag.For != "unsloth" || ag.MiB[0] != 12000 {
		t.Fatalf("activate = %v, want tabby aground", err)
	}
	if !strings.Contains(err.Error(), "not starting unsloth") || !strings.Contains(err.Error(), "baton reclaim tabby") {
		t.Fatalf("message = %q", err)
	}
	if got, want := r.log.String(), "release tabby unload"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if a := engineView(r.b.View(0), "tabby").Aground; a == nil || a.MiB[1] != 12000 || a.TotalMiB != 24000 {
		t.Fatalf("view aground = %+v", a)
	}
	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); !errors.As(err, &ag) {
		t.Fatalf("second activate = %v, want AgroundError", err)
	}
	ok, why := r.b.GateAcquire(r.ctx, "p1", 300*time.Millisecond)
	if ok || strings.HasPrefix(why, "error: ") || !strings.Contains(why, "still holds") {
		t.Fatalf("gate = %v %q, want the job kept waiting", ok, why)
	}
	if got, want := r.log.String(), "release tabby unload"; got != want {
		t.Fatalf("refusals touched engines: %q", got)
	}
	// Memory that frees on its own clears the mark on the next cycle.
	r.tabby.set(func(f *fake) { f.vram = 0 })
	r.b.tick(r.ctx, false)
	if a := engineView(r.b.View(0), "tabby").Aground; a != nil {
		t.Fatalf("aground should clear once the memory is free: %+v", a)
	}
}

// Force stop (baton reclaim) ends a stuck engine whatever its release mode;
// the mark clears and the switch goes through.
func TestReclaimClearsAground(t *testing.T) {
	r := newRig(t)
	stuckTabby(r)
	r.b.tick(r.ctx, false)
	var ag *AgroundError
	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); !errors.As(err, &ag) {
		t.Fatalf("activate = %v, want AgroundError", err)
	}
	if err := r.b.Reclaim(r.ctx, "tabby"); err != nil {
		t.Fatal(err)
	}
	if a := engineView(r.b.View(0), "tabby").Aground; a != nil {
		t.Fatalf("reclaim should clear aground: %+v", a)
	}
	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log.String(), "release tabby unload, release tabby stop, acquire unsloth"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

// strict_release = false: warn and start the next engine anyway.
func TestLaxReleaseContinues(t *testing.T) {
	r := newRig(t)
	r.b.cfg.Policy.StrictRelease = false
	stuckTabby(r)
	r.b.tick(r.ctx, false)
	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); err != nil {
		t.Fatal(err)
	}
	if got, want := r.log.String(), "release tabby unload, acquire unsloth"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if a := engineView(r.b.View(0), "tabby").Aground; a != nil {
		t.Fatalf("no aground without strict_release: %+v", a)
	}
}

// A game or hold can't be blocked: the stuck engine is marked aground and the
// others are still released.
func TestHoldReleasesOthersPastAground(t *testing.T) {
	r := newRig(t)
	stuckTabby(r)
	r.comfy.set(func(f *fake) { f.load, f.vram = true, 6000 })
	r.b.tick(r.ctx, false)
	r.b.SetHold("game", 0)
	r.settle(t)
	if got, want := r.log.String(), "release tabby unload, release comfyui free"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	v := r.b.View(0)
	if v.Mode != Hold || engineView(v, "tabby").Aground == nil || engineView(v, "comfyui").Aground != nil {
		t.Fatalf("mode = %s, tabby aground = %v, comfyui aground = %v", v.Mode, engineView(v, "tabby").Aground, engineView(v, "comfyui").Aground)
	}
}
