/*
fisbcachecapturepath_test.go: proves two things this mission's own safety
review specifically demanded proof for, not merely documentation of:

 1. TestFISBCacheEnqueue_NeverPerformsFilesystemIO - the live capture path
    (fisbCaptureText/fisbCaptureNexrad -> fisbCacheEnqueue ->
    reserveAndEnqueue, main/fisbcachereserve.go) performs ZERO filesystem
    I/O, for every reservation outcome (admit, same-key supersession,
    capacity rejection) - see fisbcachereserve.go's own file-level doc
    comment.
 2. Fault-injection coverage for every disk-mutating step this feature's
    write/delete paths can fail at: write, short write, fsync, rename,
    directory sync, and deletion - proving each failure is reported
    honestly and never corrupts or falsely claims success.

fisbFaultFS wraps a REAL storagelifecycle.FS (backed by an actual temp
directory via storagelifecycle.NewOSFS - see withTestFISBCacheStorage)
with a thin, per-call fault-injection decorator, rather than
reimplementing a full in-memory fake filesystem: every operation this
feature's write path performs (CreateExclusive, Write, Chmod, Sync,
Close, Rename, SyncDir, Remove, ReadDir, Lstat, MkdirAll) still really
happens against real files unless a specific fault is configured for it,
so a short write, for instance, genuinely leaves a genuinely truncated
file on a real filesystem for the real validate-before-rename step to
genuinely catch - not a simulation of that step.
*/
package main

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
)

// fisbFaultFS decorates a real storagelifecycle.FS with configurable,
// per-method fault injection. Safe for concurrent use (a mutex guards
// every field) since this feature's own real cleanup worker and capture
// path can genuinely call this concurrently in a test that exercises
// both.
type fisbFaultFS struct {
	real storagelifecycle.FS

	mu                                                                  sync.Mutex
	failLstat, failReadDir, failCreateExclusive, failRename, failRemove error
	failSyncDir, failMkdirAll                                           error
	shortWriteBytes                                                     int // >0: the NEXT CreateExclusive's handle silently truncates writes to this many bytes but reports success
	failHandleWrite, failHandleChmod, failHandleSync, failHandleClose   error

	// trap, when set, fails t for ANY call to ANY method below - used by
	// TestFISBCacheEnqueue_NeverPerformsFilesystemIO to prove the capture
	// path invokes none of them. The underlying real call is never made
	// in trap mode (defense in depth: a capture-path regression that
	// tried to touch disk would hit this stub, not a real, possibly
	// slow or blocking, filesystem operation).
	trap bool
	t    *testing.T

	calls int32 // atomic: total call count across every method, trap mode or not
}

func newFisbFaultFS(real storagelifecycle.FS) *fisbFaultFS {
	return &fisbFaultFS{real: real}
}

// trapped records the call and, in trap mode, fails the test - returning
// true when the caller must stop and return a synthetic error rather
// than touch the real filesystem.
func (f *fisbFaultFS) trapped(method, path string) bool {
	atomic.AddInt32(&f.calls, 1)
	if f.trap {
		f.t.Helper()
		f.t.Errorf("fisbFaultFS: unexpected filesystem call %s(%q) - the live capture path must never perform filesystem I/O", method, path)
		return true
	}
	return false
}

func (f *fisbFaultFS) Lstat(path string) (storagelifecycle.DirEntry, error) {
	if f.trapped("Lstat", path) {
		return storagelifecycle.DirEntry{}, fmt.Errorf("fisbFaultFS: trapped")
	}
	f.mu.Lock()
	err := f.failLstat
	f.mu.Unlock()
	if err != nil {
		return storagelifecycle.DirEntry{}, err
	}
	return f.real.Lstat(path)
}

func (f *fisbFaultFS) ReadDir(path string) ([]storagelifecycle.DirEntry, error) {
	if f.trapped("ReadDir", path) {
		return nil, fmt.Errorf("fisbFaultFS: trapped")
	}
	f.mu.Lock()
	err := f.failReadDir
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.real.ReadDir(path)
}

func (f *fisbFaultFS) MkdirAll(path string, mode os.FileMode) error {
	if f.trapped("MkdirAll", path) {
		return fmt.Errorf("fisbFaultFS: trapped")
	}
	f.mu.Lock()
	err := f.failMkdirAll
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.real.MkdirAll(path, mode)
}

func (f *fisbFaultFS) CreateExclusive(path string, mode os.FileMode) (storagelifecycle.FileHandle, error) {
	if f.trapped("CreateExclusive", path) {
		return nil, fmt.Errorf("fisbFaultFS: trapped")
	}
	f.mu.Lock()
	err := f.failCreateExclusive
	short := f.shortWriteBytes
	writeErr, chmodErr, syncErr, closeErr := f.failHandleWrite, f.failHandleChmod, f.failHandleSync, f.failHandleClose
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	real, rerr := f.real.CreateExclusive(path, mode)
	if rerr != nil {
		return nil, rerr
	}
	return &fisbFaultFileHandle{
		real: real, shortWriteBytes: short,
		failWrite: writeErr, failChmod: chmodErr, failSync: syncErr, failClose: closeErr,
	}, nil
}

func (f *fisbFaultFS) Rename(oldpath, newpath string) error {
	if f.trapped("Rename", oldpath+" -> "+newpath) {
		return fmt.Errorf("fisbFaultFS: trapped")
	}
	f.mu.Lock()
	err := f.failRename
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.real.Rename(oldpath, newpath)
}

func (f *fisbFaultFS) Remove(path string) error {
	if f.trapped("Remove", path) {
		return fmt.Errorf("fisbFaultFS: trapped")
	}
	f.mu.Lock()
	err := f.failRemove
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.real.Remove(path)
}

func (f *fisbFaultFS) SyncDir(path string) error {
	if f.trapped("SyncDir", path) {
		return fmt.Errorf("fisbFaultFS: trapped")
	}
	f.mu.Lock()
	err := f.failSyncDir
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.real.SyncDir(path)
}

// fisbFaultFileHandle decorates one real, already-created FileHandle -
// only CreateExclusive's own fault configuration (captured at the moment
// the handle was created) governs it, matching the real lifetime of one
// temp-file write.
type fisbFaultFileHandle struct {
	real                                      storagelifecycle.FileHandle
	shortWriteBytes                           int
	failWrite, failChmod, failSync, failClose error
}

// Write, when shortWriteBytes > 0, is this file's one deliberately
// dangerous fault: it writes only shortWriteBytes of p to the REAL
// underlying file but reports success as if all of p were written - the
// most dangerous shape a short write can take (a caller that checked n
// == len(p) would see no problem at all). Exercises exactly the case
// fisbCacheValidateEntryFile's re-decode step exists to catch before any
// rename ever happens - see storagelifecycle.AtomicWriter.Write's own
// doc comment.
func (h *fisbFaultFileHandle) Write(p []byte) (int, error) {
	if h.failWrite != nil {
		return 0, h.failWrite
	}
	if h.shortWriteBytes > 0 {
		n := h.shortWriteBytes
		if n > len(p) {
			n = len(p)
		}
		if _, err := h.real.Write(p[:n]); err != nil {
			return 0, err
		}
		h.shortWriteBytes = 0 // only the first Write call is truncated
		return len(p), nil
	}
	return h.real.Write(p)
}

func (h *fisbFaultFileHandle) Chmod(mode os.FileMode) error {
	if h.failChmod != nil {
		return h.failChmod
	}
	return h.real.Chmod(mode)
}

func (h *fisbFaultFileHandle) Sync() error {
	if h.failSync != nil {
		return h.failSync
	}
	return h.real.Sync()
}

func (h *fisbFaultFileHandle) Close() error {
	if h.failClose != nil {
		// Still close the real handle so the test's own cleanup (temp
		// directory removal) never leaves a dangling open fd - only the
		// caller-observed result is faulty, matching a real close(2)
		// failure's own semantics (the fd is still released).
		_ = h.real.Close()
		return h.failClose
	}
	return h.real.Close()
}

// --- capture-path zero-filesystem-I/O regression ------------------------

// TestFISBCacheEnqueue_NeverPerformsFilesystemIO is Phase 2's explicitly
// named regression test: it fails if ANY filesystem operation is ever
// invoked synchronously from the capture-facing call
// (fisbCaptureText/fisbCaptureNexrad -> fisbCacheEnqueue ->
// reserveAndEnqueue). fisbCacheCaptureWorker is never started - only the
// capture path itself runs, on this test's own goroutine, exactly as it
// would on the live UAT/978 decode goroutine in production.
func TestFISBCacheEnqueue_NeverPerformsFilesystemIO(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	trap := &fisbFaultFS{t: t, trap: true}
	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	// A small budget so this test also exercises the capacity-rejection
	// path (fisbCacheRequestCleanup itself is a pure channel send/atomic
	// increment - no FS call - see fisbCacheRequestCleanup's own doc
	// comment) - not just straightforward admits.
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: 3}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	origFS := trapSwapFISBCacheFS(trap)
	fisbCacheMu.Unlock()
	t.Cleanup(func() {
		fisbCacheMu.Lock()
		fisbCacheFS = origFS
		fisbCacheMu.Unlock()
	})

	for i := 0; i < 10; i++ {
		fisbCaptureText("METAR", fmt.Sprintf("KTST%02d", i), "METAR body", fisbcache.FISBTime{})
	}
	// A same-key supersession - a distinct code path inside
	// fisbPendingQueue.reserveAndEnqueue from a fresh admit.
	fisbCaptureText("METAR", "KTST00", "METAR body v2", fisbcache.FISBTime{})
	// A NEXRAD capture, exercising fisbCaptureNexrad's own, separate
	// call site into the exact same fisbCacheEnqueue.
	fisbCaptureNexrad(63, 1, 40, -80, 100, 100, "nexrad-payload", fisbcache.FISBTime{})

	queued, inFlight, _, _ := fisbCachePending.stats()
	if queued == 0 && inFlight == 0 {
		t.Fatal("test did not actually exercise the capture path - nothing was queued")
	}
	if got := atomic.LoadInt32(&trap.calls); got != 0 {
		t.Errorf("expected zero filesystem calls from the capture path, got %d (see individual t.Errorf calls above for which)", got)
	}
}

// trapSwapFISBCacheFS installs fs as fisbCacheFS and returns the
// previous value - callers must already hold fisbCacheMu (a tiny helper
// only so the swap and its surrounding lock stay visually together at
// each call site).
func trapSwapFISBCacheFS(fs storagelifecycle.FS) storagelifecycle.FS {
	old := fisbCacheFS
	fisbCacheFS = fs
	return old
}

// --- fault injection: the write path -------------------------------------

// withFaultFISBCacheFS installs a fisbFaultFS wrapping the real OSFS
// withTestFISBCacheStorage already set up, for the duration of one test,
// and returns it for the test to configure.
func withFaultFISBCacheFS(t *testing.T) *fisbFaultFS {
	t.Helper()
	fisbCacheMu.Lock()
	real := fisbCacheFS
	fault := newFisbFaultFS(real)
	fisbCacheFS = fault
	fisbCacheAtomicWriter = storagelifecycle.NewAtomicWriter(fault)
	fisbCacheMu.Unlock()
	t.Cleanup(func() {
		fisbCacheMu.Lock()
		fisbCacheFS = real
		fisbCacheAtomicWriter = storagelifecycle.NewAtomicWriter(real)
		fisbCacheMu.Unlock()
	})
	return fault
}

func fisbFaultTestEntry(id string) (fisbcache.Entry, string) {
	e := fisbcache.Entry{Key: makeFISBTestKey(id), ReceivedAtMonotonic: monotonicSeconds()}
	return e, "METAR " + id + " 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"
}

// TestFISBCachePersist_WriteFailureLeavesNoFileAndReportsError proves a
// failure at the very first Write call is reported honestly and leaves
// nothing behind - no partial file at the final path (nothing was ever
// renamed there) and no leaked temp file (AtomicWriter's own deferred
// cleanup removes it).
func TestFISBCachePersist_WriteFailureLeavesNoFileAndReportsError(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	fault := withFaultFISBCacheFS(t)
	fault.failHandleWrite = fmt.Errorf("injected write failure")

	e, payload := fisbFaultTestEntry("KWRITEFAIL")
	if err := fisbCachePersist(e, payload); err == nil {
		t.Fatal("expected fisbCachePersist to report the injected write failure")
	}
	assertNoFISBCacheFilesOnDisk(t, dir)
}

// TestFISBCachePersist_ShortWriteCaughtByValidateNeverRenamed proves the
// EXISTING validate-before-rename safety net
// (fisbCacheValidateEntryFile) catches a short write that lies about
// having succeeded - the write path's own re-decode step, not new code
// this mission adds, but specifically demanded proof by this mission's
// own fault-injection requirement.
func TestFISBCachePersist_ShortWriteCaughtByValidateNeverRenamed(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	fault := withFaultFISBCacheFS(t)
	fault.shortWriteBytes = 4 // far too little to ever be valid JSON

	e, payload := fisbFaultTestEntry("KSHORTWRITE")
	if err := fisbCachePersist(e, payload); err == nil {
		t.Fatal("expected fisbCachePersist to report the short write as a validate failure")
	}
	assertNoFISBCacheFilesOnDisk(t, dir)
}

// TestFISBCachePersist_FsyncFailureLeavesNoFileAndReportsError proves an
// fsync (Sync) failure - after a fully valid write - is still reported
// and still never renamed into place.
func TestFISBCachePersist_FsyncFailureLeavesNoFileAndReportsError(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	fault := withFaultFISBCacheFS(t)
	fault.failHandleSync = fmt.Errorf("injected fsync failure")

	e, payload := fisbFaultTestEntry("KFSYNCFAIL")
	if err := fisbCachePersist(e, payload); err == nil {
		t.Fatal("expected fisbCachePersist to report the injected fsync failure")
	}
	assertNoFISBCacheFilesOnDisk(t, dir)
}

// TestFISBCachePersist_RenameFailureLeavesNoFileAndReportsError proves a
// rename failure is reported and leaves neither a final file (the rename
// never completed) nor a leaked temp file (AtomicWriter's own deferred
// cleanup still runs - Write only marks committed=true AFTER Rename
// succeeds).
func TestFISBCachePersist_RenameFailureLeavesNoFileAndReportsError(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	fault := withFaultFISBCacheFS(t)
	fault.failRename = fmt.Errorf("injected rename failure")

	e, payload := fisbFaultTestEntry("KRENAMEFAIL")
	if err := fisbCachePersist(e, payload); err == nil {
		t.Fatal("expected fisbCachePersist to report the injected rename failure")
	}
	assertNoFISBCacheFilesOnDisk(t, dir)
}

// TestFISBCachePersist_DirectorySyncFailureStillReportsErrorButFileIsWritten
// proves the one case where "an error was returned" does NOT mean
// "nothing was written": SyncDir runs AFTER Rename already succeeded, so
// the content is genuinely on disk under its final name even though this
// call reports failure (the rename's own durability is what is in
// question, not the content) - see AtomicWriter.Write's own doc comment
// for why this is the one deliberate exception to "error means
// untouched."
func TestFISBCachePersist_DirectorySyncFailureStillReportsErrorButFileIsWritten(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	fault := withFaultFISBCacheFS(t)
	fault.failSyncDir = fmt.Errorf("injected directory sync failure")

	e, payload := fisbFaultTestEntry("KDIRSYNCFAIL")
	if err := fisbCachePersist(e, payload); err == nil {
		t.Fatal("expected fisbCachePersist to report the injected directory sync failure")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected the file to have actually been written despite the reported directory-sync error, got %d files", len(entries))
	}
}

// TestFISBCacheEvictKeyIfUnchanged_DeletionFailureIsNeverCountedAsSuccess
// proves a Remove failure leaves accounting conservative: the Store
// entry survives untouched, and the caller receives (false, non-nil
// error) rather than a false claim of deletion - see
// fisbCacheEvictKeyIfUnchanged's own doc comment for why any error other
// than os.IsNotExist must refuse to touch the Store.
func TestFISBCacheEvictKeyIfUnchanged_DeletionFailureIsNeverCountedAsSuccess(t *testing.T) {
	withTestFISBCacheStorage(t)
	fault := withFaultFISBCacheFS(t)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheMu.Unlock()

	e, payload := fisbFaultTestEntry("KDELETEFAIL")
	if err := fisbCachePersist(e, payload); err != nil {
		t.Fatalf("test precondition failed: could not persist: %v", err)
	}
	if fisbCacheStore.Admit(e) != fisbcache.AdmitAccepted {
		t.Fatal("test precondition failed: could not admit")
	}

	fault.failRemove = fmt.Errorf("injected deletion failure")
	deleted, err := fisbCacheEvictKeyIfUnchanged(e.Key, e)
	if err == nil {
		t.Fatal("expected the injected deletion failure to be reported")
	}
	if deleted {
		t.Error("a failed deletion must never be reported as deleted")
	}
	if _, stillPresent := fisbCacheStore.Get(e.Key); !stillPresent {
		t.Error("a failed deletion must leave the Store entry untouched - accounting must stay conservative")
	}
}

// assertNoFISBCacheFilesOnDisk fails t if the test's cache directory
// contains anything at all - neither a final file (never renamed into
// place) nor a leaked temp file (AtomicWriter's own deferred cleanup
// must have removed it).
func assertNoFISBCacheFilesOnDisk(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("expected no files left on disk, found %v", names)
	}
}

// --- shutdown -------------------------------------------------------------

// TestFISBCacheEnqueue_RejectsAndCountsDuringShutdown proves
// fisbCacheHandleShutdown's contract: once set, fisbCacheEnqueue rejects
// every further capture immediately (nothing queued) and counts it via
// fisbCacheShutdownRejected, distinct from every other rejection reason.
func TestFISBCacheEnqueue_RejectsAndCountsDuringShutdown(t *testing.T) {
	withTestFISBCacheStorage(t)
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)

	fisbCacheMu.Lock()
	fisbCacheStore = fisbcache.NewStore()
	fisbCacheSettingsCache = FISBCacheSettings{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 1 << 20, MaxEntries: 100}
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheShuttingDown = false
	fisbCacheMu.Unlock()

	before := atomic.LoadUint64(&fisbCacheShutdownRejected)
	fisbCacheHandleShutdown()

	fisbCaptureText("METAR", "KSHUTDOWN", "METAR body", fisbcache.FISBTime{})

	if got := atomic.LoadUint64(&fisbCacheShutdownRejected); got != before+1 {
		t.Errorf("expected fisbCacheShutdownRejected to increase by exactly 1, before=%d after=%d", before, got)
	}
	if queued, inFlight, _, _ := fisbCachePending.stats(); queued != 0 || inFlight != 0 {
		t.Errorf("expected nothing to be queued once shutdown began, got queued=%d inFlight=%d", queued, inFlight)
	}
}
