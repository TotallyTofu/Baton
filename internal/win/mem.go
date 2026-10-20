//go:build windows

package win

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	k32GetProcessMemoryInfo  = kernel32.NewProc("K32GetProcessMemoryInfo")
	k32GetPerformanceInfo    = kernel32.NewProc("K32GetPerformanceInfo")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type processMemoryCountersEx struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

// processMemory returns a process's resident and private bytes.
func processMemory(h windows.Handle) (ws, priv uint64) {
	var m processMemoryCountersEx
	m.CB = uint32(unsafe.Sizeof(m))
	if r, _, _ := k32GetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&m)), uintptr(m.CB)); r == 0 {
		return 0, 0
	}
	return uint64(m.WorkingSetSize), uint64(m.PrivateUsage)
}

// MemInfo is the machine's RAM as Task Manager shows it.
type MemInfo struct {
	Total       uint64 `json:"total"`
	Available   uint64 `json:"available"`
	Used        uint64 `json:"used"`
	Cached      uint64 `json:"cached"` // file cache (standby and modified pages)
	CommitTotal uint64 `json:"commit_total"`
	CommitLimit uint64 `json:"commit_limit"`
	Kernel      uint64 `json:"kernel"`
	Load        int    `json:"load"` // percent in use
}

type performanceInformation struct {
	CB                uint32
	CommitTotal       uintptr
	CommitLimit       uintptr
	CommitPeak        uintptr
	PhysicalTotal     uintptr
	PhysicalAvailable uintptr
	SystemCache       uintptr
	KernelTotal       uintptr
	KernelPaged       uintptr
	KernelNonpaged    uintptr
	PageSize          uintptr
	HandleCount       uint32
	ProcessCount      uint32
	ThreadCount       uint32
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// Memory reads the machine's RAM and commit charge.
func Memory() (MemInfo, error) {
	var ms memoryStatusEx
	ms.Length = uint32(unsafe.Sizeof(ms))
	if r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms))); r == 0 {
		return MemInfo{}, err
	}
	m := MemInfo{Total: ms.TotalPhys, Available: ms.AvailPhys, Used: ms.TotalPhys - ms.AvailPhys, Load: int(ms.MemoryLoad),
		CommitTotal: ms.TotalPageFile - ms.AvailPageFile, CommitLimit: ms.TotalPageFile}
	var pi performanceInformation
	pi.CB = uint32(unsafe.Sizeof(pi))
	if r, _, _ := k32GetPerformanceInfo.Call(uintptr(unsafe.Pointer(&pi)), uintptr(pi.CB)); r != 0 {
		page := uint64(pi.PageSize)
		m.Cached = uint64(pi.SystemCache) * page
		m.Kernel = uint64(pi.KernelTotal) * page
		m.CommitTotal = uint64(pi.CommitTotal) * page
		m.CommitLimit = uint64(pi.CommitLimit) * page
	}
	return m, nil
}
