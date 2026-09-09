/*
autorecordstorage_test.go: proves the main-to-autorecord storage-denial
wiring (autoRecordStorageDecision, in autorecordstorage.go) end to end -
the existing autorecord.Machine-level tests (TestMachine_StartBlockedBy-
StorageDenied/Unknown, TestMachine_StorageCautionDoesNotBlockStart)
exercise the pure Machine given a fabricated autorecord.StorageDecision
value directly; nothing exercised the adapter that actually derives that
value from a real storagelifecycle.Manager until this file. No test here
consumes real disk space or fills the live storage volume - pressure is
injected via a fake FilesystemPressure callback (the same seam
production wiring uses: main/storagelifecycleapi.go's
storageLifecycleFilesystemPressure), exactly the "controlled test
double" this project's own convention already establishes.
*/
package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/autorecord"
	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/storagelifecycle"
)

// withTestStorageManagerAtPressure installs a Manager backed by a temp
// directory, scanned once, reporting exactly utilizationPercent as its
// whole-filesystem pressure input (via a fake FilesystemPressure
// callback) - mirrors withTestStorageManager in
// storagelifecycleapi_test.go, but additionally wires FilesystemPressure,
// which that helper deliberately leaves nil (so its own tests only ever
// see PressureNormal, regardless of FilesystemThresholds).
func withTestStorageManagerAtPressure(t *testing.T, utilizationPercent float64) {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "diagnostics"), 0o755))
	must(t, os.MkdirAll(filepath.Join(dir, "recordings"), 0o755))

	registry := storagelifecycle.NewRegistry()
	must(t, registry.Register(storagelifecycle.Namespace{
		ID: "diagnostics", Root: filepath.Join(dir, "diagnostics"),
		Criticality: storagelifecycle.CriticalityBounded, ItemKind: storagelifecycle.ItemKindFile,
		AllowedExtensions: []string{".json"},
	}))
	must(t, registry.Register(storagelifecycle.Namespace{
		ID: "recordings", Root: filepath.Join(dir, "recordings"),
		Criticality: storagelifecycle.CriticalityImportant, ItemKind: storagelifecycle.ItemKindDirectory,
	}))

	ensureStratuxClockForTest()
	orig := storageManager
	storageManager = storagelifecycle.NewManager(storagelifecycle.ManagerConfig{
		Registry:             registry,
		Policy:               storagelifecycle.Policy{RequiredConsecutivePressureSamples: 1},
		FS:                   storagelifecycle.NewOSFS(),
		Clock:                monotonicSeconds,
		FilesystemPressure:   func() (float64, bool) { return utilizationPercent, true },
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
	t.Cleanup(func() { storageManager = orig })
	if _, performed := storageManager.Scan(); !performed {
		t.Fatal("expected Scan to actually run")
	}
}

func TestAutoRecordStorageDecision_NoManagerIsUnknown(t *testing.T) {
	orig := storageManager
	storageManager = nil
	t.Cleanup(func() { storageManager = orig })
	if got := autoRecordStorageDecision(); got != autorecord.StorageUnknown {
		t.Fatalf("got %q, want %q when storageManager is nil", got, autorecord.StorageUnknown)
	}
}

func TestAutoRecordStorageDecision_NoInventoryYetIsUnknown(t *testing.T) {
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, "recordings"), 0o755))
	registry := storagelifecycle.NewRegistry()
	must(t, registry.Register(storagelifecycle.Namespace{
		ID: "recordings", Root: filepath.Join(dir, "recordings"),
		Criticality: storagelifecycle.CriticalityImportant, ItemKind: storagelifecycle.ItemKindDirectory,
	}))
	orig := storageManager
	storageManager = storagelifecycle.NewManager(storagelifecycle.ManagerConfig{
		Registry: registry, FS: storagelifecycle.NewOSFS(), Clock: monotonicSeconds,
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
	t.Cleanup(func() { storageManager = orig })
	// Deliberately never call Scan() - HasInventory must still be false.
	if got := autoRecordStorageDecision(); got != autorecord.StorageUnknown {
		t.Fatalf("got %q, want %q before any scan has ever run", got, autorecord.StorageUnknown)
	}
}

func TestAutoRecordStorageDecision_NormalPressureIsAllowed(t *testing.T) {
	withTestStorageManagerAtPressure(t, 10) // well under WarnPercent (80)
	if got := autoRecordStorageDecision(); got != autorecord.StorageAllowed {
		t.Fatalf("got %q, want %q at 10%% utilization", got, autorecord.StorageAllowed)
	}
}

func TestAutoRecordStorageDecision_ElevatedPressureIsCaution(t *testing.T) {
	withTestStorageManagerAtPressure(t, 85) // between WarnPercent (80) and CriticalPercent (90)
	if got := autoRecordStorageDecision(); got != autorecord.StorageCaution {
		t.Fatalf("got %q, want %q at 85%% utilization", got, autorecord.StorageCaution)
	}
}

func TestAutoRecordStorageDecision_CriticalPressureIsDenied(t *testing.T) {
	withTestStorageManagerAtPressure(t, 100) // at/above RecordingProhibitedPercent (95)
	if got := autoRecordStorageDecision(); got != autorecord.StorageDenied {
		t.Fatalf("got %q, want %q at 100%% utilization", got, autorecord.StorageDenied)
	}
}

// TestAutoRecordStorageDecision_NeverEvicts is a structural guardrail,
// not a behavioral one: this feature's one production RecordingLifecycle
// implementation must always report NoAutomaticDeletion() true and must
// never itself delete, evict, or move anything - ReserveSpace only ever
// answers allowed/caution/denied.
func TestAutoRecordStorageDecision_NeverEvicts(t *testing.T) {
	if !(autoRecordLifecycleAdapter{}).NoAutomaticDeletion() {
		t.Fatal("autoRecordLifecycleAdapter must always report NoAutomaticDeletion() == true")
	}
}
