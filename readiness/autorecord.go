package readiness

// AutoRecordHealth reports Automatic Flight Recording's own state,
// WITHOUT importing the autorecord package (readiness stays a leaf
// dependency - see StorageLifecycleHealth's identical avoidance
// pattern). main/'s glue translates an autorecord.Snapshot into this
// shape.
type AutoRecordHealth struct {
	State  ComponentState
	Reason string

	Enabled bool
	// MachineState is one of autorecord.State's own string values
	// ("DISABLED"/"ARMED_WAITING"/"START_CANDIDATE"/"STARTING"/
	// "RECORDING"/"STOP_CANDIDATE"/"FINALIZING"/"INHIBITED"/"ERROR"),
	// kept as a plain string for the same import-direction reason as the
	// rest of this type. Empty means the feature has not yet
	// initialized.
	MachineState      string
	ReasonCode        string
	ActiveRecordingID string
}

// BuildAutoRecordHealth derives an AutoRecordHealth from already-gathered
// signals - performs no I/O, so it is exercised directly by tests with
// synthetic values.
//
// Policy:
//   - NOT_INSTALLED: the feature is disabled - a deliberate, opt-in
//     choice, never a failure, and (per Rollup's own documented
//     exclusion of NOT_INSTALLED/UNKNOWN) never degrades overall system
//     readiness merely by being off.
//   - UNKNOWN: enabled but the state machine has not yet initialized.
//   - NOT_READY: the state machine is in ERROR.
//   - DEGRADED: the state machine is INHIBITED (a start-blocking
//     precondition currently applies - never itself a failure, but
//     worth the operator's attention).
//   - READY: any other enabled, initialized state (armed, a start/stop
//     candidate, starting, recording, or finalizing).
func BuildAutoRecordHealth(enabled bool, machineState, reasonCode, reason, activeRecordingID string) AutoRecordHealth {
	h := AutoRecordHealth{
		Enabled:           enabled,
		MachineState:      machineState,
		ReasonCode:        reasonCode,
		ActiveRecordingID: activeRecordingID,
	}
	switch {
	case !enabled:
		h.State = StateNotInstalled
		h.Reason = "automatic recording is disabled"
	case machineState == "":
		h.State = StateUnknown
		h.Reason = "automatic recording has not yet initialized"
	case machineState == "ERROR":
		h.State = StateNotReady
		h.Reason = reason
	case machineState == "INHIBITED":
		h.State = StateDegraded
		h.Reason = reason
	default:
		h.State = StateReady
		h.Reason = "automatic recording nominal"
	}
	return h
}
