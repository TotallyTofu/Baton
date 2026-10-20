package broker

import (
	"testing"

	"baton/internal/config"
	"baton/internal/engine"
	"baton/internal/events"
)

// llmStub is an LLM engine that only answers what busy asks.
type llmStub struct {
	engine.Engine
	cfg config.Engine
}

func (s llmStub) Config() config.Engine { return s.cfg }
func (s llmStub) IsLLM() bool           { return true }

func TestBusyExactSkipsGPULoad(t *testing.T) {
	cfg := config.Default()
	cfg.Daemon.StateDir = t.TempDir()
	e := llmStub{cfg: config.Engine{Name: "tabby", Kind: "tabby"}}
	b := New(cfg, nil, events.Open(""), nil, []engine.Engine{e})

	s := engine.Status{Name: "tabby", Util: 90}
	if busy, _ := b.busy(e, s); !busy {
		t.Fatal("GPU load should count as work without BusyExact")
	}
	s.BusyExact = true
	if busy, why := b.busy(e, s); busy {
		t.Fatalf("busy (%s) with BusyExact and nothing in flight", why)
	}
	s.Busy, s.BusyWhy = true, "generating #4 (8s)"
	if busy, why := b.busy(e, s); !busy || why != s.BusyWhy {
		t.Fatalf("got %v %q, want the engine's own reason", busy, why)
	}
	s.Busy, s.BusyWhy = false, ""
	b.Begin("tabby")
	if busy, _ := b.busy(e, s); !busy {
		t.Fatal("gateway requests should still count with BusyExact")
	}
}
