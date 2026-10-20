//go:build windows

// Package win wraps the handful of Windows and NVIDIA driver APIs baton needs,
// all through syscalls so the binary needs no C toolchain.
package win

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	nvmlDLL                     = windows.NewLazySystemDLL("nvml.dll")
	nvmlInit                    = nvmlDLL.NewProc("nvmlInit_v2")
	nvmlDeviceGetCount          = nvmlDLL.NewProc("nvmlDeviceGetCount_v2")
	nvmlDeviceGetHandleByIndex  = nvmlDLL.NewProc("nvmlDeviceGetHandleByIndex_v2")
	nvmlDeviceGetName           = nvmlDLL.NewProc("nvmlDeviceGetName")
	nvmlDeviceGetUUID           = nvmlDLL.NewProc("nvmlDeviceGetUUID")
	nvmlDeviceGetMemoryInfo     = nvmlDLL.NewProc("nvmlDeviceGetMemoryInfo")
	nvmlDeviceGetUtilization    = nvmlDLL.NewProc("nvmlDeviceGetUtilizationRates")
	nvmlDeviceGetTemperature    = nvmlDLL.NewProc("nvmlDeviceGetTemperature")
	nvmlDeviceGetPowerUsage     = nvmlDLL.NewProc("nvmlDeviceGetPowerUsage")
	nvmlDeviceGetPowerLimit     = nvmlDLL.NewProc("nvmlDeviceGetEnforcedPowerLimit")
	nvmlDeviceGetFanSpeed       = nvmlDLL.NewProc("nvmlDeviceGetFanSpeed")
	nvmlDeviceGetPciInfo        = nvmlDLL.NewProc("nvmlDeviceGetPciInfo_v3")
	nvmlDeviceGetEncoderSession = nvmlDLL.NewProc("nvmlDeviceGetEncoderSessions")
	nvmlDeviceGetClockInfo      = nvmlDLL.NewProc("nvmlDeviceGetClockInfo")

	nvmlOnce    sync.Once
	nvmlInitErr error
)

type nvmlMemory struct {
	Total, Free, Used uint64
}

type nvmlUtilization struct {
	GPU, Memory uint32
}

type nvmlPciInfo struct {
	BusIDLegacy    [16]byte
	Domain         uint32
	Bus            uint32
	Device         uint32
	PciDeviceID    uint32
	PciSubSystemID uint32
	BusID          [32]byte
}

type nvmlEncoderSession struct {
	SessionID      uint32
	PID            uint32
	VgpuInstance   uint32
	CodecType      uint32
	HResolution    uint32
	VResolution    uint32
	AverageFps     uint32
	AverageLatency uint32
}

// EncoderSession is one active NVENC session.
type EncoderSession struct {
	PID    int    `json:"pid"`
	Codec  string `json:"codec"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	FPS    int    `json:"fps"`
}

// GPUInfo is one card as NVML reports it.
type GPUInfo struct {
	Index      int              `json:"index"`
	Name       string           `json:"name"`
	UUID       string           `json:"uuid"`
	Bus        int              `json:"bus"`
	MemTotal   uint64           `json:"mem_total"`
	MemUsed    uint64           `json:"mem_used"`
	MemFree    uint64           `json:"mem_free"`
	Util       int              `json:"util"`
	MemUtil    int              `json:"mem_util"`
	TempC      int              `json:"temp_c"`
	PowerW     float64          `json:"power_w"`
	PowerMaxW  float64          `json:"power_max_w"`
	FanPct     int              `json:"fan_pct"`
	ClockMHz   int              `json:"clock_mhz"`
	MemClkMHz  int              `json:"mem_clock_mhz"`
	Encoders   []EncoderSession `json:"encoders"`
	ProbeError string           `json:"probe_error,omitempty"`
}

func nvmlErr(name string, r uintptr) error {
	if r == 0 {
		return nil
	}
	return fmt.Errorf("%s: nvml error %d", name, r)
}

func initNVML() error {
	nvmlOnce.Do(func() {
		if err := nvmlDLL.Load(); err != nil {
			nvmlInitErr = fmt.Errorf("nvml.dll not found (is the NVIDIA driver installed?): %w", err)
			return
		}
		r, _, _ := nvmlInit.Call()
		nvmlInitErr = nvmlErr("nvmlInit", r)
	})
	return nvmlInitErr
}

func cstr(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

var codecNames = map[uint32]string{0: "h264", 1: "hevc", 2: "av1"}

// GPUs reads every NVIDIA card. A card that fails one query still reports the
// rest; only a failed init is an error.
func GPUs() ([]GPUInfo, error) {
	if err := initNVML(); err != nil {
		return nil, err
	}
	var count uint32
	if r, _, _ := nvmlDeviceGetCount.Call(uintptr(unsafe.Pointer(&count))); r != 0 {
		return nil, nvmlErr("nvmlDeviceGetCount", r)
	}
	out := make([]GPUInfo, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev uintptr
		if r, _, _ := nvmlDeviceGetHandleByIndex.Call(uintptr(i), uintptr(unsafe.Pointer(&dev))); r != 0 {
			out = append(out, GPUInfo{Index: int(i), ProbeError: nvmlErr("handle", r).Error()})
			continue
		}
		g := GPUInfo{Index: int(i)}
		name := make([]byte, 96)
		nvmlDeviceGetName.Call(dev, uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
		g.Name = cstr(name)
		uuid := make([]byte, 96)
		nvmlDeviceGetUUID.Call(dev, uintptr(unsafe.Pointer(&uuid[0])), uintptr(len(uuid)))
		g.UUID = cstr(uuid)

		var pci nvmlPciInfo
		if r, _, _ := nvmlDeviceGetPciInfo.Call(dev, uintptr(unsafe.Pointer(&pci))); r == 0 {
			g.Bus = int(pci.Bus)
		}
		var mem nvmlMemory
		if r, _, _ := nvmlDeviceGetMemoryInfo.Call(dev, uintptr(unsafe.Pointer(&mem))); r == 0 {
			g.MemTotal, g.MemUsed, g.MemFree = mem.Total, mem.Used, mem.Free
		} else {
			g.ProbeError = nvmlErr("memory", r).Error()
		}
		var util nvmlUtilization
		if r, _, _ := nvmlDeviceGetUtilization.Call(dev, uintptr(unsafe.Pointer(&util))); r == 0 {
			g.Util, g.MemUtil = int(util.GPU), int(util.Memory)
		}
		var v uint32
		if r, _, _ := nvmlDeviceGetTemperature.Call(dev, 0, uintptr(unsafe.Pointer(&v))); r == 0 {
			g.TempC = int(v)
		}
		if r, _, _ := nvmlDeviceGetPowerUsage.Call(dev, uintptr(unsafe.Pointer(&v))); r == 0 {
			g.PowerW = float64(v) / 1000
		}
		if r, _, _ := nvmlDeviceGetPowerLimit.Call(dev, uintptr(unsafe.Pointer(&v))); r == 0 {
			g.PowerMaxW = float64(v) / 1000
		}
		if r, _, _ := nvmlDeviceGetFanSpeed.Call(dev, uintptr(unsafe.Pointer(&v))); r == 0 {
			g.FanPct = int(v)
		}
		if r, _, _ := nvmlDeviceGetClockInfo.Call(dev, 1, uintptr(unsafe.Pointer(&v))); r == 0 { // NVML_CLOCK_SM
			g.ClockMHz = int(v)
		}
		if r, _, _ := nvmlDeviceGetClockInfo.Call(dev, 2, uintptr(unsafe.Pointer(&v))); r == 0 { // NVML_CLOCK_MEM
			g.MemClkMHz = int(v)
		}
		g.Encoders = encoderSessions(dev)
		out = append(out, g)
	}
	return out, nil
}

func encoderSessions(dev uintptr) []EncoderSession {
	buf := make([]nvmlEncoderSession, 16)
	n := uint32(len(buf))
	r, _, _ := nvmlDeviceGetEncoderSession.Call(dev, uintptr(unsafe.Pointer(&n)), uintptr(unsafe.Pointer(&buf[0])))
	if r != 0 || n == 0 {
		return nil
	}
	if int(n) > len(buf) {
		n = uint32(len(buf))
	}
	out := make([]EncoderSession, 0, n)
	for _, s := range buf[:n] {
		out = append(out, EncoderSession{
			PID: int(s.PID), Codec: codecNames[s.CodecType],
			Width: int(s.HResolution), Height: int(s.VResolution), FPS: int(s.AverageFps),
		})
	}
	return out
}

// ErrNoNVML is returned by callers that need NVML and cannot get it.
var ErrNoNVML = errors.New("nvml unavailable")
