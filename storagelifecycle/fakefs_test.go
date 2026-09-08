package storagelifecycle

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// fakeStoredEntry is one committed filesystem entry the fake FS knows
// about - deliberately a much smaller model than a real filesystem
// (enough to exercise every decision this package makes, nothing more).
type fakeStoredEntry struct {
	isDir     bool
	isSymlink bool
	isOther   bool
	mode      os.FileMode
	content   []byte
	modTime   time.Time
}

// fakeFS is a complete in-memory FS with per-call fault injection, used
// by every test in this package instead of a real temp directory - see
// this package's mission requirement that tests never touch a real
// filesystem for fault-injection scenarios (temp-dir-based tests are used
// separately, only where exercising real os-level atomicity/durability
// behavior is the point - see atomicwrite_realfs_test.go).
type fakeFS struct {
	mu      sync.Mutex
	entries map[string]*fakeStoredEntry
	clock   *fakeClock // for ModTime stamping on write, if set

	failLstat       map[string]error
	failReadDir     map[string]error
	failCreate      map[string]error
	failRename      map[string]error
	failRemove      map[string]error
	failSyncDir     map[string]error
	nextHandleFault handleFault
}

type handleFault struct {
	write, chmod, sync, close error
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		entries:     make(map[string]*fakeStoredEntry),
		failLstat:   make(map[string]error),
		failReadDir: make(map[string]error),
		failCreate:  make(map[string]error),
		failRename:  make(map[string]error),
		failRemove:  make(map[string]error),
		failSyncDir: make(map[string]error),
	}
}

func (f *fakeFS) mkdir(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[path] = &fakeStoredEntry{isDir: true, mode: 0o755, modTime: time.Now()}
}

func (f *fakeFS) putFile(path string, content []byte, mode os.FileMode, modTime time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[path] = &fakeStoredEntry{content: append([]byte(nil), content...), mode: mode, modTime: modTime}
}

func (f *fakeFS) putSymlink(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[path] = &fakeStoredEntry{isSymlink: true, modTime: time.Now()}
}

func (f *fakeFS) putOther(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[path] = &fakeStoredEntry{isOther: true, modTime: time.Now()}
}

func (f *fakeFS) exists(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.entries[path]
	return ok
}

func (f *fakeFS) content(path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.entries[path]
	if !ok {
		return nil
	}
	return append([]byte(nil), e.content...)
}

func baseName(path string) string {
	i := strings.LastIndexByte(path, '/')
	if i < 0 {
		return path
	}
	return path[i+1:]
}

func (f *fakeFS) Lstat(path string) (DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failLstat[path]; ok {
		return DirEntry{}, err
	}
	e, ok := f.entries[path]
	if !ok {
		return DirEntry{}, fmt.Errorf("%s: %w", path, os.ErrNotExist)
	}
	return entryToDirEntry(baseName(path), e), nil
}

func entryToDirEntry(name string, e *fakeStoredEntry) DirEntry {
	return DirEntry{
		Name:      name,
		IsDir:     e.isDir,
		IsRegular: !e.isDir && !e.isSymlink && !e.isOther,
		IsSymlink: e.isSymlink,
		IsOther:   e.isOther,
		Size:      int64(len(e.content)),
		ModTime:   e.modTime,
		Mode:      e.mode,
	}
}

func (f *fakeFS) ReadDir(path string) ([]DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failReadDir[path]; ok {
		return nil, err
	}
	prefix := path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var out []DirEntry
	for p, e := range f.entries {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if strings.Contains(rest, "/") {
			continue // not a direct child
		}
		out = append(out, entryToDirEntry(rest, e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeFS) CreateExclusive(path string, mode os.FileMode) (FileHandle, error) {
	f.mu.Lock()
	if err, ok := f.failCreate[path]; ok {
		f.mu.Unlock()
		return nil, err
	}
	if _, exists := f.entries[path]; exists {
		f.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", path, os.ErrExist)
	}
	fault := f.nextHandleFault
	f.nextHandleFault = handleFault{}
	f.mu.Unlock()
	return &fakeFileHandle{fs: f, path: path, mode: mode, fault: fault}, nil
}

func (f *fakeFS) Rename(oldpath, newpath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failRename[oldpath]; ok {
		return err
	}
	e, ok := f.entries[oldpath]
	if !ok {
		return fmt.Errorf("rename %s: %w", oldpath, os.ErrNotExist)
	}
	delete(f.entries, oldpath)
	f.entries[newpath] = e
	return nil
}

func (f *fakeFS) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failRemove[path]; ok {
		return err
	}
	if _, ok := f.entries[path]; !ok {
		return fmt.Errorf("remove %s: %w", path, os.ErrNotExist)
	}
	delete(f.entries, path)
	return nil
}

func (f *fakeFS) SyncDir(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failSyncDir[path]; ok {
		return err
	}
	return nil
}

func (f *fakeFS) MkdirAll(path string, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[path] = &fakeStoredEntry{isDir: true, mode: mode}
	return nil
}

// fakeFileHandle is one in-flight write's handle - it does not commit
// anything into fakeFS.entries until CreateExclusive's caller (this
// package's AtomicWriter) later calls fs.Rename on its path, exactly
// mirroring how a real temp file is invisible under its final name until
// renamed.
type fakeFileHandle struct {
	fs       *fakeFS
	path     string
	mode     os.FileMode
	buf      []byte
	fault    handleFault
	closed   bool
	comitted bool
}

func (h *fakeFileHandle) Write(p []byte) (int, error) {
	if h.fault.write != nil {
		return 0, h.fault.write
	}
	h.buf = append(h.buf, p...)
	return len(p), nil
}

func (h *fakeFileHandle) Chmod(mode os.FileMode) error {
	if h.fault.chmod != nil {
		return h.fault.chmod
	}
	h.mode = mode
	return nil
}

func (h *fakeFileHandle) Sync() error {
	if h.fault.sync != nil {
		return h.fault.sync
	}
	// Committing on Sync (not Close) mirrors a real file: the content is
	// durable to a reader that opens the path directly (which this fake
	// does not otherwise support pre-rename anyway) once fsync'd.
	h.fs.mu.Lock()
	h.fs.entries[h.path] = &fakeStoredEntry{content: append([]byte(nil), h.buf...), mode: h.mode, modTime: time.Now()}
	h.fs.mu.Unlock()
	h.comitted = true
	return nil
}

func (h *fakeFileHandle) Close() error {
	if h.fault.close != nil {
		return h.fault.close
	}
	if !h.comitted {
		// A real os.File makes its written bytes visible to anyone
		// re-opening the path even without an explicit Sync (the data is
		// in the kernel page cache) - mirror that so a test forcing a
		// failure AFTER Write but with no fault on Sync/Close still sees
		// the temp file "exist" for a subsequent Lstat/ReadDir check.
		h.fs.mu.Lock()
		h.fs.entries[h.path] = &fakeStoredEntry{content: append([]byte(nil), h.buf...), mode: h.mode, modTime: time.Now()}
		h.fs.mu.Unlock()
	}
	h.closed = true
	return nil
}

// fakeClock is a settable Clock for deterministic monotonic-time tests.
type fakeClock struct {
	mu  sync.Mutex
	now float64
}

func (c *fakeClock) Now() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t float64) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *fakeClock) Advance(d float64) {
	c.mu.Lock()
	c.now += d
	c.mu.Unlock()
}
