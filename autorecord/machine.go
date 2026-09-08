package autorecord

import "fmt"

// Action is what the caller (main/'s glue) must actually do after one
// Evaluate call - Evaluate itself never starts or stops a recording; it
// only ever returns an intent, mirroring this project's established
// pure-decision/impure-execution split (see storagelifecycle.Plan vs
// ExecuteRecovery).
type Action string

const (
	ActionNone         Action = "none"
	ActionRequestStart Action = "request_start"
	ActionRequestStop  Action = "request_stop"
)

// maxGPSSampleAgeSeconds bounds how old a GPS sample may be and still
// count as "current" - roughly 5x this project's ~1 Hz GPS update rate
// (see main/gps.go), generous enough to absorb a few missed updates
// without immediately treating a healthy receiver as lost, tight enough
// that a genuinely stale reading is never mistaken for live motion.
const maxGPSSampleAgeSeconds = 5.0

// Machine is the automatic-recording state machine - see state.go for
// the State/ReasonCode vocabulary and Snapshot's full exposed shape.
// Machine holds no lock of its own: main/'s glue is responsible for
// calling Evaluate/OnStartResult/OnStopResult/OnExternalStop/ClearError
// from a single goroutine (or under its own external lock) - see
// docs/automatic-flight-recording.md's concurrency section for exactly
// how main/'s glue satisfies this.
type Machine struct {
	state      State
	reasonCode ReasonCode

	stateEnteredAt float64

	lastGPSSampleAt float64
	lastGPSValid    bool
	lastGroundSpeed float64

	candidateBeganAt float64
	gpsLossBeganAt   float64

	activeRecordingID  string
	trigger            TriggerSource
	recordingStartedAt float64

	storageDecision StorageDecision

	lastStartAttempt *AttemptResult
	lastStopAttempt  *AttemptResult
	lastError        string

	cooldownUntil float64

	// lastCooldownSetting is the most recently observed
	// Settings.RestartCooldownSeconds (recorded on every Evaluate call) -
	// OnStopResult/OnExternalStop have no MachineInput of their own to
	// read a cooldown duration from, so this is the one piece of the last
	// Settings this Machine remembers, kept deliberately narrow rather
	// than caching the whole Settings value.
	lastCooldownSetting float64

	counters Counters
}

// NewMachine returns a Machine starting in StateDisabled - the only
// state a Machine is ever constructed into, regardless of Settings.Enabled
// (the first Evaluate call transitions it to StateArmedWaiting if
// Settings.Enabled is already true - see Evaluate's StateDisabled case).
func NewMachine() *Machine {
	return &Machine{state: StateDisabled, reasonCode: ReasonDisabled}
}

// Evaluate is the machine's one decision function, called once per
// detection tick (main/'s glue drives this on a timer - see
// docs/automatic-flight-recording.md). It reads no state but in and m's
// own prior internal state, calls no clock, and performs no I/O - every
// input is in, and the only output is a Snapshot (for callers that only
// want to observe) plus an Action (for the one caller responsible for
// actually carrying it out).
//
// Decision policy, by current state:
//
//   - DISABLED: transitions to ARMED_WAITING the moment Settings.Enabled
//     is true - with no candidate dwell carried over, so enabling the
//     feature can never immediately start a recording from a stale
//     pre-enable sample (the first post-enable sample only ever *begins*
//     a fresh dwell).
//   - ARMED_WAITING / START_CANDIDATE / INHIBITED: re-checks every
//     start-blocking precondition (see startBlocked) on every tick,
//     regardless of dwell progress - a conflict appearing mid-dwell
//     resets the candidate rather than pausing it (a deliberate,
//     documented choice - see docs/automatic-flight-recording.md - a
//     "paused" dwell would need its own bookkeeping for how long a pause
//     may last, which is exactly the kind of edge case this package
//     avoids by resetting instead). Once nothing blocks, a groundspeed
//     sample at or above Settings.StartGroundspeedKnots begins (or
//     continues) a start candidate; once continuous, uninterrupted
//     StartDwellSeconds pass, Evaluate transitions to STARTING and
//     returns ActionRequestStart.
//   - STARTING / FINALIZING: Evaluate does nothing but re-report the
//     current snapshot - a request is already in flight, and Evaluate
//     never issues a second one while one is outstanding. The caller
//     must report the real outcome via OnStartResult/OnStopResult.
//   - RECORDING / STOP_CANDIDATE: Settings.Enabled=false or
//     Conflicts.ShutdownRequested transitions immediately to FINALIZING
//     and returns ActionRequestStop, bypassing any dwell - see Phase 6/10's
//     explicit "never silently abandon an active recording" and "shutdown
//     must finalize it" requirements. Otherwise, an invalid/stale GPS
//     sample never itself stops a recording (see the GPS-loss policy in
//     docs/automatic-flight-recording.md); only a valid sample at or
//     below Settings.StopGroundspeedKnots, held continuously for
//     StopDwellSeconds, transitions to FINALIZING with ActionRequestStop
//   - gated by Settings.MinimumRecordingDurationSeconds if configured.
//   - ERROR: requires either Settings.Enabled to become false (returns to
//     DISABLED) or an explicit ClearError call - Evaluate alone never
//     recovers from ERROR on its own.
func (m *Machine) Evaluate(in MachineInput) (Snapshot, Action) {
	m.lastGPSSampleAt = in.NowMonotonic - in.GPS.SampleAgeSeconds
	m.lastGPSValid = in.GPS.FixValid
	m.lastGroundSpeed = in.GPS.GroundSpeedKnots
	m.storageDecision = in.Storage
	m.lastCooldownSetting = in.Settings.RestartCooldownSeconds

	switch m.state {
	case StateDisabled:
		if in.Settings.Enabled {
			m.transition(StateArmedWaiting, ReasonWaitingForMotion, in.NowMonotonic)
		}
		return m.snapshot(in.NowMonotonic), ActionNone

	case StateArmedWaiting, StateStartCandidate, StateInhibited:
		return m.evaluateWaitingForStart(in)

	case StateStarting:
		// A request is already in flight - see OnStartResult. Disabling
		// mid-flight is handled the moment the outcome arrives and the
		// next Evaluate call sees StateRecording/StateError with
		// Settings.Enabled already false.
		return m.snapshot(in.NowMonotonic), ActionNone

	case StateRecording, StateStopCandidate:
		return m.evaluateRecording(in)

	case StateFinalizing:
		return m.snapshot(in.NowMonotonic), ActionNone

	case StateError:
		if !in.Settings.Enabled {
			m.transition(StateDisabled, ReasonDisabled, in.NowMonotonic)
		}
		return m.snapshot(in.NowMonotonic), ActionNone

	default:
		return m.snapshot(in.NowMonotonic), ActionNone
	}
}

func (m *Machine) evaluateWaitingForStart(in MachineInput) (Snapshot, Action) {
	if !in.Settings.Enabled {
		m.candidateBeganAt = 0
		m.transition(StateDisabled, ReasonDisabled, in.NowMonotonic)
		return m.snapshot(in.NowMonotonic), ActionNone
	}

	if blocked, reason := m.startBlocked(in); blocked {
		if m.state != StateInhibited {
			m.counters.InhibitedStarts++
		}
		m.candidateBeganAt = 0
		m.transition(StateInhibited, reason, in.NowMonotonic)
		return m.snapshot(in.NowMonotonic), ActionNone
	}

	if in.GPS.GroundSpeedKnots >= in.Settings.StartGroundspeedKnots {
		if m.state != StateStartCandidate {
			m.counters.StartCandidates++
			m.candidateBeganAt = in.NowMonotonic
			m.transition(StateStartCandidate, ReasonStartDwellInProgress, in.NowMonotonic)
		}
		dwell := in.NowMonotonic - m.candidateBeganAt
		if dwell >= in.Settings.StartDwellSeconds {
			m.trigger = TriggerAutomatic
			m.transition(StateStarting, ReasonStartRequested, in.NowMonotonic)
			return m.snapshot(in.NowMonotonic), ActionRequestStart
		}
		return m.snapshot(in.NowMonotonic), ActionNone
	}

	m.candidateBeganAt = 0
	m.transition(StateArmedWaiting, ReasonWaitingForMotion, in.NowMonotonic)
	return m.snapshot(in.NowMonotonic), ActionNone
}

// startBlocked reports whether a start candidate may even be considered,
// checked before groundspeed is evaluated at all - order matters only
// for which single ReasonCode is reported (the first true wins); every
// listed condition is independently sufficient to block on its own.
func (m *Machine) startBlocked(in MachineInput) (bool, ReasonCode) {
	if in.ManualRecordingActive {
		return true, ReasonManualRecordingActive
	}
	if in.Conflicts.ShutdownRequested {
		return true, ReasonShutdownInProgress
	}
	if in.Conflicts.OTABusy {
		return true, ReasonOTAInProgress
	}
	if in.Conflicts.ConfigRestoreBusy {
		return true, ReasonConfigRestoreInProgress
	}
	if m.cooldownUntil > 0 && in.NowMonotonic < m.cooldownUntil {
		return true, ReasonRestartCooldown
	}
	if !in.TrustedTime {
		return true, ReasonTrustedTimeUnavailable
	}
	if !in.GPS.FixValid {
		return true, ReasonGPSInvalid
	}
	if in.GPS.SampleAgeSeconds >= maxGPSSampleAgeSeconds {
		return true, ReasonGPSStale
	}
	switch in.Storage {
	case StorageDenied:
		return true, ReasonStorageDenied
	case StorageUnknown:
		return true, ReasonStorageUnknown
	}
	return false, ""
}

func (m *Machine) evaluateRecording(in MachineInput) (Snapshot, Action) {
	if !in.Settings.Enabled {
		m.trigger = TriggerFeatureDisabled
		m.transition(StateFinalizing, ReasonFeatureDisabled, in.NowMonotonic)
		return m.snapshot(in.NowMonotonic), ActionRequestStop
	}
	if in.Conflicts.ShutdownRequested {
		m.trigger = TriggerShutdown
		m.transition(StateFinalizing, ReasonShutdownInProgress, in.NowMonotonic)
		return m.snapshot(in.NowMonotonic), ActionRequestStop
	}

	gpsCurrent := in.GPS.FixValid && in.GPS.SampleAgeSeconds < maxGPSSampleAgeSeconds
	if !gpsCurrent {
		if m.gpsLossBeganAt == 0 {
			m.gpsLossBeganAt = in.NowMonotonic
			m.counters.GPSLossEvents++
		}
		lossDuration := in.NowMonotonic - m.gpsLossBeganAt
		// A GPS dropout - brief or extended - never itself stops a
		// recording (see docs/automatic-flight-recording.md's GPS-loss
		// policy): only a later valid sample at/below the stop threshold,
		// held for the full stop dwell, can do that. The two branches
		// below differ only in the reported reason, both leave state and
		// any in-progress stop-candidate dwell completely untouched.
		if m.state == StateRecording {
			if lossDuration < in.Settings.GPSLossGraceSeconds {
				m.reasonCode = ReasonGPSLossGrace
			} else {
				m.reasonCode = ReasonGPSUnavailableExtended
			}
		}
		return m.snapshot(in.NowMonotonic), ActionNone
	}
	m.gpsLossBeganAt = 0

	qualifiesStop := in.GPS.GroundSpeedKnots <= in.Settings.StopGroundspeedKnots

	if m.state == StateRecording {
		if qualifiesStop {
			m.candidateBeganAt = in.NowMonotonic
			m.transition(StateStopCandidate, ReasonStopDwellInProgress, in.NowMonotonic)
		} else {
			m.reasonCode = ReasonRecording
		}
		return m.snapshot(in.NowMonotonic), ActionNone
	}

	// StateStopCandidate
	if !qualifiesStop {
		m.candidateBeganAt = 0
		m.transition(StateRecording, ReasonRecording, in.NowMonotonic)
		return m.snapshot(in.NowMonotonic), ActionNone
	}
	dwell := in.NowMonotonic - m.candidateBeganAt
	if dwell < in.Settings.StopDwellSeconds {
		return m.snapshot(in.NowMonotonic), ActionNone
	}
	if in.Settings.MinimumRecordingDurationSeconds > 0 {
		recordingAge := in.NowMonotonic - m.recordingStartedAt
		if recordingAge < in.Settings.MinimumRecordingDurationSeconds {
			m.reasonCode = ReasonMinimumDurationNotMet
			return m.snapshot(in.NowMonotonic), ActionNone
		}
	}
	m.trigger = TriggerAutomatic
	m.transition(StateFinalizing, ReasonStopRequested, in.NowMonotonic)
	return m.snapshot(in.NowMonotonic), ActionRequestStop
}

// OnStartResult reports the real outcome of an ActionRequestStart the
// caller just carried out - must be called exactly once per
// ActionRequestStart, before the next Evaluate call. Calling it while not
// in StateStarting is a caller error (see the returned error) - reported
// rather than silently ignored, so a real integration bug surfaces
// immediately instead of leaving the machine in a state that quietly
// stops making sense.
func (m *Machine) OnStartResult(ok bool, recordingID string, errMsg string, nowMonotonic float64) error {
	if m.state != StateStarting {
		return fmt.Errorf("autorecord: OnStartResult called while not STARTING (state=%s)", m.state)
	}
	if ok {
		m.activeRecordingID = recordingID
		m.recordingStartedAt = nowMonotonic
		m.lastStartAttempt = &AttemptResult{AtMonotonic: nowMonotonic, Success: true}
		m.counters.Starts++
		m.transition(StateRecording, ReasonRecording, nowMonotonic)
		return nil
	}
	m.lastStartAttempt = &AttemptResult{AtMonotonic: nowMonotonic, Success: false, Error: errMsg}
	m.lastError = errMsg
	m.counters.Errors++
	m.transition(StateError, ReasonRecorderError, nowMonotonic)
	return nil
}

// OnStopResult reports the real outcome of an ActionRequestStop the
// caller just carried out - must be called exactly once per
// ActionRequestStop, before the next Evaluate call.
func (m *Machine) OnStopResult(ok bool, errMsg string, nowMonotonic float64) error {
	if m.state != StateFinalizing {
		return fmt.Errorf("autorecord: OnStopResult called while not FINALIZING (state=%s)", m.state)
	}
	wasFeatureDisabled := m.trigger == TriggerFeatureDisabled
	if ok {
		m.lastStopAttempt = &AttemptResult{AtMonotonic: nowMonotonic, Success: true}
		if m.trigger == TriggerAutomatic {
			m.counters.AutomaticStops++
		}
		m.activeRecordingID = ""
		m.candidateBeganAt = 0
		m.cooldownUntil = nowMonotonic + activeCooldownSeconds(m)
		if wasFeatureDisabled {
			m.transition(StateDisabled, ReasonDisabled, nowMonotonic)
		} else {
			m.transition(StateArmedWaiting, ReasonWaitingForMotion, nowMonotonic)
		}
		return nil
	}
	m.lastStopAttempt = &AttemptResult{AtMonotonic: nowMonotonic, Success: false, Error: errMsg}
	m.lastError = errMsg
	m.counters.Errors++
	m.transition(StateError, ReasonRecorderError, nowMonotonic)
	return nil
}

// lastConfiguredCooldown is set by evaluateRecording/evaluateWaitingForStart
// indirectly via the most recent Evaluate's Settings - OnStopResult has no
// MachineInput of its own, so it needs the cooldown duration from
// somewhere. Storing just this one field (rather than the whole last
// Settings) keeps the surface small and obviously single-purpose.
func activeCooldownSeconds(m *Machine) float64 {
	return m.lastCooldownSetting
}

// OnExternalStop reports that the currently active recording ended for a
// reason this Machine did not itself request - the one real case is the
// owner calling the existing manual Stop endpoint while an automatic
// recording was active (see docs/automatic-flight-recording.md's manual-
// coexistence section). The machine was never in STARTING/FINALIZING for
// this stop (no ActionRequestStop was ever issued), so this is a distinct
// method from OnStopResult, not an alternate way to report the same
// thing. Valid to call from StateRecording or StateStopCandidate only.
func (m *Machine) OnExternalStop(nowMonotonic float64) error {
	if m.state != StateRecording && m.state != StateStopCandidate {
		return fmt.Errorf("autorecord: OnExternalStop called while not RECORDING/STOP_CANDIDATE (state=%s)", m.state)
	}
	m.lastStopAttempt = &AttemptResult{AtMonotonic: nowMonotonic, Success: true}
	m.counters.ManualOverrides++
	m.activeRecordingID = ""
	m.candidateBeganAt = 0
	m.trigger = TriggerManual
	m.cooldownUntil = nowMonotonic + activeCooldownSeconds(m)
	m.transition(StateArmedWaiting, ReasonManualStop, nowMonotonic)
	return nil
}

// ClearError transitions out of StateError back to StateArmedWaiting
// (if Settings.Enabled) or StateDisabled - the explicit, owner-initiated
// recovery path Evaluate alone never takes. A no-op (returns an error,
// does not panic) if not currently in StateError.
func (m *Machine) ClearError(enabled bool, nowMonotonic float64) error {
	if m.state != StateError {
		return fmt.Errorf("autorecord: ClearError called while not in ERROR (state=%s)", m.state)
	}
	m.lastError = ""
	m.counters.RecoveryEvents++
	if enabled {
		m.transition(StateArmedWaiting, ReasonWaitingForMotion, nowMonotonic)
	} else {
		m.transition(StateDisabled, ReasonDisabled, nowMonotonic)
	}
	return nil
}

func (m *Machine) transition(s State, reason ReasonCode, nowMonotonic float64) {
	m.state = s
	m.reasonCode = reason
	m.stateEnteredAt = nowMonotonic
}

// Snapshot returns the machine's current, read-only state without taking
// a new sample - safe to call at any time, including from a different
// goroutine than the one driving Evaluate, AS LONG AS the caller applies
// its own synchronization (Machine itself has none - see the package doc
// comment).
func (m *Machine) Snapshot(nowMonotonic float64) Snapshot {
	return m.snapshot(nowMonotonic)
}

func (m *Machine) snapshot(nowMonotonic float64) Snapshot {
	return Snapshot{
		State:                         m.state,
		ReasonCode:                    m.reasonCode,
		Reason:                        reasonText(m.reasonCode),
		StateEnteredAtMonotonic:       m.stateEnteredAt,
		StateAgeSeconds:               nowMonotonic - m.stateEnteredAt,
		LastGPSSampleAtMonotonic:      m.lastGPSSampleAt,
		LastGPSValid:                  m.lastGPSValid,
		LastGroundSpeedKnots:          m.lastGroundSpeed,
		CandidateDwellSeconds:         candidateDwell(m, nowMonotonic),
		ActiveRecordingID:             m.activeRecordingID,
		TriggerSource:                 m.trigger,
		StorageDecision:               string(m.storageDecision),
		LastStartAttempt:              m.lastStartAttempt,
		LastStopAttempt:               m.lastStopAttempt,
		LastError:                     m.lastError,
		RestartCooldownUntilMonotonic: m.cooldownUntil,
		Counters:                      m.counters,
	}
}

func candidateDwell(m *Machine, nowMonotonic float64) float64 {
	if m.state != StateStartCandidate && m.state != StateStopCandidate {
		return 0
	}
	if m.candidateBeganAt == 0 {
		return 0
	}
	return nowMonotonic - m.candidateBeganAt
}

func reasonText(r ReasonCode) string {
	switch r {
	case ReasonDisabled:
		return "automatic recording is disabled"
	case ReasonWaitingForMotion:
		return "waiting for qualifying motion"
	case ReasonStartDwellInProgress:
		return "qualifying motion detected, confirming"
	case ReasonStartRequested:
		return "starting automatic recording"
	case ReasonRecording:
		return "automatic recording in progress"
	case ReasonStopDwellInProgress:
		return "stationary dwell in progress"
	case ReasonStopRequested:
		return "stopping automatic recording"
	case ReasonManualRecordingActive:
		return "inhibited: a manual recording is already active"
	case ReasonOTAInProgress:
		return "inhibited: an update is in progress"
	case ReasonConfigRestoreInProgress:
		return "inhibited: a configuration restore is in progress"
	case ReasonShutdownInProgress:
		return "inhibited: a controlled shutdown is in progress"
	case ReasonStorageDenied:
		return "inhibited: insufficient storage capacity"
	case ReasonStorageCaution:
		return "storage capacity is limited"
	case ReasonStorageUnknown:
		return "inhibited: storage capacity could not be determined"
	case ReasonGPSInvalid:
		return "inhibited: no valid GPS fix"
	case ReasonGPSStale:
		return "inhibited: GPS data is stale"
	case ReasonTrustedTimeUnavailable:
		return "inhibited: trusted time is not yet available"
	case ReasonRestartCooldown:
		return "inhibited: restart cooldown in effect"
	case ReasonGPSLossGrace:
		return "GPS unavailable, within grace period"
	case ReasonGPSUnavailableExtended:
		return "GPS unavailable for an extended period"
	case ReasonManualStop:
		return "manually stopped"
	case ReasonFeatureDisabled:
		return "automatic recording disabled while active"
	case ReasonRecorderError:
		return "recorder reported an error"
	case ReasonMinimumDurationNotMet:
		return "stationary dwell satisfied, waiting for minimum recording duration"
	default:
		return string(r)
	}
}
