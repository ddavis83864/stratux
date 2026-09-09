/*
autorecordstorage.go: Automatic Flight Recording's use of the validated
Storage Lifecycle Foundation - see docs/storage-lifecycle.md and
docs/automatic-flight-recording.md's "Storage integration" section.

autoRecordLifecycleAdapter implements storagelifecycle.RecordingLifecycle
against the existing storageManager - this feature introduces no new
quota/pressure computation of its own; every decision is
storagelifecycle.EvaluateRecordingSpace, fed storageManager's own already-
computed Status(). RegisterActive/Complete are effectively redundant with
storageLifecycleActiveChecker (which already keys off recCurrent.ID/State
for ANY active recording, automatic or manual - see
main/storagelifecycleapi.go), kept here only for full, honest compliance
with the RecordingLifecycle contract's shape; nothing in this file ever
deletes, evicts, or moves anything.
*/
package main

import (
	"github.com/stratux/stratux/autorecord"
	"github.com/stratux/stratux/storagelifecycle"
)

// recordingsNamespaceID must match the "recordings" Namespace.ID
// registered in main/storagelifecycleapi.go's initStorageLifecycle.
const recordingsNamespaceID = "recordings"

// autoRecordLifecycleAdapter is the one production implementation of
// storagelifecycle.RecordingLifecycle - a thin wrapper around the
// existing storageManager, never a new pressure/quota mechanism.
type autoRecordLifecycleAdapter struct{}

var _ storagelifecycle.RecordingLifecycle = autoRecordLifecycleAdapter{}

// ReserveSpace answers whether starting a new automatic recording now
// looks safe - see storagelifecycle.EvaluateRecordingSpace. If
// storageManager has not been initialized, or has no inventory yet
// (nothing scanned since boot), this conservatively reports Denied with
// a distinct "unknown" pressure rather than guessing Allowed.
func (autoRecordLifecycleAdapter) ReserveSpace(req storagelifecycle.RecordingSpaceRequest) storagelifecycle.RecordingSpaceResult {
	if storageManager == nil {
		return storagelifecycle.RecordingSpaceResult{
			Decision: storagelifecycle.RecordingSpaceDenied,
			Reason:   "storage lifecycle manager not initialized",
			Pressure: storagelifecycle.PressureUnknown,
		}
	}
	status := storageManager.Status()
	if !status.HasInventory {
		return storagelifecycle.RecordingSpaceResult{
			Decision: storagelifecycle.RecordingSpaceDenied,
			Reason:   "no storage inventory available yet",
			Pressure: storagelifecycle.PressureUnknown,
		}
	}
	usage := status.Namespaces[recordingsNamespaceID]
	currentUsageBytes := usage.ManagedBytes + usage.ActiveBytes
	// No fixed byte quota is configured for the recordings namespace today
	// (Quota{} zero value - see main/storagelifecycleapi.go); the whole-
	// filesystem pressure reading (status.Pressure) is what actually gates
	// automatic starts here, exactly like the existing manual recording
	// feature's own coarser check (recordingMinFreeBytes in
	// main/recordingapi.go), just expressed through the validated
	// storagelifecycle contract instead of a second, separate threshold.
	return storagelifecycle.EvaluateRecordingSpace(status.Pressure, currentUsageBytes, storagelifecycle.Quota{}, req)
}

// RegisterActive/Complete are accepted for interface completeness -
// storageLifecycleActiveChecker already reports any recCurrent (automatic
// or manual) as active without needing to be separately told, so these
// are intentionally no-ops rather than a second, redundant bookkeeping
// path that could drift out of sync with recCurrent's own authoritative
// state.
func (autoRecordLifecycleAdapter) RegisterActive(name string)                        {}
func (autoRecordLifecycleAdapter) Complete(report storagelifecycle.CompletionReport) {}

// NoAutomaticDeletion is always true - see the RecordingLifecycle
// interface's own doc comment. This feature introduces no deletion
// behavior of its own; ReserveSpace only ever reports Denied, it never
// proposes evicting anything to make room.
func (autoRecordLifecycleAdapter) NoAutomaticDeletion() bool { return true }

// autoRecordStorageDecision adapts storageManager's status into the
// small, package-independent autorecord.StorageDecision the pure Machine
// consumes - called once per detection tick.
func autoRecordStorageDecision() autorecord.StorageDecision {
	result := autoRecordLifecycleAdapter{}.ReserveSpace(storagelifecycle.RecordingSpaceRequest{})
	switch result.Decision {
	case storagelifecycle.RecordingSpaceAllowed:
		return autorecord.StorageAllowed
	case storagelifecycle.RecordingSpaceCaution:
		return autorecord.StorageCaution
	case storagelifecycle.RecordingSpaceDenied:
		if result.Pressure == storagelifecycle.PressureUnknown {
			return autorecord.StorageUnknown
		}
		return autorecord.StorageDenied
	default:
		return autorecord.StorageUnknown
	}
}
