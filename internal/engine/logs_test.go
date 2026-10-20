package engine

import (
	"os"
	"testing"
)

func TestParseTabbyMetricsWrapped(t *testing.T) {
	// As TabbyAPI wrote it before baton widened the lines.
	log := "11:55:30.076 INFO:     #58 chat/completions (stream): 1,515 prompt tokens \xb7\n" +
		"11:55:33.565 INFO:     #58 chat/completions (stream): 77 tokens generated at\n" +
		"                       46.9 T/s \xb7 prompt 1,515 tokens, 34% cached, 1,003 new in\n" +
		"                       1.79 s (560 T/s) \xb7 first token 1.84 s, total 3.48 s \xb7\n" +
		"                       draft 49/84 accepted (58%)\n" +
		"11:55:39.243 INFO:     #64 chat/completions (stream): 5,383 prompt tokens \xb7\n"
	m := ParseTabbyMetrics(CleanLog([]byte(log)))
	if m == nil {
		t.Fatal("no metrics found")
	}
	want := TabbyMetrics{At: "11:55:33", Request: "#58 chat/completions (stream)", Tokens: 77, Decode: 46.9, Prompt: 1515, CachedPct: 34, PromptTPS: 560, FirstToken: 1.84, Total: 3.48, DraftPct: 58, DraftOK: 49, DraftAll: 84}
	if *m != want {
		t.Fatalf("got %+v\nwant %+v", *m, want)
	}
}

func TestCleanLog(t *testing.T) {
	in := "\x1b[32mok\x1b[0m\r\nLoading 10%\rLoading 100%\nYour API key is: abc123\n"
	if got, want := CleanLog([]byte(in)), "ok\nLoading 100%\nYour API key is: [redacted]\n"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestParseRealLog(t *testing.T) {
	p := LogPath(os.Getenv("LOCALAPPDATA")+`\baton`, "tabby")
	text, _, _, err := ReadLog(p, -1)
	if err != nil {
		t.Skip("no tabby log on this machine")
	}
	t.Logf("last request: %+v", ParseTabbyMetrics(text))
}

func TestParseTabbyMetricsNoneCached(t *testing.T) {
	log := "12:46:13.391 INFO:     #4 chat/completions (stream): 64 tokens generated at 36.2 T/s · prompt 58 tokens, none cached, 58 new in 0.48 s · first token 0.48 s, total 2.25 s · draft 44/60 accepted (73%)\n"
	m := ParseTabbyMetrics(log)
	if m == nil || m.Prompt != 58 || m.CachedPct != 0 || m.PromptTPS != 120.8 || m.DraftPct != 73 || m.Decode != 36.2 {
		t.Fatalf("got %+v", m)
	}
}
