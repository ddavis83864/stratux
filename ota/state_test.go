package ota

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadState_MissingFileIsIdleNotError(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadState(dir)
	if err != nil {
		t.Fatalf("unexpected error loading missing state: %v", err)
	}
	if s.Stage != StageIdle {
		t.Errorf("missing state file should report StageIdle, got %q", s.Stage)
	}
}

func TestSaveAndLoadState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	s := NewState("/var/lib/stratux-data/updates/staged/stratux.deb", "abc123", "2.0-pre5", "deadbeef", now)
	if err := SaveState(dir, s, now); err != nil {
		t.Fatalf("SaveState error: %v", err)
	}
	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState error: %v", err)
	}
	if got.Stage != StageStaged || got.ExpectedSHA256 != "abc123" || got.ExpectedCommit != "deadbeef" {
		t.Errorf("round-tripped state mismatch: %+v", got)
	}
}

func TestSaveState_IsAtomic(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	s := NewState("pkg.deb", "hash", "2.0-pre5", "commit", now)
	if err := SaveState(dir, s, now); err != nil {
		t.Fatalf("SaveState error: %v", err)
	}
	// The .tmp file must not be left behind after a successful save.
	if _, err := os.Stat(StatePath(dir) + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file should not exist after a successful SaveState")
	}
}

func TestLoadState_CorruptFileErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := LoadState(dir); err == nil {
		t.Error("expected an error loading a corrupt state file")
	}
}

func TestLoadState_UnrecognizedStageErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte(`{"Stage":"bogus_stage"}`), 0o644); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if _, err := LoadState(dir); err == nil {
		t.Error("expected an error loading a state file with an unrecognized stage")
	}
}

func TestClearState_RemovesFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := SaveState(dir, NewState("p", "h", "v", "c", now), now); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if err := ClearState(dir); err != nil {
		t.Fatalf("ClearState error: %v", err)
	}
	s, err := LoadState(dir)
	if err != nil || s.Stage != StageIdle {
		t.Errorf("expected idle state after clearing, got %+v err=%v", s, err)
	}
}

func TestClearState_MissingFileIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := ClearState(dir); err != nil {
		t.Errorf("clearing an already-absent state should not error, got %v", err)
	}
}

func TestStage_ValidAndTerminal(t *testing.T) {
	valid := []Stage{StageIdle, StageStaged, StageDisableRequested, StageInstalling,
		StageInstalled, StageVerifying, StageComplete, StageFailed, StageRolledBack}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if Stage("nonsense").Valid() {
		t.Error("an unrecognized stage string must not be valid")
	}
	if !StageComplete.Terminal() || !StageRolledBack.Terminal() {
		t.Error("complete and rolled_back should be terminal")
	}
	if StageStaged.Terminal() || StageInstalling.Terminal() {
		t.Error("in-progress stages must not be terminal")
	}
}

func TestStatePath(t *testing.T) {
	got := StatePath("/var/lib/stratux-data/updates")
	want := filepath.Join("/var/lib/stratux-data/updates", "state.json")
	if got != want {
		t.Errorf("StatePath = %q, want %q", got, want)
	}
}
