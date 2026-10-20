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

	"baton/internal/config"
	"baton/internal/telemetry"
	"baton/internal/win"
)

// comfy drives ComfyUI. ComfyUI loads models lazily per job, so "acquire" only
// needs the server up; "release" waits for the queue (the broker does that)
// and then asks ComfyUI to unload its models and free its cache.
type comfy struct {
	base
	umu sync.Mutex
	cur string // the URL that answered last
}

func newComfy(c config.Engine, env *Env) *comfy {
	return &comfy{base: base{cfg: c, env: env}}
}

func (c *comfy) IsLLM() bool                     { return false }
func (c *comfy) Serves(string) (bool, string)    { return false, "" }
func (c *comfy) Upstream() (string, http.Header) { return c.url(), nil }

func (c *comfy) url() string {
	c.umu.Lock()
	defer c.umu.Unlock()
	if c.cur != "" {
		return c.cur
	}
	return strings.TrimRight(c.cfg.URL, "/")
}

// candidates lists the configured URL and any port ComfyUI Desktop recorded
// for a server it still runs (Desktop moves to another port on a conflict).
func (c *comfy) candidates() []string {
	out := []string{strings.TrimRight(c.cfg.URL, "/")}
	if c.cfg.DesktopProcsDir == "" {
		return out
	}
	files, _ := filepath.Glob(filepath.Join(c.cfg.DesktopProcsDir, "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rec struct {
			Port     int   `json:"port"`
			ChildPID int   `json:"childPid"`
			StopReq  int64 `json:"stopRequestedAt"`
		}
		if json.Unmarshal(b, &rec) != nil || rec.Port == 0 || rec.StopReq != 0 {
			continue
		}
		if rec.ChildPID != 0 && !win.Alive(rec.ChildPID, 0) {
			continue
		}
		u := fmt.Sprintf("http://127.0.0.1:%d", rec.Port)
		if u != out[0] {
			out = append([]string{u}, out...)
		}
	}
	return out
}

type comfyStats struct {
	System struct {
		ComfyVersion string `json:"comfyui_version"`
	} `json:"system"`
	Devices []struct {
		Name           string `json:"name"`
		Index          int    `json:"index"`
		VRAMTotal      int64  `json:"vram_total"`
		VRAMFree       int64  `json:"vram_free"`
		TorchVRAMTotal int64  `json:"torch_vram_total"`
		TorchVRAMFree  int64  `json:"torch_vram_free"`
	} `json:"devices"`
}

func (c *comfy) stats(ctx context.Context) (string, *comfyStats, error) {
	var lastErr error
	for _, u := range c.candidates() {
		var s comfyStats
		cctx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
		err := doJSON(cctx, "GET", u+"/system_stats", nil, nil, &s)
		cancel()
		if err == nil {
			c.umu.Lock()
			c.cur = u
			c.umu.Unlock()
			return u, &s, nil
		}
		lastErr = err
	}
	return "", nil, lastErr
}

func (c *comfy) Probe(ctx context.Context, snap *telemetry.Snapshot) Status {
	st := Status{URL: c.url()}
	pctx, cancel := probeCtx(ctx)
	defer cancel()
	u, stats, err := c.stats(pctx)
	if err != nil {
		c.fill(&st, snap, c.attribute(snap, portOf(c.url())))
		if !isConnRefused(err) {
			st.Err = err.Error()
		}
		st.Detail = "not running"
		return st
	}
	st.URL, st.Up = u, true
	c.fill(&st, snap, c.attribute(snap, portOf(u)))
	var torch int64
	for _, d := range stats.Devices {
		torch += d.TorchVRAMTotal
	}
	var q struct {
		Running []any `json:"queue_running"`
		Pending []any `json:"queue_pending"`
	}
	if err := doJSON(pctx, "GET", u+"/queue", nil, nil, &q); err == nil {
		st.Extra = map[string]any{"running": len(q.Running), "pending": len(q.Pending), "torch_mib": torch >> 20}
		if len(q.Running) > 0 {
			st.Busy, st.BusyWhy = true, "a job is running"
		}
		if len(q.Running)+len(q.Pending) > 0 {
			st.Detail = fmt.Sprintf("%d running, %d queued", len(q.Running), len(q.Pending))
		} else {
			st.Detail = "queue empty"
		}
	}
	st.Loaded = torch > 512<<20 || maxMiB(st.VRAMMiB) > 1536
	if stats.System.ComfyVersion != "" {
		st.Model = "ComfyUI " + stats.System.ComfyVersion
	}
	return st
}

func (c *comfy) Interrupt(ctx context.Context) error {
	c.env.Events.Warnf("release", c.cfg.Name, "interrupting the running job")
	return doJSON(ctx, "POST", c.url()+"/interrupt", nil, map[string]any{}, nil)
}

func (c *comfy) Release(ctx context.Context, mode string, st Status) error {
	if mode == "stop" {
		return c.stop(st)
	}
	if !st.Up {
		return nil
	}
	return doJSON(ctx, "POST", c.url()+"/free", nil, map[string]any{"unload_models": true, "free_memory": true}, nil)
}

func (c *comfy) Acquire(ctx context.Context, _ string, progress func(string)) error {
	if c.Probe(ctx, c.env.Sample()).Up {
		return nil
	}
	if len(c.cfg.Start) == 0 {
		return errors.New("ComfyUI is not running; open ComfyUI Desktop (or give the engine a start command)")
	}
	if err := c.launch(); err != nil {
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, c.cfg.ReadyTimeout.Duration)
	defer cancel()
	start := time.Now()
	return waitUntil(rctx, time.Second, func() (bool, error) {
		if gone, msg := c.managedExited(); gone {
			return false, fmt.Errorf("ComfyUI exited while starting (%s)", msg)
		}
		progress(fmt.Sprintf("starting ComfyUI, %ds", int(time.Since(start).Seconds())))
		return c.Probe(rctx, c.env.Sample()).Up, nil
	})
}
