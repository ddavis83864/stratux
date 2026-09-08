/*
contracts.go defines the exact interfaces and pure decision functions the
two upcoming features - automatic flight recording and a rolling FIS-B
weather cache - will implement/call against this package. Nothing in this
file is wired into main/ by this mission: no automatic recording starts,
no FIS-B product is ever ingested, persisted, or served because of
anything here. See docs/storage-lifecycle.md's "Future contracts"
section.
*/
package storagelifecycle

import "time"

// --- Automatic recording -------------------------------------------

// RecordingSpaceDecision is ReserveSpace's outcome.
type RecordingSpaceDecision string

const (
	RecordingSpaceAllowed RecordingSpaceDecision = "allowed"
	// RecordingSpaceCaution: technically allowed, but pressure is already
	// elevated/high enough that the caller should think twice (e.g. warn
	// the pilot) before actually starting.
	RecordingSpaceCaution RecordingSpaceDecision = "caution"
	RecordingSpaceDenied  RecordingSpaceDecision = "denied"
)

// RecordingSpaceRequest is what a future automatic-recording feature asks
// before starting a new recording.
type RecordingSpaceRequest struct {
	// ExpectedBytes is a best-effort size estimate for the recording
	// about to start (e.g. based on a typical past session) - 0 means
	// "unknown," which EvaluateRecordingSpace treats conservatively (it
	// can still deny on existing pressure, but never on the unestimated
	// request itself).
	ExpectedBytes int64
}

// RecordingSpaceResult is ReserveSpace's answer - see
// EvaluateRecordingSpace for the actual decision logic.
type RecordingSpaceResult struct {
	Decision RecordingSpaceDecision
	Reason   string
	Pressure PressureState
}

// EvaluateRecordingSpace is the pure decision function behind a future
// RecordingLifecycle.ReserveSpace: never deletes anything, never
// evicts a prior recording to make room - it only ever reports whether
// starting a new one now looks safe, given the namespace's own current
// usage/quota and the whole-system pressure already computed by Manager.
// currentUsageBytes and quota describe the recordings namespace
// specifically (CriticalityImportant, never itself an eviction
// candidate - see Policy.Evictable) - EvaluateRecordingSpace never
// proposes evicting a recording either; a Denied result just means "do
// not start," the same way readiness.StorageHealth.RecordingAllowed
// already works for the existing manual recording feature.
func EvaluateRecordingSpace(pressure PressureState, currentUsageBytes int64, quota Quota, req RecordingSpaceRequest) RecordingSpaceResult {
	switch pressure {
	case PressureCritical:
		return RecordingSpaceResult{Decision: RecordingSpaceDenied, Reason: "storage pressure is critical", Pressure: pressure}
	case PressureUnknown:
		return RecordingSpaceResult{Decision: RecordingSpaceDenied, Reason: "storage pressure could not be determined", Pressure: pressure}
	}
	if quota.MaxBytes > 0 && req.ExpectedBytes > 0 && currentUsageBytes+req.ExpectedBytes > quota.MaxBytes {
		return RecordingSpaceResult{Decision: RecordingSpaceDenied, Reason: "expected recording size would exceed the recordings namespace quota", Pressure: pressure}
	}
	if pressure == PressureHigh || pressure == PressureElevated {
		return RecordingSpaceResult{Decision: RecordingSpaceCaution, Reason: "storage pressure is " + string(pressure), Pressure: pressure}
	}
	return RecordingSpaceResult{Decision: RecordingSpaceAllowed, Reason: "storage pressure is normal", Pressure: pressure}
}

// CompletionReport is what a future automatic-recording feature supplies
// once a recording it started stops.
type CompletionReport struct {
	Name           string
	FinalSizeBytes int64
}

// RecordingLifecycle is the exact contract a future automatic-recording
// feature implements against this package. No production code
// implements or calls this interface in this mission - see
// contracts_test.go for a fake implementation exercising the contract
// shape only.
type RecordingLifecycle interface {
	// ReserveSpace answers whether starting a new recording now looks
	// safe - see EvaluateRecordingSpace.
	ReserveSpace(req RecordingSpaceRequest) RecordingSpaceResult
	// RegisterActive marks name (a recording directory's own name) as
	// currently active, so the Scanner's ActiveChecker reports it as
	// StatusActive and it is never an eviction candidate.
	RegisterActive(name string)
	// Complete reports a recording's final size and that it is no longer
	// active.
	Complete(report CompletionReport)
	// NoAutomaticDeletion documents (and, for a real implementation,
	// enforces) that completing a recording never triggers this package
	// to delete any OTHER recording - automatic recording introduces no
	// new deletion behavior on its own; only a future, separately
	// authorized, explicit recording-retention policy could ever change
	// that. A method rather than only a comment, so a fake implementation
	// in a contract test can assert on it being called/true.
	NoAutomaticDeletion() bool
}

// --- FIS-B weather cache ---------------------------------------------

// CacheProductKey identifies one cached product - opaque to this
// package (a future FIS-B cache defines what ProductType/ProductID
// actually mean; this package only needs them to be stable, comparable
// identifiers).
type CacheProductKey struct {
	ProductType string
	ProductID   string
}

// CacheEntryMetadata is one cached product's lifecycle metadata. Never
// its content - this package's contract only ever handles metadata plus
// an opaque WriteFunc for the content itself (see AtomicWriter).
type CacheEntryMetadata struct {
	Key CacheProductKey
	// ReceivedAtMonotonic/ExpiresAtMonotonic drive expiration and
	// reclaim-priority decisions - monotonic only, so a GNSS/NTP wall-
	// clock correction can never make an unexpired product look expired
	// or vice versa (see IsCacheEntryExpired).
	ReceivedAtMonotonic float64
	ExpiresAtMonotonic  float64
	// GeneratedAtWallClock is informational/display-only (e.g. "received
	// at HH:MM") - never used by IsCacheEntryExpired or any reclaim
	// decision.
	GeneratedAtWallClock time.Time
	SizeBytes            int64
}

// IsCacheEntryExpired reports whether meta's validity window has passed,
// using nowMonotonic - see CacheEntryMetadata's doc comment for why this
// is monotonic-only. An entry with ExpiresAtMonotonic == 0 (unset) is
// never treated as expired by this function alone - a future cache
// feature that wants a hard maximum age even for a product with no
// declared expiry would apply that as a separate, explicit rule of its
// own.
func IsCacheEntryExpired(meta CacheEntryMetadata, nowMonotonic float64) bool {
	if meta.ExpiresAtMonotonic == 0 {
		return false
	}
	return nowMonotonic >= meta.ExpiresAtMonotonic
}

// CacheLifecycle is the exact contract a future FIS-B cache feature
// implements against this package. No production code implements or
// calls this interface in this mission.
type CacheLifecycle interface {
	// ReplaceProduct atomically writes a new version of the product
	// identified by meta.Key, superseding any existing entry with the
	// same Key, via AtomicWriter - a reader never observes a partially
	// written product.
	ReplaceProduct(meta CacheEntryMetadata, write WriteFunc) error
	// IsExpired reports whether an existing entry should be treated as
	// stale - see IsCacheEntryExpired.
	IsExpired(meta CacheEntryMetadata, nowMonotonic float64) bool
	// ReclaimPriority always reports CriticalityCache - defined as a
	// method (not left implicit) so a contract test can assert a real
	// implementation never claims a higher priority for itself than this
	// foundation's fixed ordering allows (see Policy.Evictable and
	// docs/storage-lifecycle.md's retention-order section).
	ReclaimPriority() Criticality
}
