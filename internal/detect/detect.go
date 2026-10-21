// Package detect turns the process table and the encoder sessions into
// "a game is running", "someone is streaming" and "a guest needs the cards".
package detect

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"baton/internal/config"
	"baton/internal/games"
	"baton/internal/telemetry"
	"baton/internal/win"
)

type Hit struct {
	Kind  string `json:"kind"` // game | stream | guest
	Rule  string `json:"rule"`
	PID   int    `json:"pid,omitempty"`
	Exe   string `json:"exe"`
	Path  string `json:"path,omitempty"`
	Match string `json:"match,omitempty"` // the games.json entry that matched
}

type rule struct {
	name   string
	kind   string
	exe    []string
	path   *regexp.Regexp
	not    *regexp.Regexp
	args   *regexp.Regexp // on the command line, read only for processes exe or path already matched
	dir    *regexp.Regexp // on the folder the process runs in, read likewise
	source string         // builtin | config
}

// Builtin game rules match where Windows launchers install games. Windows
// gives the image path for free, so path rules are reliable; a command line
// or working folder costs a query per process, so only claims with args or
// dir read them.
var builtin = []rule{
	{name: "steam", kind: "game", path: regexp.MustCompile(`(?i)\\steamapps\\common\\`),
		not: regexp.MustCompile(`(?i)\\steamapps\\common\\(wallpaper_engine|Lossless Scaling|Steamworks Shared|Steam Controller Configs|SteamVR\\bin\\win64\\(vrserver|vrmonitor|vrcompositor|vrdashboard|vrwebhelper))`)},
	{name: "epic", kind: "game", path: regexp.MustCompile(`(?i)\\Epic Games\\`),
		not: regexp.MustCompile(`(?i)\\Epic Games\\(Launcher|EpicOnlineServices|DirectXRedist)\\`)},
	{name: "gog", kind: "game", path: regexp.MustCompile(`(?i)\\GOG (Galaxy\\)?Games\\`)},
	{name: "xbox", kind: "game", path: regexp.MustCompile(`(?i)^[a-z]:\\XboxGames\\`),
		not: regexp.MustCompile(`(?i)\\XboxGames\\GameSave\\`)},
	{name: "ea", kind: "game", path: regexp.MustCompile(`(?i)\\EA Games\\`)},
	{name: "ubisoft", kind: "game", path: regexp.MustCompile(`(?i)\\Ubisoft Game Launcher\\games\\`)},
	{name: "emulator", kind: "game", exe: []string{
		"retroarch.exe", "yuzu.exe", "suyu.exe", "citron.exe", "eden.exe", "ryujinx.exe", "cemu.exe",
		"dolphin.exe", "pcsx2*.exe", "rpcs3.exe", "xenia*.exe", "ppsspp*.exe", "duckstation*.exe",
		"shadps4.exe", "vita3k.exe", "xemu.exe", "melonds.exe", "azahar.exe", "lime3ds*.exe", "citra*.exe",
	}},
}

// Helpers that live in game folders but are not the game.
var helperExe = []string{
	"unitycrashhandler*.exe", "crashreportclient.exe", "*crashhandler*.exe", "*crashpad*.exe",
	"easyanticheat*.exe", "beservice*.exe", "vc_redist*.exe", "vcredist*.exe", "dxsetup.exe",
	"ue4prereqsetup*.exe", "ueprereqsetup*.exe", "dotnet*.exe", "qtwebengineprocess.exe",
	"cefsharp.browsersubprocess.exe", "*uninst*.exe", "*setup*.exe", "*installer*.exe",
}

type Detector struct {
	builtinDefault bool
	claims         []rule
	ignore         []string
	encoders       map[string]bool
	cmdline        func(pid int, start int64) (string, error)
	cwd            func(pid int, start int64) (string, error)

	mu   sync.Mutex
	user games.Data
}

// SetUser installs the game list edited in the dashboard.
func (d *Detector) SetUser(u games.Data) {
	d.mu.Lock()
	d.user = u
	d.mu.Unlock()
}

// BuiltinOn reports whether the built-in launcher rules are active.
func (d *Detector) BuiltinOn() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.user.Builtin != nil {
		return *d.user.Builtin
	}
	return d.builtinDefault
}

// BuiltinNames lists the built-in rules for display.
func BuiltinNames() []string {
	out := make([]string, len(builtin))
	for i, r := range builtin {
		out[i] = r.name
	}
	return out
}

func New(cfg *config.Config) (*Detector, error) {
	d := &Detector{encoders: map[string]bool{}, ignore: append(helperExe, lower(cfg.Detect.IgnoreExe)...), builtinDefault: cfg.Detect.Games, cmdline: win.CommandLine, cwd: win.CurrentDir}
	for _, c := range cfg.Claims {
		r := rule{name: c.Name, kind: c.Action, exe: lower(c.Exe), source: "config"}
		if r.kind == "" {
			r.kind = "game"
		}
		if c.Path != "" {
			re, err := regexp.Compile(c.Path)
			if err != nil {
				return nil, err
			}
			r.path = re
		}
		if c.Not != "" {
			re, err := regexp.Compile(c.Not)
			if err != nil {
				return nil, err
			}
			r.not = re
		}
		if c.Args != "" {
			re, err := regexp.Compile(c.Args)
			if err != nil {
				return nil, err
			}
			r.args = re
		}
		if c.Dir != "" {
			re, err := regexp.Compile(c.Dir)
			if err != nil {
				return nil, err
			}
			r.dir = re
		}
		d.claims = append(d.claims, r)
	}
	for _, e := range cfg.Detect.StreamEncoders {
		d.encoders[strings.ToLower(e)] = true
	}
	return d, nil
}

func lower(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

func globAny(name string, pats []string) bool {
	for _, p := range pats {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Scan reports games, streams and guests in the snapshot. exclude names PIDs
// that belong to managed engines, which are never games.
func (d *Detector) Scan(snap *telemetry.Snapshot, exclude map[int]bool) []Hit {
	d.mu.Lock()
	user := d.user
	rules := d.claims
	if (user.Builtin == nil && d.builtinDefault) || (user.Builtin != nil && *user.Builtin) {
		rules = append(append([]rule(nil), builtin...), d.claims...)
	}
	d.mu.Unlock()
	var out []Hit
	for _, p := range snap.Procs {
		if exclude[p.PID] {
			continue
		}
		name := strings.ToLower(p.Name)
		if globAny(name, d.ignore) {
			continue
		}
		if p.Path != "" {
			r, m, ignored := user.Classify(p.Path)
			if ignored {
				continue
			}
			if r != "" {
				out = append(out, Hit{Kind: "game", Rule: r, PID: p.PID, Exe: p.Name, Path: p.Path, Match: m})
				continue
			}
		}
		for _, r := range rules {
			matched := false
			if len(r.exe) > 0 && globAny(name, r.exe) {
				matched = true
			}
			if r.path != nil && p.Path != "" && r.path.MatchString(p.Path) {
				matched = true
			}
			if matched && r.not != nil && p.Path != "" && r.not.MatchString(p.Path) {
				matched = false
			}
			if matched && r.args != nil {
				line, err := d.cmdline(p.PID, p.Start)
				matched = err == nil && r.args.MatchString(line)
			}
			if matched && r.dir != nil {
				dir, err := d.cwd(p.PID, p.Start)
				matched = err == nil && r.dir.MatchString(dir)
			}
			if matched {
				out = append(out, Hit{Kind: r.kind, Rule: r.name, PID: p.PID, Exe: p.Name, Path: p.Path})
				break
			}
		}
	}
	// NVENC sessions owned by a streaming host mean a stream is live.
	for _, g := range snap.GPUs {
		for _, s := range g.Encoders {
			p := snap.Tree.ByPID[s.PID]
			if d.encoders[strings.ToLower(p.Name)] {
				out = append(out, Hit{Kind: "stream", Rule: "nvenc", PID: s.PID, Exe: p.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}
