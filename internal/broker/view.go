package broker

import (
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"baton/internal/detect"
	"baton/internal/engine"
	"baton/internal/events"
	"baton/internal/telemetry"
	"baton/internal/win"
)

// View is everything the dashboard, tray and CLI show.
type View struct {
	At        time.Time       `json:"at"`
	Mode      Mode            `json:"mode"`
	ModeWhy   string          `json:"mode_why,omitempty"`
	ModeSince time.Time       `json:"mode_since"`
	Hold      *manualHold     `json:"hold,omitempty"`
	Hits      []detect.Hit    `json:"hits"`
	Holder    string          `json:"holder,omitempty"`
	RestoreTo string          `json:"restore_to,omitempty"`
	Op        *OpView         `json:"op,omitempty"`
	Queue     []string        `json:"queue,omitempty"`
	GPUs      []telemetry.GPU `json:"gpus"`
	Memory    *MemView        `json:"memory,omitempty"`
	Engines   []EngineView    `json:"engines"`
	Gate      GateView        `json:"gate"`
	Events    []events.Event  `json:"events"`
	Error     string          `json:"error,omitempty"`
	Policy    map[string]any  `json:"policy"`
	Gateway   map[string]any  `json:"gateway"`
	Remote    *RemoteView     `json:"remote,omitempty"`
}

// RemoteView is how to reach baton from your phone over Tailscale.
type RemoteView struct {
	Enabled   bool   `json:"enabled"`
	IP        string `json:"ip,omitempty"`
	Dashboard string `json:"dashboard,omitempty"`
	Gateway   string `json:"gateway,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MemView is system RAM with its biggest users, grouped by app.
type MemView struct {
	win.MemInfo
	Holders []RAMHolder `json:"holders"`
}

type RAMHolder struct {
	Name  string `json:"name"`
	Tag   string `json:"tag,omitempty"`
	MiB   int    `json:"mib"`
	Count int    `json:"count"`
}

type OpView struct {
	Name  string    `json:"name"`
	Stage string    `json:"stage,omitempty"`
	Since time.Time `json:"since"`
}

type EngineView struct {
	engine.Status
	Inflight int  `json:"inflight"`
	Holder   bool `json:"holder"`
	Restore  bool `json:"restore"`
	LLM      bool `json:"llm"`
	HasLog   bool `json:"has_log"`
}

type GateView struct {
	Waiting int `json:"waiting"`
	Running int `json:"running"`
}

var systemExe = map[string]bool{
	"dwm.exe": true, "csrss.exe": true, "explorer.exe": true, "searchhost.exe": true,
	"startmenuexperiencehost.exe": true, "shellexperiencehost.exe": true, "textinputhost.exe": true,
	"applicationframehost.exe": true, "lockapp.exe": true, "widgets.exe": true, "shellhost.exe": true,
	"systemsettings.exe": true, "phoneexperiencehost.exe": true, "crossdeviceresume.exe": true,
	"memory compression": true, "system": true, "registry": true, "svchost.exe": true, "msmpeng.exe": true,
	"searchindexer.exe": true, "lsass.exe": true, "services.exe": true, "wininit.exe": true, "winlogon.exe": true,
	"audiodg.exe": true, "sihost.exe": true, "ctfmon.exe": true, "runtimebroker.exe": true, "fontdrvhost.exe": true,
}

// ramHolders groups resident memory by engine, game, or app name.
func ramHolders(snap *telemetry.Snapshot, tag map[int]string) []RAMHolder {
	groups := map[string]*RAMHolder{}
	for _, p := range snap.Procs {
		if p.WorkingSet < 1<<20 {
			continue
		}
		key, h := strings.ToLower(p.Name), RAMHolder{Name: p.Name}
		if t, ok := tag[p.PID]; ok {
			h.Tag = t
			if t != "game" && t != "stream" {
				key = "engine:" + t // all of an engine's processes together
			}
		} else if systemExe[key] {
			h.Tag = "system"
		}
		g := groups[key]
		if g == nil {
			g = &h
			groups[key] = g
		}
		g.MiB += int(p.WorkingSet >> 20)
		g.Count++
	}
	out := make([]RAMHolder, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MiB > out[j].MiB })
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func (b *Broker) View(eventsSince int64) View {
	b.mu.Lock()
	defer b.mu.Unlock()
	v := View{
		At: time.Now(), Mode: b.mode, ModeWhy: b.modeWhy, ModeSince: b.modeSince,
		Hold: b.manual, Hits: b.hits, Holder: b.holder, RestoreTo: b.restoreTo,
		Gate: GateView{Waiting: len(b.gateWait), Running: len(b.gateRun)},
	}
	if v.RestoreTo == "" {
		v.RestoreTo = b.pendingRest
	}
	if b.cur != nil {
		v.Op = &OpView{Name: b.cur.name, Stage: b.curStage, Since: b.cur.since}
	}
	for _, o := range b.queue {
		v.Queue = append(v.Queue, o.name)
	}
	tag := map[int]string{}
	for _, e := range b.engines {
		name := e.Config().Name
		s := b.st[name]
		v.Engines = append(v.Engines, EngineView{
			Status: s, Inflight: b.inflight[name], Holder: b.holder == name,
			Restore: b.restoreTo == name || b.pendingRest == name, LLM: e.IsLLM(),
			HasLog: fileExists(engine.LogPath(b.cfg.Daemon.StateDir, name)),
		})
		for _, pid := range s.PIDs {
			tag[pid] = name
		}
	}
	for _, h := range b.hits {
		if _, ok := tag[h.PID]; !ok && h.PID != 0 {
			tag[h.PID] = h.Kind
		}
	}
	if b.snap != nil {
		v.Error = b.snap.Err
		v.Memory = &MemView{MemInfo: b.snap.Mem, Holders: ramHolders(b.snap, tag)}
		for _, g := range b.snap.GPUs {
			cp := g
			cp.Holders = make([]telemetry.Holder, len(g.Holders))
			for i, h := range g.Holders {
				if t, ok := tag[h.PID]; ok {
					h.Tag = t
				} else if systemExe[strings.ToLower(h.Name)] {
					h.Tag = "system"
				}
				cp.Holders[i] = h
			}
			v.GPUs = append(v.GPUs, cp)
		}
	}
	v.Events = b.ev.Since(eventsSince, 150)
	p := b.cfg.Policy
	v.Policy = map[string]any{
		"restore": p.Restore, "linger": p.Linger.String(), "residual_mib": p.ResidualMiB,
		"stream_encode_gpu": p.Stream.EncodeGPU, "stream_min_free_mib": p.Stream.MinFreeMiB,
		"stream_hold_comfyui": p.Stream.HoldComfy, "game_drain": p.Game.Drain.String(),
	}
	v.Gateway = map[string]any{"enabled": b.cfg.Gateway.Enabled, "listen": b.cfg.Gateway.Listen}
	v.Remote = &RemoteView{Enabled: b.cfg.Remote.Tailscale}
	if b.remote != nil {
		ip, errText := b.remote()
		v.Remote.IP, v.Remote.Error = ip, errText
		if ip != "" {
			v.Remote.Dashboard = "http://" + net.JoinHostPort(ip, portOf(b.cfg.Daemon.Listen)) + "/"
			if b.cfg.Gateway.Enabled {
				v.Remote.Gateway = "http://" + net.JoinHostPort(ip, portOf(b.cfg.Gateway.Listen)) + "/v1"
			}
		}
	}
	return v
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func portOf(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}
