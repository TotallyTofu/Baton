package gateway

import (
	"testing"
	"time"
)

func TestServedStreamUsage(t *testing.T) {
	sent := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	first, end := sent.Add(2*time.Second), sent.Add(12*time.Second)
	tail := []byte(`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}

data: {"choices":[],"usage":{"prompt_tokens":1200,"completion_tokens":101,"prompt_tokens_details":{"cached_tokens":1000}}}

data: [DONE]

`)
	q := served("unsloth", "gemma", "chat/completions", true, tail, sent, first, end)
	if q.Prompt != 1200 || q.Output != 101 || q.Cached != 1000 || q.Status != 200 {
		t.Fatalf("usage: %+v", q)
	}
	// 100 tokens after the first, over the 10 s from first byte to end.
	if q.FirstTok != 2 || q.Total != 12 || q.DecodeTPS != 10 {
		t.Fatalf("timing: first %v total %v decode %v", q.FirstTok, q.Total, q.DecodeTPS)
	}
}

func TestServedLlamaTimings(t *testing.T) {
	sent := time.Now()
	tail := []byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":50,"completion_tokens":20},` +
		`"timings":{"prompt_n":10,"prompt_per_second":812.34,"predicted_n":20,"predicted_per_second":41.27,"cache_n":40}}`)
	q := served("unsloth", "gemma", "chat/completions", false, tail, sent, sent.Add(time.Second), sent.Add(2*time.Second))
	if q.DecodeTPS != 41.3 || q.PromptTPS != 812.3 || q.Cached != 40 || q.Prompt != 50 || q.FirstTok != 0 {
		t.Fatalf("timings: %+v", q)
	}
}

func TestServedNoUsage(t *testing.T) {
	sent := time.Now()
	q := served("unsloth", "gemma", "chat/completions", false, []byte(`{"choices":[]}`), sent, sent, sent.Add(time.Second))
	if q.Prompt != 0 || q.Output != 0 || q.DecodeTPS != 0 || q.Total != 1 {
		t.Fatalf("no usage: %+v", q)
	}
}
