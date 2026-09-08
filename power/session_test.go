package power

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadSessionMarker_MissingFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	_, ok, err := ReadSessionMarker(path)
	if err != nil {
		t.Fatalf("a missing marker file must not be an error, got: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a missing marker file")
	}
}

func TestWriteReadSessionMarker_RoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	want := SessionMarker{SessionID: "boot-a", ClosedCleanly: true, ClosedReason: "controlled-shutdown", UpdatedAtMonoSeconds: 42.5}
	if err := WriteSessionMarkerAtomic(path, want); err != nil {
		t.Fatalf("WriteSessionMarkerAtomic: %v", err)
	}
	got, ok, err := ReadSessionMarker(path)
	if err != nil {
		t.Fatalf("ReadSessionMarker: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true after a successful write")
	}
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestWriteSessionMarkerAtomic_NoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := WriteSessionMarkerAtomic(path, SessionMarker{SessionID: "b"}); err != nil {
		t.Fatalf("WriteSessionMarkerAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "session.json" {
		t.Fatalf("expected exactly one final file, got %v", entries)
	}
}

func TestReadSessionMarker_CorruptFileIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, ok, err := ReadSessionMarker(path)
	if err == nil {
		t.Fatal("expected an error for a corrupt marker file")
	}
	if ok {
		t.Fatal("expected ok=false alongside the error")
	}
}

func TestEvaluatePreviousSession_NoRecordAvailable(t *testing.T) {
	a := EvaluatePreviousSession(SessionMarker{}, false)
	if a.Available {
		t.Error("expected Available=false when no marker was found")
	}
}

func TestEvaluatePreviousSession_ClosedCleanly(t *testing.T) {
	a := EvaluatePreviousSession(SessionMarker{SessionID: "prev", ClosedCleanly: true}, true)
	if !a.Available || !a.EndedCleanly {
		t.Errorf("expected Available=true, EndedCleanly=true, got %+v", a)
	}
}

func TestEvaluatePreviousSession_NotClosedCleanly(t *testing.T) {
	a := EvaluatePreviousSession(SessionMarker{SessionID: "prev", ClosedCleanly: false}, true)
	if !a.Available || a.EndedCleanly {
		t.Errorf("expected Available=true, EndedCleanly=false, got %+v", a)
	}
	// This is the specific, conservative-wording requirement: the note
	// may mention "power loss" only to disclaim it, never assert it (or
	// "crash", "battery died", etc.) as a confirmed fact.
	note := strings.ToLower(a.Note)
	for _, unhedgedClaim := range []string{"battery died", "the device crashed", "caused by power loss", "due to power loss"} {
		if strings.Contains(note, unhedgedClaim) {
			t.Errorf("Note must never assert %q as a confirmed diagnosis, got: %s", unhedgedClaim, a.Note)
		}
	}
	if !strings.Contains(note, "not a confirmed diagnosis") {
		t.Errorf("Note must explicitly hedge that this is not a confirmed diagnosis, got: %s", a.Note)
	}
}
