// Package gateway is one OpenAI-compatible port for every LLM engine. A
// request names a model; baton gives that model's engine the GPUs (releasing
// whatever holds them) and streams the answer through.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"baton/internal/broker"
	"baton/internal/config"
	"baton/internal/events"
	"baton/internal/harbor"
	"baton/internal/netx"
)

type Gateway struct {
	b   *broker.Broker
	cfg *config.Config
	ev  *events.Log
	key string
}

// KeyPath is where the gateway key lives.
func KeyPath(cfg *config.Config) string { return filepath.Join(cfg.Daemon.StateDir, "gateway.key") }

// LoadKey reads the gateway key, creating it on first use.
func LoadKey(cfg *config.Config) (string, error) {
	p := KeyPath(cfg)
	if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	k := "bt-" + hex.EncodeToString(raw)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	return k, os.WriteFile(p, []byte(k+"\n"), 0o600)
}

func New(b *broker.Broker, cfg *config.Config, ev *events.Log) (*Gateway, error) {
	k, err := LoadKey(cfg)
	if err != nil {
		return nil, err
	}
	return &Gateway{b: b, cfg: cfg, ev: ev, key: k}, nil
}

var upstream = &http.Client{Transport: &http.Transport{
	Proxy:                 nil,
	MaxIdleConnsPerHost:   8,
	IdleConnTimeout:       90 * time.Second,
	ResponseHeaderTimeout: 0, // a long prompt can take minutes to its first byte
	DisableCompression:    true,
}}

func jsonErr(w http.ResponseWriter, status int, code, msg string, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "baton_error", "code": code}})
}

// LoopbackHost reports whether a Host header names this machine's loopback,
// or one of the extra names allowed in config (DNS-rebinding guard).
func LoopbackHost(host string, allow []string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	for _, a := range allow {
		if strings.EqualFold(a, h) {
			return true
		}
	}
	return false
}

func (g *Gateway) authorized(r *http.Request) bool {
	if !g.cfg.Gateway.RequireKey {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" {
		got = r.Header.Get("X-Api-Key")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(g.key)) == 1
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !LoopbackHost(r.Host, g.cfg.Gateway.AllowHosts) && !g.listensWide() && !netx.Trusted(r) {
		jsonErr(w, http.StatusForbidden, "bad_host", "host not allowed", 0)
		return
	}
	if r.Header.Get("Origin") != "" {
		jsonErr(w, http.StatusForbidden, "browser_origin", "browser requests are refused", 0)
		return
	}
	if r.URL.Path == "/health" {
		w.Write([]byte("ok"))
		return
	}
	if !g.authorized(r) {
		jsonErr(w, http.StatusUnauthorized, "unauthorized", "missing or wrong API key (baton key prints it)", 0)
		return
	}
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models"):
		g.models(w)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/"):
		g.infer(w, r)
	default:
		jsonErr(w, http.StatusNotFound, "not_found", "unknown route "+r.Method+" "+r.URL.Path, 0)
	}
}

func (g *Gateway) listensWide() bool {
	h, _, _ := net.SplitHostPort(g.cfg.Gateway.Listen)
	return h == "" || h == "0.0.0.0" || h == "::"
}

func (g *Gateway) models(w http.ResponseWriter) {
	type m struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []m
	for _, e := range g.b.Models() {
		data = append(data, m{ID: e.ID, Object: "model", Created: time.Now().Unix(), OwnedBy: "baton:" + e.Engine})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// sse writes server-sent events once the response is committed to streaming.
type sse struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	f       http.Flusher
	started bool
}

func (s *sse) start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sse) comment(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, ": baton: %s\n\n", strings.ReplaceAll(text, "\n", " "))
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sse) fail(code, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": "baton_error", "code": code}})
	fmt.Fprintf(s.w, "data: %s\n\ndata: [DONE]\n\n", b)
	if s.f != nil {
		s.f.Flush()
	}
}

func (g *Gateway) infer(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "bad_body", err.Error(), 0)
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		jsonErr(w, http.StatusBadRequest, "bad_json", "request body is not a JSON object", 0)
		return
	}
	var model string
	_ = json.Unmarshal(req["model"], &model)
	var stream bool
	_ = json.Unmarshal(req["stream"], &stream)

	began := time.Now()
	eng, upModel, err := g.b.Route(model)
	if err != nil {
		jsonErr(w, http.StatusNotFound, "model_not_found", err.Error(), 0)
		return
	}
	name := eng.Config().Name
	out := &sse{w: w}
	out.f, _ = w.(http.Flusher)

	if !g.b.Ready(name) {
		if h := g.b.Held(name); h != nil {
			jsonErr(w, http.StatusServiceUnavailable, "gpu_reserved", h.Error(), 60)
			return
		}
		g.ev.Infof("gateway", name, "request for %q: bringing up %s", model, name)
		done := make(chan error, 1)
		go func() { done <- g.b.Activate(r.Context(), name, model, "gateway request for "+model) }()
		start := time.Now()
		tick := time.NewTicker(5 * time.Second)
		var aerr error
	waiting:
		for {
			select {
			case aerr = <-done:
				break waiting
			case <-tick.C:
				if stream {
					out.start()
					out.comment(fmt.Sprintf("starting %s, %ds", name, int(time.Since(start).Seconds())))
				}
			}
		}
		tick.Stop()
		if aerr != nil {
			code, retry := "start_failed", 15
			var h *broker.HeldError
			var ag *broker.AgroundError
			if errors.As(aerr, &h) {
				code, retry = "gpu_reserved", 60
			} else if errors.As(aerr, &ag) {
				code, retry = "engine_aground", 30
			}
			if errors.Is(aerr, context.Canceled) && r.Context().Err() != nil {
				return // client went away
			}
			g.record(harbor.Request{At: time.Now(), Engine: name, Model: model, Route: route(r), Status: http.StatusServiceUnavailable, Error: code, Total: time.Since(began).Seconds()})
			if out.started {
				out.fail(code, aerr.Error())
			} else {
				jsonErr(w, http.StatusServiceUnavailable, code, aerr.Error(), retry)
			}
			return
		}
	}

	if upModel != "" && upModel != model {
		req["model"], _ = json.Marshal(upModel)
		body, _ = json.Marshal(req)
	}
	g.b.Begin(name)
	defer g.b.End(name)

	base, hdr := eng.Upstream()
	target := base + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	ureq, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "proxy", err.Error(), 0)
		return
	}
	for k, v := range r.Header {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "host", "content-length", "origin", "connection", "accept-encoding":
			continue
		}
		ureq.Header[k] = v
	}
	for k, v := range hdr {
		ureq.Header[k] = v
	}
	ureq.Header.Set("Content-Type", "application/json")
	sent := time.Now()
	resp, err := upstream.Do(ureq)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		g.record(harbor.Request{At: time.Now(), Engine: name, Model: model, Route: route(r), Status: http.StatusBadGateway, Error: "upstream", Total: time.Since(began).Seconds()})
		if out.started {
			out.fail("upstream", err.Error())
		} else {
			jsonErr(w, http.StatusBadGateway, "upstream", fmt.Sprintf("%s: %v", name, err), 5)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		// An engine's log does not see the requests it turned away.
		g.record(harbor.Request{At: time.Now(), Engine: name, Model: model, Route: route(r), Status: resp.StatusCode, Error: "upstream", Total: time.Since(began).Seconds()})
	}

	if out.started {
		// Headers are gone already: an upstream error becomes an SSE error.
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			out.fail("upstream", fmt.Sprintf("%s answered HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(b))))
			return
		}
	} else {
		for k, v := range resp.Header {
			switch strings.ToLower(k) {
			case "content-length", "connection", "transfer-encoding":
				continue
			}
			w.Header()[k] = v
		}
		w.Header().Set("X-Baton-Engine", name)
		w.WriteHeader(resp.StatusCode)
	}
	// The tail of the answer holds its usage: the last chunk of a stream, the
	// end of a JSON body.
	meter := resp.StatusCode/100 == 2 && !g.b.Metered(name)
	var tail []byte
	var first time.Time
	buf := make([]byte, 16<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if first.IsZero() {
				first = time.Now()
			}
			if meter {
				tail = append(tail, buf[:n]...)
				if len(tail) > tailMax {
					tail = append(tail[:0], tail[len(tail)-tailMax/2:]...)
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if out.f != nil {
				out.f.Flush()
			}
		}
		if rerr != nil {
			if meter && errors.Is(rerr, io.EOF) {
				g.record(served(name, model, route(r), stream, tail, sent, first, time.Now()))
			}
			return
		}
	}
}

const tailMax = 256 << 10

// record keeps a request in the dashboard's history.
func (g *Gateway) record(q harbor.Request) {
	q.Source = "gateway"
	g.b.Recorder().Request(q)
}

// route is the API path a request went to, as TabbyAPI's log names it.
func route(r *http.Request) string { return strings.TrimPrefix(r.URL.Path, "/v1/") }

// served reads what an engine answered: OpenAI usage (token counts), and
// llama.cpp's timings when it sends them (its own rates). Without timings a
// stream's decode rate is worked out from the time between its first byte and
// its end; a plain answer gets none, its prefill can't be told apart.
func served(engine, model, route string, stream bool, tail []byte, sent, first, end time.Time) harbor.Request {
	q := harbor.Request{At: end, Engine: engine, Model: model, Route: route, Status: http.StatusOK, Total: end.Sub(sent).Seconds()}
	if stream && !first.IsZero() {
		q.FirstTok = first.Sub(sent).Seconds()
	}
	var u struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
		Details    struct {
			Cached int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if lastObject(tail, "usage", &u) {
		q.Prompt, q.Output, q.Cached = u.Prompt, u.Completion, u.Details.Cached
	}
	var t struct {
		PromptN  int     `json:"prompt_n"`
		PromptPS float64 `json:"prompt_per_second"`
		PredN    int     `json:"predicted_n"`
		PredPS   float64 `json:"predicted_per_second"`
		CacheN   int     `json:"cache_n"`
	}
	if lastObject(tail, "timings", &t) {
		q.PromptTPS, q.DecodeTPS = round1(t.PromptPS), round1(t.PredPS)
		if q.Output == 0 {
			q.Output = t.PredN
		}
		if q.Prompt == 0 {
			q.Prompt = t.PromptN + t.CacheN
		}
		if q.Cached == 0 {
			q.Cached = t.CacheN
		}
	}
	if q.DecodeTPS == 0 && stream && q.Output > 1 && !first.IsZero() {
		if d := end.Sub(first).Seconds(); d > 0.05 {
			q.DecodeTPS = round1(float64(q.Output-1) / d)
		}
	}
	return q
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// lastObject decodes the JSON object that is the value of the last "key" in
// b, skipping a null (a stream's chunks carry "usage": null until the last).
func lastObject(b []byte, key string, v any) bool {
	k := []byte(`"` + key + `"`)
	for end := len(b); end > 0; {
		i := bytes.LastIndex(b[:end], k)
		if i < 0 {
			return false
		}
		end = i
		rest := bytes.TrimLeft(b[i+len(k):], " \t\r\n")
		if len(rest) == 0 || rest[0] != ':' {
			continue
		}
		rest = bytes.TrimLeft(rest[1:], " \t\r\n")
		if len(rest) == 0 || rest[0] != '{' {
			continue
		}
		if json.NewDecoder(bytes.NewReader(rest)).Decode(v) == nil {
			return true
		}
	}
	return false
}
