package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stratux/stratux/wifiadmin"
)

// withTestWifiAdminPersistencePaths redirects both persistence files to
// a fresh temp directory for the duration of one test - this file's own
// tests exercise the REAL filePersistence implementation and the REAL
// filesystem (a temp dir, never PersistentDataPath), unlike
// wifiadminapi_test.go's fakePersistence-backed tests, which never touch
// a disk at all.
func withTestWifiAdminPersistencePaths(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origGood, origPending := wifiAdminLastKnownGoodPath, wifiAdminPendingPath
	wifiAdminLastKnownGoodPath = filepath.Join(dir, "wifi-admin-last-known-good.json")
	wifiAdminPendingPath = filepath.Join(dir, "wifi-admin-pending-transaction.json")
	t.Cleanup(func() {
		wifiAdminLastKnownGoodPath, wifiAdminPendingPath = origGood, origPending
	})
	return dir
}

func TestFilePersistence_LastKnownGood_RoundTrip(t *testing.T) {
	withTestWifiAdminPersistencePaths(t)
	p := filePersistence{}

	cfg := wifiadmin.DefaultConfig()
	cfg.SSID = "real-fs-test-ssid"
	if err := p.SaveLastKnownGood(cfg); err != nil {
		t.Fatalf("SaveLastKnownGood: %v", err)
	}
	got, ok, err := p.LoadLastKnownGood()
	if err != nil || !ok {
		t.Fatalf("LoadLastKnownGood: ok=%v err=%v", ok, err)
	}
	if got.SSID != "real-fs-test-ssid" {
		t.Errorf("SSID = %q, want real-fs-test-ssid", got.SSID)
	}
}

func TestFilePersistence_PendingTransaction_RoundTrip(t *testing.T) {
	withTestWifiAdminPersistencePaths(t)
	p := filePersistence{}

	rec := wifiadmin.PendingTransactionRecord{
		BootSessionID:  "boot-xyz",
		ProposedConfig: wifiadmin.DefaultConfig(),
		PreviousConfig: wifiadmin.DefaultConfig(),
		Stage:          wifiadmin.StageAwaitingReconnection,
	}
	if err := p.SavePendingTransaction(rec); err != nil {
		t.Fatalf("SavePendingTransaction: %v", err)
	}
	got, ok, err := p.LoadPendingTransaction()
	if err != nil || !ok {
		t.Fatalf("LoadPendingTransaction: ok=%v err=%v", ok, err)
	}
	if got.BootSessionID != "boot-xyz" || got.Stage != wifiadmin.StageAwaitingReconnection {
		t.Errorf("loaded record = %+v", got)
	}

	if err := p.ClearPendingTransaction(); err != nil {
		t.Fatalf("ClearPendingTransaction: %v", err)
	}
	if _, ok, _ := p.LoadPendingTransaction(); ok {
		t.Error("expected no pending transaction after Clear")
	}
}

func TestFilePersistence_ClearPendingTransaction_MissingFileIsNoOp(t *testing.T) {
	withTestWifiAdminPersistencePaths(t)
	p := filePersistence{}
	if err := p.ClearPendingTransaction(); err != nil {
		t.Errorf("ClearPendingTransaction on a never-created file: %v", err)
	}
}

func TestFilePersistence_LoadMissingFile_ReturnsOkFalseNotError(t *testing.T) {
	withTestWifiAdminPersistencePaths(t)
	p := filePersistence{}
	_, ok, err := p.LoadLastKnownGood()
	if err != nil {
		t.Errorf("a missing file must not be an error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a missing file")
	}
}

func TestFilePersistence_CorruptFile_IsAnErrorNotSilentlyAbsent(t *testing.T) {
	// Required invariant (Phase 4.2): the system must never silently
	// treat corrupt/mixed state as absent - a corrupt last-known-good
	// file must surface as an error, not fall back to DefaultConfig()
	// as if nothing had ever been persisted.
	withTestWifiAdminPersistencePaths(t)
	if err := os.WriteFile(wifiAdminLastKnownGoodPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := filePersistence{}
	_, ok, err := p.LoadLastKnownGood()
	if err == nil {
		t.Fatal("expected an error for a corrupt file, got nil")
	}
	if ok {
		t.Error("a corrupt file must never report ok=true")
	}
}

func TestFilePersistence_EmptyFile_TreatedAsAbsent(t *testing.T) {
	// A zero-byte file is this project's own established convention for
	// "interrupted before any content was written" (see saveAlertSettings's
	// sibling file for the same idiom) - distinct from a corrupt
	// (non-empty but unparseable) file, which IS an error above.
	withTestWifiAdminPersistencePaths(t)
	if err := os.WriteFile(wifiAdminLastKnownGoodPath, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	p := filePersistence{}
	_, ok, err := p.LoadLastKnownGood()
	if err != nil {
		t.Errorf("an empty file should not be an error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for an empty file")
	}
}

func TestFilePersistence_StaleTempFile_DoesNotCorruptFreshWrite(t *testing.T) {
	// Simulates "interrupted temporary file" (Phase 4.3): a .tmp file
	// left over from a prior crash mid-write must not survive into, or
	// corrupt, the next successful write - atomicWriteJSON always
	// truncates its own .tmp path fresh (os.O_TRUNC) before writing.
	withTestWifiAdminPersistencePaths(t)
	staleTmp := wifiAdminLastKnownGoodPath + ".tmp"
	if err := os.WriteFile(staleTmp, []byte("garbage-from-a-past-crash"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := filePersistence{}
	cfg := wifiadmin.DefaultConfig()
	cfg.SSID = "fresh-write-after-stale-tmp"
	if err := p.SaveLastKnownGood(cfg); err != nil {
		t.Fatalf("SaveLastKnownGood: %v", err)
	}
	got, ok, err := p.LoadLastKnownGood()
	if err != nil || !ok {
		t.Fatalf("LoadLastKnownGood: ok=%v err=%v", ok, err)
	}
	if got.SSID != "fresh-write-after-stale-tmp" {
		t.Errorf("SSID = %q - stale .tmp content leaked into the real file", got.SSID)
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Error("expected the .tmp file to be gone (renamed away) after a successful save")
	}
}

func TestFilePersistence_WriteFailure_LeavesExistingFileUntouched(t *testing.T) {
	// Proves atomicity from the real filesystem's own perspective: if a
	// write fails partway, the PREVIOUSLY persisted (good) content must
	// still be exactly what LoadLastKnownGood returns - never a torn or
	// partially-written file, and never silently "no last known good."
	withTestWifiAdminPersistencePaths(t)
	p := filePersistence{}

	good := wifiadmin.DefaultConfig()
	good.SSID = "original-good-config"
	if err := p.SaveLastKnownGood(good); err != nil {
		t.Fatalf("initial SaveLastKnownGood: %v", err)
	}

	// Force the next write to fail in a way that doesn't depend on
	// permission bits (this test suite may run as root, which bypasses
	// them): occupy atomicWriteJSON's own ".tmp" path with a directory,
	// so its os.Create of that exact path fails regardless of UID.
	tmpPath := wifiAdminLastKnownGoodPath + ".tmp"
	if err := os.Mkdir(tmpPath, 0o755); err != nil {
		t.Fatalf("Mkdir(%s): %v", tmpPath, err)
	}
	defer os.Remove(tmpPath)

	bad := wifiadmin.DefaultConfig()
	bad.SSID = "this-write-should-fail"
	if err := p.SaveLastKnownGood(bad); err == nil {
		t.Fatal("expected the write to fail when its .tmp path is occupied by a directory")
	}

	got, ok, loadErr := p.LoadLastKnownGood()
	if loadErr != nil || !ok {
		t.Fatalf("LoadLastKnownGood after failed write: ok=%v err=%v", ok, loadErr)
	}
	if got.SSID != "original-good-config" {
		t.Errorf("last known good = %q after a failed write, want the untouched original %q", got.SSID, "original-good-config")
	}
}

func TestFilePersistence_ConcurrentSaves_NoTornWrite(t *testing.T) {
	// wifiAdminPersistenceMu serializes every Save/Load call - this test
	// proves concurrent callers never observe a torn (partially-written,
	// unparseable) file, only ever one complete generation or another.
	withTestWifiAdminPersistencePaths(t)
	p := filePersistence{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg := wifiadmin.DefaultConfig()
			cfg.SSID = "concurrent-" + itoaTest(i)
			_ = p.SaveLastKnownGood(cfg)
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(wifiAdminLastKnownGoodPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var cfg wifiadmin.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("final file is not valid JSON (torn write): %v\ncontent: %s", err, data)
	}
}

// TestWifiAdminRealPersistence_CrashMidTransaction_RecoversOnRestart is
// this feature's central Phase-4 proof: it exercises wifiadmin.NewManager
// (the real function main/wifiadminapi.go's own InitializeWifiAdmin
// calls at boot) against the REAL filePersistence implementation and
// REAL JSON files on disk - not wifiadmin's own fakePersistence
// in-memory map, and not this file's own withTestWifiAdminPersistencePaths
// helper's simpler round-trip tests. It simulates a process that died
// after Apply() wrote a new live configuration but before the owner
// ever confirmed reconnection, then proves a fresh startup against the
// SAME on-disk files deterministically rolls back to a complete,
// self-consistent last-known-good generation - never a mixed or
// half-applied one - and that a SECOND subsequent restart against the
// now-clean files is a true no-op (no rollback loop).
func TestWifiAdminRealPersistence_CrashMidTransaction_RecoversOnRestart(t *testing.T) {
	withTestWifiAdminPersistencePaths(t)
	p := filePersistence{}

	good := wifiadmin.DefaultConfig()
	good.SSID = "original-good-ssid"
	if err := p.SaveLastKnownGood(good); err != nil {
		t.Fatalf("seed SaveLastKnownGood: %v", err)
	}

	bad := wifiadmin.DefaultConfig()
	bad.SSID = "abandoned-mid-transaction-ssid"
	if err := p.SavePendingTransaction(wifiadmin.PendingTransactionRecord{
		BootSessionID:  "boot-that-crashed",
		ProposedConfig: bad,
		PreviousConfig: good,
		Stage:          wifiadmin.StageAwaitingReconnection,
	}); err != nil {
		t.Fatalf("seed SavePendingTransaction: %v", err)
	}

	// The abandoned config is what the interrupted Apply left "live" -
	// exactly what startup recovery must overwrite, never trust.
	exec := &fakeWifiExecutorForAPI{live: bad}

	mgr, err := wifiadmin.NewManager("boot-after-crash", monotonicSeconds, exec, p, nil)
	if err != nil {
		t.Fatalf("NewManager (simulated restart): %v", err)
	}

	status := mgr.Status()
	if status.Stage != wifiadmin.StageIdle {
		t.Errorf("stage after recovery = %v, want idle", status.Stage)
	}
	if status.LastResult != wifiadmin.ResultRolledBack {
		t.Errorf("lastResult = %v, want rolled_back", status.LastResult)
	}
	if status.LastKnownGood.SSID != "original-good-ssid" {
		t.Errorf("Manager's own lastKnownGood = %q, want the original", status.LastKnownGood.SSID)
	}
	if exec.live.SSID != "original-good-ssid" {
		t.Errorf("executor's live config = %q after recovery, want the original restored", exec.live.SSID)
	}

	// Verify against the REAL files directly, not just through the
	// Manager's own in-memory view - proves the on-disk generation
	// itself is complete and self-consistent, and the journal (pending
	// transaction) is truly gone, not merely reported gone by a mock.
	diskGood, ok, err := p.LoadLastKnownGood()
	if err != nil || !ok {
		t.Fatalf("re-reading last-known-good from disk: ok=%v err=%v", ok, err)
	}
	if diskGood.SSID != "original-good-ssid" {
		t.Errorf("on-disk last-known-good SSID = %q after recovery, want the original", diskGood.SSID)
	}
	if _, err := os.Stat(wifiAdminPendingPath); !os.IsNotExist(err) {
		t.Errorf("expected the pending-transaction journal file to be gone from disk after recovery, stat err = %v", err)
	}

	// A second restart against the now-clean real files must be a true
	// no-op: no pending record survives, so recovery must not run again
	// and the executor must not be touched a second time.
	exec2 := &fakeWifiExecutorForAPI{live: good}
	mgr2, err := wifiadmin.NewManager("boot-after-second-restart", monotonicSeconds, exec2, p, nil)
	if err != nil {
		t.Fatalf("NewManager (second simulated restart): %v", err)
	}
	status2 := mgr2.Status()
	if status2.Stage != wifiadmin.StageIdle {
		t.Errorf("stage after a clean restart = %v, want idle", status2.Stage)
	}
	if status2.LastResult == wifiadmin.ResultRolledBack {
		t.Error("a second, clean restart must not report a rollback that never happened")
	}
	if exec2.applyCount() != 0 {
		t.Error("a restart with no pending transaction must never call the executor")
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
