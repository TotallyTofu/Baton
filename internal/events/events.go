// Package events is baton's log of what it did and why: a ring in memory for
// the UI and an append-only JSON-lines file on disk.
package events

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Level string

const (
	Info Level = "info"
	Warn Level = "warn"
	Err  Level = "error"
)

type Event struct {
	Seq    int64     `json:"seq"`
	At     time.Time `json:"at"`
	Level  Level     `json:"level"`
	Kind   string    `json:"kind"`             // mode, switch, release, start, game, stream, gate, gateway, error
	Engine string    `json:"engine,omitempty"` // engine it concerns
	Msg    string    `json:"msg"`
}

type Log struct {
	mu   sync.Mutex
	seq  int64
	ring []Event
	max  int
	file *os.File
	path string
	subs map[chan Event]struct{}
}

const rotateAt = 8 << 20

func Open(dir string) *Log {
	l := &Log{max: 500, subs: map[chan Event]struct{}{}}
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
		l.path = filepath.Join(dir, "events.jsonl")
		l.file, _ = os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		l.seq = time.Now().Unix() * 1000 // keeps sequence numbers rising across restarts
	}
	return l
}

func (l *Log) Add(level Level, kind, engine, format string, args ...any) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	// The same message again within half a minute is noise (a client
	// retrying); keep the first.
	for i := len(l.ring) - 1; i >= 0 && i >= len(l.ring)-20; i-- {
		p := l.ring[i]
		if time.Since(p.At) > 30*time.Second {
			break
		}
		if p.Msg == msg && p.Kind == kind && p.Engine == engine {
			return p
		}
	}
	l.seq++
	e := Event{Seq: l.seq, At: time.Now(), Level: level, Kind: kind, Engine: engine, Msg: msg}
	l.ring = append(l.ring, e)
	if len(l.ring) > l.max {
		l.ring = append([]Event(nil), l.ring[len(l.ring)-l.max:]...)
	}
	if l.file != nil {
		b, _ := json.Marshal(e)
		_, _ = l.file.Write(append(b, '\n'))
		if st, err := l.file.Stat(); err == nil && st.Size() > rotateAt {
			l.file.Close()
			_ = os.Rename(l.path, l.path+".1")
			l.file, _ = os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		}
	}
	for ch := range l.subs {
		select {
		case ch <- e:
		default:
		}
	}
	return e
}

func (l *Log) Infof(kind, engine, format string, args ...any) {
	l.Add(Info, kind, engine, format, args...)
}

func (l *Log) Warnf(kind, engine, format string, args ...any) {
	l.Add(Warn, kind, engine, format, args...)
}

func (l *Log) Errorf(kind, engine, format string, args ...any) {
	l.Add(Err, kind, engine, format, args...)
}

// Since returns events after seq, oldest first.
func (l *Log) Since(seq int64, limit int) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, e := range l.ring {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe streams new events until cancel is called.
func (l *Log) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		delete(l.subs, ch)
		l.mu.Unlock()
	}
}
