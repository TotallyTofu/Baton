//go:build windows

package win

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The NVIDIA driver reports no per-process memory under WDDM, but Windows
// itself does: the "GPU Process Memory" and "GPU Engine" performance counters
// (what Task Manager reads) carry a pid and an adapter LUID per instance.

var (
	pdhDLL                       = windows.NewLazySystemDLL("pdh.dll")
	pdhOpenQuery                 = pdhDLL.NewProc("PdhOpenQueryW")
	pdhAddEnglishCounter         = pdhDLL.NewProc("PdhAddEnglishCounterW")
	pdhCollectQueryData          = pdhDLL.NewProc("PdhCollectQueryData")
	pdhGetFormattedCounterArrayW = pdhDLL.NewProc("PdhGetFormattedCounterArrayW")
	pdhCloseQuery                = pdhDLL.NewProc("PdhCloseQuery")
)

const (
	pdhFmtDouble   = 0x00000200
	pdhFmtLarge    = 0x00000400
	pdhFmtNoCap100 = 0x00008000
	pdhMoreData    = 0x800007D2
	pdhNoData      = 0x800007D5
	pdhCstatusOK   = 0x0
	pdhCstatusNew  = 0x1
)

// LUID identifies a display adapter for this boot.
type LUID struct {
	High int32
	Low  uint32
}

func (l LUID) String() string { return fmt.Sprintf("0x%08x_0x%08x", uint32(l.High), l.Low) }

// ProcGPU is one process's use of one adapter.
type ProcGPU struct {
	PID       int
	LUID      LUID
	Dedicated uint64  // bytes of dedicated (on-card) memory
	Shared    uint64  // bytes of shared system memory mapped for the GPU
	Util      float64 // busiest engine, percent
}

// GPUCounters samples the per-process GPU counters. It keeps one PDH query open
// so utilization (a rate counter) has a previous sample to diff against.
type GPUCounters struct {
	mu        sync.Mutex
	query     uintptr
	dedicated uintptr
	shared    uintptr
	engine    uintptr
	primed    bool
}

func NewGPUCounters() (*GPUCounters, error) {
	if err := pdhDLL.Load(); err != nil {
		return nil, err
	}
	c := &GPUCounters{}
	if r, _, _ := pdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&c.query))); r != 0 {
		return nil, fmt.Errorf("PdhOpenQuery: 0x%x", r)
	}
	add := func(path string, h *uintptr) error {
		p, _ := windows.UTF16PtrFromString(path)
		if r, _, _ := pdhAddEnglishCounter.Call(c.query, uintptr(unsafe.Pointer(p)), 0, uintptr(unsafe.Pointer(h))); r != 0 {
			return fmt.Errorf("PdhAddEnglishCounter %s: 0x%x", path, r)
		}
		return nil
	}
	if err := add(`\GPU Process Memory(*)\Dedicated Usage`, &c.dedicated); err != nil {
		c.Close()
		return nil, err
	}
	// Shared and engine counters are nice to have; a box without them still
	// gets the memory picture.
	_ = add(`\GPU Process Memory(*)\Shared Usage`, &c.shared)
	_ = add(`\GPU Engine(*)\Utilization Percentage`, &c.engine)
	return c, nil
}

func (c *GPUCounters) Close() {
	if c.query != 0 {
		pdhCloseQuery.Call(c.query)
		c.query = 0
	}
}

var instRe = regexp.MustCompile(`^pid_(\d+)_luid_0x([0-9a-fA-F]+)_0x([0-9a-fA-F]+)_phys_\d+`)

func parseInstance(name string) (pid int, luid LUID, ok bool) {
	m := instRe.FindStringSubmatch(name)
	if m == nil {
		return 0, LUID{}, false
	}
	pid, _ = strconv.Atoi(m[1])
	hi, _ := strconv.ParseUint(m[2], 16, 32)
	lo, _ := strconv.ParseUint(m[3], 16, 32)
	return pid, LUID{High: int32(hi), Low: uint32(lo)}, true
}

type pdhItem struct {
	name  string
	large int64
	dbl   float64
}

// readArray returns every instance of a wildcard counter.
func readArray(counter uintptr, format uint32) ([]pdhItem, error) {
	if counter == 0 {
		return nil, nil
	}
	var size, count uint32
	r, _, _ := pdhGetFormattedCounterArrayW.Call(counter, uintptr(format), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), 0)
	if r == pdhNoData || (r == 0 && count == 0) {
		return nil, nil
	}
	if r != pdhMoreData {
		return nil, fmt.Errorf("PdhGetFormattedCounterArray size: 0x%x", r)
	}
	buf := make([]byte, size)
	r, _, _ = pdhGetFormattedCounterArrayW.Call(counter, uintptr(format), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&count)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 {
		return nil, fmt.Errorf("PdhGetFormattedCounterArray: 0x%x", r)
	}
	// PDH_FMT_COUNTERVALUE_ITEM_W on amd64: LPWSTR (8) + {DWORD CStatus, pad, union 8}.
	const itemSize = 24
	out := make([]pdhItem, 0, count)
	for i := uint32(0); i < count; i++ {
		base := unsafe.Pointer(&buf[uintptr(i)*itemSize])
		namePtr := *(**uint16)(base)
		status := *(*uint32)(unsafe.Add(base, 8))
		if status != pdhCstatusOK && status != pdhCstatusNew {
			continue
		}
		it := pdhItem{name: windows.UTF16PtrToString(namePtr)}
		val := unsafe.Add(base, 16)
		if format&pdhFmtDouble != 0 {
			it.dbl = *(*float64)(val)
		} else {
			it.large = *(*int64)(val)
		}
		out = append(out, it)
	}
	return out, nil
}

type procKey struct {
	pid  int
	luid LUID
}

// Sample collects once and returns per-process, per-adapter use. Processes
// with no dedicated memory and no engine activity are left out.
func (c *GPUCounters) Sample() ([]ProcGPU, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, _, _ := pdhCollectQueryData.Call(c.query); r != 0 && r != pdhNoData {
		return nil, fmt.Errorf("PdhCollectQueryData: 0x%x", r)
	}
	if !c.primed {
		// Rate counters need two samples; take the second right away.
		c.primed = true
		pdhCollectQueryData.Call(c.query)
	}
	agg := map[procKey]*ProcGPU{}
	get := func(name string) *ProcGPU {
		pid, luid, ok := parseInstance(name)
		if !ok || pid == 0 {
			return nil
		}
		k := procKey{pid, luid}
		p := agg[k]
		if p == nil {
			p = &ProcGPU{PID: pid, LUID: luid}
			agg[k] = p
		}
		return p
	}
	items, err := readArray(c.dedicated, pdhFmtLarge)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if p := get(it.name); p != nil {
			p.Dedicated += uint64(it.large)
		}
	}
	if items, err := readArray(c.shared, pdhFmtLarge); err == nil {
		for _, it := range items {
			if p := get(it.name); p != nil {
				p.Shared += uint64(it.large)
			}
		}
	}
	if items, err := readArray(c.engine, pdhFmtDouble|pdhFmtNoCap100); err == nil {
		for _, it := range items {
			if it.dbl <= 0 {
				continue
			}
			if p := get(it.name); p != nil && it.dbl > p.Util {
				p.Util = it.dbl
			}
		}
	}
	out := make([]ProcGPU, 0, len(agg))
	for _, p := range agg {
		if p.Dedicated == 0 && p.Util < 0.5 {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

// --- adapter LUID -> PCI bus, so counters can be matched to NVML's cards ---

var (
	gdi32                     = windows.NewLazySystemDLL("gdi32.dll")
	d3dkmtOpenAdapterFromLuid = gdi32.NewProc("D3DKMTOpenAdapterFromLuid")
	d3dkmtQueryAdapterInfo    = gdi32.NewProc("D3DKMTQueryAdapterInfo")
	d3dkmtCloseAdapter        = gdi32.NewProc("D3DKMTCloseAdapter")
)

type kmtQueryAdapterInfo struct {
	HAdapter uint32
	Type     int32
	Data     uintptr
	Size     uint32
}

type kmtAdapterAddress struct {
	Bus, Device, Function uint32
}

const kmtqaiAdapterAddress = 6

// AdapterBus returns the PCI bus number of the adapter with this LUID.
func AdapterBus(l LUID) (int, error) {
	// D3DKMT_OPENADAPTERFROMLUID puts LowPart first, as LUID does.
	open := struct {
		Low      uint32
		High     int32
		HAdapter uint32
	}{Low: l.Low, High: l.High}
	if r, _, _ := d3dkmtOpenAdapterFromLuid.Call(uintptr(unsafe.Pointer(&open))); r != 0 {
		return -1, fmt.Errorf("D3DKMTOpenAdapterFromLuid %s: 0x%x", l, r)
	}
	defer func() {
		h := open.HAdapter
		d3dkmtCloseAdapter.Call(uintptr(unsafe.Pointer(&h)))
	}()
	var addr kmtAdapterAddress
	q := kmtQueryAdapterInfo{HAdapter: open.HAdapter, Type: kmtqaiAdapterAddress, Data: uintptr(unsafe.Pointer(&addr)), Size: uint32(unsafe.Sizeof(addr))}
	if r, _, _ := d3dkmtQueryAdapterInfo.Call(uintptr(unsafe.Pointer(&q))); r != 0 {
		return -1, fmt.Errorf("D3DKMTQueryAdapterInfo %s: 0x%x", l, r)
	}
	return int(addr.Bus), nil
}

// ParseLUID reads the "0xHHHHHHHH_0xLLLLLLLL" form the counters use.
func ParseLUID(s string) (LUID, bool) {
	parts := strings.Split(strings.TrimPrefix(s, "luid_"), "_")
	if len(parts) < 2 {
		return LUID{}, false
	}
	hi, err1 := strconv.ParseUint(strings.TrimPrefix(parts[0], "0x"), 16, 32)
	lo, err2 := strconv.ParseUint(strings.TrimPrefix(parts[1], "0x"), 16, 32)
	if err1 != nil || err2 != nil {
		return LUID{}, false
	}
	return LUID{High: int32(hi), Low: uint32(lo)}, true
}
