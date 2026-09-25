package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/stratux/stratux/calprofile"
	"github.com/stratux/stratux/power"
	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/recording"
)

// TestWireProductionPersistenceGuards_SwitchesFromNoOpToRealCheck proves
// wireProductionPersistenceGuards actually replaces every persistence
// namespace's injectable guard with the real
// readiness.EnsurePersistentDir(PersistentDataPath) check, rather than
// silently leaving any of them at their safe-but-inert default - see
// ensurePersistentDataMounted's own doc comment (main/health.go) for why
// this two-state (test-safe no-op / real production check) design
// exists at all.
//
// PersistentDataPath is never a genuine dedicated mount in this test
// environment, so the real check is expected to (correctly) refuse -
// this test is not asserting the mount is somehow present, only that
// wiring occurred at all (a nil guard, or one still returning nil after
// wiring, would mean the switch silently did not happen).
func TestWireProductionPersistenceGuards_SwitchesFromNoOpToRealCheck(t *testing.T) {
	// Every subsystem's guard defaults to a no-op until wired - restore
	// that afterward so this test never leaks state into any test that
	// runs after it in the same binary.
	defer func() {
		ensurePersistentDataMounted = func() error { return nil }
		calprofile.SetPersistenceGuard(func() error { return nil })
		recording.SetPersistenceGuard(func() error { return nil })
		power.SetPersistenceGuard(func() error { return nil })
		readiness.SetDiagnosticsPersistenceGuard(func() error { return nil })
	}()

	if err := ensurePersistentDataMounted(); err != nil {
		t.Fatalf("the default guard must be a no-op before wiring, got: %v", err)
	}

	wireProductionPersistenceGuards()

	err := ensurePersistentDataMounted()
	if err == nil {
		t.Fatal("after wiring, the real check should refuse in a test environment where PersistentDataPath is not a genuine mount")
	}
	if !strings.Contains(err.Error(), PersistentDataPath) {
		t.Errorf("expected the real check's error to mention %q, got: %v", PersistentDataPath, err)
	}

	// Confirm the same wiring actually reached each leaf package too -
	// not just this package's own copy of the check - by exercising
	// each one's real write entry point and confirming it now refuses
	// for the same reason, rather than silently succeeding against its
	// t.TempDir() argument.
	dir := t.TempDir()
	if _, storeErr := recording.NewStore(dir, 0, 0); storeErr == nil {
		t.Error("recording.NewStore should refuse after wireProductionPersistenceGuards switched its guard to the real check")
	}
	profilesStoreTest := calprofile.NewStore(dir)
	if saveErr := profilesStoreTest.Save(calprofile.Profile{ID: calprofile.NewID(), Name: "x", Kind: calprofile.KindUser, SchemaVersion: calprofile.SchemaVersion}); saveErr == nil {
		t.Error("calprofile.Store.Save should refuse after wireProductionPersistenceGuards switched its guard to the real check")
	}
	if markerErr := power.WriteSessionMarkerAtomic(dir+"/session.json", power.SessionMarker{SessionID: "x"}); markerErr == nil {
		t.Error("power.WriteSessionMarkerAtomic should refuse after wireProductionPersistenceGuards switched its guard to the real check")
	}
}

// --- Dynamic mount-state scenarios (mission requirement: "delayed mount,
// failed mount, mount appearing after service startup, and mount
// disappearing during operation") -------------------------------------
//
// These exercise the shared guard mechanism every one of the 9
// previously-unguarded namespaces now uses (calprofile, recording,
// power, readiness diagnostics, plus this package's own settings/export
// writers), using recording.NewStore as the concrete write path - the
// guard check itself, and every namespace's wiring to it, is identical
// regardless of which one is under test, so exercising it once here
// covers the shared foundation directly rather than duplicating the
// same four scenarios nine times over.
//
// The auto-record subsystem additionally has its own namespace-specific
// mount-state handling (main/autorecordrun.go's autoRecordAwaitMountAndReload,
// covered by its own TestAutoRecordAwaitMountAndReload_* tests) for a
// different concern entirely - whether to enable auto-recording at all
// based on the mount's state at startup - which these tests do not
// duplicate.

// TestDynamicMountState_FailedMount_WriteNeverSucceeds simulates a mount
// that never becomes available (nofail took effect, the partition truly
// failed) - every write attempt must be refused, never silently
// succeed against the overlay directory that still exists at the same
// path regardless.
func TestDynamicMountState_FailedMount_WriteNeverSucceeds(t *testing.T) {
	mountErr := errors.New("persistent-data mount failed")
	recording.SetPersistenceGuard(func() error { return mountErr })
	defer recording.SetPersistenceGuard(func() error { return nil })

	for attempt := 0; attempt < 3; attempt++ {
		if _, err := recording.NewStore(t.TempDir(), 0, 0); err == nil || !errors.Is(err, mountErr) {
			t.Fatalf("attempt %d: expected the write to be refused for a permanently failed mount, got: %v", attempt, err)
		}
	}
}

// TestDynamicMountState_DelayedMountAppearingAfterStartup simulates a
// mount that is not yet ready at the moment a write is first attempted
// (still enumerating, or the service simply started before the fstab
// mount job finished) but becomes ready shortly after - covering both
// "delayed mount" and "mount appearing after service startup" from the
// writer's own point of view, which are indistinguishable: a write
// refused now must not be treated as a permanent verdict, and a write
// attempted again once the guard reports ready must succeed normally.
func TestDynamicMountState_DelayedMountAppearingAfterStartup(t *testing.T) {
	ready := false
	recording.SetPersistenceGuard(func() error {
		if !ready {
			return errors.New("persistent-data mount not yet ready")
		}
		return nil
	})
	defer recording.SetPersistenceGuard(func() error { return nil })

	dir := t.TempDir() + "/recordings"
	if _, err := recording.NewStore(dir, 0, 0); err == nil {
		t.Fatal("expected the write to be refused before the mount becomes ready")
	}

	// The mount finishes mounting (init-overlay's fstab job completes,
	// or - in this test - simply the next poll of the same underlying
	// check now sees it).
	ready = true

	store, err := recording.NewStore(dir, 0, 0)
	if err != nil {
		t.Fatalf("expected the write to succeed once the mount becomes ready, got: %v", err)
	}
	store.Close()
}

// TestDynamicMountState_MountDisappearsDuringOperation simulates a mount
// that is genuinely present and writable when a recording starts, then
// becomes unavailable partway through (e.g. an SD card fault, or the
// mount being lazily unmounted) - a later rotation (the periodic
// checkpoint recording.Store's own rotateLocked re-runs the guard at,
// see store.go's own doc comment on ensurePersistentDir) must refuse
// rather than silently continue writing into whatever now backs that
// path.
func TestDynamicMountState_MountDisappearsDuringOperation(t *testing.T) {
	mounted := true
	recording.SetPersistenceGuard(func() error {
		if !mounted {
			return errors.New("persistent-data mount disappeared")
		}
		return nil
	})
	defer recording.SetPersistenceGuard(func() error { return nil })

	store, err := recording.NewStore(t.TempDir(), 1, 0) // maxFileBytes=1 forces a rotation on the very next Append
	if err != nil {
		t.Fatalf("NewStore should succeed while the mount is present: %v", err)
	}
	defer store.Close()

	mounted = false // the mount vanishes mid-session

	if err := store.Append(recording.Sample{}); err == nil {
		t.Fatal("Append should refuse to rotate into a new file once the mount has disappeared")
	}
}
