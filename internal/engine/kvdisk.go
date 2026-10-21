package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TabbyAPI on ExLlamaV3 1.6.2 or later can keep its KV cache on disk across
// restarts (memory.disk_cache_dir). Each page and recurrent checkpoint is
// written to "<name>.<pid>.tmp" and renamed into place, so a kill mid-write
// loses only what was not written yet. Its idle flush starts when the last
// request ends; before a release that kills it, baton waits for that flush
// to settle.

// KVFlusher is an engine that writes its KV cache to disk by itself.
type KVFlusher interface {
	// FlushKV waits, at most max, until the engine has finished writing its
	// KV cache before a release in mode. ok is false when there was nothing
	// to wait for: no disk cache, no model loaded, or a mode that unloads
	// through the engine's API (which writes the cache out itself).
	FlushKV(ctx context.Context, mode string, st Status, max time.Duration, progress func(string)) (res FlushResult, ok bool)
}

// FlushResult is how a wait for the disk cache went.
type FlushResult struct {
	At       time.Time `json:"at"`
	Secs     float64   `json:"secs"`      // how long baton waited
	BusySecs float64   `json:"busy_secs"` // how long the cache kept changing
	WroteMiB float64   `json:"wrote_mib"` // pages and checkpoints written meanwhile
	Files    int       `json:"files"`     // how many
	TimedOut bool      `json:"timed_out,omitempty"`
	Writing  int       `json:"writing,omitempty"` // writes still open when it gave up
}

const (
	kvQuiet    = 2 * time.Second        // no change for this long: the flush is over
	kvPoll     = 250 * time.Millisecond // how often the directory is looked at
	kvTmpFresh = 10 * time.Second       // a temp file of no known writer this recent is still being written
	kvUsageTTL = 30 * time.Second       // how long a measured directory size is reused
)

// kvDisk is TabbyAPI's disk cache setting (memory: in config.yml).
type kvDisk struct {
	Dir       string // absolute; "" when the disk cache is off
	BudgetMiB int    // disk_cache_size: for the whole directory
	Note      string // why a configured disk cache is off
}

// readKVDisk reads the memory: settings of TabbyAPI's config.yml in dir.
// Relative paths are resolved against dir, where TabbyAPI runs.
func readKVDisk(dir string) kvDisk {
	if dir == "" {
		return kvDisk{}
	}
	m := readYAMLSection(filepath.Join(dir, "config.yml"), "memory")
	raw := yamlString(m["disk_cache_dir"])
	if raw == "" {
		return kvDisk{}
	}
	k := kvDisk{BudgetMiB: 51200}
	if v, err := strconv.Atoi(yamlString(m["disk_cache_size"])); err == nil && v > 0 {
		k.BudgetMiB = v
	}
	// TabbyAPI ignores the disk cache, with a warning, when there is no
	// system-memory tier to write through from.
	if v, _ := strconv.Atoi(yamlString(m["sysmem_kv_cache"])); v <= 0 {
		k.Note = "disk_cache_dir is set but sysmem_kv_cache is 0, so TabbyAPI runs without the disk cache"
		return k
	}
	k.Dir = resolvePath(raw, dir)
	return k
}

// resolvePath makes a path from TabbyAPI's config absolute the way TabbyAPI
// (pathlib, expanduser) would from its working directory base.
func resolvePath(p, base string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	switch {
	case filepath.IsAbs(p):
	case filepath.VolumeName(p) == "" && (strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/")):
		p = filepath.VolumeName(base) + p // rooted on the working directory's drive
	default:
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

// readYAMLSection reads the scalar values inside a top-level section of a
// YAML file, raw: quotes kept, for yamlString to decode.
func readYAMLSection(path, section string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	in := false
	text := strings.TrimPrefix(string(b), "\xef\xbb\xbf")
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			in = strings.TrimSuffix(trim, ":") == section
			continue
		}
		if in {
			if k, v, ok := strings.Cut(trim, ":"); ok {
				out[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

// yamlString decodes a YAML scalar: quoted ('...' or "..." with escapes) or
// plain, with a trailing comment dropped. Null reads as "".
func yamlString(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	switch v[0] {
	case '\'':
		var sb strings.Builder
		for i := 1; i < len(v); i++ {
			if v[i] == '\'' {
				if i+1 < len(v) && v[i+1] == '\'' {
					sb.WriteByte('\'')
					i++
					continue
				}
				break
			}
			sb.WriteByte(v[i])
		}
		return sb.String()
	case '"':
		var sb strings.Builder
		for i := 1; i < len(v); i++ {
			c := v[i]
			if c == '"' {
				break
			}
			if c == '\\' && i+1 < len(v) {
				i++
				switch v[i] {
				case 't':
					sb.WriteByte('\t')
				case 'n':
					sb.WriteByte('\n')
				case '\\', '"', '/':
					sb.WriteByte(v[i])
				default:
					sb.WriteByte('\\')
					sb.WriteByte(v[i])
				}
				continue
			}
			sb.WriteByte(c)
		}
		return sb.String()
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	switch v {
	case "~", "null", "Null", "NULL":
		return ""
	}
	return v
}

// kvScan is one look at a disk cache directory.
type kvScan struct {
	files    int       // published files: pages, checkpoints, index, manifest
	bytes    int64     // their size
	newest   time.Time // the latest change among them
	writing  int       // temp files a live writer is filling
	newFiles int       // pages and checkpoints written since the wait began
	newBytes int64
}

func (a kvScan) same(b kvScan) bool {
	return a.files == b.files && a.bytes == b.bytes && a.newest.Equal(b.newest) && a.writing == b.writing
}

// scanKVDir looks at every file under dir. A temp file counts as a write in
// progress when its writer (the PID in its name) is one of pids, or when it
// changed in the last few seconds; a stale one an earlier, killed TabbyAPI
// left behind does not. Pages and checkpoints changed at or after since
// count as written during the wait.
func scanKVDir(dir string, pids map[int]bool, since, now time.Time) kvScan {
	var s kvScan
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil // renamed or removed meanwhile
		}
		name := d.Name()
		if pid, ok := tmpPID(name); ok {
			if pids[pid] || now.Sub(fi.ModTime()) < kvTmpFresh {
				s.writing++
			}
			return nil
		}
		s.files++
		s.bytes += fi.Size()
		if fi.ModTime().After(s.newest) {
			s.newest = fi.ModTime()
		}
		if (strings.HasSuffix(name, ".kv") || strings.HasSuffix(name, ".rs")) && !fi.ModTime().Before(since) {
			s.newFiles++
			s.newBytes += fi.Size()
		}
		return nil
	})
	return s
}

// tmpPID tells whether name is one of the disk cache's temp files
// ("<hash>.kv.<pid>.tmp", or a rank marker "<...>.tmp.d0") and the PID of
// the process writing it; -1 when the name carries none.
func tmpPID(name string) (int, bool) {
	i := strings.Index(name, ".tmp")
	if i < 0 {
		return 0, false
	}
	head := name[:i]
	pid, err := strconv.Atoi(head[strings.LastIndexByte(head, '.')+1:])
	if err != nil {
		return -1, true
	}
	return pid, true
}

// waitFlushed waits until nothing under dir has changed for quiet and no
// write is open, or max passes, or ctx ends. The files' own times count as
// changes too, so a cache that settled before the wait returns at once.
func waitFlushed(ctx context.Context, dir string, pids []int, quiet, max, poll time.Duration, progress func(string)) FlushResult {
	live := map[int]bool{}
	for _, p := range pids {
		live[p] = true
	}
	start := time.Now()
	last := scanKVDir(dir, live, start, start)
	changed := last.newest // the last change, by file time or as seen
	if changed.After(start) {
		changed = start
	}
	var seen time.Time // the last change the wait itself saw
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for cur := last; ; {
		now := time.Now()
		if !cur.same(last) {
			changed, seen = now, now
		}
		last = cur
		busy := 0.0
		if !seen.IsZero() {
			busy = round1(seen.Sub(start).Seconds())
		}
		res := FlushResult{
			At: now, Secs: round1(now.Sub(start).Seconds()), BusySecs: busy,
			WroteMiB: round1(float64(cur.newBytes) / (1 << 20)), Files: cur.newFiles,
		}
		switch {
		case ctx.Err() != nil:
			return res
		case cur.writing == 0 && now.Sub(changed) >= quiet:
			return res
		case now.Sub(start) >= max:
			res.TimedOut, res.Writing = true, cur.writing
			return res
		}
		if progress != nil {
			msg := fmt.Sprintf("%.0f MB written", res.WroteMiB)
			if cur.writing > 0 {
				msg += fmt.Sprintf(", %d write(s) open", cur.writing)
			}
			progress(msg)
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
		cur = scanKVDir(dir, live, start, time.Now())
	}
}

// dirUsage sums the size of every file under dir.
func dirUsage(dir string) (int64, bool) {
	if _, err := os.Stat(dir); err != nil {
		return 0, false
	}
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n, true
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
