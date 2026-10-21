package broker

import (
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"baton/internal/detect"
	"baton/internal/harbor"
)

// What the dashboard's harbor and request pages read: the history the broker
// records every cycle, and the live state it is drawn against.

// pidTagsLocked names what each process belongs to: an engine, or a game or
// stream baton detected.
func (b *Broker) pidTagsLocked() map[int]string {
	tag := map[int]string{}
	for _, e := range b.engines {
		name := e.Config().Name
		for _, pid := range b.st[name].PIDs {
			tag[pid] = name
		}
	}
	for _, h := range b.hits {
		if _, ok := tag[h.PID]; !ok && h.PID != 0 {
			tag[h.PID] = h.Kind
		}
	}
	return tag
}

// recordLocked adds this cycle to the history: each card's memory by holder
// and system RAM (the tide), and what is on the cards (the timeline).
func (b *Broker) recordLocked(now time.Time) {
	if b.rec == nil || b.snap == nil {
		return
	}
	tag := b.pidTagsLocked()
	var levels []harbor.Level
	for _, g := range b.snap.GPUs {
		l := harbor.Level{
			Key: "gpu" + strconv.Itoa(g.Index), Name: strings.TrimPrefix(g.Name, "NVIDIA GeForce "),
			Total: int(g.MemTotal >> 20), Used: int(g.MemUsed >> 20), By: map[string]int{},
		}
		seen := 0
		for _, h := range g.Holders {
			k := tag[h.PID]
			switch {
			case k != "":
			case systemExe[strings.ToLower(h.Name)]:
				k = "system"
			default:
				k = "apps"
			}
			l.By[k] += h.MiB
			seen += h.MiB
		}
		if rest := l.Used - seen; rest > 0 {
			l.By["driver"] = rest // the driver and what no process owns
		}
		levels = append(levels, l)
	}
	if m := b.snap.Mem; m.Total > 0 {
		l := harbor.Level{Key: "ram", Name: "System RAM", Total: int(m.Total >> 20), Used: int(m.Used >> 20), By: map[string]int{}}
		left := l.Used
		for _, e := range b.engines {
			if mib := b.st[e.Config().Name].RAMMiB; mib > 0 {
				l.By[e.Config().Name] = mib
				left -= mib
			}
		}
		if left > 0 {
			l.By["other"] = left
		}
		levels = append(levels, l)
	}
	b.rec.Tide(now, levels)

	var occ []harbor.Occupant
	for _, e := range b.engines {
		s := b.st[e.Config().Name]
		if !s.Loaded {
			continue
		}
		label := s.Model
		if label == "" {
			label = s.Kind
		}
		occ = append(occ, harbor.Occupant{Kind: s.Kind, Owner: s.Name, Label: label, GPUs: b.cardsOf(e, s)})
	}
	for _, h := range b.hits {
		occ = append(occ, harbor.Occupant{Kind: h.Kind, Owner: h.Kind, Label: hitLabel(h)})
	}
	for src, h := range b.hooks {
		occ = append(occ, harbor.Occupant{Kind: h.kind, Owner: h.kind, Label: "hook: " + src})
	}
	if b.manual != nil {
		occ = append(occ, harbor.Occupant{Kind: "hold", Owner: "hold", Label: b.manual.Reason})
	}
	b.rec.Occupants(now, occ)
}

// hitLabel names a detected game, stream or guest: a game by its title, a
// guest by its claim in baton.toml ("digup"), a stream by its program.
func hitLabel(h detect.Hit) string {
	switch h.Kind {
	case "game":
		return GameTitle(h)
	case "guest":
		return h.Rule
	}
	return h.Exe
}

var launcherDir = regexp.MustCompile(`(?i)\\(?:steamapps\\common|Epic Games|XboxGames|GOG Games|GOG Galaxy\\Games|EA Games)\\([^\\]+)`)

// GameTitle is a readable name for a running game: its folder under the
// library or launcher, not the shipping exe ("Stellar Blade", not
// "SB-Win64-Shipping").
func GameTitle(h detect.Hit) string {
	p := h.Path
	if h.Match != "" && strings.HasPrefix(strings.ToLower(p), strings.ToLower(h.Match)+`\`) {
		t, _, _ := strings.Cut(p[len(h.Match)+1:], `\`)
		if t != "" && !strings.HasSuffix(strings.ToLower(t), ".exe") {
			return t
		}
	}
	if m := launcherDir.FindStringSubmatch(p); m != nil {
		return m[1]
	}
	return strings.TrimSuffix(strings.TrimSuffix(h.Exe, ".exe"), ".EXE")
}

// Departure is one row of the departures board: something on the cards, on
// its way on or off, or waiting its turn.
type Departure struct {
	No      int64     `json:"no,omitempty"`
	Vessel  string    `json:"vessel"`
	Kind    string    `json:"kind"`
	Cargo   string    `json:"cargo,omitempty"`
	GPUs    []int     `json:"gpus,omitempty"`
	HoldMiB int       `json:"hold_mib,omitempty"`
	Status  string    `json:"status"` // docked, moored, boarding, casting-off, aground, queued, reserved
	Detail  string    `json:"detail,omitempty"`
	Since   time.Time `json:"since,omitzero"`
}

// HarborView is the front page's history and board.
type HarborView struct {
	At       time.Time           `json:"at"`
	Range    string              `json:"range"`
	Counters harbor.Counters     `json:"counters"`
	KPIs     []harbor.KPI        `json:"kpis"`
	Queue    int                 `json:"queue"` // waiting now: ops, ComfyUI jobs, gateway requests
	Tide     []harbor.TideSeries `json:"tide"`
	Timeline []harbor.Span       `json:"timeline"`
	Board    []Departure         `json:"board"`
	Voyages  []harbor.Voyage     `json:"voyages"`
}

// ParseRange reads a range like "1h", "6h", "24h" or "7d", within limits.
func ParseRange(s string, def, most time.Duration) time.Duration {
	d := def
	if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") {
		d = time.Duration(n) * 24 * time.Hour
	} else if v, err := time.ParseDuration(s); err == nil {
		d = v
	}
	if d <= 0 {
		d = def
	}
	return min(d, most)
}

func rangeName(d time.Duration) string {
	if d >= 24*time.Hour && d%(24*time.Hour) == 0 && d > 24*time.Hour {
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
	return strconv.Itoa(int(d.Hours())) + "h"
}

func (b *Broker) Harbor(rng time.Duration) HarborView {
	now := time.Now()
	rec := b.Recorder()
	v := HarborView{
		At: now, Range: rangeName(rng), Counters: rec.Counters(),
		KPIs:     harbor.KPIs(rec.Requests(now.Add(-time.Hour)), now, time.Hour),
		Tide:     rec.TideView(rng, now),
		Timeline: rec.Timeline(rng, now),
		Voyages:  rec.Voyages(12),
	}
	open := map[string]time.Time{}
	for _, s := range rec.Timeline(24*time.Hour, now) {
		if s.End.IsZero() {
			open[s.Owner+"\x00"+s.Label] = s.Start
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	v.Queue = len(b.queue) + len(b.gateWait)
	for _, n := range b.inflight {
		v.Queue += n
	}
	if o := b.cur; o != nil {
		status := "casting-off"
		if strings.HasPrefix(o.name, "switch to ") || strings.HasPrefix(o.name, "restore") {
			status = "boarding"
		}
		v.Board = append(v.Board, Departure{No: o.no, Vessel: o.name, Kind: "op", Status: status, Detail: b.curStage, Since: o.started})
	}
	for _, o := range b.queue {
		v.Board = append(v.Board, Departure{No: o.no, Vessel: o.name, Kind: "op", Status: "queued", Since: o.since})
	}
	for _, e := range b.engines {
		name := e.Config().Name
		s := b.st[name]
		d := Departure{Vessel: name, Kind: s.Kind, Cargo: s.Model}
		for g, mib := range s.VRAMMiB {
			if mib > b.residual() {
				d.GPUs = append(d.GPUs, g)
				d.HoldMiB += mib
			}
		}
		sort.Ints(d.GPUs)
		label := s.Model
		if label == "" {
			label = s.Kind
		}
		d.Since = open[name+"\x00"+label]
		switch a := b.aground[name]; {
		case a != nil:
			d.Status, d.Since, d.GPUs = "aground", a.since, a.gpus
			d.HoldMiB = 0
			for _, mib := range a.mib {
				d.HoldMiB += mib
			}
		case s.Loading:
			d.Status, d.Detail, d.Since = "boarding", s.Detail, b.since[name]
		case s.Loaded && b.holder == name:
			d.Status, d.Detail = "docked", s.BusyWhy
		case s.Loaded:
			d.Status, d.Detail = "moored", s.BusyWhy
		default:
			continue
		}
		v.Board = append(v.Board, d)
	}
	guestRow := map[string]int{}
	for _, h := range b.hits {
		label := hitLabel(h)
		d := Departure{Vessel: label, Kind: h.Kind, Cargo: h.Exe, Status: "docked", Detail: h.Rule, Since: open[h.Kind+"\x00"+label]}
		if h.Kind == "guest" {
			// One row per guest: a venv's python.exe and the interpreter it
			// starts both match, and only the second holds memory.
			i, seen := guestRow[label]
			if !seen {
				i = len(v.Board)
				guestRow[label] = i
				d.Detail = "the LLMs wait"
				v.Board = append(v.Board, d)
			}
			if b.snap != nil {
				// A guest takes only what it allocates: show where.
				g := &v.Board[i]
				for card, u := range b.snap.ByPID[h.PID] {
					if mib := int(u.Dedicated >> 20); mib > 0 {
						if !slices.Contains(g.GPUs, card) {
							g.GPUs = append(g.GPUs, card)
						}
						g.HoldMiB += mib
					}
				}
				sort.Ints(g.GPUs)
			}
			continue
		}
		v.Board = append(v.Board, d)
	}
	if m := b.manual; m != nil {
		v.Board = append(v.Board, Departure{Vessel: "hold", Kind: "hold", Cargo: m.Reason, Status: "reserved", Since: m.Since})
	}
	return v
}

// Pool is one engine's KV cache: its window and pool, the requests in flight
// in it (TabbyAPI, from its log) or its slots (llama.cpp's /slots).
type Pool struct {
	Engine  string `json:"engine"`
	Kind    string `json:"kind"`
	Model   string `json:"model,omitempty"`
	Up      bool   `json:"up"`
	Loaded  bool   `json:"loaded"`
	Window  int    `json:"window,omitempty"`
	Context any    `json:"context,omitempty"` // Unsloth's context length, as it reports it
	KV      any    `json:"kv,omitempty"`
	Slots   any    `json:"slots,omitempty"`
	Disk    any    `json:"disk,omitempty"` // TabbyAPI's disk KV cache
}

// RequestsView is the engines and requests page.
type RequestsView struct {
	At         time.Time          `json:"at"`
	Range      string             `json:"range"`
	KPIs       []harbor.KPI       `json:"kpis"`
	Pools      []Pool             `json:"pools"`
	Board      []harbor.Board     `json:"leaderboard"`
	Ledger     []harbor.LedgerCol `json:"ledger"`
	Recent     []harbor.Request   `json:"recent"`    // newest first
	Residency  []harbor.Request   `json:"residency"` // served requests, oldest first
	Throughput []Throughput       `json:"throughput"`
	Sources    map[string]string  `json:"sources"` // engine -> where its request figures come from
}

// Throughput is one served request on the throughput chart.
type Throughput struct {
	T      int64   `json:"t"`
	Engine string  `json:"engine"`
	Decode float64 `json:"decode,omitempty"`
	Prompt float64 `json:"prompt,omitempty"`
}

func (b *Broker) Requests(rng time.Duration) RequestsView {
	now := time.Now()
	rec := b.Recorder()
	all := rec.Requests(now.Add(-7 * 24 * time.Hour))
	inRange := all[sort.Search(len(all), func(i int) bool { return all[i].At.After(now.Add(-rng)) }):]
	v := RequestsView{
		At: now, Range: rangeName(rng),
		KPIs:    harbor.KPIs(all, now, time.Hour),
		Board:   harbor.Leaderboard(all, now.Add(-rng)),
		Ledger:  harbor.Ledger(all, now, rec.Counters()),
		Sources: map[string]string{},
	}
	for i := len(all) - 1; i >= 0 && len(v.Recent) < 30; i-- {
		v.Recent = append(v.Recent, all[i])
	}
	for i := len(all) - 1; i >= 0 && len(v.Residency) < 40; i-- {
		if all[i].OK() && all[i].Prompt+all[i].Output > 0 {
			v.Residency = append(v.Residency, all[i])
		}
	}
	for i, j := 0, len(v.Residency)-1; i < j; i, j = i+1, j-1 {
		v.Residency[i], v.Residency[j] = v.Residency[j], v.Residency[i]
	}
	for _, q := range inRange {
		if q.OK() && (q.DecodeTPS > 0 || q.PromptTPS > 0) {
			v.Throughput = append(v.Throughput, Throughput{T: q.At.Unix(), Engine: q.Engine, Decode: q.DecodeTPS, Prompt: q.PromptTPS})
		}
	}
	if n := len(v.Throughput); n > 2000 {
		v.Throughput = v.Throughput[n-2000:]
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.engines {
		if !e.IsLLM() {
			continue
		}
		name := e.Config().Name
		s := b.st[name]
		p := Pool{Engine: name, Kind: s.Kind, Model: s.Model, Up: s.Up, Loaded: s.Loaded}
		if s.Extra != nil {
			p.KV, p.Slots, p.Context, p.Disk = s.Extra["kv"], s.Extra["slots"], s.Extra["context"], s.Extra["disk"]
		}
		if kv, ok := p.KV.(map[string]any); ok {
			p.Window, _ = kv["window"].(int)
		} else if n, ok := p.Context.(float64); ok {
			p.Window = int(n)
		}
		v.Pools = append(v.Pools, p)
		switch {
		case s.Kind == "tabby" && b.cfg.Gateway.Enabled:
			v.Sources[name] = "TabbyAPI's log when baton started it, else the gateway"
		case s.Kind == "tabby":
			v.Sources[name] = "TabbyAPI's log when baton started it"
		case b.cfg.Gateway.Enabled:
			v.Sources[name] = "the gateway (requests sent to " + name + " directly are not seen)"
		default:
			v.Sources[name] = "nothing: turn the gateway on to count its requests"
		}
	}
	return v
}

// Metered reports whether an engine's own log counts its requests right now
// (TabbyAPI that baton started), so the gateway need not.
func (b *Broker) Metered(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st[name].BusyExact
}
