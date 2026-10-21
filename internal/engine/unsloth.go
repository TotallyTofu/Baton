package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows/registry"

	"baton/internal/config"
	"baton/internal/telemetry"
	"baton/internal/win"
)

// unsloth drives Unsloth Studio. Studio stays up (its desktop app owns it);
// baton unloads and reloads the model over Studio's API, which needs an API
// key made in Studio (Settings > API keys). Without a key baton can still free
// the card by stopping the llama-server Studio runs the model in, but it
// cannot load the model back.
type unsloth struct {
	base
	smu     sync.Mutex
	loaded  []string       // identifiers loaded at last probe
	current map[string]any // last status of a loaded model, for restoring it
}

func newUnsloth(c config.Engine, env *Env) *unsloth {
	u := &unsloth{base: base{cfg: c, env: env}}
	u.current = u.readSaved()
	return u
}

func (u *unsloth) IsLLM() bool { return true }

func (u *unsloth) key() string {
	if u.cfg.APIKeyEnv != "" {
		if v := strings.TrimSpace(os.Getenv(u.cfg.APIKeyEnv)); v != "" {
			return v
		}
		// setx writes the user environment, which processes started earlier
		// (this daemon, the terminal it came from) never see; read it there.
		if k, err := registry.OpenKey(registry.CURRENT_USER, "Environment", registry.QUERY_VALUE); err == nil {
			v, _, err := k.GetStringValue(u.cfg.APIKeyEnv)
			k.Close()
			if err == nil && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return readSecretFile(u.cfg.APIKeyFile)
}

func (u *unsloth) hdr() http.Header {
	h := http.Header{}
	if k := u.key(); k != "" {
		h.Set("Authorization", "Bearer "+k)
	}
	return h
}

func (u *unsloth) Upstream() (string, http.Header) { return strings.TrimRight(u.cfg.URL, "/"), u.hdr() }

func (u *unsloth) Serves(model string) (bool, string) {
	u.smu.Lock()
	extra := append([]string(nil), u.loaded...)
	u.smu.Unlock()
	return u.servesList(model, extra)
}

func (u *unsloth) savedPath() string {
	return filepath.Join(u.env.StateDir, u.cfg.Name+"-last-model.json")
}

func (u *unsloth) readSaved() map[string]any {
	b, err := os.ReadFile(u.savedPath())
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func (u *unsloth) save(m map[string]any) {
	_ = os.MkdirAll(u.env.StateDir, 0o755)
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(u.savedPath(), b, 0o600)
}

// ids reads model identifiers out of Studio's loaded/loading arrays, whose
// entries may be plain strings or objects.
func ids(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, e := range arr {
		switch x := e.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			for _, k := range []string{"model_identifier", "model_path", "id", "model", "name"} {
				if s, ok := x[k].(string); ok && s != "" {
					out = append(out, s)
					break
				}
			}
		}
	}
	return out
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (u *unsloth) status(ctx context.Context) (map[string]any, error) {
	var m map[string]any
	err := doJSON(ctx, "GET", u.cfg.URL+"/api/inference/status", u.hdr(), nil, &m)
	return m, err
}

func (u *unsloth) Probe(ctx context.Context, snap *telemetry.Snapshot) Status {
	st := Status{URL: u.cfg.URL}
	u.fill(&st, snap, u.attribute(snap, portOf(u.cfg.URL)))
	pctx, cancel := probeCtx(ctx)
	defer cancel()
	var health map[string]any
	if err := doJSON(pctx, "GET", u.cfg.URL+"/api/health", nil, nil, &health); err != nil {
		if isConnRefused(err) {
			st.Detail = "Studio not running"
		} else {
			st.Err = err.Error()
		}
		return st
	}
	st.Up = true
	if u.key() == "" {
		st.Loaded = maxMiB(st.VRAMMiB) > 1024
		st.Detail = "no API key: baton can free the GPU but not reload the model"
		if u.cfg.APIKeyEnv != "" {
			st.Detail += " (set " + u.cfg.APIKeyEnv + ")"
		}
		return st
	}
	m, err := u.status(pctx)
	if err != nil {
		st.Err = "status: " + err.Error()
		st.Loaded = maxMiB(st.VRAMMiB) > 1024
		return st
	}
	loaded, loading := ids(m["loaded"]), ids(m["loading"])
	st.Loaded, st.Loading = len(loaded) > 0, len(loading) > 0
	st.Model = str(m, "active_model")
	if st.Model == "" && len(loaded) > 0 {
		st.Model = loaded[0]
	}
	if st.Loading && !st.Loaded {
		st.Detail = "loading " + strings.Join(loading, ", ")
	}
	if ctx := m["context_length"]; ctx != nil {
		st.Extra = map[string]any{"context": ctx}
	}
	u.smu.Lock()
	u.loaded = loaded
	if st.Loaded && str(m, "model_identifier") != "" {
		u.current = m
	}
	u.smu.Unlock()

	var runs any
	if err := doJSON(pctx, "GET", u.cfg.URL+"/api/inference/chat-runs/active", u.hdr(), nil, &runs); err == nil {
		if n := countActive(runs); n > 0 {
			st.Busy, st.BusyWhy = true, fmt.Sprintf("%d chat run(s) generating", n)
		}
	}
	if st.Loaded {
		if sl := slotsOf(pctx, snap, st, portOf(u.cfg.URL)); len(sl) > 0 {
			if st.Extra == nil {
				st.Extra = map[string]any{}
			}
			st.Extra["slots"] = sl
		}
	}
	return st
}

// Slot is one llama.cpp server slot: a context window and what fills it.
type Slot struct {
	ID     int  `json:"id"`
	Window int  `json:"window"`
	Used   int  `json:"used"`   // prompt tokens in the slot
	Cached int  `json:"cached"` // of those, reused from the cache
	Busy   bool `json:"busy"`
}

// slotsOf reads /slots from a llama-server among the engine's processes
// (Studio runs GGUF models in one), when it listens on loopback and answers
// without a key. Anything else is no slot data, not an error.
func slotsOf(ctx context.Context, snap *telemetry.Snapshot, st Status, skip int) []Slot {
	llama := map[int]bool{}
	for _, p := range st.procs {
		if strings.HasPrefix(strings.ToLower(p.Name), "llama-server") {
			llama[p.PID] = true
		}
	}
	if len(llama) == 0 {
		return nil
	}
	for port, pid := range snap.Listeners {
		if !llama[pid] || port == skip {
			continue
		}
		var raw []struct {
			ID           int  `json:"id"`
			State        any  `json:"state"`
			IsProcessing bool `json:"is_processing"`
			NCtx         int  `json:"n_ctx"`
			NPrompt      int  `json:"n_prompt_tokens"`
			NCache       int  `json:"n_prompt_tokens_cache"`
		}
		sctx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
		err := doJSON(sctx, "GET", fmt.Sprintf("http://127.0.0.1:%d/slots", port), nil, nil, &raw)
		cancel()
		if err != nil || len(raw) == 0 {
			continue
		}
		out := make([]Slot, 0, len(raw))
		for _, s := range raw {
			busy := s.IsProcessing
			switch v := s.State.(type) {
			case string:
				busy = busy || v == "processing"
			case float64:
				busy = busy || v != 0
			}
			out = append(out, Slot{ID: s.ID, Window: s.NCtx, Used: s.NPrompt, Cached: s.NCache, Busy: busy})
		}
		return out
	}
	return nil
}

func countActive(v any) int {
	switch x := v.(type) {
	case []any:
		return len(x)
	case map[string]any:
		for _, k := range []string{"runs", "active", "items", "data"} {
			if arr, ok := x[k].([]any); ok {
				return len(arr)
			}
		}
	}
	return 0
}

func (u *unsloth) Release(ctx context.Context, mode string, st Status) error {
	if mode == "stop" {
		u.env.Events.Warnf("release", u.cfg.Name, "stopping Unsloth Studio itself; reopen it to use it again")
		return u.stop(st)
	}
	if !st.Up {
		return nil
	}
	if u.key() == "" {
		return u.killRuntime(st)
	}
	m, err := u.status(ctx)
	if err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if str(m, "model_identifier") != "" {
		u.save(m)
		u.smu.Lock()
		u.current = m
		u.smu.Unlock()
	}
	loaded := ids(m["loaded"])
	if len(loaded) == 0 && str(m, "model_identifier") != "" {
		loaded = []string{str(m, "model_identifier")}
	}
	for _, id := range loaded {
		u.env.Events.Infof("release", u.cfg.Name, "unloading %s", id)
		uctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err := doJSON(uctx, "POST", u.cfg.URL+"/api/inference/unload", u.hdr(), map[string]any{"model_path": id, "force_cancel_active": true}, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("unload %s: %w", id, err)
		}
	}
	return nil
}

// killRuntime frees the card without an API key by stopping the processes
// Studio spawned that hold VRAM (its llama-server), leaving Studio up.
func (u *unsloth) killRuntime(st Status) error {
	snap := u.env.Sample()
	port := portOf(u.cfg.URL)
	root := snap.Listeners[port]
	var victims []int
	for _, p := range st.Procs() {
		if p.PID == root {
			continue
		}
		vram, _ := snap.VRAM([]int{p.PID})
		var total uint64
		for _, v := range vram {
			total += v
		}
		if total > 256<<20 {
			victims = append(victims, snap.Tree.Descendants(p.PID)...)
		}
	}
	if len(victims) == 0 {
		if v, _ := snap.VRAM([]int{root}); len(v) > 0 {
			return errors.New("Studio holds the model in its own process; set an API key so baton can unload it")
		}
		return nil
	}
	procs := make([]win.Proc, 0, len(victims))
	for _, pid := range victims {
		procs = append(procs, snap.Tree.ByPID[pid])
	}
	u.env.Events.Infof("release", u.cfg.Name, "no API key: stopping Studio's runtime (%d process(es))", len(procs))
	return win.Kill(procs, 15*time.Second)
}

// loadRequest builds a load from a configured model, else from what was
// loaded last.
func (u *unsloth) loadRequest(model string) (map[string]any, string) {
	for _, m := range u.cfg.Models {
		if strings.EqualFold(m.Name, model) || (model != "" && strings.EqualFold(m.ModelPath, model)) {
			req := map[string]any{"model_path": m.ModelPath}
			if m.GGUFVariant != "" {
				req["gguf_variant"] = m.GGUFVariant
			}
			if m.MaxSeqLength > 0 {
				req["max_seq_length"] = m.MaxSeqLength
			}
			for k, v := range m.Extra {
				req[k] = v
			}
			return req, m.ModelPath
		}
	}
	u.smu.Lock()
	cur := u.current
	u.smu.Unlock()
	if cur == nil {
		return nil, ""
	}
	id := str(cur, "model_identifier")
	if id == "" || (model != "" && !strings.EqualFold(model, id) && !strings.EqualFold(model, str(cur, "active_model"))) {
		return nil, ""
	}
	req := map[string]any{"model_path": id}
	copyIf := func(to, from string) {
		if v, ok := cur[from]; ok && v != nil {
			if s, isStr := v.(string); isStr && s == "" {
				return
			}
			req[to] = v
		}
	}
	copyIf("gguf_variant", "gguf_variant")
	copyIf("max_seq_length", "requested_context_length")
	copyIf("cache_type_kv", "cache_type_kv")
	copyIf("gpu_ids", "requested_gpu_ids")
	copyIf("tensor_split", "tensor_split")
	copyIf("n_parallel", "requested_parallel_slots")
	copyIf("speculative_type", "speculative_type")
	copyIf("spec_draft_n_max", "spec_draft_n_max")
	copyIf("gpu_memory_mode", "gpu_memory_mode")
	copyIf("n_cpu_moe", "n_cpu_moe")
	copyIf("llama_extra_args", "requested_llama_extra_args")
	copyIf("chat_template_override", "chat_template_override")
	copyIf("disable_vision", "disable_vision")
	copyIf("engine", "engine")
	if str(cur, "gpu_memory_mode") == "manual" {
		copyIf("gpu_layers", "gpu_layers")
	}
	return req, id
}

func (u *unsloth) Acquire(ctx context.Context, model string, progress func(string)) error {
	st := u.Probe(ctx, u.env.Sample())
	if !st.Up {
		if !st.CanStart {
			return errors.New("Unsloth Studio is not running; open it (or give the engine a start command)")
		}
		if err := u.launch(); err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		err := waitUntil(rctx, time.Second, func() (bool, error) {
			progress("starting Unsloth Studio")
			return u.Probe(rctx, u.env.Sample()).Up, nil
		})
		cancel()
		if err != nil {
			return fmt.Errorf("Unsloth Studio did not come up: %w", err)
		}
		st = u.Probe(ctx, u.env.Sample())
	}
	if u.key() == "" {
		if !st.Loaded {
			u.env.Events.Infof("start", u.cfg.Name, "GPUs are free for Unsloth Studio; load a model there (with an API key baton could load it for you)")
		}
		return nil
	}
	if st.Loaded && (model == "" || strings.EqualFold(model, st.Model) || u.isLoaded(model)) {
		return nil
	}
	req, id := u.loadRequest(model)
	if req == nil {
		if st.Loaded {
			return nil
		}
		if model == "" {
			u.env.Events.Infof("start", u.cfg.Name, "GPUs are free for Unsloth Studio; load a model there")
			return nil
		}
		return fmt.Errorf("unknown Unsloth model %q (add it as [[engine.model]] in baton.toml)", model)
	}
	progress("loading " + id)
	u.env.Events.Infof("start", u.cfg.Name, "loading %s", id)
	lctx, cancel := context.WithTimeout(ctx, u.cfg.ReadyTimeout.Duration)
	defer cancel()
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		var resp map[string]any
		done <- doJSON(lctx, "POST", u.cfg.URL+"/api/inference/load", u.hdr(), req, &resp)
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				return fmt.Errorf("load %s: %w", id, err)
			}
			return waitUntil(lctx, time.Second, func() (bool, error) {
				return u.Probe(lctx, u.env.Sample()).Loaded, nil
			})
		case <-time.After(2 * time.Second):
			progress(fmt.Sprintf("loading %s, %ds", id, int(time.Since(start).Seconds())))
		case <-lctx.Done():
			return lctx.Err()
		}
	}
}

// Preflight refuses, before anything is released, a request for a model
// baton could not load.
func (u *unsloth) Preflight(model string, st Status) error {
	if model == "" || (st.Loaded && (strings.EqualFold(model, st.Model) || u.isLoaded(model))) {
		return nil
	}
	if u.key() == "" {
		return fmt.Errorf("%s: no Unsloth API key, so baton cannot load %q", u.cfg.Name, model)
	}
	if req, _ := u.loadRequest(model); req == nil {
		return fmt.Errorf("unknown Unsloth model %q (add it as [[engine.model]] in baton.toml)", model)
	}
	return nil
}

func (u *unsloth) isLoaded(model string) bool {
	u.smu.Lock()
	defer u.smu.Unlock()
	for _, l := range u.loaded {
		if strings.EqualFold(l, model) {
			return true
		}
	}
	return false
}
