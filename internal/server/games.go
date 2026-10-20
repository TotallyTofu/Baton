package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"baton/internal/detect"
	"baton/internal/games"
	"baton/internal/win"
)

// Apps that use the GPU but are never games; kept out of the suggestions.
var notGames = map[string]bool{
	"msedge.exe": true, "msedgewebview2.exe": true, "chrome.exe": true, "firefox.exe": true, "brave.exe": true,
	"opera.exe": true, "vivaldi.exe": true, "arc.exe": true, "zen.exe": true,
	"steam.exe": true, "steamwebhelper.exe": true, "epicgameslauncher.exe": true, "galaxyclient.exe": true,
	"eadesktop.exe": true, "ubisoftconnect.exe": true, "battle.net.exe": true, "playnite.desktopapp.exe": true,
	"playnite.fullscreenapp.exe": true, "kyber_launcher.exe": true,
	"discord.exe": true, "slack.exe": true, "spotify.exe": true, "ms-teams.exe": true, "teams.exe": true, "zoom.exe": true,
	"nvidia overlay.exe": true, "nvidia app.exe": true, "nvcontainer.exe": true, "nvidia broadcast.exe": true,
	"obs64.exe": true, "sunshine.exe": true, "parsecd.exe": true, "rtss.exe": true, "msiafterburner.exe": true,
	"explorer.exe": true, "dwm.exe": true, "windowsterminal.exe": true, "code.exe": true, "cursor.exe": true,
	"claude.exe": true, "snippingtool.exe": true, "photos.exe": true, "wallpaper64.exe": true, "wallpaper32.exe": true,
	"lossless scaling.exe": true, "python.exe": true, "pythonw.exe": true, "node.exe": true, "baton.exe": true,
	"llama-server.exe": true, "comfy desktop.exe": true, "comfyui.exe": true, "unsloth-studio.exe": true,
	"blender.exe": true, "resolve.exe": true, "photoshop.exe": true, "afterfx.exe": true, "adobe premiere pro.exe": true,
	"vlc.exe": true, "mpv.exe": true, "mpc-hc64.exe": true, "potplayermini64.exe": true,
}

type gameEntry struct {
	Path    string `json:"path"`
	Running bool   `json:"running"`
}

type suggestion struct {
	Name string  `json:"name"`
	Path string  `json:"path"`
	MiB  int     `json:"mib"`
	Util float64 `json:"util"`
}

func (s *Server) gamesView() map[string]any {
	d := s.games.Data()
	v := s.b.View(1 << 62)
	var running []detect.Hit
	matched := map[string]bool{}
	for _, h := range v.Hits {
		if h.Kind == "game" {
			running = append(running, h)
			if h.Match != "" {
				matched[strings.ToLower(h.Match)] = true
			}
		}
	}
	conv := func(es []games.Entry, mark bool) []gameEntry {
		out := make([]gameEntry, 0, len(es))
		for _, e := range es {
			out = append(out, gameEntry{Path: e.Path, Running: mark && matched[strings.ToLower(e.Path)]})
		}
		return out
	}

	// Unrecognized programs holding real VRAM right now.
	byPath := map[string]*suggestion{}
	for _, g := range v.GPUs {
		for _, h := range g.Holders {
			if h.Tag != "" || h.Path == "" || notGames[strings.ToLower(h.Name)] || d.Ignored(h.Path) {
				continue
			}
			lp := strings.ToLower(h.Path)
			if strings.HasPrefix(lp, `c:\windows\`) || strings.Contains(lp, `\windowsapps\microsoft.`) {
				continue
			}
			sg := byPath[lp]
			if sg == nil {
				sg = &suggestion{Name: strings.TrimSuffix(h.Name, filepath.Ext(h.Name)), Path: h.Path}
				byPath[lp] = sg
			}
			sg.MiB += h.MiB
			if h.Util > sg.Util {
				sg.Util = h.Util
			}
		}
	}
	sugg := []suggestion{}
	for _, sg := range byPath {
		if sg.MiB >= 200 || (sg.Util >= 15 && sg.MiB >= 64) {
			sugg = append(sugg, *sg)
		}
	}
	sort.Slice(sugg, func(i, j int) bool { return sugg[i].MiB > sugg[j].MiB })
	if len(sugg) > 6 {
		sugg = sugg[:6]
	}
	if running == nil {
		running = []detect.Hit{}
	}
	return map[string]any{
		"file":        s.games.Path(),
		"builtin":     map[string]any{"on": s.det.BuiltinOn(), "names": detect.BuiltinNames()},
		"folders":     conv(d.Folders, true),
		"games":       conv(d.Games, true),
		"ignore":      conv(d.Ignore, false),
		"running":     running,
		"suggestions": sugg,
	}
}

var pickers = map[string]struct {
	folder    bool
	list      string
	title, ok string
}{
	"folder":        {true, "folder", "Choose a folder with games in it", "Add folder"},
	"game":          {false, "game", "Choose a game", "Add game"},
	"ignore-folder": {true, "ignore", "Choose a folder baton should never count as a game", "Never count"},
	"ignore-game":   {false, "ignore", "Choose a program baton should never count as a game", "Never count"},
}

func (s *Server) gamesAction(w http.ResponseWriter, r *http.Request, action string) {
	var in struct {
		Kind string `json:"kind"`
		Path string `json:"path"`
		On   bool   `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	var err error
	resp := map[string]any{"ok": true}
	switch action {
	case "add":
		var kind string
		kind, err = s.games.Add(in.Kind, in.Path)
		resp["kind"], resp["path"] = kind, games.Clean(in.Path)
	case "remove":
		err = s.games.Remove(in.Kind, in.Path)
	case "builtin":
		err = s.games.SetBuiltin(in.On)
	case "pick":
		p, ok := pickers[in.Kind]
		if !ok {
			fail(w, 400, errors.New("unknown picker"))
			return
		}
		var path string
		path, err = win.Pick(p.folder, p.title, p.ok)
		if err == nil && path == "" {
			writeJSON(w, 200, map[string]any{"ok": true, "cancelled": true})
			return
		}
		if err == nil {
			_, err = s.games.Add(p.list, path)
			resp["kind"], resp["path"] = p.list, games.Clean(path)
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		status := 400
		if errors.Is(err, win.ErrPickerBusy) {
			status = 409
		}
		fail(w, status, err)
		return
	}
	names := map[string]string{"folder": "library folders", "game": "games", "ignore": "never count"}
	switch action {
	case "add", "pick":
		s.ev.Infof("games", "", "added %s to %s", resp["path"], names[resp["kind"].(string)])
	case "remove":
		s.ev.Infof("games", "", "removed %s from %s", games.Clean(in.Path), names[in.Kind])
	case "builtin":
		s.ev.Infof("games", "", "built-in launcher rules %s", map[bool]string{true: "on", false: "off"}[in.On])
	}
	s.b.Refresh()
	writeJSON(w, 200, resp)
}
