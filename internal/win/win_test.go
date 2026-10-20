//go:build windows

package win

import (
	"testing"
	"time"
)

func TestProbeLive(t *testing.T) {
	gs, err := GPUs()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		t.Logf("gpu%d %s bus=%d used=%dMiB/%dMiB util=%d%% temp=%dC power=%.0f/%.0fW fan=%d%% enc=%v err=%q",
			g.Index, g.Name, g.Bus, g.MemUsed>>20, g.MemTotal>>20, g.Util, g.TempC, g.PowerW, g.PowerMaxW, g.FanPct, g.Encoders, g.ProbeError)
	}
	c, err := NewGPUCounters()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	t0 := time.Now()
	ps, err := c.Sample()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pdh sample took %v, %d entries", time.Since(t0), len(ps))
	buses := map[LUID]int{}
	for _, p := range ps {
		if _, ok := buses[p.LUID]; !ok {
			b, err := AdapterBus(p.LUID)
			if err != nil {
				t.Logf("luid %s: %v", p.LUID, err)
			}
			buses[p.LUID] = b
		}
		if p.Dedicated > 32<<20 || p.Util > 1 {
			t.Logf("pid %d luid %s bus %d dedicated %dMiB shared %dMiB util %.1f", p.PID, p.LUID, buses[p.LUID], p.Dedicated>>20, p.Shared>>20, p.Util)
		}
	}
	t0 = time.Now()
	procs, err := Processes()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("processes: %d in %v", len(procs), time.Since(t0))
	l, err := Listeners()
	if err != nil {
		t.Fatal(err)
	}
	tr := NewTree(procs)
	for _, port := range []int{5000, 8000, 8001, 8888, 47989} {
		if pid, ok := l[port]; ok {
			t.Logf("port %d -> pid %d %s %s desc=%v", port, pid, tr.ByPID[pid].Name, tr.ByPID[pid].Path, tr.Descendants(pid))
		}
	}
}
