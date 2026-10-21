package detect

import (
	"testing"

	"baton/internal/config"
	"baton/internal/telemetry"
	"baton/internal/win"
)

func TestGameRules(t *testing.T) {
	d, err := New(config.Default())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		`D:\SteamLibrary\steamapps\common\Elden Ring\Game\eldenring.exe`:                         true,
		`C:\Program Files (x86)\Steam\steamapps\common\Cyberpunk 2077\bin\x64\Cyberpunk2077.exe`: true,
		`C:\Program Files\Epic Games\HogwartsLegacy\Phoenix\Binaries\Win64\HogwartsLegacy.exe`:   true,
		`C:\Program Files\Epic Games\Control\Control_DX12.exe`:                                   true,
		`C:\XboxGames\Forza Horizon 5\Content\ForzaHorizon5.exe`:                                 true,
		`D:\SteamLibrary\steamapps\common\wallpaper_engine\wallpaper64.exe`:                      false,
		`D:\SteamLibrary\steamapps\common\Elden Ring\Game\UnityCrashHandler64.exe`:               false,
		`C:\Program Files (x86)\Steam\steam.exe`:                                                 false,
		`C:\Program Files\Epic Games\Launcher\Portal\Binaries\Win64\EpicGamesLauncher.exe`:       false,
		`C:\Users\me\AppData\Local\Programs\Python\Python311\python.exe`:                         false,
	}
	for path, want := range cases {
		name := path[len(path)-len(baseName(path)):]
		snap := &telemetry.Snapshot{Procs: []win.Proc{{PID: 100, Name: name, Path: path}}, Tree: win.NewTree(nil)}
		got := len(d.Scan(snap, nil)) > 0
		if got != want {
			t.Errorf("%s: game=%v, want %v", path, got, want)
		}
	}
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == 92 {
			return p[i+1:]
		}
	}
	return p
}

// DigUp's indexer and its search service are the same exe: args tells them
// apart, and only processes the exe already matched have their command line
// read.
func TestGuestClaimArgs(t *testing.T) {
	cfg := config.Default()
	cfg.Claims = []config.Claim{{Name: "digup", Exe: []string{"digup.exe"}, Args: `\bindex-service\b`, Action: "guest"}}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	lines := map[int]string{
		1: `"D:\DigUp\helper\digup.exe" search-service`,
		2: `"D:\DigUp\helper\digup.exe" index-service --backfill C:\Users\me\Documents`,
		3: `"C:\tools\other.exe" index-service`,
	}
	var asked []int
	d.cmdline = func(pid int, _ int64) (string, error) {
		asked = append(asked, pid)
		return lines[pid], nil
	}
	snap := &telemetry.Snapshot{Procs: []win.Proc{
		{PID: 1, Name: "digup.exe", Path: `D:\DigUp\helper\digup.exe`},
		{PID: 2, Name: "digup.exe", Path: `D:\DigUp\helper\digup.exe`},
		{PID: 3, Name: "other.exe", Path: `C:\tools\other.exe`},
		{PID: 4, Name: "DigUp.exe", Path: `D:\DigUp\DigUp.exe`},
	}, Tree: win.NewTree(nil)}
	hits := d.Scan(snap, nil)
	if len(hits) != 1 || hits[0].PID != 2 || hits[0].Kind != "guest" || hits[0].Rule != "digup" {
		t.Fatalf("hits = %+v, want the indexer (pid 2) as guest digup", hits)
	}
	for _, pid := range asked {
		if pid == 3 {
			t.Fatal("read the command line of a process whose exe didn't match")
		}
	}
}

// Qwen3-TTS apps run as "python app.py": the folder tells them from other
// scripts, args from the tests and pip runs in that folder.
func TestGuestClaimDir(t *testing.T) {
	cfg := config.Default()
	cfg.Claims = []config.Claim{{Name: "qwen3-tts", Exe: []string{"python*.exe"},
		Dir:  `(?i)\\(qwen3-TTS-Studio|Qwen3-Audiobook-Converter)$`,
		Args: `\b(qwen_tts_ui|app|audiobook_converter)\.py\b`, Action: "guest"}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	studio, book := `C:\vibe\podcast\qwen3-TTS-Studio`, `C:\vibe\audiobook converter\Qwen3-Audiobook-Converter`
	procs := []struct {
		name, line, dir string
		guest           bool
	}{
		{"python.exe", `.venv\Scripts\python.exe  qwen_tts_ui.py`, studio, true},                                  // venv launcher
		{"python.exe", `"C:\Python311\python.exe" qwen_tts_ui.py`, studio, true},                                  // the interpreter it starts
		{"python3.11.exe", `python3.11  app.py`, book, true},                                                      // Store Python
		{"python.exe", `python  app.py`, `C:\vibe\other`, false},                                                  // someone else's app.py
		{"python.exe", `python  test_text_pipeline.py`, book, false},                                              // tests
		{"python.exe", `python -m pip install -r requirements.txt`, studio, false},                                // installs
		{"pythonw.exe", `"C:\Python311\pythonw.exe" audiobook_converter.py --voice-design`, book + `\sub`, false}, // another folder
	}
	snap := &telemetry.Snapshot{Tree: win.NewTree(nil)}
	for i, p := range procs {
		snap.Procs = append(snap.Procs, win.Proc{PID: i + 1, Name: p.name})
	}
	d.cmdline = func(pid int, _ int64) (string, error) { return procs[pid-1].line, nil }
	d.cwd = func(pid int, _ int64) (string, error) { return procs[pid-1].dir, nil }
	got := map[int]bool{}
	for _, h := range d.Scan(snap, nil) {
		if h.Kind != "guest" || h.Rule != "qwen3-tts" {
			t.Fatalf("hit %+v", h)
		}
		got[h.PID] = true
	}
	for i, p := range procs {
		if got[i+1] != p.guest {
			t.Errorf("%s %q in %s: guest = %v, want %v", p.name, p.line, p.dir, got[i+1], p.guest)
		}
	}
}

// The games-folder rule from baton.toml.
func TestGamesFolderClaim(t *testing.T) {
	cfg := config.Default()
	cfg.Claims = []config.Claim{{Name: "games-folder", Path: `(?i)^[cd]:\\Games\\`, Not: `(?i)^c:\\Games\\KYBER Launcher\\`, Action: "game"}}
	d, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		`C:\Games\Stellar Blade\SB\Binaries\Win64\SB-Win64-Shipping.exe`: true,
		`C:\Games\Cbrpnk 2zero77\bin\x64\Cyberpunk2077.exe`:              true,
		`D:\Games\Marvels Spider-Man 2\Spider-Man2.exe`:                  true,
		`D:\Games\Resident Evil Requiem\re9.exe`:                         true,
		`C:\Games\KYBER Launcher\kyber_launcher.exe`:                     false,
		`C:\Games\KYBER Launcher\maxima-service.exe`:                     false,
		`D:\Games\Star Wars Outlaws\UnityCrashHandler64.exe`:             false,
		`D:\Gamesbackup\thing.exe`:                                       false,
	}
	for path, want := range cases {
		snap := &telemetry.Snapshot{Procs: []win.Proc{{PID: 100, Name: baseName(path), Path: path}}, Tree: win.NewTree(nil)}
		if got := len(d.Scan(snap, nil)) > 0; got != want {
			t.Errorf("%s: game=%v, want %v", path, got, want)
		}
	}
}
