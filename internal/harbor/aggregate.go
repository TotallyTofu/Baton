package harbor

import (
	"math"
	"sort"
	"time"
)

// Board is one leaderboard row: a model on an engine.
type Board struct {
	Engine    string   `json:"engine"`
	Model     string   `json:"model"`
	Requests  int      `json:"requests"`
	Errors    int      `json:"errors"`
	Prompt    int64    `json:"prompt"`
	Cached    int64    `json:"cached"`
	Output    int64    `json:"output"`
	DecodeMed *float64 `json:"decode_med"`
	FirstMed  *float64 `json:"first_token_med"`
	CachePct  *float64 `json:"cache_pct"` // of prompt tokens
	DraftPct  *float64 `json:"draft_pct"` // of drafted tokens
}

// Leaderboard ranks the models by requests served since since.
func Leaderboard(reqs []Request, since time.Time) []Board {
	type acc struct {
		b               Board
		decode, first   []float64
		draftOK, drafts int64
	}
	rows := map[string]*acc{}
	var order []string
	for _, q := range reqs {
		if !q.At.After(since) {
			continue
		}
		k := q.Engine + "\x00" + q.Model
		a := rows[k]
		if a == nil {
			a = &acc{b: Board{Engine: q.Engine, Model: q.Model}}
			rows[k] = a
			order = append(order, k)
		}
		if !q.OK() {
			a.b.Errors++
			continue
		}
		a.b.Requests++
		a.b.Prompt += int64(q.Prompt)
		a.b.Cached += int64(q.Cached)
		a.b.Output += int64(q.Output)
		if q.DecodeTPS > 0 {
			a.decode = append(a.decode, q.DecodeTPS)
		}
		if q.FirstTok > 0 {
			a.first = append(a.first, q.FirstTok)
		}
		a.draftOK += int64(q.DraftOK)
		a.drafts += int64(q.DraftAll)
	}
	out := make([]Board, 0, len(order))
	for _, k := range order {
		a := rows[k]
		a.b.DecodeMed = quantile(a.decode, .5)
		a.b.FirstMed = quantile(a.first, .5)
		a.b.CachePct = pct(a.b.Cached, a.b.Prompt)
		a.b.DraftPct = pct(a.draftOK, a.drafts)
		out = append(out, a.b)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Output > out[j].Output
	})
	return out
}

// LedgerCol is the tokens of one window: generated, read fresh, served from
// cache.
type LedgerCol struct {
	Label     string `json:"label"`
	Requests  int64  `json:"requests"`
	Generated int64  `json:"generated"`
	Fresh     int64  `json:"fresh"`
	Cached    int64  `json:"cached"`
}

// Ledger sums the tokens of the last hour, day and week, and since the
// counters began.
func Ledger(reqs []Request, now time.Time, c Counters) []LedgerCol {
	wins := []struct {
		label string
		d     time.Duration
	}{{"1h", time.Hour}, {"24h", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}}
	out := make([]LedgerCol, 0, len(wins)+1)
	for _, w := range wins {
		col := LedgerCol{Label: w.label}
		cut := now.Add(-w.d)
		for _, q := range reqs {
			if !q.At.After(cut) || !q.OK() {
				continue
			}
			col.Requests++
			col.Generated += int64(q.Output)
			col.Cached += int64(q.Cached)
			col.Fresh += int64(max(q.Prompt-q.Cached, 0))
		}
		out = append(out, col)
	}
	return append(out, LedgerCol{Label: "since " + c.Since.Format("Jan 2"), Requests: c.Requests, Generated: c.Generated, Fresh: c.Fresh, Cached: c.Cached})
}

// KPI is one figure of the request strip with its recent history.
type KPI struct {
	Key   string    `json:"key"`
	Value *float64  `json:"value"` // nil: nothing measured in the window
	Spark []float64 `json:"spark,omitempty"`
	N     int       `json:"n"` // requests behind it
}

const sparkLen = 30

// KPIs works out the request strip over the last window: median decode and
// first-token time, p95 total time, requests per minute, the share of prompt
// tokens served from cache and of drafted tokens accepted.
func KPIs(reqs []Request, now time.Time, window time.Duration) []KPI {
	cut := now.Add(-window)
	var decode, first, total []float64
	var prompt, cached, dOK, dAll int64
	n := 0
	for _, q := range reqs {
		if !q.At.After(cut) || !q.OK() {
			continue
		}
		n++
		if q.DecodeTPS > 0 {
			decode = append(decode, q.DecodeTPS)
		}
		if q.FirstTok > 0 {
			first = append(first, q.FirstTok)
		}
		if q.Total > 0 {
			total = append(total, q.Total)
		}
		prompt += int64(q.Prompt)
		cached += int64(q.Cached)
		dOK += int64(q.DraftOK)
		dAll += int64(q.DraftAll)
	}
	// Requests per minute over the window, one spark bar per minute.
	mins := max(int(window/time.Minute), 1)
	perMin := make([]float64, min(mins, sparkLen))
	for _, q := range reqs {
		if !q.At.After(cut) {
			continue
		}
		ago := int(now.Sub(q.At) / time.Minute)
		if i := len(perMin) - 1 - ago; i >= 0 && i < len(perMin) {
			perMin[i]++
		}
	}
	var rpm *float64
	if n > 0 {
		rpm = round1(float64(n) / float64(mins))
	}
	return []KPI{
		{Key: "decode", Value: quantile(decode, .5), Spark: tail(decode), N: len(decode)},
		{Key: "first_token", Value: quantile(first, .5), Spark: tail(first), N: len(first)},
		{Key: "p95_total", Value: quantile(total, .95), Spark: tail(total), N: len(total)},
		{Key: "req_min", Value: rpm, Spark: perMin, N: n},
		{Key: "cache", Value: pct(cached, prompt), N: n},
		{Key: "draft", Value: pct(dOK, dAll), N: n},
	}
}

func tail(v []float64) []float64 {
	if len(v) > sparkLen {
		v = v[len(v)-sparkLen:]
	}
	return append([]float64(nil), v...)
}

// quantile is the nearest-rank quantile, or nil for no values.
func quantile(v []float64, q float64) *float64 {
	if len(v) == 0 {
		return nil
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(q*float64(len(s)))) - 1
	r := math.Round(s[min(max(i, 0), len(s)-1)]*100) / 100
	return &r
}

func pct(part, whole int64) *float64 {
	if whole <= 0 {
		return nil
	}
	return round1(float64(part) / float64(whole) * 100)
}

func round1(f float64) *float64 {
	r := math.Round(f*10) / 10
	return &r
}
