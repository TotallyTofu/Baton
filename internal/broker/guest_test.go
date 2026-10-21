package broker

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"baton/internal/config"
	"baton/internal/detect"
)

// guest makes the test binary a guest named digup (on) or nothing (off), as
// DigUp's indexer starting and quitting would. Call it between ticks.
func (r *rig) guest(t *testing.T, on bool) {
	t.Helper()
	cfg := *r.b.cfg
	cfg.Claims = nil
	if on {
		cfg.Claims = []config.Claim{{Name: "digup", Exe: []string{filepath.Base(os.Args[0])}, Action: "guest"}}
	}
	det, err := detect.New(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.b.det = det
}

// setHold reserves the cards as SetHold does, without its tick in the
// background (which would race the detector swap in guest).
func (r *rig) setHold(on bool) {
	r.b.mu.Lock()
	r.b.manual = nil
	if on {
		r.b.manual = &manualHold{Reason: "game", Since: time.Now()}
	}
	r.b.mu.Unlock()
}

// The LLM gives the cards to a guest and comes back once it is done;
// ComfyUI, which loads per job, stays.
func TestGuestTakesLLMsAndRestores(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.comfy.set(func(f *fake) { f.load, f.vram = true, 3000 })
	r.b.tick(r.ctx, false)
	r.guest(t, true)
	r.settle(t)
	v := r.b.View(0)
	if v.Guest == nil || v.Guest.Why != "digup" {
		t.Fatalf("guest = %+v, want digup", v.Guest)
	}
	if v.Mode != Normal {
		t.Fatalf("mode = %s, want normal: a guest is not a mode", v.Mode)
	}
	if got, want := r.log.String(), "release tabby stop"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if v.RestoreTo != "tabby" {
		t.Fatalf("restore = %q, want tabby", v.RestoreTo)
	}
	var h *HeldError
	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); !errors.As(err, &h) || h.Mode != Guest {
		t.Fatalf("LLM during guest = %v, want a guest HeldError", err)
	}
	if h := r.b.Held("comfyui"); h != nil {
		t.Fatalf("ComfyUI held by a guest: %v", h)
	}
	r.guest(t, false)
	r.settle(t)
	// Tabby coming back takes its turn as any switch does: ComfyUI's models
	// make way for it then, not for the guest.
	if got, want := r.log.String(), "release tabby stop, release comfyui free, acquire tabby"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if v := r.b.View(0); v.Guest != nil || v.RestoreTo != "" {
		t.Fatalf("after guest: guest=%+v restore=%q", v.Guest, v.RestoreTo)
	}
}

// A guest may run through a game. The LLM waits for both: the game ending
// doesn't bring it back while the guest still runs.
func TestGuestOutlastsGame(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.b.tick(r.ctx, false)
	r.setHold(true)
	r.settle(t)
	r.guest(t, true)
	r.settle(t)
	if v := r.b.View(0); v.Mode != Hold || v.Guest == nil {
		t.Fatalf("mode=%s guest=%+v, want hold and a guest", v.Mode, v.Guest)
	}
	r.setHold(false)
	r.settle(t)
	if got := r.log.String(); strings.Contains(got, "acquire") {
		t.Fatalf("tabby came back while the guest runs: %q", got)
	}
	if r.b.View(0).RestoreTo != "tabby" {
		t.Fatal("tabby should still be marked for restore")
	}
	r.guest(t, false)
	r.settle(t)
	if got, want := r.log.String(), "release tabby stop, acquire tabby"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

// And the other way: the guest leaves during the game, the LLM comes back
// when the game ends.
func TestGuestLeavesDuringGame(t *testing.T) {
	r := newRig(t)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.b.tick(r.ctx, false)
	r.guest(t, true)
	r.settle(t)
	r.setHold(true)
	r.settle(t)
	r.guest(t, false)
	r.settle(t)
	if got := r.log.String(); strings.Contains(got, "acquire") {
		t.Fatalf("tabby came back during the game: %q", got)
	}
	r.setHold(false)
	r.settle(t)
	if got, want := r.log.String(), "release tabby stop, acquire tabby"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
}

// An LLM loaded by hand while a guest runs is released again.
func TestLLMLoadedDuringGuestIsReleased(t *testing.T) {
	r := newRig(t)
	r.guest(t, true)
	r.settle(t)
	r.unsloth.set(func(f *fake) { f.up, f.load, f.vram = true, true, 7000 })
	r.settle(t)
	r.settle(t)
	if got, want := r.log.String(), "release unsloth unload"; got != want {
		t.Fatalf("actions = %q, want %q", got, want)
	}
	if r.b.Ready("unsloth") {
		t.Fatal("an LLM must not be ready while a guest runs")
	}
}
