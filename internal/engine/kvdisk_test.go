package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"baton/internal/config"
	"baton/internal/win"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- config.yml: the memory: settings ---

func TestYAMLString(t *testing.T) {
	for in, want := range map[string]string{
		``:                               ``,
		`~`:                              ``,
		`null`:                           ``,
		`D:\kvcache`:                     `D:\kvcache`,
		`D:\kvcache # on the NVMe`:       `D:\kvcache`,
		`'D:\kv cache'`:                  `D:\kv cache`,
		`'it''s here' # note`:            `it's here`,
		`"D:\\kvcache"`:                  `D:\kvcache`,
		`"D:/kv # not a comment" # one`:  `D:/kv # not a comment`,
		`"say \"hi\""`:                   `say "hi"`,
		`51200`:                          `51200`,
		`true`:                           `true`,
		`"\\\\server\\share\\kv"`:        `\\server\share\kv`,
		`D:\kv#cache`:                    `D:\kv#cache`,
		`'unterminated`:                  `unterminated`,
		`"esc \q kept"`:                  `esc \q kept`,
		`  padded  `:                     `padded`,
		`plain with spaces  # and notes`: `plain with spaces`,
	} {
		if got := yamlString(in); got != want {
			t.Errorf("yamlString(%q) = %q, want %q", in, got, want)
		}
	}
}

func memoryYAML(lines ...string) string {
	return "model:\n  model_name: Qwen\n\n# Global memory settings\nmemory:\n\n  # A comment\n" +
		strings.Join(lines, "\n") + "\n\n# Next section\ndeveloper:\n  disk_cache_dir: wrong-section\n"
}

func TestReadKVDisk(t *testing.T) {
	dir := t.TempDir()
	home, _ := os.UserHomeDir()
	vol := filepath.VolumeName(dir)
	cases := []struct {
		name   string
		yaml   string
		dir    string
		budget int
		note   bool
	}{
		{"empty is off", memoryYAML("  sysmem_kv_cache: 1024", "  disk_cache_dir:", "  disk_cache_size: 51200"), "", 0, false},
		{"null is off", memoryYAML("  sysmem_kv_cache: 1024", "  disk_cache_dir: null"), "", 0, false},
		{"absolute, plain", memoryYAML("  sysmem_kv_cache: 1024", `  disk_cache_dir: D:\kvcache`, "  disk_cache_size: 20480"), `D:\kvcache`, 20480, false},
		{"single-quoted with a space", memoryYAML("  sysmem_kv_cache: 2048", `  disk_cache_dir: 'D:\kv cache'  # fast drive`), `D:\kv cache`, 51200, false},
		{"double-quoted escapes", memoryYAML("  sysmem_kv_cache: 2048", `  disk_cache_dir: "D:\\tabbyAPI\\kvcache"`), `D:\tabbyAPI\kvcache`, 51200, false},
		{"forward slashes", memoryYAML("  sysmem_kv_cache: 2048", `  disk_cache_dir: D:/kv/cache`), `D:\kv\cache`, 51200, false},
		{"relative to TabbyAPI's folder", memoryYAML("  sysmem_kv_cache: 2048", "  disk_cache_dir: kvcache"), filepath.Join(dir, "kvcache"), 51200, false},
		{"rooted on its drive", memoryYAML("  sysmem_kv_cache: 2048", `  disk_cache_dir: \kv`), vol + `\kv`, 51200, false},
		{"home", memoryYAML("  sysmem_kv_cache: 2048", "  disk_cache_dir: ~/kv"), filepath.Join(home, "kv"), 51200, false},
		{"no sysmem tier", memoryYAML("  sysmem_kv_cache: 0", `  disk_cache_dir: D:\kvcache`), "", 51200, true},
		{"sysmem tier missing", memoryYAML(`  disk_cache_dir: D:\kvcache`), "", 51200, true},
		{"bad size keeps the default", memoryYAML("  sysmem_kv_cache: 1024", `  disk_cache_dir: D:\kvcache`, "  disk_cache_size: lots"), `D:\kvcache`, 51200, false},
	}
	for _, c := range cases {
		writeFile(t, filepath.Join(dir, "config.yml"), c.yaml)
		k := readKVDisk(dir)
		if k.Dir != c.dir || k.BudgetMiB != c.budget || (k.Note != "") != c.note {
			t.Errorf("%s: got dir=%q budget=%d note=%q, want dir=%q budget=%d note=%v", c.name, k.Dir, k.BudgetMiB, k.Note, c.dir, c.budget, c.note)
		}
	}
	if k := readKVDisk(filepath.Join(dir, "missing")); k.Dir != "" {
		t.Errorf("no config.yml: dir = %q", k.Dir)
	}
	writeFile(t, filepath.Join(dir, "config.yml"), "\ufeffmemory:\r\n  sysmem_kv_cache: 1024\r\n  disk_cache_dir: D:\\kv\r\n")
	if k := readKVDisk(dir); k.Dir != `D:\kv` {
		t.Errorf("CRLF file with a BOM: dir = %q", k.Dir)
	}
}

func TestTmpPID(t *testing.T) {
	for name, want := range map[string]int{
		"0a1b.kv.4242.tmp":        4242,
		"0a1b.rs.17.tmp":          17,
		"0a1b.kv.4242.tmp.d0":     4242,
		"0a1b.kv.4242.tmp.d1.err": 4242,
		"odd.tmp":                 -1,
	} {
		if pid, ok := tmpPID(name); !ok || pid != want {
			t.Errorf("tmpPID(%q) = %d, %v; want %d", name, pid, ok, want)
		}
	}
	for _, name := range []string{"0a1b.kv", "0a1b.rs", "index.sqlite", "index.sqlite-wal", "manifest.json"} {
		if _, ok := tmpPID(name); ok {
			t.Errorf("tmpPID(%q) took it for a temp file", name)
		}
	}
}

// --- waiting for the disk cache to settle ---

// cacheDir is a disk cache directory as ExLlamaV3 lays it out, last written
// long ago.
func cacheDir(t *testing.T) (root, model string) {
	t.Helper()
	root = t.TempDir()
	model = filepath.Join(root, "3f2a9c0d1e2b4a5c")
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{"manifest.json", "index.sqlite", filepath.Join("pages", "0a", "0a11.kv"), filepath.Join("states", "0a11.rs")} {
		p := filepath.Join(model, f)
		writeFile(t, p, strings.Repeat("x", 4096))
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return root, model
}

// publish writes a page the way ExLlamaV3 does: a temp file named after the
// writer's PID, renamed into place when complete.
func publish(t *testing.T, dir, name string, size int) {
	tmp := filepath.Join(dir, fmt.Sprintf("%s.%d.tmp", name, os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Error(err)
		return
	}
	if err := os.WriteFile(tmp, make([]byte, size), 0o644); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		t.Error(err)
	}
}

func TestWaitFlushedSettledReturnsAtOnce(t *testing.T) {
	root, model := cacheDir(t)
	// A temp file an earlier, killed TabbyAPI left behind is no write.
	stale := filepath.Join(model, "pages", "0a", "0a22.kv.999999.tmp")
	writeFile(t, stale, "partial")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	res := waitFlushed(context.Background(), root, []int{os.Getpid()}, 2*time.Second, 10*time.Second, 20*time.Millisecond, nil)
	if res.TimedOut || res.Secs > 0.5 || res.Files != 0 || res.WroteMiB != 0 {
		t.Fatalf("settled cache: %+v", res)
	}
}

func TestWaitFlushedWaitsForTheFlush(t *testing.T) {
	root, model := cacheDir(t)
	pages := filepath.Join(model, "pages", "0b")
	const n = 8
	go func() {
		for i := 0; i < n; i++ {
			publish(t, pages, fmt.Sprintf("0b%02d.kv", i), 1<<20)
			time.Sleep(100 * time.Millisecond)
		}
	}()
	time.Sleep(30 * time.Millisecond) // the flush is under way when the wait begins
	var progress []string
	start := time.Now()
	res := waitFlushed(context.Background(), root, []int{os.Getpid()}, 300*time.Millisecond, 10*time.Second, 20*time.Millisecond, func(s string) { progress = append(progress, s) })
	took := time.Since(start)
	if res.TimedOut {
		t.Fatalf("timed out: %+v", res)
	}
	if took < 700*time.Millisecond {
		t.Fatalf("returned after %s, before the writes ended", took)
	}
	// The first page landed before the wait began; the rest during it.
	if res.Files != n-1 || res.WroteMiB != n-1 {
		t.Fatalf("written: %d files, %.1f MiB; want %d, %d", res.Files, res.WroteMiB, n-1, n-1)
	}
	if res.BusySecs < 0.5 || res.BusySecs > res.Secs {
		t.Fatalf("busy %.1fs of %.1fs", res.BusySecs, res.Secs)
	}
	if len(progress) == 0 || !strings.Contains(progress[len(progress)-1], "MB written") {
		t.Fatalf("progress = %q", progress)
	}
}

func TestWaitFlushedGivesUp(t *testing.T) {
	root, model := cacheDir(t)
	// A write of the live TabbyAPI that never completes.
	writeFile(t, filepath.Join(model, "states", fmt.Sprintf("0c33.rs.%d.tmp", os.Getpid())), "half a checkpoint")
	start := time.Now()
	res := waitFlushed(context.Background(), root, []int{os.Getpid()}, 100*time.Millisecond, 400*time.Millisecond, 20*time.Millisecond, nil)
	if !res.TimedOut || res.Writing != 1 {
		t.Fatalf("want a timeout with one write open: %+v", res)
	}
	if took := time.Since(start); took < 400*time.Millisecond || took > 2*time.Second {
		t.Fatalf("gave up after %s", took)
	}

	// A temp file of an unknown writer counts while it is fresh.
	root2, model2 := cacheDir(t)
	writeFile(t, filepath.Join(model2, "pages", "0a", "0a99.kv.31337.tmp"), "rank marker")
	res = waitFlushed(context.Background(), root2, nil, 100*time.Millisecond, 300*time.Millisecond, 20*time.Millisecond, nil)
	if !res.TimedOut {
		t.Fatalf("a fresh temp file should hold the wait: %+v", res)
	}
}

func TestWaitFlushedStopsWithContext(t *testing.T) {
	root, model := cacheDir(t)
	writeFile(t, filepath.Join(model, "pages", "0a", fmt.Sprintf("0a44.kv.%d.tmp", os.Getpid())), "open")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := waitFlushed(ctx, root, []int{os.Getpid()}, time.Second, 10*time.Second, 20*time.Millisecond, nil)
	if res.TimedOut || time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled wait: %+v after %s", res, time.Since(start))
	}
}

// --- the tabby engine: reloading its files, the disk status, FlushKV ---

// tabbyDir is a TabbyAPI folder with api_tokens.yml and config.yml, and a
// tabby engine on it.
func tabbyDir(t *testing.T, key, cfg string) (*tabby, string) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "api_tokens.yml"), "api_key: "+key+"\nadmin_key: admin-"+key+"\n")
	writeFile(t, filepath.Join(dir, "config.yml"), cfg)
	tb := newTabby(config.Engine{Name: "tabby", Kind: "tabby", Dir: dir, URL: "http://127.0.0.1:5000"}, &Env{StateDir: t.TempDir()})
	return tb, dir
}

func TestTabbyReloadsChangedKey(t *testing.T) {
	tb, dir := tabbyDir(t, "fake-key-one", memoryYAML("  sysmem_kv_cache: 1024"))
	if _, h := tb.Upstream(); h.Get("Authorization") != "Bearer fake-key-one" {
		t.Fatalf("first key: %q", h.Get("Authorization"))
	}
	// TabbyAPI regenerates its key while baton runs (and didn't start it).
	writeFile(t, filepath.Join(dir, "api_tokens.yml"), "api_key: fake-key-number-two\nadmin_key: fake-admin-two\n")
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "api_tokens.yml"), future, future); err != nil {
		t.Fatal(err)
	}
	if _, h := tb.Upstream(); h.Get("Authorization") != "Bearer fake-key-number-two" {
		t.Fatalf("after the change: %q", h.Get("Authorization"))
	}
	if h := tb.hdr(true); h.Get("Authorization") != "Bearer fake-admin-two" {
		t.Fatalf("admin key after the change: %q", h.Get("Authorization"))
	}

	// config.yml changes are picked up the same way.
	writeFile(t, filepath.Join(dir, "config.yml"), strings.Replace(memoryYAML("  sysmem_kv_cache: 1024", `  disk_cache_dir: D:\kvcache`), "model_name: Qwen", "model_name: Other-Model", 1))
	if err := os.Chtimes(filepath.Join(dir, "config.yml"), future, future); err != nil {
		t.Fatal(err)
	}
	tb.refreshFiles()
	if tb.model() != "Other-Model" || tb.diskConf().Dir != `D:\kvcache` {
		t.Fatalf("after config.yml changed: model %q, disk %+v", tb.model(), tb.diskConf())
	}
}

func TestTabbyDiskStatusFromLog(t *testing.T) {
	root, model := cacheDir(t)
	tb, _ := tabbyDir(t, "fake-key", memoryYAML("  sysmem_kv_cache: 1024", "  disk_cache_dir: '"+root+"'", "  disk_cache_size: 1024"))
	log := testMarker +
		"12:45:58.000 INFO:     Loading model D:\\tabbyAPI\\models\\Qwen3.8-27B-exl3 (manual GPU split)\n" +
		"12:46:02.000 INFO:     Disk cache: " + model + " holds 1,234 pages (315,904 reusable tokens) and 12 recurrent\n" +
		"                       checkpoints, 8.61 GB of 50.00 GB.\n" +
		"12:46:30.000 INFO:     Disk cache tier: saved 125 pages (32000 tokens) and 3 checkpoints, 870 MB of pages; restored 100 pages\n" +
		"                       (25600 tokens) and 1 checkpoints. On disk: 1359 pages, 15 checkpoints, 9.5 of 50.0 GB\n"
	logPath := LogPath(tb.env.StateDir, "tabby")
	writeFile(t, logPath, log)
	publish(t, filepath.Join(model, "pages", "0e"), "0e01.kv", 3<<20)
	st := upStatus(testMarkerTime.Add(2 * time.Second))
	st.Loaded = true
	tb.trackRequests(&st, at12)
	d, _ := st.Extra["disk"].(map[string]any)
	if d == nil {
		t.Fatalf("no disk status: %v", st.Extra)
	}
	if d["dir"] != root || d["budget_mib"] != 1024 || d["active"] != true {
		t.Fatalf("disk = %v", d)
	}
	if d["reusable_tokens"] != 315904 || d["saved_tokens"] != 32000 || d["saved_mib"] != 870.0 || d["restored_tokens"] != 25600 || d["pages"] != 1359 {
		t.Fatalf("disk figures = %v", d)
	}
	if used, _ := d["used_mib"].(float64); used < 3 || used > 3.1 {
		t.Fatalf("used_mib = %v, want the 3 MiB page and the small files", d["used_mib"])
	}
	if tb.runDiskDir != model {
		t.Fatalf("run dir = %q, want %q", tb.runDiskDir, model)
	}

	// The writer's next summary arrives in two reads: its wrapped line must
	// not count twice.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("12:47:00.000 INFO:     Disk cache tier: restored 10 pages (2560 tokens) and 0 checkpoints. On disk: 1359\n")
	f.Close()
	st = upStatus(testMarkerTime.Add(2 * time.Second))
	st.Loaded = true
	tb.trackRequests(&st, at12)
	f, _ = os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("                       pages, 15 checkpoints, 9.5 of 50.0 GB\n")
	f.Close()
	st = upStatus(testMarkerTime.Add(2 * time.Second))
	st.Loaded = true
	tb.trackRequests(&st, at12)
	d = st.Extra["disk"].(map[string]any)
	if d["restored_tokens"] != 25600+2560 || d["saved_tokens"] != 32000 {
		t.Fatalf("after a wrapped summary: %v", d)
	}

	// A warning is shown; a new start forgets the last one's figures.
	f, _ = os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("12:47:05.000 WARNING:  Disk cache tier: generator error, writes disabled for the rest of this session\n")
	f.Close()
	st = upStatus(testMarkerTime.Add(2 * time.Second))
	st.Loaded = true
	tb.trackRequests(&st, at12)
	if d = st.Extra["disk"].(map[string]any); !strings.Contains(fmt.Sprint(d["note"]), "writes disabled") {
		t.Fatalf("note = %v", d["note"])
	}
	writeFile(t, logPath, "===== baton started cmd.exe /c start.bat at 2026-10-20T12:50:00-07:00 =====\n"+
		"12:50:10.000 INFO:     Loading model D:\\tabbyAPI\\models\\Qwen3.8-27B-exl3 (manual GPU split)\n")
	st = upStatus(time.Date(2026, 10, 20, 12, 50, 1, 0, time.FixedZone("", -7*3600)))
	st.Loaded = true
	tb.trackRequests(&st, at12.Add(5*time.Minute))
	d = st.Extra["disk"].(map[string]any)
	if d["active"] != false || d["reusable_tokens"] != nil || d["note"] != nil {
		t.Fatalf("a load without the disk cache: %v", d)
	}
	if tb.runDiskDir != "" || !tb.runDiskOff {
		t.Fatalf("run dir = %q, off = %v", tb.runDiskDir, tb.runDiskOff)
	}
}

func TestTabbyNoDiskCacheNoStatus(t *testing.T) {
	tb, _ := tabbyDir(t, "fake-key", memoryYAML("  sysmem_kv_cache: 1024", "  disk_cache_dir:"))
	st := upStatus(testMarkerTime)
	tb.trackRequests(&st, at12)
	if _, ok := st.Extra["disk"]; ok {
		t.Fatalf("disk status without a disk cache: %v", st.Extra["disk"])
	}
	res, ok := tb.FlushKV(context.Background(), "stop", Status{Loaded: true, procs: []win.Proc{{PID: os.Getpid()}}}, time.Second, nil)
	if ok {
		t.Fatalf("FlushKV without a disk cache: %+v", res)
	}
}

func TestTabbyFlushKV(t *testing.T) {
	root, model := cacheDir(t)
	tb, _ := tabbyDir(t, "fake-key", memoryYAML("  sysmem_kv_cache: 1024", "  disk_cache_dir: '"+root+"'"))
	st := Status{Loaded: true, procs: []win.Proc{{PID: os.Getpid()}}}
	for _, mode := range []string{"unload", "free"} {
		if _, ok := tb.FlushKV(context.Background(), mode, st, time.Second, nil); ok {
			t.Fatalf("mode %s waited: TabbyAPI flushes on unload itself", mode)
		}
	}
	if _, ok := tb.FlushKV(context.Background(), "stop", Status{procs: st.procs}, time.Second, nil); ok {
		t.Fatal("waited with no model loaded")
	}
	if _, ok := tb.FlushKV(context.Background(), "stop", st, 0, nil); ok {
		t.Fatal("waited with kv_flush_wait 0")
	}

	go publish(t, filepath.Join(model, "pages", "0d"), "0d01.kv", 2<<20)
	res, ok := tb.FlushKV(context.Background(), "stop", st, 10*time.Second, nil)
	if !ok || res.TimedOut {
		t.Fatalf("FlushKV = %+v, %v", res, ok)
	}
	if tb.lastFlush == nil || tb.lastFlush.At.IsZero() {
		t.Fatal("last flush not kept")
	}
	// It shows on the status, also once TabbyAPI is stopped.
	stopped := Status{}
	tb.trackRequests(&stopped, at12)
	d, _ := stopped.Extra["disk"].(map[string]any)
	if d == nil || d["last_flush"] == nil || d["active"] != nil {
		t.Fatalf("stopped status: %v", d)
	}
}
