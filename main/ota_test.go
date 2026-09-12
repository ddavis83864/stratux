package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stratux/stratux/ota"
)

// withResetOTATestEnv wires a temp OTA directory and a clean recording/
// restore state for the duration of one test - mirrors
// withConfigBackupTestEnv's otaDir redirection (configbackupapi_test.go),
// reused here in the opposite direction: those tests redirect otaDir to
// keep OTA out of the way of restore tests, these redirect it to actually
// exercise /resetOTA.
func withResetOTATestEnv(t *testing.T) {
	t.Helper()
	origOTADir := otaDir
	t.Cleanup(func() { otaDir = origOTADir })
	otaDir = t.TempDir()

	origRecCurrent := recCurrent
	t.Cleanup(func() { recCurrent = origRecCurrent })
	recCurrent = nil

	origConfigBackupState := configBackupState
	t.Cleanup(func() { configBackupState = origConfigBackupState })
	configBackupState = restoreStateIdle
}

func postResetOTA(t *testing.T) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/resetOTA", nil)
	rec := httptest.NewRecorder()
	handleResetOTARequest(rec, req)
	return rec, decodeJSONBody(t, rec)
}

func TestHandleResetOTA_WrongMethodRejected(t *testing.T) {
	withResetOTATestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/resetOTA", nil)
	rec := httptest.NewRecorder()
	handleResetOTARequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should be rejected with 405, got %d", rec.Code)
	}
}

func TestHandleResetOTA_IdleIsNoOpNotError(t *testing.T) {
	withResetOTATestEnv(t)
	// No state.json at all -> ota.LoadState reports StageIdle.
	rec, body := postResetOTA(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("idle reset should be 200, got %d: %v", rec.Code, body)
	}
	if body["success"] != true || body["reset"] != false {
		t.Errorf("idle reset should report success=true, reset=false, got %v", body)
	}
}

func TestHandleResetOTA_MidFlightStageIsRejected(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	for _, stage := range []ota.Stage{
		ota.StageStaged, ota.StageDisableRequested, ota.StageInstalling,
		ota.StageInstalled, ota.StageVerifying,
	} {
		s := ota.NewState("/x/stratux.deb", "hash", "2.0-pre5", "commit", now)
		s.Stage = stage
		if err := ota.SaveState(otaDir, s, now); err != nil {
			t.Fatalf("SaveState: %v", err)
		}
		rec, body := postResetOTA(t)
		if rec.Code != http.StatusConflict {
			t.Errorf("stage %s should be rejected with 409, got %d: %v", stage, rec.Code, body)
		}
		if body["success"] != false {
			t.Errorf("stage %s: expected success=false, got %v", stage, body)
		}
	}
}

func TestHandleResetOTA_FailedStageIsReset(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	s := ota.NewState("/x/stratux.deb", "hash", "2.0-pre5", "commit", now)
	s = s.EnterFailed("overlayctl lock: exit status 32: mount point is busy")
	if err := ota.SaveState(otaDir, s, now); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	rec, body := postResetOTA(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed-stage reset should be 200, got %d: %v", rec.Code, body)
	}
	if body["success"] != true || body["reset"] != true {
		t.Errorf("expected success=true, reset=true, got %v", body)
	}

	got, err := ota.LoadState(otaDir)
	if err != nil {
		t.Fatalf("LoadState after reset: %v", err)
	}
	if got.Stage != ota.StageIdle {
		t.Errorf("expected StageIdle after reset, got %q", got.Stage)
	}
}

func TestHandleResetOTA_RecoveryExhaustedStageIsReset(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	s := ota.NewState("/x/stratux.deb", "hash", "2.0-pre5", "commit", now)
	s = s.EnterFailed("overlayctl lock: exit status 32: mount point is busy")
	s.Recovery.Attempts = ota.MaxRecoveryAttempts
	s.Stage = ota.StageRecoveryExhausted
	if err := ota.SaveState(otaDir, s, now); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	rec, body := postResetOTA(t)
	if rec.Code != http.StatusOK || body["reset"] != true {
		t.Fatalf("recovery-exhausted reset should succeed, got %d: %v", rec.Code, body)
	}
	got, _ := ota.LoadState(otaDir)
	if got.Stage != ota.StageIdle {
		t.Errorf("expected StageIdle after reset, got %q", got.Stage)
	}
}

func TestHandleResetOTA_PreservesPreResetStateForDiagnosis(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	s := ota.NewState("/x/stratux.deb", "hash", "2.0-pre5", "commit", now)
	s = s.EnterFailed("overlayctl lock: exit status 32: mount point is busy")
	if err := ota.SaveState(otaDir, s, now); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	_, body := postResetOTA(t)
	preservedAt, _ := body["preservedAt"].(string)
	if preservedAt == "" {
		t.Fatalf("response did not report where the pre-reset state was preserved: %v", body)
	}
	data, err := os.ReadFile(preservedAt)
	if err != nil {
		t.Fatalf("preserved pre-reset state file not found at %s: %v", preservedAt, err)
	}
	if len(data) == 0 {
		t.Error("preserved pre-reset state file is empty")
	}
}

func TestHandleResetOTA_DoesNotDeleteStagedOrBackupFiles(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	if err := os.MkdirAll(otaStagedDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll staged: %v", err)
	}
	if err := os.MkdirAll(otaBackupDir(), 0o755); err != nil {
		t.Fatalf("MkdirAll backup: %v", err)
	}
	stagedFile := filepath.Join(otaStagedDir(), "stratux.deb")
	backupFile := filepath.Join(otaBackupDir(), "pre-install-backup.tar.gz")
	if err := os.WriteFile(stagedFile, []byte("deb"), 0o644); err != nil {
		t.Fatalf("write staged: %v", err)
	}
	if err := os.WriteFile(backupFile, []byte("backup"), 0o644); err != nil {
		t.Fatalf("write backup: %v", err)
	}

	s := ota.NewState(stagedFile, "hash", "2.0-pre5", "commit", now)
	s = s.EnterFailed("overlayctl lock: exit status 32: mount point is busy")
	s.BackupPath = backupFile
	if err := ota.SaveState(otaDir, s, now); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if _, body := postResetOTA(t); body["reset"] != true {
		t.Fatalf("reset did not succeed: %v", body)
	}
	if _, err := os.Stat(stagedFile); err != nil {
		t.Errorf("staged package was deleted by reset (should only clear the state pointer, not the artifact): %v", err)
	}
	if _, err := os.Stat(backupFile); err != nil {
		t.Errorf("pre-install backup was deleted by reset: %v", err)
	}
}

func TestHandleResetOTA_RejectsWhileRecordingActive(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	s := ota.NewState("/x/stratux.deb", "hash", "2.0-pre5", "commit", now)
	s = s.EnterFailed("overlayctl lock: exit status 32: mount point is busy")
	if err := ota.SaveState(otaDir, s, now); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	recCurrent = &recordingSession{State: recordingStateActive}

	rec, body := postResetOTA(t)
	if rec.Code != http.StatusConflict {
		t.Errorf("reset during an active recording should be 409, got %d: %v", rec.Code, body)
	}
	got, _ := ota.LoadState(otaDir)
	if got.Stage != ota.StageFailed {
		t.Errorf("a rejected reset must not mutate state; still expected StageFailed, got %q", got.Stage)
	}
}

func TestHandleResetOTA_RejectsWhileConfigRestoreInProgress(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	s := ota.NewState("/x/stratux.deb", "hash", "2.0-pre5", "commit", now)
	s = s.EnterFailed("overlayctl lock: exit status 32: mount point is busy")
	if err := ota.SaveState(otaDir, s, now); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	configBackupState = restoreStateApplying

	rec, body := postResetOTA(t)
	if rec.Code != http.StatusConflict {
		t.Errorf("reset during an in-progress restore should be 409, got %d: %v", rec.Code, body)
	}
}

func TestHandleResetOTA_RepeatedResetIsIdempotent(t *testing.T) {
	withResetOTATestEnv(t)
	now := time.Now().UTC()
	s := ota.NewState("/x/stratux.deb", "hash", "2.0-pre5", "commit", now)
	s = s.EnterFailed("overlayctl lock: exit status 32: mount point is busy")
	if err := ota.SaveState(otaDir, s, now); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	if rec, body := postResetOTA(t); rec.Code != http.StatusOK || body["reset"] != true {
		t.Fatalf("first reset should succeed, got %d: %v", rec.Code, body)
	}
	// Second call, now idle: must be a safe no-op, not an error.
	rec, body := postResetOTA(t)
	if rec.Code != http.StatusOK || body["reset"] != false {
		t.Errorf("repeated reset should be an idempotent no-op (200, reset=false), got %d: %v", rec.Code, body)
	}
}
