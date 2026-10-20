//go:build windows

package win

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Proc is one row of the process table.
type Proc struct {
	PID    int    `json:"pid"`
	PPID   int    `json:"ppid"`
	Name   string `json:"name"`
	Path   string `json:"path,omitempty"`
	Start  int64  `json:"start"` // creation time, 100ns since 1601; with PID it names one process
	Access bool   `json:"-"`     // we could open it for query

	WorkingSet uint64 `json:"-"` // bytes of RAM the process has resident
	Private    uint64 `json:"-"` // bytes it has committed for itself
}

type pathEntry struct {
	start int64
	path  string
}

var (
	pathCacheMu sync.Mutex
	pathCache   = map[int]pathEntry{}
)

// Processes snapshots the process table with each image's full path where
// Windows lets us read it (protected processes keep only their name).
func Processes() ([]Proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return nil, err
	}
	var out []Proc
	seen := map[int]bool{}
	for {
		pid := int(e.ProcessID)
		if pid != 0 {
			p := Proc{PID: pid, PPID: int(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
			p.Path, p.Start, p.WorkingSet, p.Private, p.Access = identity(pid)
			out = append(out, p)
			seen[pid] = true
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			break
		}
	}
	pathCacheMu.Lock()
	for pid := range pathCache {
		if !seen[pid] {
			delete(pathCache, pid)
		}
	}
	pathCacheMu.Unlock()
	return out, nil
}

func identity(pid int) (path string, start int64, ws, priv uint64, ok bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", 0, 0, 0, false
	}
	defer windows.CloseHandle(h)
	var c, x, k, u windows.Filetime
	if err := windows.GetProcessTimes(h, &c, &x, &k, &u); err == nil {
		start = int64(c.HighDateTime)<<32 | int64(c.LowDateTime)
	}
	ws, priv = processMemory(h)
	pathCacheMu.Lock()
	if ent, ok := pathCache[pid]; ok && ent.start == start {
		pathCacheMu.Unlock()
		return ent.path, start, ws, priv, true
	}
	pathCacheMu.Unlock()
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
		path = windows.UTF16ToString(buf[:n])
	}
	pathCacheMu.Lock()
	pathCache[pid] = pathEntry{start: start, path: path}
	pathCacheMu.Unlock()
	return path, start, ws, priv, true
}

// Tree is a process table indexed for parent/child walks.
type Tree struct {
	ByPID    map[int]Proc
	Children map[int][]int
}

func NewTree(ps []Proc) *Tree {
	t := &Tree{ByPID: make(map[int]Proc, len(ps)), Children: map[int][]int{}}
	for _, p := range ps {
		t.ByPID[p.PID] = p
	}
	for _, p := range ps {
		// A parent PID can be reused by a younger process; only link to a
		// parent that started before the child.
		if par, ok := t.ByPID[p.PPID]; ok && p.PPID != p.PID && (par.Start == 0 || p.Start == 0 || par.Start <= p.Start) {
			t.Children[p.PPID] = append(t.Children[p.PPID], p.PID)
		}
	}
	return t
}

// Descendants returns pid and everything below it.
func (t *Tree) Descendants(pid int) []int {
	var out []int
	var walk func(int, int)
	walk = func(p, depth int) {
		if depth > 64 {
			return
		}
		out = append(out, p)
		for _, c := range t.Children[p] {
			walk(c, depth+1)
		}
	}
	if _, ok := t.ByPID[pid]; ok {
		walk(pid, 0)
	}
	return out
}

// Ancestors returns the parent chain of pid, nearest first.
func (t *Tree) Ancestors(pid int) []Proc {
	var out []Proc
	cur, ok := t.ByPID[pid]
	for i := 0; ok && i < 64; i++ {
		par, found := t.ByPID[cur.PPID]
		if !found || par.PID == cur.PID || (par.Start != 0 && cur.Start != 0 && par.Start > cur.Start) {
			break
		}
		out = append(out, par)
		cur = par
	}
	return out
}

// Kill terminates the given processes, children before parents, and waits up
// to wait for them to exit. Each PID is checked against its recorded start
// time right before the signal so a reused PID is never hit.
func Kill(procs []Proc, wait time.Duration) error {
	// Deepest first: callers pass trees in walk order (parents first).
	ordered := make([]Proc, 0, len(procs))
	for i := len(procs) - 1; i >= 0; i-- {
		ordered = append(ordered, procs[i])
	}
	var handles []windows.Handle
	var errs []string
	for _, p := range ordered {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.PID))
		if err != nil {
			continue // already gone, or not ours to touch
		}
		if p.Start != 0 {
			var c, x, k, u windows.Filetime
			if windows.GetProcessTimes(h, &c, &x, &k, &u) == nil {
				if int64(c.HighDateTime)<<32|int64(c.LowDateTime) != p.Start {
					windows.CloseHandle(h)
					continue
				}
			}
		}
		if err := windows.TerminateProcess(h, 1); err != nil {
			errs = append(errs, fmt.Sprintf("%s (%d): %v", p.Name, p.PID, err))
		}
		handles = append(handles, h)
	}
	deadline := time.Now().Add(wait)
	for _, h := range handles {
		ms := time.Until(deadline).Milliseconds()
		if ms < 0 {
			ms = 0
		}
		windows.WaitForSingleObject(h, uint32(ms))
		windows.CloseHandle(h)
	}
	if len(errs) > 0 {
		return fmt.Errorf("terminate: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Alive reports whether a process with this PID and start time still runs.
func Alive(pid int, start int64) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil || code != 259 { // STILL_ACTIVE
		return false
	}
	if start == 0 {
		return true
	}
	var c, x, k, u windows.Filetime
	if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil {
		return true
	}
	return int64(c.HighDateTime)<<32|int64(c.LowDateTime) == start
}

// --- TCP listeners, to find which process serves an engine's port ---

var (
	iphlpapi            = windows.NewLazySystemDLL("iphlpapi.dll")
	getExtendedTcpTable = iphlpapi.NewProc("GetExtendedTcpTable")
)

const tcpTableOwnerPidListener = 3

// Listeners maps every listening TCP port (IPv4 and IPv6) to its owning PID.
func Listeners() (map[int]int, error) {
	out := map[int]int{}
	for _, af := range []uint32{windows.AF_INET, windows.AF_INET6} {
		var size uint32
		getExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, uintptr(af), tcpTableOwnerPidListener, 0)
		if size == 0 {
			continue
		}
		buf := make([]byte, size+1024)
		size = uint32(len(buf))
		r, _, _ := getExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(af), tcpTableOwnerPidListener, 0)
		if r != 0 {
			return out, fmt.Errorf("GetExtendedTcpTable: %d", r)
		}
		n := binary.LittleEndian.Uint32(buf[0:4])
		if af == windows.AF_INET {
			// MIB_TCPROW_OWNER_PID: state, localAddr, localPort, remoteAddr, remotePort, pid
			for i := uint32(0); i < n; i++ {
				row := buf[4+i*24:]
				port := int(binary.BigEndian.Uint16(row[8:10]))
				pid := int(binary.LittleEndian.Uint32(row[20:24]))
				if _, ok := out[port]; !ok {
					out[port] = pid
				}
			}
		} else {
			// MIB_TCP6ROW_OWNER_PID: localAddr[16], scope, localPort, remoteAddr[16], scope, remotePort, state, pid
			for i := uint32(0); i < n; i++ {
				row := buf[4+i*56:]
				port := int(binary.BigEndian.Uint16(row[20:22]))
				pid := int(binary.LittleEndian.Uint32(row[52:56]))
				if _, ok := out[port]; !ok {
					out[port] = pid
				}
			}
		}
	}
	return out, nil
}
