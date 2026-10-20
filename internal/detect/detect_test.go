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
