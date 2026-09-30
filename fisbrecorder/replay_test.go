package fisbrecorder

import (
	"path/filepath"
	"testing"
	"time"
)

// recordSession is a test helper: records the given frames (with real
// inter-frame delays applied via the caller-supplied sleep durations) and
// returns the session directory.
func recordSession(t *testing.T, frames []string, delays []time.Duration) string {
	t.Helper()
	if len(delays) != len(frames) {
		t.Fatalf("test setup error: %d frames but %d delays", len(frames), len(delays))
	}
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	sid, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i, f := range frames {
		if delays[i] > 0 {
			time.Sleep(delays[i])
		}
		r.RecordFrame(f)
	}
	if _, err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return filepath.Join(dir, sid)
}

func TestReplayFrames_PreservesBytesOrderAndCount(t *testing.T) {
	frames := []string{"frame-one", "frame-two", "frame-three"}
	delays := []time.Duration{0, 5 * time.Millisecond, 5 * time.Millisecond}
	sessionDir := recordSession(t, frames, delays)

	var got []string
	stats, err := ReplayFrames(sessionDir, func(rec FrameRecord) {
		got = append(got, rec.Frame)
	}, ReplayOptions{SpeedMultiplier: -1}) // as fast as possible
	if err != nil {
		t.Fatalf("ReplayFrames: %v", err)
	}
	if stats.FramesReplayed != 3 {
		t.Errorf("FramesReplayed = %d, want 3", stats.FramesReplayed)
	}
	if len(got) != len(frames) {
		t.Fatalf("replayed %d frames, want %d", len(got), len(frames))
	}
	for i := range frames {
		if got[i] != frames[i] {
			t.Errorf("frame %d = %q, want %q (order/content not preserved)", i, got[i], frames[i])
		}
	}
	if len(stats.SeqGaps) != 0 {
		t.Errorf("unexpected seq gaps in a clean recording: %v", stats.SeqGaps)
	}
}

func TestReplayFrames_OriginalPacingRespectsSourceTimestamps(t *testing.T) {
	frames := []string{"a", "b"}
	delays := []time.Duration{0, 40 * time.Millisecond}
	sessionDir := recordSession(t, frames, delays)

	start := time.Now()
	stats, err := ReplayFrames(sessionDir, func(rec FrameRecord) {}, ReplayOptions{SpeedMultiplier: 1.0})
	if err != nil {
		t.Fatalf("ReplayFrames: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 30*time.Millisecond {
		t.Errorf("original-pacing replay took %v, want >= ~40ms (the recorded gap)", elapsed)
	}
	if stats.FramesReplayed != 2 {
		t.Errorf("FramesReplayed = %d, want 2", stats.FramesReplayed)
	}
}

func TestReplayFrames_AcceleratedModeIsFasterThanOriginal(t *testing.T) {
	frames := []string{"a", "b"}
	delays := []time.Duration{0, 60 * time.Millisecond}
	sessionDir := recordSession(t, frames, delays)

	start := time.Now()
	_, err := ReplayFrames(sessionDir, func(rec FrameRecord) {}, ReplayOptions{SpeedMultiplier: 20})
	if err != nil {
		t.Fatalf("ReplayFrames: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed > 40*time.Millisecond {
		t.Errorf("20x-accelerated replay of a 60ms gap took %v, want well under the original 60ms", elapsed)
	}
}

func TestReplayFrames_StopAfterBoundsReplay(t *testing.T) {
	frames := []string{"a", "b", "c", "d", "e"}
	delays := make([]time.Duration, len(frames))
	sessionDir := recordSession(t, frames, delays)

	var count int
	stats, err := ReplayFrames(sessionDir, func(rec FrameRecord) { count++ }, ReplayOptions{SpeedMultiplier: -1, StopAfter: 2})
	if err != nil {
		t.Fatalf("ReplayFrames: %v", err)
	}
	if count != 2 || stats.FramesReplayed != 2 {
		t.Errorf("StopAfter=2: handler called %d times, stats.FramesReplayed=%d, want 2/2", count, stats.FramesReplayed)
	}
}

func TestReplayFrames_MissingFileReturnsError(t *testing.T) {
	dir := t.TempDir() // empty - no session was ever recorded here
	_, err := ReplayFrames(dir, func(rec FrameRecord) {}, ReplayOptions{SpeedMultiplier: -1})
	if err == nil {
		t.Fatal("ReplayFrames on a directory with no frames.jsonl.gz returned no error")
	}
}

func TestReplayFrames_DoesNotTouchGDL90OrSnapshotFiles(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	sid, _ := r.Start()
	r.RecordFrame("a")
	r.RecordGDL90("1.2.3.4:1", []byte{1, 2, 3})
	r.RecordSnapshot("status", map[string]string{"x": "y"})
	r.Stop()
	sessionDir := filepath.Join(dir, sid)

	beforeGDL90 := readGDL90(t, sessionDir)

	_, err := ReplayFrames(sessionDir, func(rec FrameRecord) {}, ReplayOptions{SpeedMultiplier: -1})
	if err != nil {
		t.Fatalf("ReplayFrames: %v", err)
	}

	afterGDL90 := readGDL90(t, sessionDir)
	if len(beforeGDL90) != len(afterGDL90) || len(beforeGDL90) != 1 {
		t.Errorf("gdl90.jsonl.gz changed size across a frame-only replay: before=%d after=%d", len(beforeGDL90), len(afterGDL90))
	}
}
