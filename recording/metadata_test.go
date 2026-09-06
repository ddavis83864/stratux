package recording

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/preflight"
)

func sampleSnapshot() SessionSnapshot {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return SessionSnapshot{
		CapturedAtUTC:                   &now,
		CapturedAtMonoSeconds:           512.34,
		StratuxVersion:                  "2.0-pre5",
		StratuxCommit:                   "abc123",
		PreflightBootSessionID:          "preflight-test",
		PreflightGeneratedAt:            &now,
		PreflightGeneratedAtMonoSeconds: 512.30,
		PreflightOverallState:           "CAUTION",
		PreflightRequiredActionCount:    0,
		PreflightCautionCount:           6,
		PreflightAutomated: []preflight.CheckResult{
			{Component: "GPS", CheckID: "gps_fix", Label: "GPS fix", State: preflight.StateCaution, Severity: preflight.SeverityCaution, Reason: "no fix", Source: "automated"},
		},
		PreflightManual: []preflight.CheckResult{
			{Component: "Manual", CheckID: "antennas_attached", Label: "Antennas", State: preflight.StateReady, Severity: preflight.SeverityCaution, Source: "manual"},
		},
		TrustedTimeAvailable:        true,
		GPSFixAvailable:             false,
		CalibrationProfileID:        "profile-abc",
		CalibrationProfileName:      "Current Installation",
		CalibrationProfileKind:      "migrated",
		CalibrationValid:            true,
		CalibrationProfileAvailable: true,
	}
}

func TestWriteInitialMetadata_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	snap := sampleSnapshot()
	if err := WriteInitialMetadata(dir, "rec-20260601T120000Z", snap); err != nil {
		t.Fatalf("WriteInitialMetadata: %v", err)
	}

	result := ReadMetadata(dir)
	if result.Status != MetadataOK {
		t.Fatalf("Status = %v, want MetadataOK (error: %s)", result.Status, result.Error)
	}
	if result.Metadata.SchemaVersion != MetadataSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", result.Metadata.SchemaVersion, MetadataSchemaVersion)
	}
	if result.Metadata.RecordingID != "rec-20260601T120000Z" {
		t.Errorf("RecordingID = %q, want rec-20260601T120000Z", result.Metadata.RecordingID)
	}
	if result.Metadata.Snapshot.CalibrationProfileID != "profile-abc" {
		t.Errorf("Snapshot.CalibrationProfileID = %q, want profile-abc", result.Metadata.Snapshot.CalibrationProfileID)
	}
	if len(result.Metadata.Snapshot.PreflightAutomated) != 1 || result.Metadata.Snapshot.PreflightAutomated[0].CheckID != "gps_fix" {
		t.Errorf("PreflightAutomated did not round-trip: %+v", result.Metadata.Snapshot.PreflightAutomated)
	}
	if result.Metadata.Finalization.Complete {
		t.Error("Finalization.Complete = true immediately after WriteInitialMetadata, want false")
	}
}

func TestWriteInitialMetadata_AtomicNoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	if err := WriteInitialMetadata(dir, "rec-x", sampleSnapshot()); err != nil {
		t.Fatalf("WriteInitialMetadata: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, metadataFileName+".tmp")); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after a successful write: err=%v", err)
	}
	if _, err := os.Stat(metadataPath(dir)); err != nil {
		t.Errorf("final metadata file missing after a successful write: %v", err)
	}
}

func TestFinalizeMetadata_UpdatesOnlyFinalization(t *testing.T) {
	dir := t.TempDir()
	snap := sampleSnapshot()
	if err := WriteInitialMetadata(dir, "rec-x", snap); err != nil {
		t.Fatalf("WriteInitialMetadata: %v", err)
	}
	stoppedAt := snap.CapturedAtUTC.Add(5 * time.Minute)
	fin := SessionFinalization{Complete: true, StoppedAtUTC: &stoppedAt, DurationSeconds: 300, SampleCount: 300}
	if err := FinalizeMetadata(dir, fin); err != nil {
		t.Fatalf("FinalizeMetadata: %v", err)
	}

	result := ReadMetadata(dir)
	if result.Status != MetadataOK {
		t.Fatalf("Status = %v after finalize, want MetadataOK", result.Status)
	}
	if !result.Metadata.Finalization.Complete || result.Metadata.Finalization.SampleCount != 300 {
		t.Errorf("Finalization did not update correctly: %+v", result.Metadata.Finalization)
	}
	// Snapshot must be byte-for-byte unchanged.
	if result.Metadata.Snapshot.CalibrationProfileID != snap.CalibrationProfileID ||
		result.Metadata.Snapshot.PreflightOverallState != snap.PreflightOverallState ||
		len(result.Metadata.Snapshot.PreflightAutomated) != len(snap.PreflightAutomated) {
		t.Errorf("Snapshot mutated by FinalizeMetadata: got %+v, want unchanged from %+v", result.Metadata.Snapshot, snap)
	}
}

func TestFinalizeMetadata_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteInitialMetadata(dir, "rec-x", sampleSnapshot()); err != nil {
		t.Fatalf("WriteInitialMetadata: %v", err)
	}
	stoppedAt := time.Date(2026, 6, 1, 12, 5, 0, 0, time.UTC)
	fin := SessionFinalization{Complete: true, StoppedAtUTC: &stoppedAt, DurationSeconds: 300, SampleCount: 300}
	if err := FinalizeMetadata(dir, fin); err != nil {
		t.Fatalf("first FinalizeMetadata: %v", err)
	}
	if err := FinalizeMetadata(dir, fin); err != nil {
		t.Fatalf("second (idempotent) FinalizeMetadata: %v", err)
	}
	result := ReadMetadata(dir)
	if result.Metadata.Finalization.SampleCount != 300 {
		t.Errorf("repeated finalize changed content unexpectedly: %+v", result.Metadata.Finalization)
	}
}

func TestFinalizeMetadata_NoInitialWrite(t *testing.T) {
	dir := t.TempDir()
	err := FinalizeMetadata(dir, SessionFinalization{Complete: true})
	if err != ErrNoMetadata {
		t.Errorf("FinalizeMetadata on a directory with no metadata.json: err = %v, want ErrNoMetadata", err)
	}
}

func TestReadMetadata_LegacyRecording(t *testing.T) {
	dir := t.TempDir() // no metadata.json ever written - simulates a pre-feature recording
	result := ReadMetadata(dir)
	if result.Status != MetadataUnavailable {
		t.Errorf("Status = %v, want MetadataUnavailable for a legacy recording", result.Status)
	}
}

func TestReadMetadata_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(metadataPath(dir), []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("could not write corrupt fixture: %v", err)
	}
	result := ReadMetadata(dir)
	if result.Status != MetadataCorrupt {
		t.Errorf("Status = %v, want MetadataCorrupt for truncated/invalid JSON", result.Status)
	}
	if result.Error == "" {
		t.Error("Error should be populated for MetadataCorrupt")
	}
}

func TestReadMetadata_TruncatedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := WriteInitialMetadata(dir, "rec-x", sampleSnapshot()); err != nil {
		t.Fatalf("WriteInitialMetadata: %v", err)
	}
	full, err := os.ReadFile(metadataPath(dir))
	if err != nil {
		t.Fatalf("could not read back fixture: %v", err)
	}
	truncated := full[:len(full)/2]
	if err := os.WriteFile(metadataPath(dir), truncated, 0o644); err != nil {
		t.Fatalf("could not write truncated fixture: %v", err)
	}
	result := ReadMetadata(dir)
	if result.Status != MetadataCorrupt {
		t.Errorf("Status = %v, want MetadataCorrupt for a truncated file", result.Status)
	}
}

func TestReadMetadata_IgnoresTempFileResidue(t *testing.T) {
	dir := t.TempDir()
	if err := WriteInitialMetadata(dir, "rec-x", sampleSnapshot()); err != nil {
		t.Fatalf("WriteInitialMetadata: %v", err)
	}
	// Simulate a daemon killed mid-write on a *later* operation: a stray
	// .tmp file sits next to an otherwise-valid, already-finalized-looking
	// metadata.json. It must never be read as if it were the real file.
	if err := os.WriteFile(metadataPath(dir)+".tmp", []byte("{garbage"), 0o644); err != nil {
		t.Fatalf("could not write stray temp file: %v", err)
	}
	result := ReadMetadata(dir)
	if result.Status != MetadataOK {
		t.Errorf("Status = %v, want MetadataOK - a stray .tmp file must not affect reading the real metadata.json", result.Status)
	}
}

func TestReadMetadata_MissingDirectory(t *testing.T) {
	result := ReadMetadata(filepath.Join(t.TempDir(), "does-not-exist"))
	if result.Status != MetadataUnavailable {
		t.Errorf("Status = %v, want MetadataUnavailable for a nonexistent directory", result.Status)
	}
}

func TestWriteInitialMetadata_PersistenceFailureDoesNotPanic(t *testing.T) {
	// Point at a path whose parent is actually a file, not a directory -
	// os.OpenFile for the temp file must fail cleanly, not panic.
	base := t.TempDir()
	blocker := filepath.Join(base, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("could not create blocker file: %v", err)
	}
	dir := filepath.Join(blocker, "recording-dir") // blocker is a file, not a dir
	err := WriteInitialMetadata(dir, "rec-x", sampleSnapshot())
	if err == nil {
		t.Error("expected an error writing metadata under a non-directory parent, got nil")
	}
	// The recording itself must remain safe: no panic occurred getting
	// here, and a failed write must never be reported as MetadataOK - the
	// underlying OS error for "parent path component is not a directory"
	// is platform-dependent (some report it as not-exist, some as a
	// generic access error), so either MetadataUnavailable or
	// MetadataCorrupt is an acceptable honest answer here; MetadataOK
	// (fabricated success) is the only unacceptable one.
	if result := ReadMetadata(dir); result.Status == MetadataOK {
		t.Errorf("Status = %v after a failed write, want anything but MetadataOK (never fabricate success)", result.Status)
	}
}

func TestReadMetadata_ConcurrentDuringFinalize(t *testing.T) {
	dir := t.TempDir()
	if err := WriteInitialMetadata(dir, "rec-x", sampleSnapshot()); err != nil {
		t.Fatalf("WriteInitialMetadata: %v", err)
	}
	stoppedAt := time.Now().UTC()
	fin := SessionFinalization{Complete: true, StoppedAtUTC: &stoppedAt, DurationSeconds: 1, SampleCount: 1}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Many concurrent readers while one writer finalizes repeatedly -
	// every read must land on either the pre- or post-finalization state,
	// never a corrupt/torn file (atomic rename guarantees this).
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if result := ReadMetadata(dir); result.Status == MetadataCorrupt {
						t.Errorf("reader observed MetadataCorrupt during concurrent finalize: %s", result.Error)
					}
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := FinalizeMetadata(dir, fin); err != nil {
			t.Errorf("FinalizeMetadata under concurrent read: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestSummarizeMetadata_Counts(t *testing.T) {
	base := t.TempDir()
	newest := filepath.Join(base, "rec-20260601T120300Z")
	middle := filepath.Join(base, "rec-20260601T120200Z")
	oldest := filepath.Join(base, "rec-20260601T120100Z")
	corrupt := filepath.Join(base, "rec-20260601T120000Z")
	for _, d := range []string{newest, middle, oldest, corrupt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	// newest: complete, metadata OK
	if err := WriteInitialMetadata(newest, "rec-20260601T120300Z", sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Now().UTC()
	if err := FinalizeMetadata(newest, SessionFinalization{Complete: true, StoppedAtUTC: &stoppedAt, SampleCount: 10}); err != nil {
		t.Fatal(err)
	}
	// middle: metadata OK but never finalized (interrupted recording)
	if err := WriteInitialMetadata(middle, "rec-20260601T120200Z", sampleSnapshot()); err != nil {
		t.Fatal(err)
	}
	// oldest: legacy, no metadata.json at all
	// corrupt: invalid JSON
	if err := os.WriteFile(metadataPath(corrupt), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	refs := []RecordingRef{
		{ID: "rec-20260601T120300Z", Dir: newest}, // newest-first, as callers must provide
		{ID: "rec-20260601T120200Z", Dir: middle},
		{ID: "rec-20260601T120100Z", Dir: oldest},
		{ID: "rec-20260601T120000Z", Dir: corrupt},
	}
	sum := SummarizeMetadata(refs)
	if sum.RecordingCount != 4 {
		t.Errorf("RecordingCount = %d, want 4", sum.RecordingCount)
	}
	if sum.WithMetadata != 2 {
		t.Errorf("WithMetadata = %d, want 2", sum.WithMetadata)
	}
	if sum.Legacy != 1 {
		t.Errorf("Legacy = %d, want 1", sum.Legacy)
	}
	if sum.Incomplete != 1 {
		t.Errorf("Incomplete = %d, want 1 (the never-finalized 'middle' recording)", sum.Incomplete)
	}
	if sum.CorruptMetadata != 1 {
		t.Errorf("CorruptMetadata = %d, want 1", sum.CorruptMetadata)
	}
	if sum.MostRecentRecordingID != "rec-20260601T120300Z" {
		t.Errorf("MostRecentRecordingID = %q, want the newest recording's id", sum.MostRecentRecordingID)
	}
	if !sum.MostRecentStartStateAvailable {
		t.Error("MostRecentStartStateAvailable = false, want true (the most recent recording has valid metadata)")
	}
	if sum.MostRecentSchemaVersion != MetadataSchemaVersion {
		t.Errorf("MostRecentSchemaVersion = %d, want %d", sum.MostRecentSchemaVersion, MetadataSchemaVersion)
	}
}

func TestSummarizeMetadata_Empty(t *testing.T) {
	sum := SummarizeMetadata(nil)
	if sum.RecordingCount != 0 || sum.MostRecentRecordingID != "" {
		t.Errorf("expected a zero-value summary for no recordings, got %+v", sum)
	}
}
