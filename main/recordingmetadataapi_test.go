package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/preflight"
	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/recording"
)

// TestDiagnosticBundle_IncludesSanitizedRecordingMetadataSummary mirrors
// TestDiagnosticBundle_IncludesSanitizedPreflightReport in
// main/preflightapi_test.go, for the recording-metadata summary added by
// this mission - exercises the real listRecordingRefs/SummarizeMetadata
// code path (not just a fabricated value) and confirms the result is
// bounded (counts only, no individual recording's Preflight/calibration
// content) and free of anything the sanitization scan would flag.
func TestDiagnosticBundle_IncludesSanitizedRecordingMetadataSummary(t *testing.T) {
	startTestRecording(t)
	stopActiveRecording("manual")

	summary := recording.SummarizeMetadata(listRecordingRefs())
	if summary.RecordingCount != 1 || summary.WithMetadata != 1 {
		t.Fatalf("unexpected summary from a single completed recording: %+v", summary)
	}

	bundle := readiness.BuildDiagnosticBundle(time.Now().UTC(), "v", "c", readiness.HealthReport{}, nil, nil, nil, "", nil, summary, nil, nil, nil, nil, nil)
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("bundle with a recording metadata summary failed to marshal: %v", err)
	}

	var roundTrip struct {
		RecordingMetadataSummary recording.MetadataDiagnosticsSummary `json:"RecordingMetadataSummary"`
	}
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("could not round-trip RecordingMetadataSummary out of the bundle: %v", err)
	}
	if roundTrip.RecordingMetadataSummary.RecordingCount != 1 {
		t.Errorf("RecordingMetadataSummary did not round-trip correctly: %+v", roundTrip.RecordingMetadataSummary)
	}

	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{"password", "wifipassphrase", "ssh-rsa", "ssh-ed25519", "begin rsa private key", "latitude", "longitude"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("diagnostic bundle containing a recording metadata summary leaked a forbidden substring: %q", forbidden)
		}
	}
}

func TestBuildSessionSnapshot_TrustedTimeAndGPSFixDerivedFromReport(t *testing.T) {
	now := time.Now().UTC()
	report := preflight.Report{
		GeneratedAt:            &now,
		GeneratedAtMonoSeconds: 42.5,
		BootSessionID:          "preflight-test",
		Overall:                preflight.StateCaution,
		RequiredActionCount:    0,
		CautionCount:           3,
		Automated: []preflight.CheckResult{
			{CheckID: "gps_fix", State: preflight.StateReady},
		},
	}
	session := &recordingSession{
		CalibrationProfileID:        "profile-1",
		CalibrationProfileName:      "Test Aircraft",
		CalibrationProfileKind:      "migrated",
		CalibrationValid:            true,
		CalibrationProfileAvailable: true,
	}

	snap := buildSessionSnapshot(report, session, nil)
	if !snap.TrustedTimeAvailable {
		t.Error("TrustedTimeAvailable = false, want true when report.GeneratedAt is non-nil")
	}
	if !snap.GPSFixAvailable {
		t.Error("GPSFixAvailable = false, want true when gps_fix check is StateReady")
	}
	if snap.CalibrationProfileName != "Test Aircraft" {
		t.Errorf("CalibrationProfileName = %q, want Test Aircraft", snap.CalibrationProfileName)
	}
	if snap.PreflightBootSessionID != "preflight-test" {
		t.Errorf("PreflightBootSessionID = %q, want preflight-test", snap.PreflightBootSessionID)
	}
}

func TestBuildSessionSnapshot_UntrustedTimeAndNoGPSFix(t *testing.T) {
	report := preflight.Report{
		GeneratedAt: nil, // no trusted wall clock yet
		Automated: []preflight.CheckResult{
			{CheckID: "gps_fix", State: preflight.StateCaution},
		},
	}
	snap := buildSessionSnapshot(report, &recordingSession{}, nil)
	if snap.TrustedTimeAvailable {
		t.Error("TrustedTimeAvailable = true, want false when report.GeneratedAt is nil")
	}
	if snap.GPSFixAvailable {
		t.Error("GPSFixAvailable = true, want false when gps_fix check is not StateReady")
	}
}

func TestBuildSessionSnapshot_NoGPSFixCheckPresent(t *testing.T) {
	// A report with no gps_fix entry at all (e.g. GPS hardware absent)
	// must not be misread as a fix being available.
	snap := buildSessionSnapshot(preflight.Report{}, &recordingSession{}, nil)
	if snap.GPSFixAvailable {
		t.Error("GPSFixAvailable = true with no gps_fix check present, want false")
	}
}

func TestHandleRecordingMetadataRequest_ValidRecording(t *testing.T) {
	startTestRecording(t)
	id := currentSessionID(t)

	req := httptest.NewRequest(http.MethodGet, "/getRecordingMetadata?id="+id, nil)
	w := httptest.NewRecorder()
	handleRecordingMetadataRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	var body struct {
		Available bool                      `json:"available"`
		Metadata  recording.SessionMetadata `json:"metadata"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if !body.Available {
		t.Error("available = false for a recording with real metadata")
	}
	if body.Metadata.RecordingID != id {
		t.Errorf("Metadata.RecordingID = %q, want %q", body.Metadata.RecordingID, id)
	}
}

func TestHandleRecordingMetadataRequest_LegacyRecording(t *testing.T) {
	withTestRecordingsDir(t)
	dir := filepath.Join(recordingsDir, "rec-20200101T000000Z")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/getRecordingMetadata?id=rec-20200101T000000Z", nil)
	w := httptest.NewRecorder()
	handleRecordingMetadataRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a legacy recording is not an error)", w.Code)
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if avail, _ := body["available"].(bool); avail {
		t.Error("available = true for a legacy recording with no metadata.json")
	}
	if legacy, _ := body["legacy"].(bool); !legacy {
		t.Errorf("legacy flag not set for a legacy recording: %v", body)
	}
}

func TestHandleRecordingMetadataRequest_CorruptMetadata(t *testing.T) {
	withTestRecordingsDir(t)
	dir := filepath.Join(recordingsDir, "rec-20200101T000000Z")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "metadata.json"), []byte("{garbage"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/getRecordingMetadata?id=rec-20200101T000000Z", nil)
	w := httptest.NewRecorder()
	handleRecordingMetadataRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (corrupt metadata is reported, not a request error)", w.Code)
	}
	var body map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &body)
	if corrupt, _ := body["corrupt"].(bool); !corrupt {
		t.Errorf("corrupt flag not set: %v", body)
	}
	if avail, _ := body["available"].(bool); avail {
		t.Error("available = true for corrupt metadata, want false")
	}
}

func TestHandleRecordingMetadataRequest_UnknownRecording(t *testing.T) {
	withTestRecordingsDir(t)
	req := httptest.NewRequest(http.MethodGet, "/getRecordingMetadata?id=rec-19990101T000000Z", nil)
	w := httptest.NewRecorder()
	handleRecordingMetadataRequest(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a well-formed but nonexistent id", w.Code)
	}
}

func TestHandleRecordingMetadataRequest_InvalidID(t *testing.T) {
	withTestRecordingsDir(t)
	for _, id := range []string{"", "../../etc/passwd", "rec-123", "not-a-recording"} {
		req := httptest.NewRequest(http.MethodGet, "/getRecordingMetadata?id="+id, nil)
		w := httptest.NewRecorder()
		handleRecordingMetadataRequest(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("id=%q: status = %d, want 400", id, w.Code)
		}
	}
}

func TestHandleRecordingMetadataRequest_WrongMethod(t *testing.T) {
	withTestRecordingsDir(t)
	req := httptest.NewRequest(http.MethodPost, "/getRecordingMetadata?id=rec-20260101T000000Z", nil)
	w := httptest.NewRecorder()
	handleRecordingMetadataRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleDownloadRecordingMetadataRequest_ValidRecording(t *testing.T) {
	startTestRecording(t)
	id := currentSessionID(t)

	req := httptest.NewRequest(http.MethodGet, "/downloadRecordingMetadata?id="+id, nil)
	w := httptest.NewRecorder()
	handleDownloadRecordingMetadataRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	wantDisposition := "attachment; filename=" + id + ".metadata.json"
	if cd := w.Header().Get("Content-Disposition"); cd != wantDisposition {
		t.Errorf("Content-Disposition = %q, want %q", cd, wantDisposition)
	}
	var meta recording.SessionMetadata
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatalf("downloaded body is not valid JSON: %v", err)
	}
	if meta.RecordingID != id {
		t.Errorf("downloaded RecordingID = %q, want %q", meta.RecordingID, id)
	}
}

func TestHandleDownloadRecordingMetadataRequest_LegacyRecordingIs404(t *testing.T) {
	withTestRecordingsDir(t)
	dir := filepath.Join(recordingsDir, "rec-20200101T000000Z")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/downloadRecordingMetadata?id=rec-20200101T000000Z", nil)
	w := httptest.NewRecorder()
	handleDownloadRecordingMetadataRequest(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a legacy recording with nothing to download", w.Code)
	}
}

func TestHandleDownloadRecordingMetadataRequest_PathTraversalRejected(t *testing.T) {
	withTestRecordingsDir(t)
	for _, id := range []string{"../../etc/passwd", "rec-20260101T000000Z/../../etc/passwd"} {
		req := httptest.NewRequest(http.MethodGet, "/downloadRecordingMetadata?id="+id, nil)
		w := httptest.NewRecorder()
		handleDownloadRecordingMetadataRequest(w, req)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Errorf("id=%q: status = %d, want 400 or 404, never a successful traversal", id, w.Code)
		}
	}
}

func TestHandleDownloadRecordingMetadataRequest_WrongMethod(t *testing.T) {
	withTestRecordingsDir(t)
	req := httptest.NewRequest(http.MethodPost, "/downloadRecordingMetadata?id=rec-20260101T000000Z", nil)
	w := httptest.NewRecorder()
	handleDownloadRecordingMetadataRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}
