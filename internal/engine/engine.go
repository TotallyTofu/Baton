// Package engine adapts each inference server baton manages: how to see what
// it holds, how to make it give the GPUs back, and how to bring it back.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"baton/internal/config"
	"baton/internal/events"
	"baton/internal/telemetry"
	"baton/internal/win"
)

// Status is what an engine looks like right now.
type Status struct {
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	URL      string         `json:"url"`
	GPUs     []int          `json:"gpus"`
	Up       bool           `json:"up"`      // its server answers
	Loaded   bool           `json:"loaded"`  // it holds a model (or real VRAM)
	Loading  bool           `json:"loading"` // a load is in progress
	Model    string         `json:"model,omitempty"`
	Busy     bool           `json:"busy"`
	BusyWhy  string         `json:"busy_why,omitempty"`
	Detail   string         `json:"detail,omitempty"`
	PIDs     []int          `json:"pids,omitempty"`
	VRAMMiB  map[int]int    `json:"vram_mib"` // per GPU index
	RAMMiB   int            `json:"ram_mib"`  // resident system RAM of its processes
	Util     float64        `json:"util"`
	Err      string         `json:"error,omitempty"`
	CanStart bool           `json:"can_start"`
	Extra    map[string]any `json:"extra,omitempty"`

	procs []win.Proc // what Release would kill, with start times
}

// Procs returns the processes attributed to the engine at probe time.
func (s Status) Procs() []win.Proc { return s.procs }

// Engine is one managed server.
type Engine interface {
	Config() config.Engine
	// Probe looks without changing anything. It must return within ctx.
	Probe(ctx context.Context, snap *telemetry.Snapshot) Status
	// Release gives the GPUs back: mode is stop, unload or free.
	Release(ctx context.Context, mode string, st Status) error
	// Acquire makes the engine ready to serve, loading model if it names one.
	Acquire(ctx context.Context, model string, progress func(string)) error
	// Interrupt stops in-flight work (ComfyUI's running job); others no-op.
	Interrupt(ctx context.Context) error
	// Upstream is where the gateway sends requests, with the engine's auth.
	Upstream() (base string, hdr http.Header)
	// Serves reports whether a client model name routes here, and the name to
	// send upstream.
	Serves(model string) (bool, string)
	IsLLM() bool
}

// Preflighter is implemented by engines that can tell, before anything is
// released, that they would fail to serve model.
type Preflighter interface {
	Preflight(model string, st Status) error
}

// Env is what engines need from the daemon.
type Env struct {
	StateDir string
	Events   *events.Log
	Sample   func() *telemetry.Snapshot // a fresh snapshot
}

func New(c config.Engine, env *Env) (Engine, error) {
	switch c.Kind {
	case "tabby":
		return newTabby(c, env), nil
	case "unsloth":
		return newUnsloth(c, env), nil
	case "comfyui":
		return newComfy(c, env), nil
	}
	return nil, fmt.Errorf("unknown engine kind %q", c.Kind)
}

// --- shared pieces ---

type base struct {
	cfg config.Engine
	env *Env

	mu      sync.Mutex
	managed *exec.Cmd // a server we started, if still running
	mStart  int64
	exited  bool // the server we started has exited
	exitMsg string
}

func (b *base) Config() config.Engine { return b.cfg }

func (b *base) Interrupt(context.Context) error { return nil }

func (b *base) servesList(model string, extra []string) (bool, string) {
	m := strings.ToLower(strings.TrimSpace(model))
	for _, s := range append(append([]string(nil), b.cfg.Serves...), extra...) {
		if strings.ToLower(s) == m {
			return true, model
		}
	}
	for _, mm := range b.cfg.Models {
		if strings.ToLower(mm.Name) == m {
			return true, mm.ModelPath
		}
	}
	return false, ""
}

func portOf(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	if p := u.Port(); p != "" {
		n, _ := strconv.Atoi(p)
		return n
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

// attribute finds the engine's processes: the tree under whoever listens on
// its port, ancestors that live in its install dir (venv launchers), and the
// tree of a server we started ourselves.
func (b *base) attribute(snap *telemetry.Snapshot, port int) []win.Proc {
	seen := map[int]bool{}
	var out []win.Proc
	addTree := func(root int) {
		for _, pid := range snap.Tree.Descendants(root) {
			if !seen[pid] {
				seen[pid] = true
				out = append(out, snap.Tree.ByPID[pid])
			}
		}
	}
	// Anything running from the engine's install dir is the engine, even
	// before it opens its port (TabbyAPI loads its model first).
	if b.cfg.Dir != "" {
		dir := strings.ToLower(filepath.Clean(b.cfg.Dir)) + `\`
		for _, p := range snap.Procs {
			if p.Path != "" && strings.HasPrefix(strings.ToLower(p.Path), dir) && !seen[p.PID] {
				addTree(p.PID)
			}
		}
	}
	if pid, ok := snap.Listeners[port]; ok && pid > 4 {
		root := pid
		if b.cfg.Dir != "" {
			dir := strings.ToLower(filepath.Clean(b.cfg.Dir)) + `\`
			for _, a := range snap.Tree.Ancestors(pid) {
				if strings.HasPrefix(strings.ToLower(a.Path), dir) {
					root = a.PID
				}
			}
		}
		addTree(root)
	}
	b.mu.Lock()
	if b.managed != nil && b.managed.Process != nil {
		pid := b.managed.Process.Pid
		if !b.exited && win.Alive(pid, b.mStart) {
			addTree(pid)
		}
	}
	b.mu.Unlock()
	return out
}

func pidsOf(ps []win.Proc) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.PID
	}
	return out
}

func (b *base) fill(st *Status, snap *telemetry.Snapshot, procs []win.Proc) {
	st.Name, st.Kind, st.GPUs = b.cfg.Name, b.cfg.Kind, b.cfg.GPUs
	st.procs = procs
	st.PIDs = pidsOf(procs)
	vram, util := snap.VRAM(st.PIDs)
	st.VRAMMiB = map[int]int{}
	for gi, v := range vram {
		st.VRAMMiB[gi] = int(v >> 20)
	}
	st.Util = util
	var ram uint64
	for _, p := range procs {
		ram += p.WorkingSet
	}
	st.RAMMiB = int(ram >> 20)
	st.CanStart = len(b.cfg.Start) > 0
}

// launch starts the engine's server hidden, logging to the state dir.
func (b *base) launch() error {
	if len(b.cfg.Start) == 0 {
		return fmt.Errorf("%s is not running and has no start command", b.cfg.Name)
	}
	// Resolve scripts named relative to the engine's folder ("start.bat"):
	// cmd.exe will not look in the current directory when the environment
	// sets NoDefaultCurrentDirectoryInExePath.
	argv := append([]string(nil), b.cfg.Start...)
	if b.cfg.Dir != "" {
		for i, a := range argv {
			if a == "" || filepath.IsAbs(a) || strings.HasPrefix(a, "/") || strings.HasPrefix(a, "-") {
				continue
			}
			if p := filepath.Join(b.cfg.Dir, a); fileExists(p) {
				argv[i] = p
			}
		}
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = b.cfg.Dir
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "NODEFAULTCURRENTDIRECTORYINEXEPATH=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "PYTHONUNBUFFERED=1")
	for k, v := range b.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	const createNewConsole, createNoWindow, createNewProcessGroup = 0x10, 0x08000000, 0x200
	attr := &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	if b.cfg.ShowWindow {
		attr.CreationFlags |= createNewConsole
	} else {
		attr.CreationFlags |= createNoWindow
		attr.HideWindow = true
		path := LogPath(b.env.StateDir, b.cfg.Name)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		rotateLog(path)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		// Without a console, Python writes in the ANSI code page (box-drawing
		// characters then crash its logger) and wraps at 80 columns.
		cmd.Env = append(cmd.Env, "PYTHONUTF8=1", "PYTHONIOENCODING=utf-8", "COLUMNS=200")
		fmt.Fprintf(f, "\n===== baton started %s at %s =====\n", strings.Join(b.cfg.Start, " "), time.Now().Format(time.RFC3339))
		cmd.Stdout, cmd.Stderr = f, f
		defer f.Close() // the child keeps its own handle
	}
	cmd.SysProcAttr = attr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", b.cfg.Name, err)
	}
	var start int64
	for _, p := range mustProcs() {
		if p.PID == cmd.Process.Pid {
			start = p.Start
		}
	}
	b.mu.Lock()
	b.managed, b.mStart = cmd, start
	b.exited, b.exitMsg = false, ""
	b.mu.Unlock()
	go func() {
		err := cmd.Wait()
		b.mu.Lock()
		if b.managed == cmd {
			b.exited = true
			if cmd.ProcessState != nil {
				b.exitMsg = fmt.Sprintf("exit code %d", cmd.ProcessState.ExitCode())
			} else if err != nil {
				b.exitMsg = err.Error()
			}
		}
		b.mu.Unlock()
	}()
	b.env.Events.Infof("start", b.cfg.Name, "started %s (pid %d)", strings.Join(b.cfg.Start, " "), cmd.Process.Pid)
	return nil
}

// managedExited reports whether a server we launched has already died.
func (b *base) managedExited() (bool, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.exited, b.exitMsg
}

// managedRunning reports whether a server we launched is still alive.
func (b *base) managedRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.managed != nil && !b.exited
}

func mustProcs() []win.Proc {
	ps, _ := win.Processes()
	return ps
}

// stop kills the engine's processes.
func (b *base) stop(st Status) error {
	if len(st.procs) == 0 {
		return nil
	}
	names := make([]string, 0, len(st.procs))
	for _, p := range st.procs {
		names = append(names, fmt.Sprintf("%s(%d)", p.Name, p.PID))
	}
	b.env.Events.Infof("release", b.cfg.Name, "stopping %s", strings.Join(names, ", "))
	err := win.Kill(st.procs, 15*time.Second)
	b.mu.Lock()
	b.managed, b.exited = nil, false
	b.mu.Unlock()
	return err
}

func logTail(stateDir, name string, n int) string {
	text, _, _, err := ReadLog(LogPath(stateDir, name), -1)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// --- HTTP ---

var client = &http.Client{Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, IdleConnTimeout: 30 * time.Second}}

type httpError struct {
	Code int
	Body string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body) }

func doJSON(ctx context.Context, method, u string, hdr http.Header, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode/100 != 2 {
		s := strings.TrimSpace(string(b))
		if len(s) > 300 {
			s = s[:300] + "..."
		}
		return &httpError{resp.StatusCode, s}
	}
	if out == nil {
		return nil
	}
	// Some servers pad long responses with leading whitespace to keep proxies
	// from timing out; json.Unmarshal skips it.
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

func isConnRefused(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return false
	}
	return err != nil
}

func probeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 1500*time.Millisecond)
}

func readSecretFile(p string) string {
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// waitUntil polls fn every interval until it reports done, fails, or ctx ends.
func waitUntil(ctx context.Context, every time.Duration, fn func() (bool, error)) error {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		done, err := fn()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
