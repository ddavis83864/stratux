/*
autorecordreadiness.go: Automatic Flight Recording's Readiness and
Preflight integration - see docs/automatic-flight-recording.md's
"Readiness, Preflight, and diagnostics integration" section.
*/
package main

import (
	"github.com/stratux/stratux/readiness"
)

// buildAutoRecordHealth adapts the Machine's own Snapshot into
// readiness.AutoRecordHealth - called once per health tick (see
// main/health.go), never itself locking anything but the brief,
// uncontended autoRecordMu inside autoRecordStatusSnapshot.
func buildAutoRecordHealth() readiness.AutoRecordHealth {
	resp := autoRecordStatusSnapshot()
	return readiness.BuildAutoRecordHealth(
		resp.Settings.Enabled,
		string(resp.Snapshot.State),
		string(resp.Snapshot.ReasonCode),
		resp.Snapshot.Reason,
		resp.Snapshot.ActiveRecordingID,
	)
}

// autoRecordDiagnosticsSummary returns a bounded, sanitized summary for
// GET /getDiagnostics - state, reason code, settings (already bounded,
// non-sensitive numeric thresholds - never a coordinate or file path),
// and the cumulative process-lifetime counters. Never a full sample
// trace, never an exact GPS coordinate.
func autoRecordDiagnosticsSummary() interface{} {
	resp := autoRecordStatusSnapshot()
	return map[string]interface{}{
		"enabled":           resp.Settings.Enabled,
		"state":             string(resp.Snapshot.State),
		"reasonCode":        string(resp.Snapshot.ReasonCode),
		"stateAgeSeconds":   resp.Snapshot.StateAgeSeconds,
		"activeRecordingId": resp.Snapshot.ActiveRecordingID,
		"storageDecision":   resp.Snapshot.StorageDecision,
		"lastError":         resp.Snapshot.LastError,
		"settings":          resp.Settings,
		"counters": map[string]int{
			"startCandidates": resp.Snapshot.Counters.StartCandidates,
			"starts":          resp.Snapshot.Counters.Starts,
			"automaticStops":  resp.Snapshot.Counters.AutomaticStops,
			"manualOverrides": resp.Snapshot.Counters.ManualOverrides,
			"inhibitedStarts": resp.Snapshot.Counters.InhibitedStarts,
			"storageDenials":  resp.Snapshot.Counters.StorageDenials,
			"gpsLossEvents":   resp.Snapshot.Counters.GPSLossEvents,
			"recoveryEvents":  resp.Snapshot.Counters.RecoveryEvents,
			"errors":          resp.Snapshot.Counters.Errors,
		},
	}
}
