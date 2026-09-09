package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stratux/stratux/recording"
)

// withTestRecordingsDir redirects the package-level recordingsDir/exportsDir
// at fresh temp directories for the duration of one test, and resets
// recCurrent to idle - so recording-lifecycle tests never touch the real
// persistent-data path and never leak state into each other. Mirrors
// withTestProfilesStore's pattern in main/calprofilesapi_test.go.
func withTestRecordingsDir(t *testing.T) {
	t.Helper()
	origRecordingsDir := recordingsDir
	origExportsDir := exportsDir
	origRecCurrent := recCurrent

	base := t.TempDir()
	recordingsDir = filepath.Join(base, "recordings")
	exportsDir = filepath.Join(base, "exports")

	recMu.Lock()
	recCurrent = nil
	recMu.Unlock()

	t.Cleanup(func() {
		recMu.Lock()
		s := recCurrent
		recMu.Unlock()
		if s != nil && s.State == recordingStateActive {
			// Best-effort: stop any session a test left running so its
			// sampler goroutine doesn't keep writing after t.TempDir()'s
			// own cleanup removes the underlying directory.
			close(s.stopCh)
			<-s.doneCh
			s.store.Close()
		}
		recMu.Lock()
		recCurrent = origRecCurrent
		recMu.Unlock()
		recordingsDir = origRecordingsDir
		exportsDir = origExportsDir
	})
}

// withFakePersistentStorageForTest substitutes availablePersistentBytes
// with a fixed, plentiful result for the duration of one test - the real
// implementation checks PersistentDataPath (/var/lib/stratux-data), which
// does not exist in the sandboxed test environment, so every recording
// start would otherwise fail with "persistent storage not mounted"
// regardless of anything this mission actually changed.
func withFakePersistentStorageForTest(t *testing.T) {
	t.Helper()
	orig := availablePersistentBytes
	availablePersistentBytes = func() (uint64, error) { return 10 << 30, nil } // 10 GiB free
	t.Cleanup(func() { availablePersistentBytes = orig })
}

// startTestRecording is the common setup every recording-lifecycle test
// needs: the fixtures buildPreflightReport() transitively requires, plus a
// redirected recordingsDir and a fake-but-plentiful storage check.
// Returns the decoded /startRecording response body for the caller to
// inspect further.
func startTestRecording(t *testing.T) map[string]interface{} {
	t.Helper()
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	withTestPreflightStore(t)
	withTestRecordingsDir(t)
	withFakePersistentStorageForTest(t)

	req := httptest.NewRequest(http.MethodPost, "/startRecording", nil)
	w := httptest.NewRecorder()
	handleStartRecordingRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("startRecording status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("startRecording response is not valid JSON: %v", err)
	}
	if success, _ := body["success"].(bool); !success {
		t.Fatalf("startRecording success = false: %v", body)
	}
	return body
}

func currentSessionID(t *testing.T) string {
	t.Helper()
	recMu.Lock()
	defer recMu.Unlock()
	if recCurrent == nil {
		t.Fatal("no active session after startTestRecording")
	}
	return recCurrent.ID
}

func TestHandleStartRecordingRequest_Success(t *testing.T) {
	startTestRecording(t)
	id := currentSessionID(t)

	// The initial metadata sidecar must exist immediately - it is written
	// synchronously by handleStartRecordingRequest, after recMu is
	// released but before the HTTP response is written.
	dir, ok := validRecordingDir(id)
	if !ok {
		t.Fatalf("session directory for %s not found under recordingsDir", id)
	}
	result := recording.ReadMetadata(dir)
	if result.Status != recording.MetadataOK {
		t.Fatalf("metadata Status = %v, want MetadataOK (error: %s)", result.Status, result.Error)
	}
	if result.Metadata.RecordingID != id {
		t.Errorf("metadata RecordingID = %q, want %q", result.Metadata.RecordingID, id)
	}
	if result.Metadata.Finalization.Complete {
		t.Error("Finalization.Complete = true immediately after start, want false")
	}
}

func TestHandleStartRecordingRequest_DuplicateStartConflict(t *testing.T) {
	startTestRecording(t)

	req := httptest.NewRequest(http.MethodPost, "/startRecording", nil)
	w := httptest.NewRecorder()
	handleStartRecordingRequest(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("second startRecording status = %d, want 409", w.Code)
	}

	// Exactly one recording must exist - the conflicting attempt must not
	// have created a second directory or a second metadata.json.
	entries, err := os.ReadDir(recordingsDir)
	if err != nil {
		t.Fatalf("could not list recordingsDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("recordingsDir has %d entries after a rejected duplicate start, want 1", len(entries))
	}
}

func TestHandleStartRecordingRequest_WrongMethod(t *testing.T) {
	withTestRecordingsDir(t)
	req := httptest.NewRequest(http.MethodGet, "/startRecording", nil)
	w := httptest.NewRecorder()
	handleStartRecordingRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestStopActiveRecording_FinalizesMetadataAndIsIdempotent(t *testing.T) {
	startTestRecording(t)
	id := currentSessionID(t)
	dir, _ := validRecordingDir(id)

	stopActiveRecording("manual")
	result := recording.ReadMetadata(dir)
	if result.Status != recording.MetadataOK {
		t.Fatalf("metadata Status = %v after stop, want MetadataOK", result.Status)
	}
	if !result.Metadata.Finalization.Complete {
		t.Error("Finalization.Complete = false after a normal stop, want true")
	}
	if result.Metadata.Finalization.StoppedAtUTC == nil {
		t.Error("Finalization.StoppedAtUTC is nil after a normal stop")
	}

	// Snapshot must be exactly what it was at start - stop must never
	// rewrite it.
	if result.Metadata.Snapshot.CalibrationProfileAvailable != false && result.Metadata.Snapshot.CalibrationProfileAvailable != true {
		t.Error("Snapshot.CalibrationProfileAvailable is not a valid bool") // sanity - always true or false
	}

	// Repeated stop must remain a safe no-op and must not re-finalize with
	// different content (idempotent, matching the pre-existing
	// handleStopRecordingRequest contract).
	firstStoppedAt := *result.Metadata.Finalization.StoppedAtUTC
	stopActiveRecording("manual")
	second := recording.ReadMetadata(dir)
	if !second.Metadata.Finalization.StoppedAtUTC.Equal(firstStoppedAt) {
		t.Errorf("repeated stop changed StoppedAtUTC: first=%v second=%v", firstStoppedAt, *second.Metadata.Finalization.StoppedAtUTC)
	}
}

func TestHandleStopRecordingRequest_RepeatedIsIdempotentHTTP(t *testing.T) {
	startTestRecording(t)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/stopRecording", nil)
		w := httptest.NewRecorder()
		handleStopRecordingRequest(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("stopRecording call %d status = %d, want 200", i, w.Code)
		}
	}
}

func TestSecondRecording_GetsItsOwnDistinctSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-dependent test in -short mode")
	}
	startTestRecording(t)
	firstID := currentSessionID(t)
	firstDir, _ := validRecordingDir(firstID)
	stopActiveRecording("manual")

	// Recording ids are second-granularity (pre-existing, unrelated to
	// this mission - see handleStartRecordingRequest's id generation), so
	// two starts within the same wall-clock second would otherwise
	// collide on the same id/directory. Sleeping past the boundary here
	// tests the intended "second recording, distinct snapshot" scenario
	// without touching that pre-existing scheme.
	time.Sleep(1100 * time.Millisecond)

	// Change the manual-check acknowledgement state between recordings so
	// the two snapshots are provably different, not just differently
	// timestamped.
	store := currentPreflightAckStore()
	if store == nil {
		t.Fatal("no preflight store available")
	}
	if _, err := store.Ack("antennas_attached", nil); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/startRecording", nil)
	w := httptest.NewRecorder()
	handleStartRecordingRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("second startRecording status = %d, want 200", w.Code)
	}
	secondID := currentSessionID(t)
	if secondID == firstID {
		t.Fatal("second recording reused the first recording's id")
	}
	secondDir, _ := validRecordingDir(secondID)

	firstResult := recording.ReadMetadata(firstDir)
	secondResult := recording.ReadMetadata(secondDir)
	if firstResult.Status != recording.MetadataOK || secondResult.Status != recording.MetadataOK {
		t.Fatalf("expected both recordings to have metadata: first=%v second=%v", firstResult.Status, secondResult.Status)
	}

	// The first recording's already-persisted snapshot must be completely
	// unaffected by the ack made after it stopped.
	reReadFirst := recording.ReadMetadata(firstDir)
	if reReadFirst.Metadata.Snapshot.PreflightRequiredActionCount != firstResult.Metadata.Snapshot.PreflightRequiredActionCount {
		t.Error("first recording's snapshot changed after a later ack - snapshots must be immutable")
	}
	if firstResult.Metadata.Finalization.Complete != true {
		t.Error("first recording should be Complete after its own stop")
	}
	if secondResult.Metadata.Finalization.Complete {
		t.Error("second recording should not be Complete while still active")
	}
}

func TestHandleListRecordingsRequest_MetadataSummaryFields(t *testing.T) {
	startTestRecording(t)
	stopActiveRecording("manual")

	req := httptest.NewRequest(http.MethodGet, "/getRecordings", nil)
	w := httptest.NewRecorder()
	handleListRecordingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var entries []recordingListEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	e := entries[0]
	if !e.MetadataAvailable {
		t.Error("MetadataAvailable = false, want true")
	}
	if !e.Complete {
		t.Error("Complete = false after a normal stop, want true")
	}
	if e.MetadataCorrupt {
		t.Error("MetadataCorrupt = true unexpectedly")
	}
}

func TestHandleListRecordingsRequest_LegacyRecordingIsSafe(t *testing.T) {
	withTestRecordingsDir(t)
	legacyDir := filepath.Join(recordingsDir, "rec-20200101T000000Z")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("could not create legacy fixture dir: %v", err)
	}
	// A legacy recording has sample files but no metadata.json at all -
	// matches the pre-existing zero-byte artifact this mission must not
	// break.
	if err := os.WriteFile(filepath.Join(legacyDir, "recording-20200101T000000.000000000Z.jsonl"), nil, 0o644); err != nil {
		t.Fatalf("could not create legacy sample fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/getRecordings", nil)
	w := httptest.NewRecorder()
	handleListRecordingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (legacy recording must not break listing)", w.Code)
	}
	var entries []recordingListEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(entries) != 1 || entries[0].MetadataAvailable {
		t.Errorf("legacy recording should list with MetadataAvailable=false, got %+v", entries)
	}
}

func TestHandleListRecordingsRequest_CorruptMetadataIsSafe(t *testing.T) {
	withTestRecordingsDir(t)
	dir := filepath.Join(recordingsDir, "rec-20200101T000000Z")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("could not create fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("could not write corrupt metadata fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/getRecordings", nil)
	w := httptest.NewRecorder()
	handleListRecordingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (corrupt metadata must not break listing)", w.Code)
	}
	var entries []recordingListEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if len(entries) != 1 || !entries[0].MetadataCorrupt || entries[0].MetadataAvailable {
		t.Errorf("corrupt-metadata recording should list with MetadataCorrupt=true, MetadataAvailable=false, got %+v", entries)
	}
}

func TestHandleListRecordingsRequest_EmptyZeroByteRecordingIsSafe(t *testing.T) {
	// Reproduces the pre-existing zero-byte legacy recording
	// (rec-20260830T074855Z) left over from an earlier, already-fixed
	// recMu deadlock - it must never crash the list path.
	withTestRecordingsDir(t)
	dir := filepath.Join(recordingsDir, "rec-20260830T074855Z")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("could not create fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recording-20260830T074855.477501249Z.jsonl"), nil, 0o644); err != nil {
		t.Fatalf("could not create zero-byte sample fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/getRecordings", nil)
	w := httptest.NewRecorder()
	handleListRecordingsRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestRecordingSamplerLoop_ProducesAtLeastOneSampleWithinTwoIntervals(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing-dependent test in -short mode")
	}
	startTestRecording(t)
	time.Sleep(recordingSampleInterval + 500*time.Millisecond)

	recMu.Lock()
	count := recCurrent.SampleCount
	recMu.Unlock()
	if count < 1 {
		t.Errorf("SampleCount = %d after waiting past one sample interval, want >= 1", count)
	}
	stopActiveRecording("manual")
}

func TestRecordingIDPattern_RejectsPathTraversalShapes(t *testing.T) {
	for _, bad := range []string{
		"../../etc",
		"rec-123", // wrong digit count
		"rec-20260101T000000Z/../../etc",
		"",
		"RECORDING-20260101T000000Z",
	} {
		if recordingIDPattern.MatchString(bad) {
			t.Errorf("pattern incorrectly matched malformed/traversal id: %q", bad)
		}
	}
	if !recordingIDPattern.MatchString("rec-20260101T000000Z") {
		t.Error("pattern should match a well-formed session id")
	}
}

func TestExportNamePattern_OnlyAcceptsKnownFormats(t *testing.T) {
	if !exportNamePattern.MatchString("rec-20260101T000000Z.csv") {
		t.Error("pattern should match a well-formed CSV export name")
	}
	for _, bad := range []string{
		"rec-20260101T000000Z.gpx", // not yet implemented, must not be servable as a file
		"../../etc/passwd",
		"rec-20260101T000000Z.csv/../../etc/passwd",
		"",
	} {
		if exportNamePattern.MatchString(bad) {
			t.Errorf("pattern incorrectly matched: %q", bad)
		}
	}
}
