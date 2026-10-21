// Command baton makes the programs on this PC take turns with the GPUs: LLM
// engines, ComfyUI, games and streams.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/windows/registry"

	"baton/internal/broker"
	"baton/internal/config"
	"baton/internal/detect"
	"baton/internal/engine"
	"baton/internal/events"
	"baton/internal/games"
	"baton/internal/gateway"
	"baton/internal/harbor"
	"baton/internal/netx"
	"baton/internal/server"
	"baton/internal/telemetry"
	"baton/internal/tray"
	"baton/internal/win"
)

var version = "0.1.0"

const usage = `baton: GPU turn-taking for LLM engines, ComfyUI, games and streams

usage: baton [--config FILE] <command> [flags]

daemon and setup
  (no command)           start baton in the background and open the dashboard
  daemon [--no-tray]     run baton here (dashboard, gateway, tray)
  start                  run the daemon in the background, no window
  stop                   stop the daemon (engines keep running)
  autostart on|off       start baton when you log in
  config                 show which config file is used and check it
  key                    print the gateway API key
  firewall               let Tailscale devices reach baton (asks for admin once)

look
  status [--json]        mode, GPUs, engines, games and streams
  gpus                   read the cards directly (no daemon needed)
  events [-f]            what baton did and why
  logs ENGINE [-f] [-n N]              output of an engine baton started (e.g. tabby)

act
  use ENGINE [--model M] [--no-wait]   give an engine the GPUs
  release ENGINE         make an engine give the GPUs back
  reclaim ENGINE         force-stop an engine that kept its VRAM after a release (aground)
  free                   release every engine
  hold [--reason R] [--for 2h]         keep every engine off the GPUs
  unhold                 end a hold
  hook EVENT [--source S]              stream-start | stream-end | game-start | game-end
                                       (for Apollo prep commands; always exits 0)
`

func main() {
	args := os.Args[1:]
	cfgPath := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
			args = append(args[:i], args[i+2:]...)
			break
		}
		if v, ok := strings.CutPrefix(args[i], "--config="); ok {
			cfgPath = v
			args = append(args[:i], args[i+1:]...)
			break
		}
	}
	if len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		fmt.Print(usage)
		return
	}
	if len(args) == 0 {
		args = []string{"open"} // double-clicked, or just "baton"
	}
	cmd, rest := args[0], args[1:]
	path := config.Locate(cfgPath)

	if cmd == "version" {
		fmt.Println("baton", version)
		return
	}
	if cmd == "gpus" {
		os.Exit(cmdGPUs())
	}
	cfg, err := config.Load(path)
	if err != nil {
		if cmd == "hook" {
			fmt.Fprintln(os.Stderr, "baton:", err)
			return
		}
		fail(err)
	}
	var code int
	switch cmd {
	case "daemon":
		code = cmdDaemon(cfg, rest)
	case "start":
		code = cmdStart(cfg, path)
	case "open":
		if code = cmdStart(cfg, path); code == 0 {
			tray.OpenURL("http://" + cfg.Daemon.Listen + "/")
			fmt.Println("(baton help lists the commands)")
		}
	case "stop":
		code = cmdStop(cfg)
	case "status":
		code = cmdStatus(cfg, rest)
	case "events":
		code = cmdEvents(cfg, rest)
	case "logs":
		code = cmdLogs(cfg, rest)
	case "use":
		code = cmdUse(cfg, rest)
	case "release":
		if len(rest) != 1 {
			fail(errors.New("usage: baton release ENGINE"))
		}
		code = simple(cfg, "/api/engines/"+rest[0]+"/release?wait=1", nil, rest[0]+" released")
	case "reclaim":
		if len(rest) != 1 {
			fail(errors.New("usage: baton reclaim ENGINE"))
		}
		code = simple(cfg, "/api/engines/"+rest[0]+"/reclaim?wait=1", nil, rest[0]+" stopped; its memory is free")
	case "free":
		code = simple(cfg, "/api/free?wait=1", nil, "all engines released")
	case "hold":
		code = cmdHold(cfg, rest)
	case "unhold":
		code = simple(cfg, "/api/unhold", nil, "hold released")
	case "hook":
		code = cmdHook(cfg, rest)
	case "key":
		k, err := gateway.LoadKey(cfg)
		if err != nil {
			fail(err)
		}
		fmt.Println(k)
	case "config":
		fmt.Println(cfg.Path)
		fmt.Println("state:", cfg.Daemon.StateDir)
		for _, e := range cfg.Engines {
			fmt.Printf("engine %-10s %-8s %s gpus=%v release=%s game_release=%s\n", e.Name, e.Kind, e.URL, e.GPUs, e.Release, e.GameRelease)
		}
		fmt.Println("config ok")
	case "autostart":
		code = cmdAutostart(rest, path)
	case "firewall":
		code = cmdFirewall(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		code = 2
	}
	os.Exit(code)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "baton:", err)
	os.Exit(2)
}

// --- daemon ---

func cmdDaemon(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	noTray := fs.Bool("no-tray", false, "no notification-area icon")
	_ = fs.Parse(args)

	ln, err := net.Listen("tcp", cfg.Daemon.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "baton: cannot listen on %s (already running?): %v\n", cfg.Daemon.Listen, err)
		return 2
	}
	ev := events.Open(cfg.Daemon.StateDir)
	tel := telemetry.NewSampler()
	det, err := detect.New(cfg)
	if err != nil {
		fail(err)
	}
	store, err := games.Open(filepath.Join(filepath.Dir(cfg.Path), "games.json"))
	if err != nil {
		fail(err)
	}
	det.SetUser(store.Data())
	store.OnChange(det.SetUser)
	rec := harbor.Open(cfg.Daemon.StateDir)
	env := &engine.Env{StateDir: cfg.Daemon.StateDir, Events: ev, Sample: tel.Sample, Record: rec.Request}
	var engines []engine.Engine
	for _, ec := range cfg.Engines {
		if !ec.IsEnabled() {
			continue
		}
		e, err := engine.New(ec, env)
		if err != nil {
			fail(err)
		}
		engines = append(engines, e)
	}
	b := broker.New(cfg, tel, ev, det, engines)
	b.SetRecorder(rec)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	srv := server.New(b, cfg, ev, store, det)
	srv.Quit = cancel
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()
	dash := "http://" + cfg.Daemon.Listen + "/"

	var gwSrv *http.Server
	var gwHandler http.Handler
	if cfg.Gateway.Enabled {
		gw, err := gateway.New(b, cfg, ev)
		if err != nil {
			fail(err)
		}
		gwHandler = gw
		gln, err := net.Listen("tcp", cfg.Gateway.Listen)
		if err != nil {
			ev.Errorf("gateway", "", "gateway cannot listen on %s: %v", cfg.Gateway.Listen, err)
		} else {
			gwSrv = &http.Server{Handler: gw, ReadHeaderTimeout: 10 * time.Second}
			go func() { _ = gwSrv.Serve(gln) }()
		}
	}
	if cfg.Remote.Tailscale {
		remote := netx.NewRemote(func(f string, a ...any) { ev.Infof("remote", "", f, a...) })
		remote.Add(cfg.Daemon.Listen, srv)
		if gwHandler != nil {
			remote.Add(cfg.Gateway.Listen, gwHandler)
		}
		b.SetRemote(remote.Status)
		go remote.Run(ctx)
	}
	ev.Infof("start", "", "baton %s started: dashboard %s, gateway http://%s/v1", version, dash, cfg.Gateway.Listen)
	fmt.Printf("baton %s\n  dashboard  %s\n  gateway    http://%s/v1  (key: baton key)\n  config     %s\n", version, dash, cfg.Gateway.Listen, cfg.Path)

	go b.Run(ctx)
	if cfg.Daemon.Tray && !*noTray {
		go func() { <-ctx.Done(); tray.Stop() }()
		tray.Run(b, dash, cancel)
		cancel()
	} else {
		<-ctx.Done()
	}
	sctx, scancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer scancel()
	_ = httpSrv.Shutdown(sctx)
	if gwSrv != nil {
		_ = gwSrv.Shutdown(sctx)
	}
	_ = rec.Save()
	ev.Infof("start", "", "baton stopped (engines left running)")
	return 0
}

// cmdStop asks the daemon to quit and waits until it has.
func cmdStop(cfg *config.Config) int {
	if err := call(cfg, "POST", "/api/quit", nil, nil, 5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "baton:", err)
		return 1
	}
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if _, err := getView(cfg); err != nil {
			fmt.Println("baton stopped")
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "baton: asked to stop but still answering")
	return 1
}

func cmdStart(cfg *config.Config, cfgPath string) int {
	if _, err := getView(cfg); err == nil {
		fmt.Println("baton is already running: http://" + cfg.Daemon.Listen + "/")
		return 0
	}
	exe, _ := os.Executable()
	_ = os.MkdirAll(cfg.Daemon.StateDir, 0o755)
	logf, err := os.OpenFile(filepath.Join(cfg.Daemon.StateDir, "baton.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fail(err)
	}
	cmd := exec.Command(exe, "--config", cfgPath, "daemon")
	cmd.Stdout, cmd.Stderr = logf, logf
	const createNoWindow, createNewProcessGroup = 0x08000000, 0x200
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow | createNewProcessGroup, HideWindow: true}
	if err := cmd.Start(); err != nil {
		fail(err)
	}
	logf.Close()
	for i := 0; i < 40; i++ {
		time.Sleep(250 * time.Millisecond)
		if _, err := getView(cfg); err == nil {
			fmt.Println("baton started: http://" + cfg.Daemon.Listen + "/")
			return 0
		}
	}
	fmt.Fprintln(os.Stderr, "baton did not answer; see", filepath.Join(cfg.Daemon.StateDir, "baton.log"))
	return 1
}

// cmdFirewall adds one inbound rule: baton.exe, private networks, Tailscale
// addresses only. netsh needs admin, so Windows asks once.
func cmdFirewall(cfg *config.Config) int {
	exe, _ := os.Executable()
	ports := []string{portOnly(cfg.Daemon.Listen)}
	if cfg.Gateway.Enabled {
		ports = append(ports, portOnly(cfg.Gateway.Listen))
	}
	const rule = `name="Baton (Tailscale devices)"`
	// Replace any earlier rule, then add it, both in one elevated shell.
	script := fmt.Sprintf(`/c netsh advfirewall firewall delete rule %s >nul 2>&1 & netsh advfirewall firewall add rule %s dir=in action=allow protocol=TCP localport=%s program="%s" profile=private remoteip=100.64.0.0/10 enable=yes`,
		rule, rule, strings.Join(ports, ","), exe)
	ps := fmt.Sprintf("$p = Start-Process cmd.exe -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList '%s'; exit $p.ExitCode", strings.ReplaceAll(script, "'", "''"))
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", ps)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "baton: firewall rule not added (admin prompt declined?):", err)
		return 1
	}
	fmt.Printf("firewall: Tailscale devices may now reach baton on TCP %s\n", strings.Join(ports, ", "))
	return 0
}

func portOnly(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func cmdAutostart(args []string, cfgPath string) int {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		fail(errors.New("usage: baton autostart on|off"))
	}
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		fail(err)
	}
	defer k.Close()
	if args[0] == "off" {
		_ = k.DeleteValue("baton")
		fmt.Println("autostart off")
		return 0
	}
	exe, _ := os.Executable()
	abs, _ := filepath.Abs(cfgPath)
	// conhost --headless runs a console program with no window at all.
	val := fmt.Sprintf(`"%s\System32\conhost.exe" --headless "%s" --config "%s" daemon`, os.Getenv("SystemRoot"), exe, abs)
	if err := k.SetStringValue("baton", val); err != nil {
		fail(err)
	}
	fmt.Println("autostart on:", val)
	return 0
}

// --- client side ---

func call(cfg *config.Config, method, path string, in, out any, timeout time.Duration) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+cfg.Daemon.Listen+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Baton", "cli")
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("baton daemon not reachable at %s (start it with: baton start)", cfg.Daemon.Listen)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		var e struct{ Error string }
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func getView(cfg *config.Config) (*broker.View, error) {
	var v broker.View
	err := call(cfg, "GET", "/api/status?since=0", nil, &v, 3*time.Second)
	return &v, err
}

func simple(cfg *config.Config, path string, in any, ok string) int {
	if err := call(cfg, "POST", path, in, nil, 30*time.Minute); err != nil {
		fmt.Fprintln(os.Stderr, "baton:", err)
		return 1
	}
	fmt.Println(ok)
	return 0
}

func cmdUse(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("use", flag.ExitOnError)
	model := fs.String("model", "", "model to load (Unsloth)")
	noWait := fs.Bool("no-wait", false, "return at once")
	if len(args) == 0 {
		fail(errors.New("usage: baton use ENGINE [--model M] [--no-wait]"))
	}
	name := args[0]
	_ = fs.Parse(args[1:])
	path := "/api/engines/" + name + "/activate"
	if !*noWait {
		path += "?wait=1"
		fmt.Printf("giving the GPUs to %s (Ctrl-C stops waiting, not the switch)...\n", name)
	}
	return simple(cfg, path, map[string]any{"model": *model}, name+" holds the GPUs")
}

func cmdHold(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("hold", flag.ExitOnError)
	reason := fs.String("reason", "manual hold", "why")
	dur := fs.Duration("for", 0, "end the hold after this long (0 = until baton unhold)")
	_ = fs.Parse(args)
	return simple(cfg, "/api/hold", map[string]any{"reason": *reason, "minutes": dur.Minutes()}, "GPUs held: "+*reason+" (baton unhold ends it)")
}

func cmdHook(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	source := fs.String("source", "apollo", "who sends it")
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: baton hook stream-start|stream-end|game-start|game-end [--source S]")
		return 0
	}
	event := args[0]
	_ = fs.Parse(args[1:])
	if err := call(cfg, "POST", "/api/hook", map[string]string{"event": event, "source": *source}, nil, 5*time.Second); err != nil {
		fmt.Fprintln(os.Stderr, "baton:", err)
	}
	return 0 // never fail the caller (Apollo aborts a launch when a prep command fails)
}

func cmdEvents(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("events", flag.ExitOnError)
	follow := fs.Bool("f", false, "follow")
	_ = fs.Parse(args)
	var seq int64
	for {
		var v broker.View
		if err := call(cfg, "GET", fmt.Sprintf("/api/status?since=%d", seq), nil, &v, 5*time.Second); err != nil {
			fmt.Fprintln(os.Stderr, "baton:", err)
			return 1
		}
		for _, e := range v.Events {
			printEvent(e)
			seq = e.Seq
		}
		if !*follow {
			return 0
		}
		time.Sleep(time.Second)
	}
}

func cmdLogs(cfg *config.Config, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fail(errors.New("usage: baton logs ENGINE [-f] [-n N]"))
	}
	name := args[0]
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "keep printing new lines")
	n := fs.Int("n", 40, "lines of history to show")
	_ = fs.Parse(args[1:])
	p := engine.LogPath(cfg.Daemon.StateDir, name)
	text, next, _, err := engine.ReadLog(p, -1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "baton: no log for %s yet (%s); it exists once baton starts %s\n", name, p, name)
		return 1
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > *n {
		lines = lines[len(lines)-*n:]
	}
	fmt.Println(strings.Join(lines, "\n"))
	for *follow {
		time.Sleep(500 * time.Millisecond)
		t, nx, size, err := engine.ReadLog(p, next)
		if err != nil {
			continue
		}
		if size < next { // rotated or truncated: start over at the top
			next = 0
			continue
		}
		fmt.Print(t)
		next = nx
	}
	return 0
}

func printEvent(e events.Event) {
	lv := " "
	switch e.Level {
	case events.Warn:
		lv = "!"
	case events.Err:
		lv = "x"
	}
	fmt.Printf("%s %s %-9s %s\n", e.At.Local().Format("15:04:05"), lv, e.Kind, e.Msg)
}

func gib(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/(1<<30)) }

func printGPUs(gpus []telemetry.GPU) {
	for _, g := range gpus {
		fmt.Printf("GPU %d  %-16s %5s/%s GiB  load %3d%%  %2d°C  %3.0f/%.0f W  fan %d%%",
			g.Index, strings.TrimPrefix(g.Name, "NVIDIA GeForce "), gib(g.MemUsed), gib(g.MemTotal), g.Util, g.TempC, g.PowerW, g.PowerMaxW, g.FanPct)
		if len(g.Encoders) > 0 {
			fmt.Printf("  NVENC x%d", len(g.Encoders))
		}
		fmt.Println()
		var parts []string
		for i, h := range g.Holders {
			if i >= 6 {
				break
			}
			s := fmt.Sprintf("%s %.1fG", h.Name, float64(h.MiB)/1024)
			if h.Tag != "" {
				s += " [" + h.Tag + "]"
			}
			parts = append(parts, s)
		}
		if len(parts) > 0 {
			fmt.Println("       " + strings.Join(parts, " · "))
		}
	}
}

func printMem(m win.MemInfo, holders []broker.RAMHolder) {
	if m.Total == 0 {
		return
	}
	fmt.Printf("RAM    %-16s %5s/%s GiB  in use %d%%  cached %s GiB  commit %s/%s GiB\n",
		"system", gib(m.Used), gib(m.Total), m.Load, gib(m.Cached), gib(m.CommitTotal), gib(m.CommitLimit))
	var parts []string
	for i, h := range holders {
		if i >= 6 {
			break
		}
		s := fmt.Sprintf("%s %.1fG", h.Name, float64(h.MiB)/1024)
		if h.Count > 1 {
			s = fmt.Sprintf("%s x%d %.1fG", h.Name, h.Count, float64(h.MiB)/1024)
		}
		if h.Tag != "" {
			s += " [" + h.Tag + "]"
		}
		parts = append(parts, s)
	}
	if len(parts) > 0 {
		fmt.Println("       " + strings.Join(parts, " · "))
	}
}

func cmdStatus(cfg *config.Config, args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(args)
	v, err := getView(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "baton:", err)
		return 2
	}
	if *asJSON {
		v.Events = nil
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return 0
	}
	line := strings.ToUpper(string(v.Mode))
	if v.ModeWhy != "" {
		line += ": " + v.ModeWhy
	} else if v.Holder != "" {
		line += ": " + v.Holder + " holds the GPUs"
	}
	if v.RestoreTo != "" {
		line += " (restores " + v.RestoreTo + " after)"
	}
	fmt.Println(line)
	if v.Op != nil {
		fmt.Printf("in progress: %s %s\n", v.Op.Name, v.Op.Stage)
	}
	fmt.Println()
	printGPUs(v.GPUs)
	if v.Memory != nil {
		printMem(v.Memory.MemInfo, v.Memory.Holders)
	}
	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ENGINE\tSTATE\tMODEL\tVRAM\tRAM\tNOTE")
	for _, e := range v.Engines {
		state := "stopped"
		switch {
		case e.Aground != nil:
			state = "aground"
		case e.Loading:
			state = "loading"
		case e.Holder && (e.Loaded || e.Up):
			state = "holds GPUs"
		case e.Loaded:
			state = "loaded"
		case e.Up:
			state = "up"
		}
		var vr []string
		keys := make([]int, 0, len(e.VRAMMiB))
		for k := range e.VRAMMiB {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, k := range keys {
			if e.VRAMMiB[k] > 0 {
				vr = append(vr, fmt.Sprintf("%d:%.1fG", k, float64(e.VRAMMiB[k])/1024))
			}
		}
		note := e.Detail
		if e.Busy {
			note = "busy: " + e.BusyWhy
		}
		if e.Err != "" {
			note = "error: " + firstLine(e.Err)
		}
		if e.Aground != nil {
			note = fmt.Sprintf("still holds %.1f GiB after release (baton reclaim %s)", float64(e.Aground.TotalMiB)/1024, e.Name)
		}
		ram := ""
		if e.RAMMiB >= 64 {
			ram = fmt.Sprintf("%.1fG", float64(e.RAMMiB)/1024)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, state, e.Model, strings.Join(vr, " "), ram, note)
	}
	tw.Flush()
	if len(v.Hits) > 0 {
		fmt.Println()
		for _, h := range v.Hits {
			fmt.Printf("%-6s %s (%s, pid %d)\n", h.Kind, h.Exe, h.Rule, h.PID)
		}
	}
	return 0
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func cmdGPUs() int {
	s := telemetry.NewSampler()
	snap := s.Sample()
	if snap.Err != "" {
		fmt.Fprintln(os.Stderr, "baton:", snap.Err)
	}
	printGPUs(snap.GPUs)
	printMem(snap.Mem, nil)
	return 0
}
