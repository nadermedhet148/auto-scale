package memory

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

const journalFile = "journal.jsonl"

// Store keeps dockscale's memory in a directory:
//
//	journal.jsonl        append-only decisions and outcomes
//	state-<service>.json latest learned state, replaced atomically
type Store struct {
	dir string
	mu  sync.Mutex
	j   *os.File
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	j, err := os.OpenFile(filepath.Join(dir, journalFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Store{dir: dir, j: j}, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Append(r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.j.Write(append(b, '\n'))
	return err
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.j.Close()
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func (s *Store) statePath(service string) string {
	return filepath.Join(s.dir, "state-"+unsafeChars.ReplaceAllString(service, "_")+".json")
}

// SaveState writes v as the service's state. It writes a temp file and
// renames it, so a crash never leaves a half-written state behind.
func (s *Store) SaveState(service string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	path := s.statePath(service)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return renameRetry(tmp, path)
}

// renameRetry renames with a few short retries. On Windows, replacing a file
// fails with "Access is denied" while another process (antivirus, the search
// indexer) briefly holds the target open; on Linux the first attempt succeeds.
func renameRetry(from, to string) error {
	var err error
	for i := range 6 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		time.Sleep(time.Duration(5<<i) * time.Millisecond) // 5ms … 160ms
	}
	return err
}

// LoadState reads the service's state into v. found is false if none exists.
func (s *Store) LoadState(service string, v any) (found bool, err error) {
	b, err := os.ReadFile(s.statePath(service))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("corrupt state for %s: %w", service, err)
	}
	return true, nil
}

// ReadJournal returns every journal record in dir, oldest first. A torn last
// line (from a crash mid-write) is skipped.
func ReadJournal(dir string) ([]Record, error) {
	f, err := os.Open(filepath.Join(dir, journalFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Type != "" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}
