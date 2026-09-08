/*
storagelifecycleapi.go: main/'s glue for the storagelifecycle package -
observational integration only. See docs/storage-lifecycle.md.

This mission (Batch 2's foundation) enables NONE of the following:
  - automatic eviction of anything on the live device;
  - automatic flight recording;
  - a FIS-B weather cache.

The only production code path here that touches the filesystem is a
periodic, bounded, read-only inventory scan
(storageLifecycleUpdateLoop) - there is no HTTP endpoint, dashboard
control, or any other code path in this file (or anywhere else this
mission touches) that deletes, moves, or renames anything.

Endpoint:

	GET /getStorageLifecycle - the current inventory/pressure snapshot.
*/
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/storagelifecycle"
)

// storageLifecycleScanInterval is deliberately much longer than
// healthUpdateInterval (5s): namespace inventory changes far more slowly
// than CPU temperature or throttle bits, and a directory listing (however
// small today) is real I/O this project's health tick should not pay for
// every 5 seconds.
const storageLifecycleScanInterval = 60 * time.Second

var storageManager *storagelifecycle.Manager

// initStorageLifecycle registers this project's known, statically-
// configured namespaces and constructs storageManager. Must run after
// PersistentDataPath is final (readSettings() has already run) and before
// storageLifecycleUpdateLoop starts.
//
// Namespace coverage is intentionally partial in this foundation - see
// docs/storage-lifecycle.md's ownership-matrix section for exactly which
// existing top-level persistent-data entries are NOT registered here yet
// (alert-settings.json and power-session.json are single bare files at
// the partition root, which this package's one-namespace-per-directory
// model does not cover; updates/ has its own nested backup/staged
// subdirectories and its own existing bounded retention already) and why
// that is an honest, documented scope limit rather than a silent gap.
func initStorageLifecycle() {
	registry := storagelifecycle.NewRegistry()
	mustRegisterNamespace(registry, storagelifecycle.Namespace{
		ID: "calibration-profiles", Root: PersistentDataPath + "/calibration-profiles",
		Criticality: storagelifecycle.CriticalityProtected, ItemKind: storagelifecycle.ItemKindFile,
		AllowedExtensions: []string{".json"},
	})
	mustRegisterNamespace(registry, storagelifecycle.Namespace{
		ID: "diagnostics", Root: diagnosticsDir,
		Criticality: storagelifecycle.CriticalityBounded, ItemKind: storagelifecycle.ItemKindFile,
		AllowedExtensions: []string{".json"},
	})
	mustRegisterNamespace(registry, storagelifecycle.Namespace{
		ID: "recordings", Root: recordingsDir,
		Criticality: storagelifecycle.CriticalityImportant, ItemKind: storagelifecycle.ItemKindDirectory,
	})
	mustRegisterNamespace(registry, storagelifecycle.Namespace{
		// Exports are technically regenerable from their source
		// recording, but this foundation treats them as
		// CriticalityImportant (never an eviction candidate) rather than
		// CriticalityCache: a user may have already downloaded/relied on
		// a specific export, and this mission has no explicit mandate or
		// evidence to treat existing export files as disposable. A
		// future, separately authorized policy could reclassify this
		// namespace deliberately.
		ID: "exports", Root: exportsDir,
		Criticality: storagelifecycle.CriticalityImportant, ItemKind: storagelifecycle.ItemKindFile,
	})

	storageManager = storagelifecycle.NewManager(storagelifecycle.ManagerConfig{
		Registry: registry,
		Policy: storagelifecycle.Policy{
			StaleAfterSeconds:                  5 * storageLifecycleScanInterval.Seconds(),
			RequiredConsecutivePressureSamples: 3,
		},
		FS:                   storagelifecycle.NewOSFS(),
		Clock:                monotonicSeconds,
		Active:               storageLifecycleActiveChecker,
		FilesystemPressure:   storageLifecycleFilesystemPressure,
		FilesystemThresholds: readiness.DefaultPersistentStorageThresholds(),
	})
}

func mustRegisterNamespace(r *storagelifecycle.Registry, ns storagelifecycle.Namespace) {
	if err := r.Register(ns); err != nil {
		// A registration error here is a programming error in this exact
		// file (a bad static Namespace literal), not a runtime condition
		// - the same class of mistake calprofile/configbackup's own
		// startup code already treats as fatal via panic elsewhere in
		// this project's init path.
		panic("storagelifecycle: " + err.Error())
	}
}

// storageLifecycleActiveChecker reports whether name is the currently
// active recording. It takes recMu only briefly (a plain read, no
// filesystem or other slow work while held) - the storagelifecycle
// Scanner never holds any lock of its own while calling this, and this
// function must never block on anything else, per
// storagelifecycle.ActiveChecker's documented contract.
func storageLifecycleActiveChecker(namespace, name string) bool {
	if namespace != "recordings" {
		return false
	}
	recMu.Lock()
	defer recMu.Unlock()
	return recCurrent != nil && recCurrent.State == recordingStateActive && recCurrent.ID == name
}

// storageLifecycleFilesystemPressure adapts globalHealth's own,
// already-computed Storage.UtilizationPercent - never a second
// independent statfs call.
func storageLifecycleFilesystemPressure() (float64, bool) {
	globalHealthMutex.Lock()
	present := globalHealth.Storage.Present && globalHealth.Storage.Mounted
	pct := globalHealth.Storage.UtilizationPercent
	globalHealthMutex.Unlock()
	if !present {
		return 0, false
	}
	return pct, true
}

// storageLifecycleUpdateLoop performs one immediate scan (so the first
// /getHealth response after startup does not report UNKNOWN for longer
// than necessary) and then one every storageLifecycleScanInterval, for
// the life of the process. A scan failure never panics or stops this
// loop - see storagelifecycle.Scanner.Scan's own failure-isolation
// contract.
func storageLifecycleUpdateLoop() {
	if storageManager == nil {
		return
	}
	storageManager.Scan()
	ticker := time.NewTicker(storageLifecycleScanInterval)
	for range ticker.C {
		storageManager.Scan()
	}
}

// buildStorageLifecycleHealth adapts storageManager's current Status into
// readiness.StorageLifecycleHealth - see that type's doc comment for why
// this translation exists instead of readiness importing storagelifecycle
// directly.
func buildStorageLifecycleHealth() readiness.StorageLifecycleHealth {
	if storageManager == nil {
		return readiness.BuildStorageLifecycleHealth(false, 0, false, string(storagelifecycle.PressureUnknown), 0, 0, 0, 0, 0, false)
	}
	st := storageManager.Status()
	return readiness.BuildStorageLifecycleHealth(
		st.HasInventory,
		st.AgeSeconds,
		st.Stale,
		string(st.Pressure),
		len(st.Namespaces),
		st.UnmanagedTotalCount,
		st.UnmanagedTotalBytes,
		st.ProtectedTotalBytes,
		len(st.ScanErrors),
		false, // enforcement is never enabled in this release
	)
}

// --- HTTP handler --------------------------------------------------

type storageLifecycleNamespaceResponse struct {
	ManagedCount   int   `json:"managedCount"`
	ManagedBytes   int64 `json:"managedBytes"`
	ActiveCount    int   `json:"activeCount"`
	ActiveBytes    int64 `json:"activeBytes"`
	UnmanagedCount int   `json:"unmanagedCount"`
	UnmanagedBytes int64 `json:"unmanagedBytes"`
}

type storageLifecycleResponse struct {
	HasInventory        bool                                         `json:"hasInventory"`
	LastScanAgeSeconds  float64                                      `json:"lastScanAgeSeconds"`
	Stale               bool                                         `json:"stale"`
	Pressure            string                                       `json:"pressure"`
	Namespaces          map[string]storageLifecycleNamespaceResponse `json:"namespaces"`
	UnmanagedTotalCount int                                          `json:"unmanagedTotalCount"`
	UnmanagedTotalBytes int64                                        `json:"unmanagedTotalBytes"`
	ProtectedTotalBytes int64                                        `json:"protectedTotalBytes"`
	ScanErrorCount      int                                          `json:"scanErrorCount"`
	EnforcementEnabled  bool                                         `json:"enforcementEnabled"`
	Notes               []string                                     `json:"notes"`
}

// storageLifecycleNotes are always included in the response - the
// mission's explicit requirement that the dashboard/API never imply more
// than this foundation actually does.
func storageLifecycleNotes() []string {
	return []string{
		"Automatic cleanup is not enabled. Nothing here is ever deleted automatically.",
		"Completed recordings, calibration profiles, and configuration state are never automatically deleted by this feature.",
	}
}

// handleGetStorageLifecycleRequest serves GET /getStorageLifecycle - the
// only endpoint this feature exposes. There is no corresponding POST/
// delete endpoint anywhere in this codebase for this feature.
func handleGetStorageLifecycleRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if storageManager == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "storage lifecycle subsystem not initialized"})
		return
	}
	st := storageManager.Status()
	resp := storageLifecycleResponse{
		HasInventory:        st.HasInventory,
		LastScanAgeSeconds:  st.AgeSeconds,
		Stale:               st.Stale,
		Pressure:            string(st.Pressure),
		Namespaces:          make(map[string]storageLifecycleNamespaceResponse, len(st.Namespaces)),
		UnmanagedTotalCount: st.UnmanagedTotalCount,
		UnmanagedTotalBytes: st.UnmanagedTotalBytes,
		ProtectedTotalBytes: st.ProtectedTotalBytes,
		ScanErrorCount:      len(st.ScanErrors),
		EnforcementEnabled:  false,
		Notes:               storageLifecycleNotes(),
	}
	for id, u := range st.Namespaces {
		resp.Namespaces[id] = storageLifecycleNamespaceResponse{
			ManagedCount: u.ManagedCount, ManagedBytes: u.ManagedBytes,
			ActiveCount: u.ActiveCount, ActiveBytes: u.ActiveBytes,
			UnmanagedCount: u.UnmanagedCount, UnmanagedBytes: u.UnmanagedBytes,
		}
	}
	json.NewEncoder(w).Encode(resp)
}

// storageLifecycleDiagnosticsSummary returns a bounded, sanitized summary
// for readiness.DiagnosticBundle.StorageLifecycleSummary - namespace IDs
// and counts only, never a file name, path, or content.
func storageLifecycleDiagnosticsSummary() interface{} {
	if storageManager == nil {
		return nil
	}
	st := storageManager.Status()
	namespaces := make(map[string]storageLifecycleNamespaceResponse, len(st.Namespaces))
	for id, u := range st.Namespaces {
		namespaces[id] = storageLifecycleNamespaceResponse{
			ManagedCount: u.ManagedCount, ManagedBytes: u.ManagedBytes,
			ActiveCount: u.ActiveCount, ActiveBytes: u.ActiveBytes,
			UnmanagedCount: u.UnmanagedCount, UnmanagedBytes: u.UnmanagedBytes,
		}
	}
	return map[string]interface{}{
		"hasInventory":        st.HasInventory,
		"pressure":            string(st.Pressure),
		"stale":               st.Stale,
		"namespaces":          namespaces,
		"unmanagedTotalCount": st.UnmanagedTotalCount,
		"unmanagedTotalBytes": st.UnmanagedTotalBytes,
		"protectedTotalBytes": st.ProtectedTotalBytes,
		"scanErrorCount":      len(st.ScanErrors),
		"enforcementEnabled":  false,
	}
}
