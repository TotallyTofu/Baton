package config

import (
	"os"
	"strings"
	"testing"
)

// The example shipped in the repo must always load.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../baton.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Engines) != 3 {
		t.Fatalf("engines = %d, want 3", len(c.Engines))
	}
	var comfy Engine
	for _, e := range c.Engines {
		if e.Kind == "comfyui" {
			comfy = e
		}
	}
	appdata := os.Getenv("APPDATA")
	if appdata == "" || strings.Contains(comfy.DesktopProcsDir, "%") || !strings.HasPrefix(comfy.DesktopProcsDir, appdata) {
		t.Fatalf("%%APPDATA%% not expanded: %q", comfy.DesktopProcsDir)
	}
	if c.Remote.Tailscale {
		t.Fatal("the example must not open baton to the tailnet by default")
	}
}
