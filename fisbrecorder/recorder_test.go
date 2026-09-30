package fisbrecorder

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestRecorder_StopNeverOrphansAnAcceptedRecord locks in a real accounting
// bug found in a live ~25-minute device session under sustained real
// GDL90 traffic: the manifest claimed 82559 GDL90 records accepted, but
// the actual file held only 82557 - a 2-record gap, neither written nor
// counted dropped. Root cause: RecordGDL90 read active/ch under the
// lock, then unlocked before its own channel send; a call already past
// that unlock when Stop() began could still be counted *Accepted and
// still reach the channel *after* the writer's own final drain had
// already found it empty and moved on to closing files - orphaned
// forever. Fixed with an inflight WaitGroup Stop()/run() both wait on
// before ever doing the final drain.
//
// Reproduced here with many goroutines hammering RecordGDL90 as fast as
// possible while Stop() is called concurrently from another goroutine -
// accepted (from the manifest) must always exactly equal the number of
// lines actually in the written file, every single run, not just on
// average.
func TestRecorder_StopNeverOrphansAnAcceptedRecord(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		dir := t.TempDir()
		r := New(dir, "build", DefaultOptions())
		sid, err := r.Start()
		if err != nil {
			t.Fatalf("iter %d: Start: %v", iter, err)
		}

		const writers = 8
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
						r.RecordGDL90("1.2.3.4:4000", []byte("payload"))
					}
				}
			}()
		}

		// Let the writers run for a moment so Stop() lands in the middle
		// of real concurrent traffic, not before it even starts.
		time.Sleep(2 * time.Millisecond)
		m, err := r.Stop()
		close(stop)
		wg.Wait()
		if err != nil {
			t.Fatalf("iter %d: Stop: %v", iter, err)
		}

		frames := readGDL90(t, filepath.Join(dir, sid))
		if uint64(len(frames)) != m.GDL90Count {
			t.Fatalf("iter %d: manifest claims GDL90Count=%d accepted, but the file contains %d records (an orphaned-record accounting gap)", iter, m.GDL90Count, len(frames))
		}
	}
}

// TestRecorder_SelfTriggeredStopReconcilesActive locks in a second bug
// found alongside the one above: a self-triggered stop (MaxDuration/
// MaxBytes/MinFreeBytes via checkBounds) never flipped active to false -
// only an external Stop() call did, and only after waiting for doneCh.
// IsActive() would keep reporting true forever after a self-triggered
// stop until something happened to call Stop() - but main's own
// fisbRecorderWatchdog only calls Stop() when it sees
// !globalSettings.FISBRecordingEnabled && IsActive(); if the operator's
// setting was still on, it would never call Stop(), and the daemon would
// believe recording was still active while the writer goroutine had
// already exited and abandoned its channels.
func TestRecorder_SelfTriggeredStopReconcilesActive(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.MaxDuration = 30 * time.Millisecond
	r := New(dir, "build", opts)
	if _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// checkBounds only runs on run()'s own 2s ticker (see recorder.go),
	// so this needs real room past that first tick - not a race against
	// it, matching TestRecorder_MaxDurationStopsSession's own note on
	// why the OTHER checkBounds tests call it directly instead.
	deadline := time.Now().Add(4 * time.Second)
	for r.IsActive() {
		if time.Now().After(deadline) {
			t.Fatal("IsActive() still true 4s after MaxDuration should have self-triggered a stop - active was never reconciled")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A subsequent explicit Stop() call must report "no active session",
	// not hang or double-finalize.
	if _, err := r.Stop(); err == nil {
		t.Error("Stop() after a self-triggered stop returned no error, want \"no active session\"")
	}
}

// TestRecorder_SecondSessionStopDoesNotHang locks in a real bug found in
// a live daemon bench session: stopOnce (a sync.Once) was never reset in
// Start(), so Stop() on any session after the first silently never sent
// on that session's own (freshly made) stopCh - sync.Once.Do only ever
// runs its function once for that Once value's whole lifetime. The
// writer goroutine never saw a stop signal, and Stop()'s own <-doneCh
// wait blocked forever. A single Recorder value is reused across many
// Start/Stop cycles in production (main/fisbrecorderwiring.go's
// fisbRecorderWatchdog starts and stops the SAME package-level
// fisbRecorder instance every time the operator toggles the setting),
// so this is not a one-off - every session after the first would have
// hung the watchdog goroutine indefinitely.
func TestRecorder_SecondSessionStopDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())

	for i := 0; i < 3; i++ {
		if _, err := r.Start(); err != nil {
			t.Fatalf("session %d: Start: %v", i, err)
		}
		r.RecordFrame("frame")

		done := make(chan struct{})
		go func() {
			r.Stop()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("session %d: Stop() did not return within 3s - stopOnce/doneCh regression", i)
		}
		if r.IsActive() {
			t.Fatalf("session %d: IsActive() true after Stop() returned", i)
		}
	}
}

func TestRecorder_StartRecordStop_BasicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "testbuild1234567890123456789012345678901234", DefaultOptions())

	sid, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if sid == "" {
		t.Fatal("Start returned empty session id")
	}
	if !r.IsActive() {
		t.Fatal("IsActive false right after Start")
	}

	r.RecordFrame("aaaa")
	r.RecordFrame("bbbb")
	r.RecordFrame("cccc")
	r.RecordGDL90("192.168.10.22:12345", []byte{0x7e, 0x01, 0x02, 0x7e})
	r.RecordSnapshot("status", map[string]interface{}{"Build": "testbuild"})

	m, err := r.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if r.IsActive() {
		t.Fatal("IsActive true after Stop")
	}
	if m.FrameCount != 3 {
		t.Errorf("FrameCount = %d, want 3", m.FrameCount)
	}
	if m.GDL90Count != 1 {
		t.Errorf("GDL90Count = %d, want 1", m.GDL90Count)
	}
	if m.SnapshotCount != 1 {
		t.Errorf("SnapshotCount = %d, want 1", m.SnapshotCount)
	}
	if m.StopReason != StopRequested {
		t.Errorf("StopReason = %q, want %q", m.StopReason, StopRequested)
	}
	if m.DroppedFrames != 0 || m.DroppedGDL90 != 0 || m.DroppedSnapshots != 0 {
		t.Errorf("unexpected drops: %+v", m)
	}
	if len(m.Files) != 3 {
		t.Fatalf("manifest lists %d files, want 3", len(m.Files))
	}
	for _, fi := range m.Files {
		if fi.SHA256 == "" || fi.Bytes == 0 {
			t.Errorf("file %s has empty hash or zero size: %+v", fi.Name, fi)
		}
	}

	// The bundle must have landed exactly under dir/sessionID, not
	// somewhere else - a field operator needs to find it deterministically.
	if _, err := os.Stat(filepath.Join(dir, sid, "manifest.json")); err != nil {
		t.Errorf("manifest.json not found at the expected session path: %v", err)
	}
}

func TestRecorder_RecordBeforeStart_IsNoOp(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	// Must not panic or block - recording before Start (or after Stop) is
	// simply ignored, so call sites never need to guard every call with
	// their own IsActive check.
	r.RecordFrame("never recorded")
	r.RecordGDL90("x", []byte("y"))
	r.RecordSnapshot("z", nil)
}

func TestRecorder_DoubleStartFails(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	if _, err := r.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer r.Stop()
	if _, err := r.Start(); err == nil {
		t.Fatal("second Start on an already-active recorder succeeded; want an error")
	}
}

func TestRecorder_NeverBlocksWhenQueueFull(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.QueueSize = 4 // tiny, to force the drop path deterministically
	r := New(dir, "build", opts)
	if _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Flood far more frames than the queue can hold, with no writer
	// draining in between (we don't sleep) - every call must return
	// immediately regardless.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100000; i++ {
			r.RecordFrame("flood")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RecordFrame blocked under queue pressure - the hot path must never block")
	}

	m, err := r.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.DroppedFrames == 0 {
		t.Error("expected some frames to be dropped and counted under a 4-deep queue flooded with 100000 sends - got 0 dropped")
	}
	if m.FrameCount+m.DroppedFrames != 100000 {
		t.Errorf("FrameCount(%d) + DroppedFrames(%d) = %d, want 100000 (every call accounted for)", m.FrameCount, m.DroppedFrames, m.FrameCount+m.DroppedFrames)
	}
}

func TestRecorder_ConcurrentRecordersFromManyGoroutines(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	if _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const goroutines = 8
	const perGoroutine = 200
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				r.RecordFrame("x")
			}
		}()
	}
	wg.Wait()

	m, err := r.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.FrameCount+m.DroppedFrames != goroutines*perGoroutine {
		t.Errorf("FrameCount(%d)+Dropped(%d) = %d, want %d", m.FrameCount, m.DroppedFrames, m.FrameCount+m.DroppedFrames, goroutines*perGoroutine)
	}
}

func TestRecorder_MaxDurationStopsSession(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.MaxDuration = 50 * time.Millisecond
	r := New(dir, "build", opts)
	if _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// checkBounds runs every 2s in the real writer loop - too slow for a
	// unit test. Directly exercise the same logic the writer goroutine
	// uses, rather than sleeping 2+ seconds in a test.
	time.Sleep(60 * time.Millisecond)
	reason, stop := r.checkBounds()
	if !stop || reason != StopMaxDuration {
		t.Fatalf("checkBounds() = (%q, %v), want (%q, true)", reason, stop, StopMaxDuration)
	}
	r.Stop()
}

func TestRecorder_MaxBytesStopsSession(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	if _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Simulate having already written more than the (tiny, test-only)
	// byte budget by writing directly to one of the session's own files -
	// checkBounds stats the real files on disk. Start() spawns run() in
	// its own goroutine, and run() opens (and O_TRUNCs) this same file
	// before entering its drain loop - writing here before that open has
	// happened is a real TOCTOU race (seen flaking in CI: run()'s own
	// truncate landing after this write wiped it back to empty, so
	// checkBounds correctly saw 0 bytes and never tripped). Wait for the
	// file to exist first, so this write is deterministically the last
	// one before checkBounds runs.
	framesFile := filepath.Join(r.dir, "frames.jsonl.gz")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(framesFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("frames.jsonl.gz was never created by run() within %s", 2*time.Second)
		}
		time.Sleep(time.Millisecond)
	}
	os.WriteFile(framesFile, make([]byte, 1000), 0o640)
	r.opts.MaxBytes = 500

	reason, stop := r.checkBounds()
	if !stop || reason != StopMaxBytes {
		t.Fatalf("checkBounds() = (%q, %v), want (%q, true)", reason, stop, StopMaxBytes)
	}
	r.Stop()
}

func TestRecorder_DiskFullStopsSession(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	if _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	orig := diskFreeBytes
	defer func() { diskFreeBytes = orig }()
	diskFreeBytes = func(string) (uint64, error) { return 1, nil } // 1 byte free

	reason, stop := r.checkBounds()
	if !stop || reason != StopDiskFull {
		t.Fatalf("checkBounds() = (%q, %v), want (%q, true)", reason, stop, StopDiskFull)
	}
	r.Stop()
}

func TestRecorder_ElapsedNanosIsMonotonicNotWallClock(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	if _, err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.RecordFrame("a")
	time.Sleep(20 * time.Millisecond)
	r.RecordFrame("b")
	m, err := r.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.FrameCount != 2 {
		t.Fatalf("FrameCount = %d, want 2", m.FrameCount)
	}
	frames := readFrames(t, filepath.Join(dir, m.SessionID))
	if len(frames) != 2 {
		t.Fatalf("read %d frames, want 2", len(frames))
	}
	gap := frames[1].ElapsedNanos - frames[0].ElapsedNanos
	if gap < 15*time.Millisecond.Nanoseconds() {
		t.Errorf("elapsed gap between frames = %dns, want >= ~20ms (the real sleep)", gap)
	}
}
