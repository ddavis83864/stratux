package readiness

// StorageLifecycleHealth reports on the storagelifecycle package's own
// inventory/pressure state, WITHOUT importing that package (readiness
// stays a leaf dependency - see docs/storage-lifecycle.md's architecture
// note, and CalibrationProfileSummary's identical avoidance pattern for
// calprofile). main/'s glue translates a storagelifecycle.Status into
// this shape - see main/storagelifecycleapi.go.
//
// This never duplicates Storage/TemporaryOverlay's own filesystem-level
// StatfsResult accounting (see storage.go) - it reports a SEPARATE
// dimension: what this project's own namespace inventory/quota model
// currently sees, which storagelifecycle.Manager itself already derives
// in part FROM Storage's own UtilizationPercent (see
// storagelifecycle.FilesystemPressureFunc) rather than recomputing it.
type StorageLifecycleHealth struct {
	State  ComponentState
	Reason string

	HasInventory       bool
	LastScanAgeSeconds float64
	Stale              bool
	// Pressure is one of storagelifecycle.PressureState's string values
	// ("NORMAL"/"ELEVATED"/"HIGH"/"CRITICAL"/"UNKNOWN") - kept as a plain
	// string rather than that package's own type, for the same import-
	// direction reason as the rest of this type.
	Pressure string

	NamespaceCount int
	UnmanagedCount int
	UnmanagedBytes int64
	// ProtectedBytes sums every non-cache-criticality namespace's usage -
	// see storagelifecycle.Status.ProtectedTotalBytes.
	ProtectedBytes int64
	ScanErrorCount int

	// EnforcementEnabled is always false in this release: the
	// storage-lifecycle foundation is observational only - see
	// docs/storage-lifecycle.md's explicit "automatic enforcement
	// disabled" statement. This field exists so a future release that
	// does enable enforcement has an honest place to report it becoming
	// true, rather than this type silently omitting the concept today.
	EnforcementEnabled bool
}

// BuildStorageLifecycleHealth derives a StorageLifecycleHealth from
// already-gathered signals - performs no I/O, so it is exercised
// directly by tests with synthetic values.
//
// Policy (see docs/storage-lifecycle.md's Readiness-integration section):
//   - UNKNOWN: no inventory has ever completed yet (startup grace).
//   - NOT_READY: pressure is CRITICAL - continued safe persistence is
//     threatened.
//   - DEGRADED: the inventory is stale, the last scan reported an error,
//     or pressure is HIGH/ELEVATED/UNKNOWN.
//   - READY: a current inventory exists, no scan errors, and pressure is
//     NORMAL.
//
// This function never reports READY merely because deletion could
// theoretically reclaim space - reclaimability is not part of this
// policy's decision at all, only currency and pressure are.
func BuildStorageLifecycleHealth(hasInventory bool, lastScanAgeSeconds float64, stale bool, pressure string, namespaceCount, unmanagedCount int, unmanagedBytes, protectedBytes int64, scanErrorCount int, enforcementEnabled bool) StorageLifecycleHealth {
	h := StorageLifecycleHealth{
		HasInventory:       hasInventory,
		LastScanAgeSeconds: lastScanAgeSeconds,
		Stale:              stale,
		Pressure:           pressure,
		NamespaceCount:     namespaceCount,
		UnmanagedCount:     unmanagedCount,
		UnmanagedBytes:     unmanagedBytes,
		ProtectedBytes:     protectedBytes,
		ScanErrorCount:     scanErrorCount,
		EnforcementEnabled: enforcementEnabled,
	}
	switch {
	case !hasInventory:
		h.State = StateUnknown
		h.Reason = "no storage-lifecycle inventory yet"
	case pressure == "CRITICAL":
		h.State = StateNotReady
		h.Reason = "storage-lifecycle pressure is critical"
	case stale:
		h.State = StateDegraded
		h.Reason = "storage-lifecycle inventory is stale"
	case scanErrorCount > 0:
		h.State = StateDegraded
		h.Reason = "the last storage-lifecycle scan reported one or more namespace errors"
	case pressure == "HIGH" || pressure == "ELEVATED":
		h.State = StateDegraded
		h.Reason = "storage-lifecycle pressure is " + pressure
	case pressure == "UNKNOWN":
		h.State = StateDegraded
		h.Reason = "storage-lifecycle pressure could not be determined"
	default:
		h.State = StateReady
		h.Reason = "storage lifecycle nominal"
	}
	return h
}
