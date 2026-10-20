// Package server is baton's control API and dashboard.
package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"baton/internal/broker"
	"baton/internal/config"
	"baton/internal/detect"
	"baton/internal/engine"
	"baton/internal/events"
	"baton/internal/games"
	"baton/internal/gateway"
	"baton/internal/netx"
)

//go:embed web/index.html
var indexHTML []byte

type Server struct {
	b     *broker.Broker
	cfg   *config.Config
	ev    *events.Log
	games *games.Store
	det   *detect.Detector
	Quit  func()
}

func New(b *broker.Broker, cfg *config.Config, ev *events.Log, store *games.Store, det *detect.Detector) *Server {
	return &Server{b: b, cfg: cfg, ev: ev, games: store, det: det}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !gateway.LoopbackHost(r.Host, nil) && !s.wide() && !netx.Trusted(r) {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost {
		// Changes need a custom header: a browser cannot send one cross-site
		// without a CORS preflight, which this server never approves.
		if r.Header.Get("X-Baton") == "" {
			http.Error(w, "missing X-Baton header", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !strings.HasSuffix(o, "://"+r.Host) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet && (path == "" || path == "/index.html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; img-src 'self' data:")
		w.Write(indexHTML)
	case r.Method == http.MethodGet && path == "/api/status":
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		writeJSON(w, 200, s.b.View(since))
	case r.Method == http.MethodGet && path == "/api/games":
		writeJSON(w, 200, s.gamesView())
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/games/"):
		s.gamesAction(w, r, strings.TrimPrefix(path, "/api/games/"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/engines/") && strings.HasSuffix(path, "/log"):
		s.engineLog(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/api/engines/"), "/log"))
	case r.Method == http.MethodGet && path == "/api/models":
		writeJSON(w, 200, s.b.Models())
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/engines/"):
		s.engineAction(w, r, strings.TrimPrefix(path, "/api/engines/"))
	case r.Method == http.MethodPost && path == "/api/free":
		s.run(w, r, "free all", func(ctx context.Context) error { return s.b.FreeAll(ctx) })
	case r.Method == http.MethodPost && path == "/api/hold":
		var in struct {
			Reason  string  `json:"reason"`
			Minutes float64 `json:"minutes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.b.SetHold(in.Reason, time.Duration(in.Minutes*float64(time.Minute)))
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodPost && path == "/api/unhold":
		s.b.ClearHold()
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodPost && path == "/api/hook":
		var in struct{ Event, Source string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if err := s.b.Hook(in.Event, in.Source); err != nil {
			fail(w, 400, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodPost && path == "/api/gate/comfyui/acquire":
		var in struct {
			PromptID string `json:"prompt_id"`
			WaitMS   int    `json:"wait_ms"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		wait := time.Duration(in.WaitMS) * time.Millisecond
		if wait <= 0 || wait > 30*time.Second {
			wait = 5 * time.Second
		}
		ok, why := s.b.GateAcquire(r.Context(), in.PromptID, wait)
		writeJSON(w, 200, map[string]any{"granted": ok, "reason": why, "error": strings.HasPrefix(why, "error: ")})
	case r.Method == http.MethodPost && path == "/api/gate/comfyui/done":
		var in struct {
			PromptID string `json:"prompt_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.b.GateDone(in.PromptID)
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodPost && path == "/api/quit":
		writeJSON(w, 200, map[string]any{"ok": true})
		if s.Quit != nil {
			go func() { time.Sleep(200 * time.Millisecond); s.Quit() }()
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) wide() bool {
	return strings.HasPrefix(s.cfg.Daemon.Listen, "0.0.0.0:") || strings.HasPrefix(s.cfg.Daemon.Listen, ":")
}

// run executes fn; with ?wait=1 it answers when fn finishes, else at once.
func (s *Server) run(w http.ResponseWriter, r *http.Request, what string, fn func(context.Context) error) {
	if r.URL.Query().Get("wait") == "1" {
		if err := fn(r.Context()); err != nil {
			status := 500
			if refused(err) {
				status = 409
			}
			fail(w, status, err)
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	go func() {
		err := fn(context.Background())
		var ag *broker.AgroundError
		// A release that ends aground has logged that itself.
		if refused(err) && !(errors.As(err, &ag) && ag.For == "") {
			s.ev.Warnf("switch", "", "%s refused: %v", what, err)
		}
	}()
	writeJSON(w, 202, map[string]any{"ok": true, "accepted": what})
}

// refused reports whether the broker turned a request down (the cards are
// reserved, or an engine is aground) rather than failed at it.
func refused(err error) bool {
	var h *broker.HeldError
	var ag *broker.AgroundError
	return errors.As(err, &h) || errors.As(err, &ag)
}

func (s *Server) engineAction(w http.ResponseWriter, r *http.Request, rest string) {
	name, action, _ := strings.Cut(rest, "/")
	if action == "log/open" {
		p := engine.LogPath(s.cfg.Daemon.StateDir, name)
		if _, err := os.Stat(p); err != nil {
			fail(w, 404, errors.New("no log yet"))
			return
		}
		_ = exec.Command("notepad.exe", p).Start()
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	var in struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	switch action {
	case "activate":
		s.run(w, r, "switch to "+name, func(ctx context.Context) error {
			return s.b.Activate(ctx, name, in.Model, "requested from "+caller(r))
		})
	case "release":
		s.run(w, r, "release "+name, func(ctx context.Context) error { return s.b.ReleaseEngine(ctx, name) })
	case "reclaim":
		s.run(w, r, "force stop "+name, func(ctx context.Context) error { return s.b.Reclaim(ctx, name) })
	default:
		http.NotFound(w, r)
	}
}

// engineLog serves an engine's log from byte offset ?from= (the tail when
// absent), plus the last request summary for TabbyAPI.
func (s *Server) engineLog(w http.ResponseWriter, r *http.Request, name string) {
	from := int64(-1)
	if v := r.URL.Query().Get("from"); v != "" {
		from, _ = strconv.ParseInt(v, 10, 64)
	}
	p := engine.LogPath(s.cfg.Daemon.StateDir, name)
	text, next, size, err := engine.ReadLog(p, from)
	if err != nil {
		writeJSON(w, 200, map[string]any{"exists": false, "path": p})
		return
	}
	out := map[string]any{"exists": true, "path": p, "text": text, "next": next, "size": size}
	if strings.Contains(strings.ToLower(name), "tabby") || r.URL.Query().Get("metrics") == "1" {
		if t, _, _, err := engine.ReadLog(p, -1); err == nil {
			out["last"] = engine.ParseTabbyMetrics(t)
		}
	}
	writeJSON(w, 200, out)
}

func caller(r *http.Request) string {
	if c := r.Header.Get("X-Baton"); c != "" && c != "1" {
		return c
	}
	return "dashboard"
}
