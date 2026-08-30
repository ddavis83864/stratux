package recording

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	fileExtension = ".jsonl"
	filePrefix    = "recording-"
)

// Store is an append-only, size-rotated, retention-bounded JSON-Lines
// store for Sample records. It is intentionally simple: one Sample per
// line, human-inspectable, trivially streamable, and independent of the
// existing SQLite-backed traffic/situation log (main/datalog.go) which
// serves a different purpose. This is the storage foundation the mission
// asks for, not a claim that this is the final format flight recording
// will ship with.
type Store struct {
	dir          string
	maxFileBytes int64
	maxFiles     int

	mu           sync.Mutex
	current      *os.File
	currentBytes int64
}

// NewStore opens (creating if necessary) a Store rooted at dir. A new file
// is started immediately; Append rotates to a new file once the current
// one reaches maxFileBytes, and prunes the oldest files beyond maxFiles.
func NewStore(dir string, maxFileBytes int64, maxFiles int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("could not create recording directory: %w", err)
	}
	s := &Store{dir: dir, maxFileBytes: maxFileBytes, maxFiles: maxFiles}
	if err := s.rotate(time.Now()); err != nil {
		return nil, err
	}
	return s, nil
}

// Append writes one Sample as a JSON line and rotates/prunes as needed.
func (s *Store) Append(sample Sample) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := json.Marshal(&sample)
	if err != nil {
		return fmt.Errorf("could not marshal sample: %w", err)
	}
	data = append(data, '\n')

	if s.maxFileBytes > 0 && s.currentBytes+int64(len(data)) > s.maxFileBytes {
		if err := s.rotateLocked(sample.UTC); err != nil {
			return err
		}
	}
	n, err := s.current.Write(data)
	s.currentBytes += int64(n)
	if err != nil {
		return fmt.Errorf("could not write sample: %w", err)
	}
	return nil
}

func (s *Store) rotate(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rotateLocked(now)
}

func (s *Store) rotateLocked(now time.Time) error {
	if s.current != nil {
		if err := s.current.Close(); err != nil {
			return fmt.Errorf("could not close previous recording file: %w", err)
		}
	}
	name := fmt.Sprintf("%s%s%s", filePrefix, now.UTC().Format("20060102T150405.000000000Z"), fileExtension)
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("could not create recording file: %w", err)
	}
	s.current = f
	s.currentBytes = 0
	return s.pruneLocked()
}

func (s *Store) pruneLocked() error {
	if s.maxFiles <= 0 {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, filePrefix) && strings.HasSuffix(n, fileExtension) {
			names = append(names, n)
		}
	}
	sort.Strings(names) // filenames embed a sortable UTC timestamp
	if len(names) <= s.maxFiles {
		return nil
	}
	for _, n := range names[:len(names)-s.maxFiles] {
		if err := os.Remove(filepath.Join(s.dir, n)); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the currently-open recording file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return nil
	}
	return s.current.Close()
}

// ReadAll reads every Sample from every recording file currently retained
// in dir, in chronological order (filenames sort chronologically). It
// exists for the export interfaces (CSVExporter etc.) and for tests; it is
// not intended for use on an unbounded amount of retained data.
func ReadAll(dir string) ([]Sample, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && strings.HasPrefix(n, filePrefix) && strings.HasSuffix(n, fileExtension) {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var samples []Sample
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var s Sample
			if err := json.Unmarshal(line, &s); err != nil {
				f.Close()
				return nil, fmt.Errorf("malformed record in %s: %w", n, err)
			}
			samples = append(samples, s)
		}
		scanErr := scanner.Err()
		f.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("error reading %s: %w", n, scanErr)
		}
	}
	return samples, nil
}
