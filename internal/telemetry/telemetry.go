// Package telemetry samples the cards, the processes on them and the process
// table in one pass, so every consumer in a cycle sees the same picture.
package telemetry

import (
	"sort"
	"sync"
	"time"

	"baton/internal/win"
)

// Use is one process's footprint on one card.
type Use struct {
	Dedicated uint64  `json:"dedicated"`
	Util      float64 `json:"util"`
}

// Holder is a process holding memory on a card, as shown in the UI.
type Holder struct {
	PID  int     `json:"pid"`
	Name string  `json:"name"`
	Path string  `json:"path,omitempty"`
	MiB  int     `json:"mib"`
	Util float64 `json:"util"`
	Tag  string  `json:"tag,omitempty"` // engine name, "game", "stream", "system"
}

type GPU struct {
	win.GPUInfo
	Holders []Holder `json:"holders"`
}

type Snapshot struct {
	At        time.Time           `json:"at"`
	GPUs      []GPU               `json:"gpus"`
	Mem       win.MemInfo         `json:"mem"`
	Err       string              `json:"error,omitempty"`
	Procs     []win.Proc          `json:"-"`
	Tree      *win.Tree           `json:"-"`
	Listeners map[int]int         `json:"-"`
	ByPID     map[int]map[int]Use `json:"-"` // pid -> gpu index -> use
}

// GPUByIndex returns the card with this NVML index.
func (s *Snapshot) GPUByIndex(i int) *GPU {
	for k := range s.GPUs {
		if s.GPUs[k].Index == i {
			return &s.GPUs[k]
		}
	}
	return nil
}

// VRAM sums what the given processes hold, per card, and their peak load.
func (s *Snapshot) VRAM(pids []int) (map[int]uint64, float64) {
	out := map[int]uint64{}
	var util float64
	for _, pid := range pids {
		for gi, u := range s.ByPID[pid] {
			out[gi] += u.Dedicated
			if u.Util > util {
				util = u.Util
			}
		}
	}
	return out, util
}

type Sampler struct {
	mu       sync.Mutex
	counters *win.GPUCounters
	luidIdx  map[win.LUID]int // adapter -> NVML index; -1 = not an NVIDIA card
	last     *Snapshot
}

func NewSampler() *Sampler {
	s := &Sampler{luidIdx: map[win.LUID]int{}}
	if c, err := win.NewGPUCounters(); err == nil {
		s.counters = c
	}
	return s
}

// Last returns the most recent snapshot without sampling.
func (s *Sampler) Last() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// Sample reads everything now.
func (s *Sampler) Sample() *Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := &Snapshot{At: time.Now(), ByPID: map[int]map[int]Use{}}
	infos, err := win.GPUs()
	if err != nil {
		snap.Err = err.Error()
	}
	busIdx := map[int]int{}
	for _, g := range infos {
		snap.GPUs = append(snap.GPUs, GPU{GPUInfo: g})
		busIdx[g.Bus] = g.Index
	}
	snap.Mem, _ = win.Memory()
	procs, _ := win.Processes()
	snap.Procs = procs
	snap.Tree = win.NewTree(procs)
	snap.Listeners, _ = win.Listeners()

	if s.counters != nil {
		uses, err := s.counters.Sample()
		if err != nil && snap.Err == "" {
			snap.Err = "gpu counters: " + err.Error()
		}
		for _, u := range uses {
			idx, ok := s.luidIdx[u.LUID]
			if !ok {
				idx = -1
				if bus, err := win.AdapterBus(u.LUID); err == nil {
					if i, found := busIdx[bus]; found {
						idx = i
					}
				}
				s.luidIdx[u.LUID] = idx
			}
			if idx < 0 {
				continue
			}
			m := snap.ByPID[u.PID]
			if m == nil {
				m = map[int]Use{}
				snap.ByPID[u.PID] = m
			}
			cur := m[idx]
			cur.Dedicated += u.Dedicated
			if u.Util > cur.Util {
				cur.Util = u.Util
			}
			m[idx] = cur
		}
	}
	for gi := range snap.GPUs {
		g := &snap.GPUs[gi]
		for pid, m := range snap.ByPID {
			u, ok := m[g.Index]
			if !ok || u.Dedicated < 16<<20 && u.Util < 1 {
				continue
			}
			p := snap.Tree.ByPID[pid]
			name := p.Name
			if name == "" {
				name = "pid " + itoa(pid)
			}
			g.Holders = append(g.Holders, Holder{PID: pid, Name: name, Path: p.Path, MiB: int(u.Dedicated >> 20), Util: u.Util})
		}
		sort.Slice(g.Holders, func(i, j int) bool { return g.Holders[i].MiB > g.Holders[j].MiB })
	}
	s.last = snap
	return snap
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	return string(b[n:])
}
