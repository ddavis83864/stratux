/*
Package autorecord is the pure, hardware-independent decision core for
Automatic Flight Recording: an opt-in feature that starts and stops a
recording (through the existing recording subsystem - see main/'s glue,
never this package) based on trustworthy GNSS-derived movement.

This package makes no I/O calls of its own - no filesystem, no GPS
hardware, no HTTP. Every input is injected (see MachineInput), and every
output is either a read-only Snapshot or an Action the caller (main/'s
glue) is responsible for actually carrying out through the existing
recording subsystem and reporting the result of back via OnStartResult/
OnStopResult. This mirrors this project's established pure-decision/
impure-glue split (see storagelifecycle.Plan vs storagelifecycle.
ExecuteRecovery, or power.Manager's own Confirm/IssuePowerOff split).

Automatic Flight Recording is disabled by default (see Settings.Enabled)
and never deletes an existing recording to make room - see
docs/automatic-flight-recording.md's explicit non-goals.
*/
package autorecord

// State is one state in the automatic-recording state machine.
type State string

const (
	// StateDisabled: the feature is turned off (Settings.Enabled is
	// false). No detection runs at all - this is the default state, and
	// the only state the machine starts in.
	StateDisabled State = "DISABLED"
	// StateArmedWaiting: enabled, no qualifying motion observed yet (or
	// motion dropped back below the start threshold before the start
	// dwell completed).
	StateArmedWaiting State = "ARMED_WAITING"
	// StateStartCandidate: a qualifying sample has been observed and the
	// start dwell timer is running. Reverts to StateArmedWaiting if a
	// disqualifying sample arrives before the dwell completes - a start
	// candidate never itself starts a recording.
	StateStartCandidate State = "START_CANDIDATE"
	// StateStarting: the dwell completed and the machine has emitted
	// ActionRequestStart, waiting for the caller to report the outcome
	// via OnStartResult. Never re-emits a second start request while in
	// this state.
	StateStarting State = "STARTING"
	// StateRecording: an automatic recording is active.
	StateRecording State = "RECORDING"
	// StateStopCandidate: groundspeed has dropped to or below the stop
	// threshold and the stop dwell timer is running. Reverts to
	// StateRecording if speed rises back above the stop threshold before
	// the dwell completes.
	StateStopCandidate State = "STOP_CANDIDATE"
	// StateFinalizing: the stop dwell completed (or another terminal
	// condition applied - manual stop, shutdown, feature disabled) and
	// the machine has emitted ActionRequestStop, waiting for
	// OnStopResult.
	StateFinalizing State = "FINALIZING"
	// StateInhibited: enabled, but a precondition currently blocks
	// automatic starting - a manual recording is already active, OTA/
	// configuration-restore/shutdown is in progress, storage is denied,
	// GPS/trusted time is unavailable, or a restart cooldown is in
	// effect. See Snapshot.Reason for which one. Never itself a terminal
	// failure - the machine returns to StateArmedWaiting the moment the
	// blocking condition clears.
	StateInhibited State = "INHIBITED"
	// StateError: the recorder reported a start/stop failure this
	// machine cannot resolve on its own. Requires either the blocking
	// condition to change or an explicit ClearError call - see
	// Machine.ClearError.
	StateError State = "ERROR"
)

// ReasonCode is a short, stable, machine-readable identifier for why the
// machine is in its current state - always paired with a human-readable
// Reason string in Snapshot, but never *only* the human string: a
// dashboard or diagnostic consumer should be able to switch on ReasonCode
// without parsing English text.
type ReasonCode string

const (
	ReasonDisabled                ReasonCode = "disabled"
	ReasonWaitingForMotion        ReasonCode = "waiting_for_motion"
	ReasonStartDwellInProgress    ReasonCode = "start_dwell_in_progress"
	ReasonStartRequested          ReasonCode = "start_requested"
	ReasonRecording               ReasonCode = "recording"
	ReasonStopDwellInProgress     ReasonCode = "stop_dwell_in_progress"
	ReasonStopRequested           ReasonCode = "stop_requested"
	ReasonManualRecordingActive   ReasonCode = "manual_recording_active"
	ReasonOTAInProgress           ReasonCode = "ota_in_progress"
	ReasonConfigRestoreInProgress ReasonCode = "config_restore_in_progress"
	ReasonShutdownInProgress      ReasonCode = "shutdown_in_progress"
	ReasonStorageDenied           ReasonCode = "storage_denied"
	ReasonStorageCaution          ReasonCode = "storage_caution"
	ReasonStorageUnknown          ReasonCode = "storage_pressure_unknown"
	ReasonGPSInvalid              ReasonCode = "gps_invalid"
	ReasonGPSStale                ReasonCode = "gps_stale"
	ReasonTrustedTimeUnavailable  ReasonCode = "trusted_time_unavailable"
	ReasonRestartCooldown         ReasonCode = "restart_cooldown"
	ReasonGPSLossGrace            ReasonCode = "gps_loss_grace"
	ReasonGPSUnavailableExtended  ReasonCode = "gps_unavailable_extended"
	ReasonManualStop              ReasonCode = "manual_stop"
	ReasonFeatureDisabled         ReasonCode = "feature_disabled_while_recording"
	ReasonRecorderError           ReasonCode = "recorder_error"
	ReasonMinimumDurationNotMet   ReasonCode = "minimum_duration_not_met"
)

// TriggerSource identifies what caused the current or most recent
// start/stop attempt - distinct from ReasonCode, which explains the
// current *state*.
type TriggerSource string

const (
	TriggerNone            TriggerSource = ""
	TriggerAutomatic       TriggerSource = "automatic"
	TriggerManual          TriggerSource = "manual"
	TriggerShutdown        TriggerSource = "shutdown"
	TriggerFeatureDisabled TriggerSource = "feature_disabled"
)

// AttemptResult records one start or stop attempt's outcome - used for
// Snapshot.LastStartAttempt/LastStopAttempt.
type AttemptResult struct {
	AtMonotonic float64
	Success     bool
	Error       string // sanitized - never raw file paths or coordinates
}

// Snapshot is the machine's complete, read-only, exposable state - the
// shape main/'s glue serializes for the API, dashboard, diagnostics, and
// Readiness/Preflight integration. Every field listed in this mission's
// own state-machine requirement is present here, deliberately, rather
// than leaving a caller to reconstruct part of the picture itself.
type Snapshot struct {
	State      State
	ReasonCode ReasonCode
	// Reason is Reason Code rendered as a short, human-readable string -
	// always derived from ReasonCode (see reasonText), never a free-form
	// message a caller could disagree with the ReasonCode about.
	Reason string

	StateEnteredAtMonotonic float64
	// StateAgeSeconds is NowMonotonic-StateEnteredAtMonotonic as of the
	// snapshot's own generation - callers should not recompute this
	// themselves from a separately-read "now."
	StateAgeSeconds float64

	// LastGPSSampleAtMonotonic/LastGPSValid describe the most recent GPS
	// sample the machine evaluated - not necessarily a qualifying one.
	LastGPSSampleAtMonotonic float64
	LastGPSValid             bool
	LastGroundSpeedKnots     float64

	// CandidateDwellSeconds is how long the current start/stop candidate
	// condition has held continuously - 0 outside StateStartCandidate/
	// StateStopCandidate.
	CandidateDwellSeconds float64

	// ActiveRecordingID is set only while StateRecording/StateStopCandidate/
	// StateFinalizing (an automatic recording this machine itself
	// started) - never a manual recording's ID.
	ActiveRecordingID string

	TriggerSource TriggerSource

	StorageDecision string // mirrors storagelifecycle.RecordingSpaceDecision as a plain string - see machine.go's doc comment on why

	LastStartAttempt *AttemptResult
	LastStopAttempt  *AttemptResult
	LastError        string

	// RestartCooldownUntilMonotonic is non-zero while a restart cooldown
	// (after a manual stop of an automatic recording, or after certain
	// error recoveries) is in effect - see Settings.RestartCooldownSeconds.
	RestartCooldownUntilMonotonic float64

	// Counters are cumulative since this Machine was constructed (process
	// lifetime - never persisted, never reset by settings changes) - see
	// docs/automatic-flight-recording.md's diagnostics-integration section
	// for exactly which counters main/'s glue surfaces.
	Counters Counters
}

// Counters are cumulative, process-lifetime event counts - see
// Snapshot.Counters.
type Counters struct {
	StartCandidates int
	Starts          int
	AutomaticStops  int
	ManualOverrides int
	InhibitedStarts int
	StorageDenials  int
	GPSLossEvents   int
	RecoveryEvents  int
	Errors          int
}
