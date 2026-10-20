// Package games keeps the game list you edit in the dashboard: library
// folders, single programs, and things that never count. It lives in
// games.json beside baton.toml and applies without a restart.
package games

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Path  string    `json:"path"`
	Added time.Time `json:"added"`
}

type Data struct {
	Builtin *bool   `json:"builtin,omitempty"` // nil: follow baton.toml's detect.games
	Folders []Entry `json:"folders"`
	Games   []Entry `json:"games"`
	Ignore  []Entry `json:"ignore"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data Data
	subs []func(Data)
}

func Open(path string) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

// OnChange registers fn to run with the new list after every edit.
func (s *Store) OnChange(fn func(Data)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func (s *Store) Data() Data {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.copyLocked()
}

func (s *Store) copyLocked() Data {
	d := Data{Builtin: s.data.Builtin}
	d.Folders = append([]Entry{}, s.data.Folders...)
	d.Games = append([]Entry{}, s.data.Games...)
	d.Ignore = append([]Entry{}, s.data.Ignore...)
	return d
}

// Clean normalizes a path the way entries are stored and compared.
func Clean(p string) string {
	p = strings.Trim(strings.TrimSpace(p), `"`)
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if len(p) > 3 {
		p = strings.TrimRight(p, `\`)
	}
	return p
}

func (s *Store) list(kind string) (*[]Entry, error) {
	switch kind {
	case "folder":
		return &s.data.Folders, nil
	case "game":
		return &s.data.Games, nil
	case "ignore":
		return &s.data.Ignore, nil
	}
	return nil, fmt.Errorf("unknown list %q", kind)
}

// Add puts path on a list. kind "auto" picks folder or game by what the path
// is on disk. It returns the list the path went to.
func (s *Store) Add(kind, path string) (string, error) {
	p := Clean(path)
	if p == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%s is not a full path (it should start with a drive, like C:\\)", p)
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("%s does not exist", p)
	}
	switch kind {
	case "auto":
		kind = "game"
		if st.IsDir() {
			kind = "folder"
		}
	case "folder":
		if !st.IsDir() {
			return "", fmt.Errorf("%s is a file, not a folder", p)
		}
	case "game":
		if st.IsDir() {
			return "", fmt.Errorf("%s is a folder; add it as a library folder instead", p)
		}
	}
	if kind == "game" && !strings.EqualFold(filepath.Ext(p), ".exe") {
		return "", fmt.Errorf("%s is not a program (.exe)", filepath.Base(p))
	}
	s.mu.Lock()
	l, err := s.list(kind)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	for _, e := range *l {
		if strings.EqualFold(e.Path, p) {
			s.mu.Unlock()
			return kind, nil
		}
	}
	*l = append(*l, Entry{Path: p, Added: time.Now()})
	sort.SliceStable(*l, func(i, j int) bool { return strings.ToLower((*l)[i].Path) < strings.ToLower((*l)[j].Path) })
	return kind, s.commitLocked()
}

func (s *Store) Remove(kind, path string) error {
	p := Clean(path)
	s.mu.Lock()
	l, err := s.list(kind)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	out := (*l)[:0]
	for _, e := range *l {
		if !strings.EqualFold(e.Path, p) {
			out = append(out, e)
		}
	}
	*l = out
	return s.commitLocked()
}

func (s *Store) SetBuiltin(on bool) error {
	s.mu.Lock()
	s.data.Builtin = &on
	return s.commitLocked()
}

// commitLocked saves and notifies; it releases s.mu.
func (s *Store) commitLocked() error {
	d := s.copyLocked()
	subs := append([]func(Data){}, s.subs...)
	b, _ := json.MarshalIndent(d, "", "  ")
	tmp := s.path + ".tmp"
	err := os.WriteFile(tmp, append(b, '\n'), 0o644)
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	s.mu.Unlock()
	for _, fn := range subs {
		fn(d)
	}
	return err
}

// --- matching ---

func under(path, dir string) bool {
	lp, ld := strings.ToLower(path), strings.ToLower(dir)
	if !strings.HasSuffix(ld, `\`) {
		ld += `\`
	}
	return strings.HasPrefix(lp, ld)
}

// Ignored reports whether a process path is on the never-count list.
func (d Data) Ignored(path string) bool {
	for _, e := range d.Ignore {
		if strings.EqualFold(e.Path, path) || under(path, e.Path) {
			return true
		}
	}
	return false
}

// Classify decides a process path: a single game you added always counts,
// then the never-count list, then library folders.
func (d Data) Classify(path string) (rule, match string, ignored bool) {
	for _, e := range d.Games {
		if strings.EqualFold(e.Path, path) {
			return "added", e.Path, false
		}
	}
	if d.Ignored(path) {
		return "", "", true
	}
	if r, m, ok := d.Match(path); ok {
		return r, m, false
	}
	return "", "", false
}

// Match returns the folder or game entry that makes path a game.
func (d Data) Match(path string) (rule, match string, ok bool) {
	for _, e := range d.Games {
		if strings.EqualFold(e.Path, path) {
			return "added", e.Path, true
		}
	}
	for _, e := range d.Folders {
		if under(path, e.Path) {
			return "library", e.Path, true
		}
	}
	return "", "", false
}
