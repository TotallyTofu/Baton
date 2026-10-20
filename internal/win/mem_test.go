//go:build windows

package win

import (
	"sort"
	"testing"
)

func TestMemoryLive(t *testing.T) {
	m, err := Memory()
	if err != nil {
		t.Fatal(err)
	}
	g := func(b uint64) float64 { return float64(b) / (1 << 30) }
	t.Logf("RAM %.1f/%.1f GiB used (%d%%), avail %.1f, cached %.1f, commit %.1f/%.1f, kernel %.1f",
		g(m.Used), g(m.Total), m.Load, g(m.Available), g(m.Cached), g(m.CommitTotal), g(m.CommitLimit), g(m.Kernel))
	ps, _ := Processes()
	sort.Slice(ps, func(i, j int) bool { return ps[i].WorkingSet > ps[j].WorkingSet })
	var sum uint64
	readable := 0
	for _, p := range ps {
		sum += p.WorkingSet
		if p.WorkingSet > 0 {
			readable++
		}
	}
	for _, p := range ps[:8] {
		t.Logf("%-28s ws %.2f GiB private %.2f GiB", p.Name, g(p.WorkingSet), g(p.Private))
	}
	t.Logf("%d/%d processes readable, working sets sum %.1f GiB", readable, len(ps), g(sum))
	if m.Total == 0 || readable < 20 { // services running as SYSTEM are not ours to read
		t.Fatal("memory not readable")
	}
}
