package harbor

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)

func TestTideFineAndCoarse(t *testing.T) {
	r := Open("")
	// Two hours of readings every 2.5 s: only one per 5 s is kept fine.
	for s := 0.0; s < 7200; s += 2.5 {
		at := t0.Add(time.Duration(s * float64(time.Second)))
		used := 1000
		if s >= 3600 {
			used = 3000
		}
		r.Tide(at, []Level{{Key: "gpu0", Name: "card", Total: 16000, Used: used, By: map[string]int{"tabby": used - 200}}})
	}
	now := t0.Add(7200 * time.Second)
	fine := r.TideView(time.Hour, now)
	// 720 readings in the hour, averaged in pairs to stay under maxPoints.
	if len(fine) != 1 || fine[0].Key != "gpu0" || fine[0].Total != 16000 || fine[0].Step != 10 {
		t.Fatalf("fine view: key %q total %d step %d", fine[0].Key, fine[0].Total, fine[0].Step)
	}
	if n := len(fine[0].Points); n < 350 || n > 361 {
		t.Fatalf("fine points = %d, want about 360", n)
	}
	if p := fine[0].Points[0]; p.Used != 3000 || p.By["tabby"] != 2800 {
		t.Fatalf("fine point: %+v", p)
	}
	day := r.TideView(24*time.Hour, now)[0]
	// 120 minutes plus the minute in progress, under maxPoints: one per minute.
	if day.Step != 60 || len(day.Points) < 119 || len(day.Points) > 121 {
		t.Fatalf("coarse view: step %d, %d points", day.Step, len(day.Points))
	}
	if day.Points[0].Used != 1000 || day.Points[len(day.Points)-1].Used != 3000 {
		t.Fatalf("coarse averages: first %+v last %+v", day.Points[0], day.Points[len(day.Points)-1])
	}
}

func TestDownsample(t *testing.T) {
	var ps []TidePoint
	for i := 0; i < 1000; i++ {
		ps = append(ps, TidePoint{T: int64(i), Used: i % 2 * 100, By: map[string]int{"a": 10}})
	}
	out, k := downsample(ps, 480)
	if k != 3 || len(out) != 334 {
		t.Fatalf("k=%d len=%d", k, len(out))
	}
	if out[0].Used != 33 || out[0].By["a"] != 10 || out[1].T != 3 {
		t.Fatalf("first buckets: %+v %+v", out[0], out[1])
	}
}

func TestOccupantsGrace(t *testing.T) {
	r := Open("")
	tabby := Occupant{Kind: "tabby", Owner: "tabby", Label: "Qwen", GPUs: []int{0, 1}}
	r.Occupants(t0, []Occupant{tabby})
	// One missed probe does not split the stay.
	r.Occupants(t0.Add(2*time.Second), nil)
	r.Occupants(t0.Add(4*time.Second), []Occupant{tabby})
	if sp := r.Timeline(time.Hour, t0.Add(5*time.Second)); len(sp) != 1 || !sp[0].End.IsZero() {
		t.Fatalf("after a blip: %+v", sp)
	}
	// Gone for good: the stay ends when it was first missed.
	game := Occupant{Kind: "game", Owner: "game", Label: "Stellar Blade"}
	r.Occupants(t0.Add(10*time.Second), []Occupant{game})
	r.Occupants(t0.Add(20*time.Second), []Occupant{game})
	sp := r.Timeline(time.Hour, t0.Add(21*time.Second))
	if len(sp) != 2 {
		t.Fatalf("spans: %+v", sp)
	}
	if !sp[0].End.Equal(t0.Add(10*time.Second)) || sp[0].Label != "Qwen" || len(sp[0].GPUs) != 2 {
		t.Fatalf("tabby stay: %+v", sp[0])
	}
	if sp[1].Kind != "game" || !sp[1].End.IsZero() || !sp[1].Start.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("game stay: %+v", sp[1])
	}
	// Stays that ended before the range are left out.
	if got := r.Timeline(5*time.Second, t0.Add(21*time.Second)); len(got) != 1 || got[0].Kind != "game" {
		t.Fatalf("short range: %+v", got)
	}
}

func TestRequestUpsertAndTotals(t *testing.T) {
	r := Open("")
	now := time.Now()
	r.Request(Request{Key: "tabby#1", At: now.Add(-time.Minute), Engine: "tabby", Prompt: 100, Cached: 40, Output: 10})
	// The log later completes the same record: it replaces, not adds.
	r.Request(Request{Key: "tabby#1", At: now.Add(-time.Minute), Engine: "tabby", Prompt: 100, Cached: 40, Output: 50, DraftOK: 5, DraftAll: 10})
	r.Request(Request{At: now.Add(-2 * time.Minute), Engine: "unsloth", Prompt: 10, Output: 5})
	r.Request(Request{At: now.Add(-30 * time.Second), Engine: "unsloth", Status: 503, Error: "gpu_reserved"})
	// Older than the retention: dropped.
	r.Request(Request{At: now.Add(-8 * 24 * time.Hour), Engine: "tabby", Prompt: 1, Output: 1})
	c := r.Counters()
	if c.Requests != 2 || c.Generated != 55 || c.Cached != 40 || c.Fresh != 70 {
		t.Fatalf("counters: %+v", c)
	}
	got := r.Requests(time.Time{})
	if len(got) != 3 || got[0].Engine != "unsloth" || got[1].Output != 50 || got[2].Status != 503 {
		t.Fatalf("requests (oldest first): %+v", got)
	}
}

func TestLeaderboardLedgerKPIs(t *testing.T) {
	now := t0
	reqs := []Request{
		{At: now.Add(-50 * time.Minute), Engine: "tabby", Model: "Qwen", Prompt: 1000, Cached: 500, Output: 100, DecodeTPS: 30, FirstTok: 1, Total: 4, DraftOK: 30, DraftAll: 60},
		{At: now.Add(-20 * time.Minute), Engine: "tabby", Model: "Qwen", Prompt: 1000, Cached: 0, Output: 300, DecodeTPS: 40, FirstTok: 2, Total: 9, DraftOK: 10, DraftAll: 40},
		{At: now.Add(-10 * time.Minute), Engine: "tabby", Model: "Qwen", Prompt: 0, Output: 10, DecodeTPS: 50, Total: 1},
		{At: now.Add(-5 * time.Minute), Engine: "unsloth", Model: "gemma", Prompt: 200, Output: 20},
		{At: now.Add(-4 * time.Minute), Engine: "unsloth", Model: "gemma", Status: 502},
		{At: now.Add(-3 * time.Hour), Engine: "tabby", Model: "Qwen", Prompt: 5, Output: 5},
	}
	lb := Leaderboard(reqs, now.Add(-time.Hour))
	if len(lb) != 2 || lb[0].Model != "Qwen" || lb[0].Requests != 3 || lb[1].Errors != 1 || lb[1].Requests != 1 {
		t.Fatalf("leaderboard: %+v", lb)
	}
	q := lb[0]
	if *q.DecodeMed != 40 || *q.CachePct != 25 || *q.DraftPct != 40 || *q.FirstMed != 1 || q.Output != 410 {
		t.Fatalf("qwen row: decode %v cache %v draft %v first %v out %d", *q.DecodeMed, *q.CachePct, *q.DraftPct, *q.FirstMed, q.Output)
	}
	if lb[1].DecodeMed != nil || lb[1].DraftPct != nil {
		t.Fatalf("gemma row should have no decode/draft figures: %+v", lb[1])
	}

	led := Ledger(reqs, now, Counters{Since: t0.Add(-48 * time.Hour), Requests: 9, Generated: 99})
	if len(led) != 4 || led[0].Label != "1h" || led[0].Requests != 4 || led[0].Generated != 430 || led[0].Fresh != 1700 || led[0].Cached != 500 {
		t.Fatalf("ledger 1h: %+v", led[0])
	}
	if led[1].Requests != 5 || led[3].Requests != 9 || led[3].Generated != 99 {
		t.Fatalf("ledger: %+v", led)
	}

	k := map[string]KPI{}
	for _, x := range KPIs(reqs, now, time.Hour) {
		k[x.Key] = x
	}
	if *k["decode"].Value != 40 || len(k["decode"].Spark) != 3 {
		t.Fatalf("decode kpi: %+v", k["decode"])
	}
	if *k["p95_total"].Value != 9 || *k["first_token"].Value != 1 {
		t.Fatalf("time kpis: p95 %v first %v", *k["p95_total"].Value, *k["first_token"].Value)
	}
	if *k["req_min"].Value != 0.1 || len(k["req_min"].Spark) != sparkLen {
		t.Fatalf("req/min: %+v", k["req_min"])
	}
	if *k["cache"].Value != 22.7 || *k["draft"].Value != 40 {
		t.Fatalf("cache %v draft %v", *k["cache"].Value, *k["draft"].Value)
	}
	if empty := KPIs(nil, now, time.Hour); empty[0].Value != nil || empty[3].Value != nil {
		t.Fatalf("no requests should give no values: %+v", empty)
	}
}

func TestSaveAndOpen(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	r.Count(func(c *Counters) { c.Handoffs += 3; c.HandedMiB += 2048 })
	n := r.Ticket()
	r.Voyage(Voyage{No: n, Name: "switch to tabby", Start: time.Now(), Dur: 12.5, Result: "ok"})
	r.Occupants(time.Now(), []Occupant{{Kind: "tabby", Owner: "tabby", Label: "Qwen"}})
	r.Request(Request{Key: "k1", At: time.Now(), Engine: "tabby", Prompt: 10, Output: 2})
	if err := r.Save(); err != nil {
		t.Fatal(err)
	}
	o := Open(dir)
	c := o.Counters()
	if c.Handoffs != 3 || c.HandedMiB != 2048 || c.Requests != 1 || !c.Since.Equal(r.Counters().Since) {
		t.Fatalf("counters after reopen: %+v", c)
	}
	if o.Ticket() != n+1 {
		t.Fatal("ticket numbers should keep rising across restarts")
	}
	if v := o.Voyages(5); len(v) != 1 || v[0].Name != "switch to tabby" {
		t.Fatalf("voyages: %+v", v)
	}
	if sp := o.Timeline(time.Hour, time.Now()); len(sp) != 1 || sp[0].End.IsZero() {
		t.Fatalf("a stay open at the save should end at it: %+v", sp)
	}
	// The same record read again (the log after a restart) is not counted twice.
	o.Request(Request{Key: "k1", At: time.Now(), Engine: "tabby", Prompt: 10, Output: 2})
	if c := o.Counters(); c.Requests != 1 || len(o.Requests(time.Time{})) != 1 {
		t.Fatalf("re-read record counted twice: %+v", c)
	}
}
