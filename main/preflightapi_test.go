package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stratux/stratux/preflight"
	"github.com/stratux/stratux/readiness"
)

// withTestPreflightStore points the package-level preflight state at a
// fresh store/session for the duration of one test, and restores the
// previous value afterward - mirrors withTestProfilesStore's pattern in
// main/calprofilesapi_test.go.
func withTestPreflightStore(t *testing.T) (*preflight.ManualAckStore, string) {
	t.Helper()
	origStore := preflightAckStore
	origSession := preflightSessionID
	sessionID := "test-session"
	store := preflight.NewManualAckStore(sessionID, func() time.Time { return stratuxClock.Time() })
	preflightMu.Lock()
	preflightAckStore = store
	preflightSessionID = sessionID
	preflightMu.Unlock()
	t.Cleanup(func() {
		preflightMu.Lock()
		preflightAckStore = origStore
		preflightSessionID = origSession
		preflightMu.Unlock()
	})
	return store, sessionID
}

func TestHandlePreflightReportRequest_ValidRequest(t *testing.T) {
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	withTestPreflightStore(t)

	req := httptest.NewRequest(http.MethodGet, "/getPreflightReport", nil)
	w := httptest.NewRecorder()
	handlePreflightReportRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var r preflight.Report
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("response is not a valid preflight.Report: %v", err)
	}
	if !r.Overall.Valid() {
		t.Errorf("Overall = %q is not a valid preflight.State", r.Overall)
	}
	if r.SchemaVersion != preflight.SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", r.SchemaVersion, preflight.SchemaVersion)
	}
	if len(r.Manual) != len(preflight.ManualCheckDefinitions) {
		t.Errorf("len(Manual) = %d, want %d", len(r.Manual), len(preflight.ManualCheckDefinitions))
	}
}

func TestHandlePreflightReportRequest_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/getPreflightReport", nil)
	w := httptest.NewRecorder()
	handlePreflightReportRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleAcknowledgePreflightCheckRequest_ValidQueryParam(t *testing.T) {
	ensureStratuxClockForTest()
	store, _ := withTestPreflightStore(t)

	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck?id="+string(preflight.CheckAntennasAttached), nil)
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	snap := store.Snapshot(preflight.DefaultAckExpiration)
	if snap[preflight.CheckAntennasAttached] == nil {
		t.Error("check was not recorded as acknowledged")
	}
}

func TestHandleAcknowledgePreflightCheckRequest_ValidJSONBody(t *testing.T) {
	ensureStratuxClockForTest()
	store, _ := withTestPreflightStore(t)

	body, _ := json.Marshal(map[string]string{"id": string(preflight.CheckMountSecure)})
	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if store.Snapshot(preflight.DefaultAckExpiration)[preflight.CheckMountSecure] == nil {
		t.Error("check was not recorded as acknowledged")
	}
}

func TestHandleAcknowledgePreflightCheckRequest_InvalidJSON(t *testing.T) {
	ensureStratuxClockForTest()
	withTestPreflightStore(t)

	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck", strings.NewReader("{not valid json"))
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAcknowledgePreflightCheckRequest_MissingField(t *testing.T) {
	ensureStratuxClockForTest()
	withTestPreflightStore(t)

	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck", strings.NewReader("{}"))
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestHandleAcknowledgePreflightCheckRequest_OversizedPayload(t *testing.T) {
	ensureStratuxClockForTest()
	withTestPreflightStore(t)

	huge := `{"id":"` + strings.Repeat("a", maxPreflightRequestBytes+1000) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck", strings.NewReader(huge))
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (oversized body should fail to decode once truncated by MaxBytesReader)", w.Code)
	}
}

func TestHandleAcknowledgePreflightCheckRequest_UnknownCheck(t *testing.T) {
	ensureStratuxClockForTest()
	withTestPreflightStore(t)

	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck?id=not_a_real_check", nil)
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestHandleAcknowledgePreflightCheckRequest_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/acknowledgePreflightCheck?id="+string(preflight.CheckAntennasAttached), nil)
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", w.Code)
	}
}

func TestHandleAcknowledgePreflightCheckRequest_Idempotent(t *testing.T) {
	ensureStratuxClockForTest()
	store, _ := withTestPreflightStore(t)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck?id="+string(preflight.CheckAntennasAttached), nil)
		w := httptest.NewRecorder()
		handleAcknowledgePreflightCheckRequest(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i, w.Code)
		}
	}
	if store.Snapshot(preflight.DefaultAckExpiration)[preflight.CheckAntennasAttached] == nil {
		t.Error("expected the check to remain acknowledged after repeated acknowledgement")
	}
}

func TestHandleAcknowledgePreflightCheckRequest_BootSessionBound(t *testing.T) {
	ensureStratuxClockForTest()
	store, session := withTestPreflightStore(t)

	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck?id="+string(preflight.CheckAntennasAttached), nil)
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if store.SessionID() != session {
		t.Errorf("SessionID() = %q, want %q", store.SessionID(), session)
	}
}

func TestHandleClearPreflightCheckRequest(t *testing.T) {
	ensureStratuxClockForTest()
	store, _ := withTestPreflightStore(t)
	store.Ack(preflight.CheckAntennasAttached, nil)

	req := httptest.NewRequest(http.MethodPost, "/clearPreflightCheck?id="+string(preflight.CheckAntennasAttached), nil)
	w := httptest.NewRecorder()
	handleClearPreflightCheckRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if store.Snapshot(preflight.DefaultAckExpiration)[preflight.CheckAntennasAttached] != nil {
		t.Error("check should be cleared")
	}
}

func TestHandleResetPreflightChecksRequest(t *testing.T) {
	ensureStratuxClockForTest()
	store, _ := withTestPreflightStore(t)
	store.Ack(preflight.CheckAntennasAttached, nil)
	store.Ack(preflight.CheckMountSecure, nil)

	req := httptest.NewRequest(http.MethodPost, "/resetPreflightChecks", nil)
	w := httptest.NewRecorder()
	handleResetPreflightChecksRequest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	snap := store.Snapshot(preflight.DefaultAckExpiration)
	for id, a := range snap {
		if a != nil {
			t.Errorf("check %s still acknowledged after reset", id)
		}
	}
}

func TestPreflightHandlers_NoStoreInitialized(t *testing.T) {
	preflightMu.Lock()
	origStore := preflightAckStore
	preflightAckStore = nil
	preflightMu.Unlock()
	t.Cleanup(func() {
		preflightMu.Lock()
		preflightAckStore = origStore
		preflightMu.Unlock()
	})

	req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck?id="+string(preflight.CheckAntennasAttached), nil)
	w := httptest.NewRecorder()
	handleAcknowledgePreflightCheckRequest(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when the preflight store is not initialized", w.Code)
	}
}

func TestBuildPreflightReport_FailureIsolation_ZeroValueHealth(t *testing.T) {
	// A realistic degenerate case: buildPreflightReport is called before
	// the first updateHealth() tick has ever populated globalHealth (or
	// in a test that never sets it up) - this must produce an honest
	// report, never panic and never crash the caller.
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	withTestPreflightStore(t)

	origHealth := globalHealth
	globalHealthMutex.Lock()
	globalHealth = readiness.HealthReport{}
	globalHealthMutex.Unlock()
	defer func() {
		globalHealthMutex.Lock()
		globalHealth = origHealth
		globalHealthMutex.Unlock()
	}()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("buildPreflightReport panicked on zero-value health: %v", r)
		}
	}()
	report := buildPreflightReport()
	if !report.Overall.Valid() {
		t.Errorf("Overall = %q is not a valid preflight.State even in the degenerate zero-value case", report.Overall)
	}
}

func TestPopulateSessionPreflightSummary(t *testing.T) {
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	withTestPreflightStore(t)

	// A zero-value globalHealth degrades most checks to NOT_READY/UNKNOWN
	// with zero manual acknowledgements - this should read as NOT_READY
	// overall with an incomplete manual checklist, and the summary must
	// reflect exactly that without touching the recording subsystem.
	session := &recordingSession{}
	populateSessionPreflightSummary(session)

	if session.PreflightOverallState == "" {
		t.Error("PreflightOverallState was not populated")
	}
	if session.PreflightManualChecksComplete {
		t.Error("PreflightManualChecksComplete = true with zero acknowledgements, want false")
	}
}

func TestPopulateSessionPreflightSummary_AllManualChecksAcknowledged(t *testing.T) {
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	store, _ := withTestPreflightStore(t)
	for _, d := range preflight.ManualCheckDefinitions {
		store.Ack(d.ID, nil)
	}

	session := &recordingSession{}
	populateSessionPreflightSummary(session)

	if !session.PreflightManualChecksComplete {
		t.Error("PreflightManualChecksComplete = false with every manual check acknowledged, want true")
	}
}

func TestDiagnosticBundle_IncludesSanitizedPreflightReport(t *testing.T) {
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	withTestPreflightStore(t)

	report := buildPreflightReport()
	bundle := readiness.BuildDiagnosticBundle(time.Now().UTC(), "v", "c", globalHealth, nil, nil, nil, "", report, nil, nil, nil, nil, nil, nil, nil, nil)

	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("bundle with a preflight report failed to marshal: %v", err)
	}
	var roundTrip struct {
		PreflightReport preflight.Report `json:"PreflightReport"`
	}
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("could not round-trip PreflightReport out of the bundle: %v", err)
	}
	if roundTrip.PreflightReport.SchemaVersion != preflight.SchemaVersion {
		t.Error("PreflightReport did not round-trip through the diagnostic bundle correctly")
	}

	lower := strings.ToLower(string(data))
	for _, forbidden := range []string{"password", "wifipassphrase", "ssh-rsa", "ssh-ed25519", "begin rsa private key"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("diagnostic bundle containing a preflight report leaked a forbidden substring: %q", forbidden)
		}
	}
}

// TestBuildPreflightReport_DoesNotDeadlockWhileRecMuHeld reproduces a real
// deadlock caught live during hardware validation:
// handleStartRecordingRequest used to call populateSessionPreflightSummary
// (-> buildPreflightReport -> recordingReadinessForPreflight) while still
// holding recMu - and recordingReadinessForPreflight locked recMu itself,
// which never returns on a plain sync.Mutex.Lock() because Go mutexes are
// not reentrant. main/recordingapi.go now computes the preflight snapshot
// before acquiring recMu (see applyPreflightSummaryToSession), and
// recordingReadinessForPreflight uses TryLock as defense in depth - this
// test exercises that defense directly, simulating "called while recMu is
// already held by this goroutine," and requires the call to return
// promptly rather than hang.
func TestBuildPreflightReport_DoesNotDeadlockWhileRecMuHeld(t *testing.T) {
	ensureSituationLocks()
	ensureADSBTowerMutexForTest()
	ensureStratuxClockForTest()
	withTestProfilesStore(t)
	withTestPreflightStore(t)

	recMu.Lock()
	defer recMu.Unlock()

	done := make(chan preflight.Report, 1)
	go func() { done <- buildPreflightReport() }()
	select {
	case r := <-done:
		found := false
		for _, c := range r.Automated {
			if c.CheckID == "recording_state" {
				found = true
				if c.Reason != "state: unknown" {
					t.Errorf("recording_state Reason = %q while recMu was held by another goroutine, want \"state: unknown\"", c.Reason)
				}
			}
		}
		if !found {
			t.Error("recording_state check not found")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("buildPreflightReport() did not return within 3s while recMu was held by another goroutine - this is the exact deadlock caught during hardware validation")
	}
}

func TestPreflightAPI_ThreadSafety(t *testing.T) {
	ensureStratuxClockForTest()
	withTestPreflightStore(t)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				req := httptest.NewRequest(http.MethodPost, "/acknowledgePreflightCheck?id="+string(preflight.CheckAntennasAttached), nil)
				w := httptest.NewRecorder()
				handleAcknowledgePreflightCheckRequest(w, req)

				req2 := httptest.NewRequest(http.MethodPost, "/clearPreflightCheck?id="+string(preflight.CheckMountSecure), nil)
				w2 := httptest.NewRecorder()
				handleClearPreflightCheckRequest(w2, req2)
			}
		}()
	}
	wg.Wait()
}
