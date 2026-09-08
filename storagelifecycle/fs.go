package storagelifecycle

import (
	"io"
	"os"
	"time"
)

// DirEntry is the subset of Lstat/ReadDir information this package's
// pure logic needs - deliberately not os.DirEntry/os.FileInfo directly,
// so a test's fake FS can construct one without touching a real
// filesystem at all.
type DirEntry struct {
	Name      string
	IsDir     bool
	IsRegular bool
	IsSymlink bool
	// IsOther is true for anything that is none of the above (FIFO,
	// socket, device file, etc.) - see inventory.go, which reports these
	// as unmanaged rather than erroring the whole scan.
	IsOther bool
	Size    int64
	ModTime time.Time
	Mode    os.FileMode
}

// FileHandle is the subset of *os.File this package's atomic writer
// needs, as an interface so a test can fault-inject at each step.
type FileHandle interface {
	io.Writer
	Chmod(mode os.FileMode) error
	Sync() error
	Close() error
}

// FS is every filesystem operation this package performs, as an
// interface so every pure decision (inventory, planning, atomic write
// sequencing, recovery) can be exercised against a fake in a temp-dir-free
// unit test, with precise fault injection at any single step. The real
// implementation (osFS) is a thin, direct wrapper around the os package -
// see NewOSFS.
type FS interface {
	// Lstat reports on path without following a trailing symlink -
	// callers must never treat an IsSymlink result as safe to open.
	// Returns os.ErrNotExist (wrapped) if path does not exist.
	Lstat(path string) (DirEntry, error)
	// ReadDir lists path's immediate children (not recursive), Lstat
	// semantics per entry (never resolves a symlink child). Deterministic
	// order (lexical by name) - callers must not rely on any other order
	// from the filesystem itself.
	ReadDir(path string) ([]DirEntry, error)
	// CreateExclusive creates a new file at path, failing if anything
	// already exists there (O_CREATE|O_EXCL) and never following a
	// symlink at that path (O_NOFOLLOW where supported) - the atomic
	// writer's temp-file-creation step, and the only way this package
	// ever creates a new file.
	CreateExclusive(path string, mode os.FileMode) (FileHandle, error)
	// Rename atomically replaces newpath with oldpath - both must be on
	// the same filesystem for this to be atomic, which is this package's
	// own responsibility to guarantee (same-directory temp file), not
	// this interface's.
	Rename(oldpath, newpath string) error
	// Remove deletes path (must be a regular file - this package never
	// asks FS to remove a directory).
	Remove(path string) error
	// SyncDir opens path (a directory) and fsyncs it, so a rename into
	// that directory is durable before this package reports success.
	SyncDir(path string) error
	// MkdirAll creates path and any missing parents.
	MkdirAll(path string, mode os.FileMode) error
}

// osFS is FS's real implementation - a direct, untested-by-choice
// wrapper (every behavior it adds beyond the os package is tested via the
// fake FS in *_test.go files) around real syscalls.
type osFS struct{}

// NewOSFS returns the real, hardware-touching FS implementation. main/'s
// glue is the only production code that should ever call this - every
// test in this package uses a fake instead.
func NewOSFS() FS { return osFS{} }

func toDirEntry(name string, info os.FileInfo) DirEntry {
	mode := info.Mode()
	e := DirEntry{
		Name:    name,
		Size:    info.Size(),
		ModTime: info.ModTime(),
		Mode:    mode,
	}
	switch {
	case mode&os.ModeSymlink != 0:
		e.IsSymlink = true
	case mode.IsDir():
		e.IsDir = true
	case mode.IsRegular():
		e.IsRegular = true
	default:
		e.IsOther = true
	}
	return e
}

func (osFS) Lstat(path string) (DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return DirEntry{}, err
	}
	return toDirEntry(info.Name(), info), nil
}

func (osFS) ReadDir(path string) ([]DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, de := range entries {
		info, err := de.Info()
		if err != nil {
			// A file that disappeared between ReadDir and Info, or a
			// permission error on one entry - reported to the caller as
			// part of the slice's accompanying error handling
			// (inventory.go tolerates this per-entry rather than failing
			// the whole scan); here, at the FS layer, propagate as-is.
			return nil, err
		}
		out = append(out, toDirEntry(de.Name(), info))
	}
	return out, nil
}

func (osFS) CreateExclusive(path string, mode os.FileMode) (FileHandle, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscallNoFollow, mode)
}

func (osFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
func (osFS) Remove(path string) error             { return os.Remove(path) }

func (osFS) SyncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (osFS) MkdirAll(path string, mode os.FileMode) error { return os.MkdirAll(path, mode) }
