package engine

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"baton/internal/config"
	"baton/internal/win"
)

// at12 is the clock the request tests run at: 12:47:00 today, local time.
var at12 = time.Date(2026, 10, 20, 12, 47, 0, 0, time.Local)

func track(text string, now time.Time) *tabbyReqs {
	r := &tabbyReqs{}
	r.feed(text, now)
	return r
}

func inFlight(r *tabbyReqs) string {
	var tags []string
	for k := range r.running {
		tags = append(tags, k)
	}
	sort.Strings(tags)
	return strings.Join(tags, " ")
}

func TestTabbyReqsInterleaved(t *testing.T) {
	log := "===== baton started cmd.exe /c start.bat at 2026-10-20T12:45:53-07:00 =====\n" +
		"12:46:11.135 INFO:     #4 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset), max_tokens: 300 (req)\n" +
		"12:46:11.900 INFO:     #5 chat/completions (stream): 1,515 prompt tokens · top_k: 20 (preset)\n" +
		"12:46:13.391 INFO:     #4 chat/completions (stream): 64 tokens generated at 36.2 T/s · prompt 58 tokens, none cached, 58 new in 0.48 s · first token 0.48 s, total 2.25 s\n" +
		"12:46:14.000 INFO:     #6 chat/completions: 59 prompt tokens · top_k: 20 (preset), max_tokens: 200 (req)\n" +
		"12:46:14.500 INFO:     #6 chat/completions: parsed 1 tool call (qwen3_coder)\n"
	r := track(log, at12)
	if got := inFlight(r); got != "5 6" {
		t.Fatalf("in flight = %q, want \"5 6\"", got)
	}
	if want := time.Date(2026, 10, 20, 12, 45, 53, 0, time.FixedZone("", -7*3600)); !r.marker.Equal(want) {
		t.Fatalf("marker = %v, want %v", r.marker, want)
	}
	if got, want := r.running["5"], time.Date(2026, 10, 20, 12, 46, 11, 0, time.Local); !got.Equal(want) {
		t.Fatalf("#5 started %v, want %v", got, want)
	}
	// Non-stream requests log the same lines without "(stream)".
	r.feed("12:46:16.000 INFO:     #6 chat/completions: 37 tokens generated at 31.1 T/s · prompt 59 tokens, none cached\n", at12)
	r.feed("12:46:20.000 INFO:     #5 chat/completions (stream): 9 tokens generated · prompt 1,515 tokens, 34% cached\n", at12)
	if got := inFlight(r); got != "" {
		t.Fatalf("in flight = %q after both finished", got)
	}
}

func TestTabbyReqsEnds(t *testing.T) {
	log := "12:40:00.000 INFO:     #7 chat/completions: 100 prompt tokens · top_k: 20 (preset)\n" +
		"12:40:00.000 INFO:     #8 chat/completions: 100 prompt tokens · top_k: 20 (preset)\n" +
		"12:40:00.000 INFO:     #9 kobold/generate: 100 prompt tokens · top_k: 20 (preset)\n" +
		"12:40:00.000 INFO:     #10.0 chat/completions (stream): 100 prompt tokens · top_k: 20 (preset)\n" +
		"12:40:00.000 INFO:     #10.1 chat/completions (stream): 100 prompt tokens · top_k: 20 (preset)\n" +
		"12:40:00.000 INFO:     #11 completions (stream): 100 prompt tokens · top_k: 20 (preset)\n" +
		"12:40:01.000 ERROR:    Sent to request: #7 chat completion aborted. Maybe the model was unloaded? Please check the server console.\n" +
		"12:40:02.000 WARNING:  #8 chat/completions: client disconnected, generation cancelled\n" +
		"12:40:03.000 ERROR:    #9 kobold/generate cancelled by user.\n" +
		"12:40:04.000 WARNING:  #10 chat/completions: client disconnected, generation cancelled\n"
	r := track(log, at12)
	if got := inFlight(r); got != "11" {
		t.Fatalf("in flight = %q, want \"11\"", got)
	}
	// An unnamed failure ends the request only when it is the one in flight.
	r.feed("12:40:05.000 WARNING:  Generation failed; error was contained to this request and the generator is still healthy.\n", at12)
	if got := inFlight(r); got != "" {
		t.Fatalf("in flight = %q after the contained failure", got)
	}
	r.feed("12:40:06.000 INFO:     #12 chat/completions: 100 prompt tokens · top_k: 20 (preset)\n"+
		"12:40:06.000 INFO:     #13 chat/completions: 100 prompt tokens · top_k: 20 (preset)\n"+
		"12:40:07.000 WARNING:  Generation failed; error was contained to this request and the generator is still healthy.\n", at12)
	if got := inFlight(r); got != "12 13" {
		t.Fatalf("in flight = %q, want both kept (the failure names neither)", got)
	}
	r.feed("12:40:08.000 WARNING:  Immediately terminating all jobs. Clients will have their requests cancelled.\n", at12)
	if got := inFlight(r); got != "" {
		t.Fatalf("in flight = %q after all jobs were terminated", got)
	}
	// A start read again (a wrapped record re-applied) does not revive it.
	r.apply("12:40:06.000 INFO:     #12 chat/completions: 100 prompt tokens · top_k: 20 (preset)", at12)
	if got := inFlight(r); got != "" {
		t.Fatalf("in flight = %q: an ended request came back", got)
	}
}

func TestTabbyReqsStale(t *testing.T) {
	r := track("12:10:00.000 INFO:     #3 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n"+
		"12:30:00.000 INFO:     #4 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n", at12)
	r.expire(at12, tabbyStale)
	if got := inFlight(r); got != "4" {
		t.Fatalf("in flight = %q, want only the recent start", got)
	}
	// Just after midnight, a start at 23:59 was yesterday.
	r = track("23:59:50.000 INFO:     #3 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n", time.Date(2026, 10, 21, 0, 0, 10, 0, time.Local))
	if got, want := r.running["3"], time.Date(2026, 10, 20, 23, 59, 50, 0, time.Local); !got.Equal(want) {
		t.Fatalf("started %v, want %v", got, want)
	}
}

func TestTabbyReqsMarkerResets(t *testing.T) {
	log := "===== baton started cmd.exe /c start.bat at 2026-10-20T12:27:32-07:00 =====\n" +
		"12:28:00.000 INFO:     #4 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n" +
		"12:28:01.000 INFO:     #9 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n" +
		"12:28:02.000 INFO:     #9 chat/completions (stream): 9 tokens generated at 30.0 T/s\n" +
		"\n===== baton started cmd.exe /c start.bat at 2026-10-20T12:45:53-07:00 =====\n" +
		"\"HAS UV\"\n" +
		"12:46:11.135 INFO:     #9 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n"
	r := track(log, at12)
	// #4 died with the old server; #9 is a new request with a reused number.
	if got := inFlight(r); got != "9" {
		t.Fatalf("in flight = %q, want \"9\"", got)
	}
	if r.marker.Format("15:04:05") != "12:45:53" {
		t.Fatalf("marker = %v, want the last one", r.marker)
	}
}

func TestTabbyReqsWrapped(t *testing.T) {
	// As TabbyAPI wrote it before baton widened the lines.
	log := "11:55:30.076 INFO:     #58 chat/completions (stream): 1,515 prompt tokens \xb7\n" +
		"                       top_k: 20 (preset), top_p: 0.95 (preset)\n" +
		"11:55:33.565 INFO:     #58 chat/completions (stream): parsed 1 tool call\n" +
		"                       (qwen3_coder)\n" +
		"11:55:33.565 INFO:     #58 chat/completions (stream): 77 tokens generated at\n" +
		"                       46.9 T/s \xb7 prompt 1,515 tokens, 34% cached\n" +
		"11:55:39.243 INFO:     #12345.10 chat/completions (stream): 5,383\n" +
		"                       prompt tokens \xb7 top_k: 20 (preset)\n" +
		"11:55:44.718 ERROR:    Exception in ASGI application\n" +
		"                       Traceback (most recent call last):\n" +
		"  File \"D:\\tabbyAPI\\venv\\Lib\\site-packages\\uvicorn\\protocols\\http\\httptools_impl.py\", line 422, in run_asgi\n" +
		"UnicodeEncodeError: 'charmap' codec can't encode character '\\u2502' in position 4\n" +
		"--- End of logging error ---\n"
	now := time.Date(2026, 10, 20, 11, 56, 0, 0, time.Local)
	r := track(CleanLog([]byte(log)), now)
	if got := inFlight(r); got != "12345.10" {
		t.Fatalf("in flight = %q, want \"12345.10\"", got)
	}

	// A wrapped record split across two reads counts once it is whole.
	r = &tabbyReqs{}
	r.feed("11:55:39.243 INFO:     #64 chat/completions (stream): 5,383\n", now)
	if got := inFlight(r); got != "" {
		t.Fatalf("in flight = %q from half a record", got)
	}
	r.feed("                       prompt tokens · top_k: 20 (preset)\n", now)
	if got := inFlight(r); got != "64" {
		t.Fatalf("in flight = %q, want \"64\"", got)
	}
	r.feed("11:55:44.000 INFO:     #64 chat/completions (stream): 77 tokens generated at\n", now)
	r.feed("                       46.9 T/s · prompt 5,383 tokens, 34% cached\n", now)
	if got := inFlight(r); got != "" {
		t.Fatalf("in flight = %q after the finish", got)
	}
}

func TestParseRealLogRequests(t *testing.T) {
	p := LogPath(os.Getenv("LOCALAPPDATA")+`\baton`, "tabby")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skip("no tabby log on this machine")
	}
	r := track(CleanLog(b), time.Now())
	t.Logf("last marker %v, in flight %v", r.marker, r.running)
}

// --- the tabby engine: when the log is trusted, and what the status says ---

func toFileTime(t time.Time) int64 { return t.UnixNano()/100 + 116444736000000000 }

func testTabby(t *testing.T, log string) (*tabby, string) {
	t.Helper()
	dir := t.TempDir()
	path := LogPath(dir, "tabby")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	return &tabby{base: base{cfg: config.Engine{Name: "tabby", Kind: "tabby"}, env: &Env{StateDir: dir}}}, path
}

// upStatus is TabbyAPI answering, generating on the GPU, its server process
// started at started.
func upStatus(started time.Time) Status {
	return Status{Up: true, VRAMMiB: map[int]int{0: 20000}, Util: 60, procs: []win.Proc{{PID: 100, Start: toFileTime(started)}}}
}

const testMarker = "===== baton started cmd.exe /c start.bat at 2026-10-20T12:45:53-07:00 =====\n"

var testMarkerTime = time.Date(2026, 10, 20, 12, 45, 53, 0, time.FixedZone("", -7*3600))

func TestTabbyTrackRequests(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 46, 19, 0, time.Local)
	tb, path := testTabby(t, testMarker+
		"12:46:11.135 INFO:     #4 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n")
	st := upStatus(testMarkerTime.Add(2 * time.Second))
	tb.trackRequests(&st, now)
	if !st.BusyExact || !st.Busy || st.BusyWhy != "generating #4 (8s)" {
		t.Fatalf("got exact=%v busy=%v %q", st.BusyExact, st.Busy, st.BusyWhy)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("12:46:12.000 INFO:     #5 chat/completions: 58 prompt tokens · top_k: 20 (preset)\n")
	f.Close()
	st = upStatus(testMarkerTime.Add(2 * time.Second))
	tb.trackRequests(&st, now)
	if !st.Busy || st.BusyWhy != "2 requests in flight" {
		t.Fatalf("got busy=%v %q", st.Busy, st.BusyWhy)
	}

	// A new, shorter log (rotated at launch) starts over.
	if err := os.WriteFile(path, []byte(testMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	st = upStatus(testMarkerTime.Add(2 * time.Second))
	tb.trackRequests(&st, now)
	if !st.BusyExact || st.Busy {
		t.Fatalf("got exact=%v busy=%v %q after the log restarted", st.BusyExact, st.Busy, st.BusyWhy)
	}
}

func TestTabbyUntrustedLog(t *testing.T) {
	now := time.Date(2026, 10, 20, 14, 0, 0, 0, time.Local)
	log := testMarker + "13:59:00.000 INFO:     #4 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n"

	// TabbyAPI started an hour after baton's last start: someone else
	// started it, and its output is not in this log.
	tb, _ := testTabby(t, log)
	st := upStatus(testMarkerTime.Add(time.Hour))
	tb.trackRequests(&st, now)
	if st.BusyExact || st.Busy {
		t.Fatalf("trusted a log of another TabbyAPI: exact=%v busy=%v %q", st.BusyExact, st.Busy, st.BusyWhy)
	}

	// Started with a console window: the output goes there.
	tb, _ = testTabby(t, log)
	tb.cfg.ShowWindow = true
	st = upStatus(testMarkerTime.Add(time.Second))
	tb.trackRequests(&st, now)
	if st.BusyExact {
		t.Fatal("trusted the log of a TabbyAPI writing to a console")
	}

	// Not answering: nothing is served, the old checks stay.
	tb, _ = testTabby(t, log)
	st = upStatus(testMarkerTime.Add(time.Second))
	st.Up = false
	tb.trackRequests(&st, now)
	if st.BusyExact {
		t.Fatal("exact while TabbyAPI is not up")
	}
}

func TestTabbyIdleGPUDropsRequests(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 47, 0, 0, time.Local)
	log := testMarker + "12:46:50.000 INFO:     #4 chat/completions (stream): 58 prompt tokens · top_k: 20 (preset)\n"
	tb, _ := testTabby(t, log)
	idle := func(at time.Time) Status {
		st := upStatus(testMarkerTime.Add(time.Second))
		st.Util = 0
		tb.trackRequests(&st, at)
		return st
	}
	if st := idle(now); !st.Busy {
		t.Fatal("a request on an idle GPU should count at first")
	}
	if st := idle(now.Add(tabbyIdleGrace - time.Second)); !st.Busy {
		t.Fatal("dropped before the grace ran out")
	}
	if st := idle(now.Add(tabbyIdleGrace)); st.Busy || !st.BusyExact {
		t.Fatalf("got exact=%v busy=%v %q, want the request dropped", st.BusyExact, st.Busy, st.BusyWhy)
	}

	// Without per-process GPU data the guard stays off.
	tb, _ = testTabby(t, log)
	for _, d := range []time.Duration{0, time.Minute} {
		st := upStatus(testMarkerTime.Add(time.Second))
		st.Util, st.VRAMMiB = 0, map[int]int{}
		tb.trackRequests(&st, now.Add(d))
		if !st.Busy {
			t.Fatalf("dropped after %s without GPU data", d)
		}
	}
}
