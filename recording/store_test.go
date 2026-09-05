package recording

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleAt(t time.Time) Sample {
	return Sample{UTC: t, Latitude: 47.0, Longitude: -122.0, GroundspeedKt: 90}
}

func TestStore_AppendAndReadAll(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, 0, 0) // no rotation/pruning for this test
	if err != nil {
		t.Fatalf("NewStore error: %v", err)
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := s.Append(sampleAt(base.Add(time.Duration(i) * time.Second))); err != nil {
			t.Fatalf("Append %d error: %v", i, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	samples, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if len(samples) != 5 {
		t.Fatalf("expected 5 samples, got %d", len(samples))
	}
	for i, s := range samples {
		want := base.Add(time.Duration(i) * time.Second)
		if !s.UTC.Equal(want) {
			t.Errorf("sample %d UTC = %v, want %v (order must be chronological)", i, s.UTC, want)
		}
	}
}

func TestStore_RotatesOnSize(t *testing.T) {
	dir := t.TempDir()
	// A tiny maxFileBytes forces a rotation on essentially every append.
	s, err := NewStore(dir, 50, 0)
	if err != nil {
		t.Fatalf("NewStore error: %v", err)
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if err := s.Append(sampleAt(base.Add(time.Duration(i) * time.Millisecond))); err != nil {
			t.Fatalf("Append %d error: %v", i, err)
		}
	}
	s.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir error: %v", err)
	}
	if len(entries) < 2 {
		t.Errorf("expected multiple rotated files with a tiny maxFileBytes, got %d", len(entries))
	}

	samples, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if len(samples) != 10 {
		t.Errorf("expected all 10 samples to survive rotation, got %d", len(samples))
	}
}

func TestStore_PrunesToMaxFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, 1, 3) // maxFileBytes=1 forces a new file nearly every append
	if err != nil {
		t.Fatalf("NewStore error: %v", err)
	}
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if err := s.Append(sampleAt(base.Add(time.Duration(i) * time.Millisecond))); err != nil {
			t.Fatalf("Append %d error: %v", i, err)
		}
	}
	s.Close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir error: %v", err)
	}
	if len(entries) > 3 {
		t.Errorf("expected retention to prune down to at most 3 files, found %d", len(entries))
	}
}

func TestNewStore_FailsCleanlyOnBadPath(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := NewStore(filepath.Join(blocker, "recordings"), 0, 0); err == nil {
		t.Error("expected an error creating a store under a non-directory path")
	}
}

func TestReadAll_EmptyDirReturnsNoSamplesNoError(t *testing.T) {
	dir := t.TempDir()
	samples, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("unexpected error on empty dir: %v", err)
	}
	if len(samples) != 0 {
		t.Errorf("expected no samples from an empty directory, got %d", len(samples))
	}
}

func TestReadAll_IgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "not-a-recording.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	s, err := NewStore(dir, 0, 0)
	if err != nil {
		t.Fatalf("NewStore error: %v", err)
	}
	s.Append(sampleAt(time.Now()))
	s.Close()

	samples, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if len(samples) != 1 {
		t.Errorf("expected exactly 1 sample (unrelated file ignored), got %d", len(samples))
	}
}
