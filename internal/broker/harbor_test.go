package broker

import (
	"errors"
	"testing"
	"time"

	"baton/internal/detect"
	"baton/internal/harbor"
	"baton/internal/telemetry"
)

func TestHistoryCountsHandoffsAndVoyages(t *testing.T) {
	r := newRig(t)
	rec := harbor.Open("")
	r.b.SetRecorder(rec)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.b.tick(r.ctx, false)

	if err := r.b.Activate(r.ctx, "unsloth", "", "test"); err != nil {
		t.Fatal(err)
	}
	c := rec.Counters()
	if c.Handoffs != 1 || c.HandedMiB != 24000 || c.Switches != 1 {
		t.Fatalf("after a switch: %+v", c)
	}
	v := rec.Voyages(5)
	if len(v) != 1 || v[0].Name != "switch to unsloth" || v[0].Result != "ok" || v[0].FreedMiB != 24000 || v[0].No == 0 {
		t.Fatalf("voyages: %+v", v)
	}

	// A release you asked for frees memory but avoids no collision.
	if err := r.b.ReleaseEngine(r.ctx, "unsloth"); err != nil {
		t.Fatal(err)
	}
	c = rec.Counters()
	if c.Handoffs != 1 || c.HandedMiB != 24000+18000 {
		t.Fatalf("after a manual release: %+v", c)
	}
	if v := rec.Voyages(1); v[0].No <= 1 || v[0].Name != "release unsloth" {
		t.Fatalf("ticket numbers should rise: %+v", v)
	}

	// A switch while the cards are reserved is refused and counted.
	r.b.SetHold("test", 0)
	r.settle(t)
	var h *HeldError
	if err := r.b.Activate(r.ctx, "tabby", "", "test"); !errors.As(err, &h) {
		t.Fatalf("activate during hold = %v", err)
	}
	if c := rec.Counters(); c.Refused != 1 || c.Yields != 1 {
		t.Fatalf("after a refused switch: %+v", c)
	}
}

func TestHistoryTimelineAndBoard(t *testing.T) {
	r := newRig(t)
	rec := harbor.Open("")
	r.b.SetRecorder(rec)
	r.tabby.set(func(f *fake) { f.up, f.load, f.vram = true, true, 12000 })
	r.b.tick(r.ctx, false)
	r.b.mu.Lock()
	r.b.holder = "tabby"
	r.b.mu.Unlock()

	hv := r.b.Harbor(time.Hour)
	if len(hv.Timeline) != 1 || hv.Timeline[0].Owner != "tabby" || hv.Timeline[0].Label != "tabby" || len(hv.Timeline[0].GPUs) != 2 {
		t.Fatalf("timeline: %+v", hv.Timeline)
	}
	var docked *Departure
	for i := range hv.Board {
		if hv.Board[i].Vessel == "tabby" {
			docked = &hv.Board[i]
		}
	}
	if docked == nil || docked.Status != "docked" || docked.HoldMiB != 24000 || docked.Since.IsZero() {
		t.Fatalf("board: %+v", hv.Board)
	}
	if len(hv.Tide) == 0 {
		t.Fatal("the tide should have a reading of the machine's memory")
	}

	// Engine requests: the pool rows and where their figures come from.
	rv := r.b.Requests(24 * time.Hour)
	if len(rv.Pools) != 2 || rv.Sources["tabby"] == "" {
		t.Fatalf("requests view: pools %+v sources %v", rv.Pools, rv.Sources)
	}
}

// A venv's python.exe and the interpreter it starts are one guest on the
// board, holding what the interpreter allocated.
func TestBoardGuestOneRow(t *testing.T) {
	r := newRig(t)
	r.b.mu.Lock()
	r.b.hits = []detect.Hit{
		{Kind: "guest", Rule: "qwen3-tts", PID: 10, Exe: "python.exe"},
		{Kind: "guest", Rule: "qwen3-tts", PID: 11, Exe: "python.exe"},
	}
	r.b.snap = &telemetry.Snapshot{ByPID: map[int]map[int]telemetry.Use{11: {0: {Dedicated: 6 << 30}}}}
	r.b.mu.Unlock()
	var rows []Departure
	for _, d := range r.b.Harbor(time.Hour).Board {
		if d.Kind == "guest" {
			rows = append(rows, d)
		}
	}
	if len(rows) != 1 || rows[0].Vessel != "qwen3-tts" || rows[0].HoldMiB != 6144 || len(rows[0].GPUs) != 1 || rows[0].GPUs[0] != 0 {
		t.Fatalf("guest rows: %+v", rows)
	}
}

func TestGameTitle(t *testing.T) {
	cases := []struct {
		h    detect.Hit
		want string
	}{
		{detect.Hit{Exe: "SB-Win64-Shipping.exe", Path: `D:\Games\Stellar Blade\SB\Binaries\SB-Win64-Shipping.exe`, Match: `D:\Games`}, "Stellar Blade"},
		{detect.Hit{Exe: "eldenring.exe", Path: `C:\Program Files (x86)\Steam\steamapps\common\ELDEN RING\Game\eldenring.exe`}, "ELDEN RING"},
		{detect.Hit{Exe: "ping.exe", Path: `C:\Windows\System32\PING.EXE`, Match: `C:\Windows\System32\PING.EXE`}, "ping"},
	}
	for _, c := range cases {
		if got := GameTitle(c.h); got != c.want {
			t.Errorf("GameTitle(%s) = %q, want %q", c.h.Path, got, c.want)
		}
	}
}

func TestParseRange(t *testing.T) {
	for in, want := range map[string]time.Duration{"": time.Hour, "6h": 6 * time.Hour, "7d": 7 * 24 * time.Hour, "90d": 7 * 24 * time.Hour, "junk": time.Hour} {
		if got := ParseRange(in, time.Hour, 7*24*time.Hour); got != want {
			t.Errorf("ParseRange(%q) = %v, want %v", in, got, want)
		}
	}
	if rangeName(7*24*time.Hour) != "7d" || rangeName(24*time.Hour) != "24h" || rangeName(time.Hour) != "1h" {
		t.Error("rangeName")
	}
}
