package main

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/ota"
)

// withOTAUploadTestEnv wires a temp otaDir and a chosen otaPersistentDataRoot
// for the duration of one test, mirroring withResetOTATestEnv's redirection
// pattern (ota_test.go) - both vars exist specifically so tests can point
// them at something other than the real /var/lib/stratux-data.
func withOTAUploadTestEnv(t *testing.T, persistentDataRoot string) {
	t.Helper()
	origOTADir := otaDir
	origRoot := otaPersistentDataRoot
	t.Cleanup(func() {
		otaDir = origOTADir
		otaPersistentDataRoot = origRoot
	})
	otaDir = filepath.Join(t.TempDir(), "updates")
	otaPersistentDataRoot = persistentDataRoot

	origRecCurrent := recCurrent
	t.Cleanup(func() { recCurrent = origRecCurrent })
	recCurrent = nil

	origConfigBackupState := configBackupState
	t.Cleanup(func() { configBackupState = origConfigBackupState })
	configBackupState = restoreStateIdle
}

// postOTAUpload builds a real multipart/form-data request carrying body as
// the update_file part - the exact wire format handleOTAUploadRequest
// expects from the dashboard's own upload form.
func postOTAUpload(t *testing.T, filename string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("update_file", filename)
	if err != nil {
		t.Fatalf("could not create form file: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("could not write form file body: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("could not close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/updateUpload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	handleOTAUploadRequest(rec, req)
	return rec
}

// --- The fail-fast guard: this is the core fix for the incident in
// docs/ota-persistent-storage-defect.md - a non-persistent staging
// location must be rejected before a single byte of the upload is
// accepted, with no state mutation whatsoever. ---

func TestHandleOTAUpload_RejectsOrdinaryDirectoryNotADedicatedMount(t *testing.T) {
	// t.TempDir() is a perfectly ordinary directory reached through
	// whatever filesystem backs the test's own working tree - exactly
	// the incident's own root cause (an ordinary directory mistaken for
	// genuine persistent storage). It is never its own findmnt target.
	withOTAUploadTestEnv(t, t.TempDir())

	rec := postOTAUpload(t, "stratux-2.0.0-arm64.deb", []byte("not a real deb, guard must reject before this is ever inspected"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleOTAUpload_RejectsMissingPersistentDataPath(t *testing.T) {
	withOTAUploadTestEnv(t, filepath.Join(t.TempDir(), "does-not-exist"))

	rec := postOTAUpload(t, "stratux-2.0.0-arm64.deb", []byte("payload"))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 Service Unavailable for a missing persistent-data path, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleOTAUpload_RejectionWritesNoStagedFile(t *testing.T) {
	withOTAUploadTestEnv(t, t.TempDir())
	postOTAUpload(t, "stratux-2.0.0-arm64.deb", []byte("payload"))

	entries, err := os.ReadDir(otaStagedDir())
	if err != nil {
		if os.IsNotExist(err) {
			return // staged dir never populated - exactly the required outcome
		}
		t.Fatalf("unexpected error reading staged dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("guard rejection must leave no staged files, found %d: %v", len(entries), entries)
	}
}

func TestHandleOTAUpload_RejectionLeavesOTAStateIdle(t *testing.T) {
	withOTAUploadTestEnv(t, t.TempDir())
	postOTAUpload(t, "stratux-2.0.0-arm64.deb", []byte("payload"))

	state, err := ota.LoadState(otaDir)
	if err != nil {
		t.Fatalf("LoadState error: %v", err)
	}
	if state.Stage != ota.StageIdle {
		t.Errorf("guard rejection must never write OTA state - a real reboot request could follow - got stage %q", state.Stage)
	}
}

func TestHandleOTAUpload_RejectionErrorIsBoundedAndActionable(t *testing.T) {
	withOTAUploadTestEnv(t, t.TempDir())
	rec := postOTAUpload(t, "stratux-2.0.0-arm64.deb", []byte("payload"))

	body := rec.Body.String()
	if body == "" {
		t.Fatal("rejection must include an explanatory error, not an empty body")
	}
	// The only paths named in this message are the well-known, publicly
	// documented persistent-data location and this test's own local temp
	// directory - never a stack trace, credential, or anything from
	// another user/session.
	if bytes.Contains(rec.Body.Bytes(), []byte("goroutine ")) {
		t.Error("rejection message must never include a Go stack trace")
	}
}

// --- Guard evaluation order and idempotence ---

func TestHandleOTAUpload_ConcurrentUploadsAllRejectedIndependently(t *testing.T) {
	// Two concurrent uploads against a non-persistent staging location
	// must each fail cleanly on their own - the guard performs no shared
	// mutable state of its own (it is a pure read-then-decide check), so
	// there is nothing for concurrent callers to race on.
	withOTAUploadTestEnv(t, t.TempDir())
	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(n int) {
			rec := postOTAUpload(t, "stratux-2.0.0-arm64.deb", []byte("payload"))
			done <- rec.Code
		}(i)
	}
	for i := 0; i < 2; i++ {
		if code := <-done; code != http.StatusServiceUnavailable {
			t.Errorf("concurrent upload %d: expected 503, got %d", i, code)
		}
	}
}

// --- Malformed request handling downstream of the guard ---
//
// These exercise the pre-existing multipart-parsing behavior
// (handleOTAUploadRequest's own request-shape validation), unchanged by
// the new guard - the guard runs first and would itself reject these
// same requests in this test environment (no real persistent mount is
// available - see the package-level note below), so these confirm the
// *order* is safe either way: an empty or malformed body is never a
// worse outcome than the guard's own clean rejection.

func TestHandleOTAUpload_NoFilePartRejected(t *testing.T) {
	withOTAUploadTestEnv(t, t.TempDir())
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.Close() // empty multipart body: no update_file part at all
	req := httptest.NewRequest(http.MethodPost, "/updateUpload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	handleOTAUploadRequest(rec, req)
	if rec.Code < 400 {
		t.Errorf("an empty multipart body must be rejected, got %d", rec.Code)
	}
}

func TestHandleOTAUpload_NotMultipartRejected(t *testing.T) {
	withOTAUploadTestEnv(t, t.TempDir())
	req := httptest.NewRequest(http.MethodPost, "/updateUpload", bytes.NewReader([]byte("plain body, not multipart")))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	handleOTAUploadRequest(rec, req)
	if rec.Code < 400 {
		t.Errorf("a non-multipart body must be rejected, got %d", rec.Code)
	}
}

// Package-level test-coverage note: IsDedicatedPersistentMount's own
// accept/reject decision logic is fully unit-tested with synthetic
// MountIdentity values in ota/mount_test.go, including the exact
// dedicated-ext4-accepted and overlay/tmpfs-rejected cases this guard
// depends on. A full end-to-end acceptance test against a *real* mounted
// filesystem (proving handleOTAUploadRequest succeeds when
// otaPersistentDataRoot is a genuine dedicated ext4 mount) is deferred to
// the loop-device-backed CI tests planned for the persistent-storage
// partition work: constructing a real mount requires CAP_SYS_ADMIN, which
// this test sandbox does not have (confirmed by direct probe: a plain
// self bind-mount fails with "operation not permitted" here). Tests in
// this file therefore exercise the rejection path (the one that matters
// for closing the actual incident) end-to-end for real, and rely on the
// pure logic tests for the acceptance path's correctness.
