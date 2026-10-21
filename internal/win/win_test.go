//go:build windows

package win

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The test's own command line names the test binary and its flags.
func TestCommandLine(t *testing.T) {
	ps, err := Processes()
	if err != nil {
		t.Fatal(err)
	}
	var self Proc
	for _, p := range ps {
		if p.PID == os.Getpid() {
			self = p
		}
	}
	line, err := CommandLine(self.PID, self.Start)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, filepath.Base(os.Args[0])) || !strings.Contains(line, "-test.") {
		t.Fatalf("command line = %q", line)
	}
	if again, _ := CommandLine(self.PID, self.Start); again != line {
		t.Fatalf("cached command line = %q, want %q", again, line)
	}
}

// The folder of this process and of a child started somewhere else.
func TestCurrentDir(t *testing.T) {
	wd, _ := os.Getwd()
	if got, err := CurrentDir(os.Getpid(), 0); err != nil || !strings.EqualFold(got, wd) {
		t.Fatalf("own folder = %q, %v; want %q", got, err, wd)
	}
	dir := t.TempDir()
	cmd := exec.Command("cmd.exe", "/c", "ping -n 30 127.0.0.1 >nul")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	got, err := CurrentDir(cmd.Process.Pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	// TempDir may come back in 8.3 form; compare what Windows resolves both to.
	if long(t, got) != long(t, dir) {
		t.Fatalf("child folder = %q, want %q", got, dir)
	}
}

func long(t *testing.T, p string) string {
	t.Helper()
	in, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetLongPathName(in, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(windows.UTF16ToString(buf[:n]))
}

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
