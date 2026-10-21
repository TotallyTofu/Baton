// Package config loads baton.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/windows/registry"
)

// Duration is a time.Duration that reads from TOML strings like "20s".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func D(v time.Duration) Duration { return Duration{v} }

type Config struct {
	Daemon  Daemon   `toml:"daemon"`
	Gateway Gateway  `toml:"gateway"`
	Policy  Policy   `toml:"policy"`
	Engines []Engine `toml:"engine"`
	Claims  []Claim  `toml:"claim"`
	Detect  Detect   `toml:"detect"`
	Remote  Remote   `toml:"remote"`

	Path string `toml:"-"` // where it was read from
}

type Daemon struct {
	Listen   string   `toml:"listen"`    // control API + dashboard
	Poll     Duration `toml:"poll"`      // telemetry and detection cycle
	StateDir string   `toml:"state_dir"` // events, state, engine logs
	Tray     bool     `toml:"tray"`
}

type Gateway struct {
	Enabled       bool     `toml:"enabled"`
	Listen        string   `toml:"listen"`
	RequireKey    bool     `toml:"require_key"`
	DefaultEngine string   `toml:"default_engine"` // engine for model names nobody serves; "" = 404
	AllowHosts    []string `toml:"allow_hosts"`    // extra Host names accepted besides loopback
}

type Policy struct {
	Restore       bool     `toml:"restore"`        // reload what a game or stream pushed out once it ends
	Linger        Duration `toml:"linger"`         // a game or stream must be gone this long before it counts as over
	ReleaseWait   Duration `toml:"release_wait"`   // how long to wait for VRAM to read free after a release
	StrictRelease bool     `toml:"strict_release"` // an engine still holding VRAM after release_wait blocks its GPUs (aground) instead of being ignored
	KVFlushWait   Duration `toml:"kv_flush_wait"`  // before stopping an engine that writes its KV cache to disk, wait this long for it to finish (0 = don't)
	ResidualMiB   int      `toml:"residual_mib"`   // VRAM a released engine may keep per GPU (its CUDA context)
	BusyUtilPct   float64  `toml:"busy_util_pct"`  // an engine's processes above this GPU load count as busy
	SwitchDrain   Duration `toml:"switch_drain"`   // how long a switch waits for the holder's work to finish
	Game          GamePol  `toml:"game"`
	Stream        StreamPo `toml:"stream"`
	ComfyIdleRet  Duration `toml:"comfy_return_after"` // after ComfyUI idles this long, give the GPU back to the engine it took it from (0 = never)
}

type GamePol struct {
	Drain Duration `toml:"drain"` // how long a running job may finish before a game takes the GPUs anyway
}

type StreamPo struct {
	EncodeGPU  int      `toml:"encode_gpu"`
	MinFreeMiB int      `toml:"min_free_mib"` // keep the LLM loaded only while the encode GPU has this much free
	HoldComfy  bool     `toml:"hold_comfyui"` // free ComfyUI and hold new jobs while streaming
	ComfyDrain Duration `toml:"comfyui_drain"`
}

// Remote opens the dashboard and gateway to your other devices.
type Remote struct {
	Tailscale bool `toml:"tailscale"` // also listen on this PC's Tailscale address, for Tailscale devices only
}

type Detect struct {
	Games          bool     `toml:"games"`           // built-in game rules (Steam, Epic, GOG, Xbox, EA, Ubisoft, emulators)
	StreamEncoders []string `toml:"stream_encoders"` // processes whose NVENC sessions mean "streaming"
	IgnoreExe      []string `toml:"ignore_exe"`      // never treat these as games
}

// Claim is a user rule that turns a running process into a game (hold) or a
// stream.
type Claim struct {
	Name   string   `toml:"name"`
	Exe    []string `toml:"exe"`    // file names, case-insensitive, globs allowed
	Path   string   `toml:"path"`   // regex on the full image path
	Not    string   `toml:"not"`    // regex on the full image path that excludes
	Action string   `toml:"action"` // "game" (default) or "stream"
}

type Model struct {
	Name         string         `toml:"name"`           // what clients ask for
	ModelPath    string         `toml:"model_path"`     // unsloth: what to load
	GGUFVariant  string         `toml:"gguf_variant"`   // unsloth
	MaxSeqLength int            `toml:"max_seq_length"` // unsloth
	Extra        map[string]any `toml:"extra"`          // unsloth: any other /api/inference/load fields
}

type Engine struct {
	Name    string `toml:"name"`
	Kind    string `toml:"kind"` // tabby | unsloth | comfyui
	URL     string `toml:"url"`
	GPUs    []int  `toml:"gpus"`
	Enabled *bool  `toml:"enabled"`

	// Starting and stopping the engine's server.
	Dir          string            `toml:"dir"`
	Start        []string          `toml:"start"`
	Env          map[string]string `toml:"env"`
	ShowWindow   bool              `toml:"show_window"`
	ReadyTimeout Duration          `toml:"ready_timeout"`

	// How it gives the GPU back: "stop" kills the server, "unload" asks it to
	// drop its model, "free" (ComfyUI) unloads models and frees cached memory.
	Release     string `toml:"release"`
	GameRelease string `toml:"game_release"`

	// Credentials: never in this file, only where to find them.
	TokensFile string `toml:"tokens_file"` // tabby: api_tokens.yml
	APIKeyEnv  string `toml:"api_key_env"` // unsloth: env var holding a Studio API key
	APIKeyFile string `toml:"api_key_file"`

	// Gateway routing.
	Serves []string `toml:"serves"`
	Models []Model  `toml:"model"`

	// ComfyUI Desktop moves its port when the configured one is taken; it
	// records the real one here.
	DesktopProcsDir string `toml:"desktop_procs_dir"`
}

func (e Engine) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

func Default() *Config {
	return &Config{
		Daemon:  Daemon{Listen: "127.0.0.1:9797", Poll: D(2 * time.Second), Tray: true},
		Gateway: Gateway{Enabled: true, Listen: "127.0.0.1:9799", RequireKey: true},
		Policy: Policy{
			Restore:       true,
			Linger:        D(20 * time.Second),
			ReleaseWait:   D(60 * time.Second),
			StrictRelease: true,
			KVFlushWait:   D(20 * time.Second),
			ResidualMiB:   900,
			BusyUtilPct:   20,
			SwitchDrain:   D(10 * time.Minute),
			Game:          GamePol{Drain: D(15 * time.Second)},
			Stream:        StreamPo{EncodeGPU: 0, MinFreeMiB: 768, HoldComfy: true, ComfyDrain: D(60 * time.Second)},
		},
		Detect: Detect{
			Games:          true,
			StreamEncoders: []string{"sunshine.exe", "apollo.exe", "steam.exe", "steamwebhelper.exe", "streaming_client.exe"},
		},
	}
}

// Locate finds the config file: an explicit path, $BATON_CONFIG, baton.toml
// beside the executable, then %APPDATA%\baton\baton.toml.
func Locate(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if p := os.Getenv("BATON_CONFIG"); p != "" {
		return p
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "baton.toml")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "baton", "baton.toml")
	}
	return "baton.toml"
}

func Load(path string) (*Config, error) {
	c := Default()
	c.Path = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s (copy baton.example.toml there)", path)
	}
	if err != nil {
		return nil, err
	}
	md, err := toml.Decode(string(b), c)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		keys := make([]string, 0, len(und))
		for _, k := range und {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	c.expandPaths()
	if c.Daemon.StateDir == "" {
		base, _ := os.UserCacheDir() // %LOCALAPPDATA%
		c.Daemon.StateDir = filepath.Join(base, "baton")
	}
	return c, c.Validate()
}

// expandPaths resolves %VARIABLES% (like %APPDATA%) in paths and start
// commands, so one config works for any Windows user.
func (c *Config) expandPaths() {
	x := func(s string) string {
		if !strings.Contains(s, "%") {
			return s
		}
		if v, err := registry.ExpandString(s); err == nil {
			return v
		}
		return s
	}
	c.Daemon.StateDir = x(c.Daemon.StateDir)
	for i := range c.Engines {
		e := &c.Engines[i]
		e.Dir, e.TokensFile, e.APIKeyFile, e.DesktopProcsDir = x(e.Dir), x(e.TokensFile), x(e.APIKeyFile), x(e.DesktopProcsDir)
		for j := range e.Start {
			e.Start[j] = x(e.Start[j])
		}
	}
}

func (c *Config) Validate() error {
	var errs []string
	names := map[string]bool{}
	served := map[string]string{}
	for i := range c.Engines {
		e := &c.Engines[i]
		if e.Name == "" {
			errs = append(errs, fmt.Sprintf("engine #%d has no name", i+1))
		}
		if names[e.Name] {
			errs = append(errs, fmt.Sprintf("engine %q defined twice", e.Name))
		}
		names[e.Name] = true
		switch e.Kind {
		case "tabby", "unsloth", "comfyui":
		default:
			errs = append(errs, fmt.Sprintf("engine %q: kind must be tabby, unsloth or comfyui", e.Name))
		}
		if e.URL == "" {
			errs = append(errs, fmt.Sprintf("engine %q: url is required", e.Name))
		}
		if len(e.GPUs) == 0 {
			errs = append(errs, fmt.Sprintf("engine %q: gpus is required (e.g. [0, 1])", e.Name))
		}
		if e.ReadyTimeout.Duration == 0 {
			e.ReadyTimeout = D(10 * time.Minute)
		}
		if e.Release == "" {
			e.Release = map[string]string{"tabby": "stop", "unsloth": "unload", "comfyui": "free"}[e.Kind]
		}
		if e.GameRelease == "" {
			e.GameRelease = e.Release
		}
		for _, r := range []string{e.Release, e.GameRelease} {
			if r != "stop" && r != "unload" && r != "free" {
				errs = append(errs, fmt.Sprintf("engine %q: release %q must be stop, unload or free", e.Name, r))
			}
		}
		all := append([]string(nil), e.Serves...)
		for _, m := range e.Models {
			all = append(all, m.Name)
		}
		for _, s := range all {
			k := strings.ToLower(s)
			if other, ok := served[k]; ok && other != e.Name {
				errs = append(errs, fmt.Sprintf("model %q is served by both %s and %s", s, other, e.Name))
			}
			served[k] = e.Name
		}
	}
	if c.Gateway.DefaultEngine != "" && !names[c.Gateway.DefaultEngine] {
		errs = append(errs, fmt.Sprintf("gateway.default_engine %q is not an engine", c.Gateway.DefaultEngine))
	}
	for _, cl := range c.Claims {
		if cl.Action != "" && cl.Action != "game" && cl.Action != "stream" {
			errs = append(errs, fmt.Sprintf("claim %q: action must be game or stream", cl.Name))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "\n"))
	}
	return nil
}
