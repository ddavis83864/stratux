package autorecord

import "testing"

// enabledSettings returns DefaultSettings with Enabled forced true - most
// tests below want the feature armed; the disabled-by-default behavior
// itself is covered by TestMachine_DefaultDisabled.
func enabledSettings() Settings {
	s := DefaultSettings()
	s.Enabled = true
	return s
}

// baseInput returns a MachineInput with every gate open (enabled, valid
// current GPS at 0 kt, trusted time, no conflicts, storage allowed) at
// the given monotonic time - tests override only the fields they care
// about.
func baseInput(now float64) MachineInput {
	return MachineInput{
		NowMonotonic: now,
		Settings:     enabledSettings(),
		GPS:          GPSSample{FixValid: true, GroundSpeedKnots: 0, SampleAgeSeconds: 1},
		TrustedTime:  true,
		Storage:      StorageAllowed,
	}
}

func evalAt(t *testing.T, m *Machine, now float64, mutate func(*MachineInput)) (Snapshot, Action) {
	t.Helper()
	in := baseInput(now)
	if mutate != nil {
		mutate(&in)
	}
	return m.Evaluate(in)
}

// runStartDwell drives m from ArmedWaiting through a full, uninterrupted
// qualifying start dwell beginning at startAt, and asserts it ends in
// StateStarting with ActionRequestStart - a building block used by many
// tests below that only care about what happens once recording begins.
func runStartDwell(t *testing.T, m *Machine, startAt float64, speed float64) float64 {
	t.Helper()
	s := enabledSettings()
	snap, action := evalAt(t, m, startAt, func(in *MachineInput) { in.GPS.GroundSpeedKnots = speed })
	if snap.State != StateStartCandidate || action != ActionNone {
		t.Fatalf("runStartDwell: first qualifying sample: got state=%s action=%s, want START_CANDIDATE/none", snap.State, action)
	}
	endAt := startAt + s.StartDwellSeconds
	snap, action = evalAt(t, m, endAt, func(in *MachineInput) { in.GPS.GroundSpeedKnots = speed })
	if snap.State != StateStarting || action != ActionRequestStart {
		t.Fatalf("runStartDwell: dwell completion: got state=%s action=%s, want STARTING/request_start", snap.State, action)
	}
	return endAt
}

func TestMachine_DefaultDisabled(t *testing.T) {
	m := NewMachine()
	snap, action := evalAt(t, m, 100, func(in *MachineInput) {
		in.Settings.Enabled = false
		in.GPS.GroundSpeedKnots = 50 // even with implausibly qualifying speed
	})
	if snap.State != StateDisabled || snap.ReasonCode != ReasonDisabled {
		t.Fatalf("got state=%s reason=%s, want DISABLED/disabled", snap.State, snap.ReasonCode)
	}
	if action != ActionNone {
		t.Fatalf("got action=%s, want none", action)
	}
}

func TestMachine_ZeroValueSafety(t *testing.T) {
	var m Machine // not via NewMachine
	snap, action := evalAt(t, &m, 100, nil)
	if action != ActionNone {
		t.Fatalf("zero-value Machine: got action=%s, want none (must never panic or emit an action)", action)
	}
	_ = snap // must not panic building the snapshot either
}

func TestMachine_NoStartFromStalePreEnableDwell(t *testing.T) {
	m := NewMachine()
	// Feature disabled, but a qualifying-speed sample arrives repeatedly -
	// must never accumulate any dwell while disabled.
	for now := 0.0; now < 60; now += 10 {
		snap, action := evalAt(t, m, now, func(in *MachineInput) {
			in.Settings.Enabled = false
			in.GPS.GroundSpeedKnots = 50
		})
		if snap.State != StateDisabled || action != ActionNone {
			t.Fatalf("at t=%v: got state=%s action=%s while disabled, want DISABLED/none", now, snap.State, action)
		}
	}
	// Now enable, with the very next sample still qualifying - must start
	// a FRESH dwell, not immediately start.
	snap, action := evalAt(t, m, 60, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 50 })
	if snap.State != StateStartCandidate || action != ActionNone {
		t.Fatalf("first enabled tick: got state=%s action=%s, want START_CANDIDATE/none (no stale dwell carryover)", snap.State, action)
	}
	if snap.CandidateDwellSeconds != 0 {
		t.Fatalf("first enabled tick: CandidateDwellSeconds=%v, want 0", snap.CandidateDwellSeconds)
	}
	// One second short of the dwell: still a candidate.
	s := enabledSettings()
	snap, action = evalAt(t, m, 60+s.StartDwellSeconds-1, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 50 })
	if snap.State != StateStartCandidate || action != ActionNone {
		t.Fatalf("just before dwell: got state=%s action=%s, want START_CANDIDATE/none", snap.State, action)
	}
	// Dwell completes.
	snap, action = evalAt(t, m, 60+s.StartDwellSeconds, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 50 })
	if snap.State != StateStarting || action != ActionRequestStart {
		t.Fatalf("at dwell completion: got state=%s action=%s, want STARTING/request_start", snap.State, action)
	}
}

func TestMachine_SustainedQualifyingStart(t *testing.T) {
	m := NewMachine()
	runStartDwell(t, m, 0, 10) // above the 8kt default start threshold
}

func TestMachine_SpikeRejection(t *testing.T) {
	m := NewMachine()
	snap, action := evalAt(t, m, 0, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 20 })
	if snap.State != StateStartCandidate || action != ActionNone {
		t.Fatalf("spike start: got state=%s action=%s", snap.State, action)
	}
	// Drops back below threshold well before the dwell completes.
	snap, action = evalAt(t, m, 5, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 1 })
	if snap.State != StateArmedWaiting || action != ActionNone {
		t.Fatalf("after spike drop: got state=%s action=%s, want ARMED_WAITING/none", snap.State, action)
	}
	if snap.CandidateDwellSeconds != 0 {
		t.Fatalf("after spike drop: CandidateDwellSeconds=%v, want 0 (must not carry over)", snap.CandidateDwellSeconds)
	}
	// Even if the full original dwell duration has now elapsed since the
	// spike, no start ever fires from that original candidate.
	s := enabledSettings()
	snap, action = evalAt(t, m, s.StartDwellSeconds+1, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 1 })
	if snap.State == StateStarting || action == ActionRequestStart {
		t.Fatalf("spike must never lead to a start: got state=%s action=%s", snap.State, action)
	}
}

func TestMachine_Hysteresis_MidBandSpeedNeverStopsRecording(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	if err := m.OnStartResult(true, "rec-test-1", "", endAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	// Default: start=8kt, stop=4kt. 6kt is strictly between the two -
	// must remain plainly RECORDING, never even a stop candidate.
	for i, now := 0, endAt; i < 5; i, now = i+1, now+30 {
		snap, action := evalAt(t, m, now, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 6 })
		if snap.State != StateRecording || action != ActionNone {
			t.Fatalf("at t=%v mid-band 6kt: got state=%s action=%s, want RECORDING/none", now, snap.State, action)
		}
	}
}

func TestMachine_StopDwell(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	if err := m.OnStartResult(true, "rec-test-2", "", endAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	s := enabledSettings()
	stopBeganAt := endAt + 10
	snap, action := evalAt(t, m, stopBeganAt, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 2 })
	if snap.State != StateStopCandidate || action != ActionNone {
		t.Fatalf("first qualifying stop sample: got state=%s action=%s", snap.State, action)
	}
	// Speed pops back up before the stop dwell completes - must cancel
	// the stop candidate and return to plain RECORDING.
	snap, action = evalAt(t, m, stopBeganAt+5, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 9 })
	if snap.State != StateRecording || action != ActionNone {
		t.Fatalf("stop-candidate cancellation: got state=%s action=%s, want RECORDING/none", snap.State, action)
	}
	// Restart the stop candidate and let it run to completion this time.
	stopBeganAt = stopBeganAt + 5
	snap, action = evalAt(t, m, stopBeganAt, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 2 })
	if snap.State != StateStopCandidate {
		t.Fatalf("restarted stop candidate: got state=%s", snap.State)
	}
	snap, action = evalAt(t, m, stopBeganAt+s.StopDwellSeconds-1, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 2 })
	if snap.State != StateStopCandidate || action != ActionNone {
		t.Fatalf("just before stop dwell completes: got state=%s action=%s", snap.State, action)
	}
	snap, action = evalAt(t, m, stopBeganAt+s.StopDwellSeconds, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 2 })
	if snap.State != StateFinalizing || action != ActionRequestStop {
		t.Fatalf("stop dwell completion: got state=%s action=%s, want FINALIZING/request_stop", snap.State, action)
	}
}

func TestMachine_NoDuplicateStartOrStopWhileRequestInFlight(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	// Still STARTING - repeated Evaluate calls must never re-emit a
	// second start request.
	for i, now := 0, endAt; i < 3; i, now = i+1, now+1 {
		snap, action := evalAt(t, m, now, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 10 })
		if snap.State != StateStarting || action != ActionNone {
			t.Fatalf("while STARTING at t=%v: got state=%s action=%s, want STARTING/none", now, snap.State, action)
		}
	}
	if err := m.OnStartResult(true, "rec-test-3", "", endAt+3); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	s := enabledSettings()
	stopBeganAt := endAt + 3 + 10
	evalAt(t, m, stopBeganAt, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 1 })
	finalizeAt := stopBeganAt + s.StopDwellSeconds
	snap, action := evalAt(t, m, finalizeAt, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 1 })
	if snap.State != StateFinalizing || action != ActionRequestStop {
		t.Fatalf("stop dwell completion: got state=%s action=%s", snap.State, action)
	}
	// Still FINALIZING - must never re-emit a second stop request.
	for i, now := 0, finalizeAt; i < 3; i, now = i+1, now+1 {
		snap, action := evalAt(t, m, now, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 1 })
		if snap.State != StateFinalizing || action != ActionNone {
			t.Fatalf("while FINALIZING at t=%v: got state=%s action=%s, want FINALIZING/none", now, snap.State, action)
		}
	}
}

func TestMachine_Cooldown_BlocksThenExpires(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	if err := m.OnStartResult(true, "rec-test-4", "", endAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	// Owner manually stops the automatic recording.
	if err := m.OnExternalStop(endAt + 1); err != nil {
		t.Fatalf("OnExternalStop: %v", err)
	}
	s := enabledSettings()
	cooldownEnd := endAt + 1 + s.RestartCooldownSeconds

	// A qualifying sample well within the cooldown window must be
	// inhibited, never allowed to become a start candidate.
	snap, action := evalAt(t, m, cooldownEnd-1, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 20 })
	if snap.State != StateInhibited || snap.ReasonCode != ReasonRestartCooldown || action != ActionNone {
		t.Fatalf("within cooldown: got state=%s reason=%s action=%s, want INHIBITED/restart_cooldown/none", snap.State, snap.ReasonCode, action)
	}

	// Once the cooldown has fully elapsed, a qualifying sample begins a
	// fresh start candidate as normal.
	snap, action = evalAt(t, m, cooldownEnd, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 20 })
	if snap.State != StateStartCandidate || action != ActionNone {
		t.Fatalf("after cooldown expires: got state=%s action=%s, want START_CANDIDATE/none", snap.State, action)
	}
}

func TestMachine_MonotonicDwellIsWallClockIndependent(t *testing.T) {
	m := NewMachine()
	// Use an arbitrarily large monotonic base, unrelated to any real wall
	// clock, and a duration only sensitive to monotonic differences - this
	// package must never call time.Now() or otherwise depend on wall time
	// for dwell/timeout accounting (see docs/automatic-flight-recording.md).
	const base = 987654321.5
	runStartDwell(t, m, base, 10)
}

func TestMachine_ErrorRecovery(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	if err := m.OnStartResult(false, "", "simulated recorder failure", endAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	snap := m.Snapshot(endAt)
	if snap.State != StateError || snap.ReasonCode != ReasonRecorderError {
		t.Fatalf("after failed start: got state=%s reason=%s, want ERROR/recorder_error", snap.State, snap.ReasonCode)
	}
	if snap.LastError == "" {
		t.Fatalf("LastError not populated after a failed start")
	}
	// Evaluate alone must never leave ERROR on its own while still enabled.
	snap, action := evalAt(t, m, endAt+100, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 20 })
	if snap.State != StateError || action != ActionNone {
		t.Fatalf("ERROR must persist until ClearError: got state=%s action=%s", snap.State, action)
	}
	if err := m.ClearError(true, endAt+100); err != nil {
		t.Fatalf("ClearError: %v", err)
	}
	if got := m.Snapshot(endAt + 100).State; got != StateArmedWaiting {
		t.Fatalf("after ClearError(enabled=true): got state=%s, want ARMED_WAITING", got)
	}
	// ClearError on a machine not in ERROR must return an error, not panic.
	if err := m.ClearError(true, endAt+100); err == nil {
		t.Fatalf("ClearError while not in ERROR: want an error, got nil")
	}
}

func TestMachine_OnStartResult_WrongStateReturnsError(t *testing.T) {
	m := NewMachine()
	if err := m.OnStartResult(true, "rec-x", "", 0); err == nil {
		t.Fatalf("OnStartResult while ARMED_WAITING (never STARTING): want an error, got nil")
	}
}

func TestMachine_OnStopResult_WrongStateReturnsError(t *testing.T) {
	m := NewMachine()
	if err := m.OnStopResult(true, "", 0); err == nil {
		t.Fatalf("OnStopResult while ARMED_WAITING (never FINALIZING): want an error, got nil")
	}
}

func TestMachine_StartBlockedByStorageDenied(t *testing.T) {
	m := NewMachine()
	snap, action := evalAt(t, m, 0, func(in *MachineInput) {
		in.GPS.GroundSpeedKnots = 20
		in.Storage = StorageDenied
	})
	if snap.State != StateInhibited || snap.ReasonCode != ReasonStorageDenied || action != ActionNone {
		t.Fatalf("got state=%s reason=%s action=%s, want INHIBITED/storage_denied/none", snap.State, snap.ReasonCode, action)
	}
}

func TestMachine_StartBlockedByStorageUnknown(t *testing.T) {
	m := NewMachine()
	snap, _ := evalAt(t, m, 0, func(in *MachineInput) {
		in.GPS.GroundSpeedKnots = 20
		in.Storage = StorageUnknown
	})
	if snap.State != StateInhibited || snap.ReasonCode != ReasonStorageUnknown {
		t.Fatalf("got state=%s reason=%s, want INHIBITED/storage_pressure_unknown", snap.State, snap.ReasonCode)
	}
}

func TestMachine_StorageCautionDoesNotBlockStart(t *testing.T) {
	m := NewMachine()
	m2 := NewMachine()
	_ = m2
	snap, action := evalAt(t, m, 0, func(in *MachineInput) {
		in.GPS.GroundSpeedKnots = 20
		in.Storage = StorageCaution
	})
	if snap.State != StateStartCandidate || action != ActionNone {
		t.Fatalf("StorageCaution: got state=%s action=%s, want START_CANDIDATE/none (caution must not block, only be reported)", snap.State, action)
	}
	if snap.StorageDecision != string(StorageCaution) {
		t.Fatalf("Snapshot.StorageDecision=%q, want %q", snap.StorageDecision, StorageCaution)
	}
}

func TestMachine_StartBlockedByManualRecordingActive(t *testing.T) {
	m := NewMachine()
	snap, action := evalAt(t, m, 0, func(in *MachineInput) {
		in.GPS.GroundSpeedKnots = 20
		in.ManualRecordingActive = true
	})
	if snap.State != StateInhibited || snap.ReasonCode != ReasonManualRecordingActive || action != ActionNone {
		t.Fatalf("got state=%s reason=%s action=%s", snap.State, snap.ReasonCode, action)
	}
}

func TestMachine_StartBlockedByOTAAndConfigRestore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Conflicts)
		want   ReasonCode
	}{
		{"ota", func(c *Conflicts) { c.OTABusy = true }, ReasonOTAInProgress},
		{"config_restore", func(c *Conflicts) { c.ConfigRestoreBusy = true }, ReasonConfigRestoreInProgress},
		{"shutdown", func(c *Conflicts) { c.ShutdownRequested = true }, ReasonShutdownInProgress},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewMachine()
			snap, action := evalAt(t, m, 0, func(in *MachineInput) {
				in.GPS.GroundSpeedKnots = 20
				tc.mutate(&in.Conflicts)
			})
			if snap.State != StateInhibited || snap.ReasonCode != tc.want || action != ActionNone {
				t.Fatalf("%s: got state=%s reason=%s action=%s, want INHIBITED/%s/none", tc.name, snap.State, snap.ReasonCode, action, tc.want)
			}
		})
	}
}

func TestMachine_StartBlockedByInvalidOrStaleGPS(t *testing.T) {
	t.Run("invalid_fix", func(t *testing.T) {
		m := NewMachine()
		snap, _ := evalAt(t, m, 0, func(in *MachineInput) {
			in.GPS.FixValid = false
			in.GPS.GroundSpeedKnots = 20
		})
		if snap.State != StateInhibited || snap.ReasonCode != ReasonGPSInvalid {
			t.Fatalf("got state=%s reason=%s, want INHIBITED/gps_invalid", snap.State, snap.ReasonCode)
		}
	})
	t.Run("stale_sample", func(t *testing.T) {
		m := NewMachine()
		snap, _ := evalAt(t, m, 0, func(in *MachineInput) {
			in.GPS.SampleAgeSeconds = 30
			in.GPS.GroundSpeedKnots = 20
		})
		if snap.State != StateInhibited || snap.ReasonCode != ReasonGPSStale {
			t.Fatalf("got state=%s reason=%s, want INHIBITED/gps_stale", snap.State, snap.ReasonCode)
		}
	})
}

func TestMachine_StartBlockedByUntrustedTime(t *testing.T) {
	m := NewMachine()
	snap, _ := evalAt(t, m, 0, func(in *MachineInput) {
		in.TrustedTime = false
		in.GPS.GroundSpeedKnots = 20
	})
	if snap.State != StateInhibited || snap.ReasonCode != ReasonTrustedTimeUnavailable {
		t.Fatalf("got state=%s reason=%s, want INHIBITED/trusted_time_unavailable", snap.State, snap.ReasonCode)
	}
}

func TestMachine_ShutdownWhileRecordingForcesImmediateFinalize(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	if err := m.OnStartResult(true, "rec-shutdown", "", endAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	// Still moving fast (would never qualify as a stop candidate on its
	// own) - shutdown must still force an immediate finalize, bypassing
	// any dwell.
	snap, action := evalAt(t, m, endAt+1, func(in *MachineInput) {
		in.GPS.GroundSpeedKnots = 40
		in.Conflicts.ShutdownRequested = true
	})
	if snap.State != StateFinalizing || action != ActionRequestStop {
		t.Fatalf("got state=%s action=%s, want FINALIZING/request_stop", snap.State, action)
	}
	if snap.TriggerSource != TriggerShutdown {
		t.Fatalf("TriggerSource=%s, want shutdown", snap.TriggerSource)
	}
}

func TestMachine_FeatureDisabledWhileRecordingFinalizes(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	if err := m.OnStartResult(true, "rec-disable", "", endAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	snap, action := evalAt(t, m, endAt+1, func(in *MachineInput) {
		in.Settings.Enabled = false
		in.GPS.GroundSpeedKnots = 40
	})
	if snap.State != StateFinalizing || action != ActionRequestStop {
		t.Fatalf("got state=%s action=%s, want FINALIZING/request_stop", snap.State, action)
	}
	if err := m.OnStopResult(true, "", endAt+2); err != nil {
		t.Fatalf("OnStopResult: %v", err)
	}
	if got := m.Snapshot(endAt + 2).State; got != StateDisabled {
		t.Fatalf("after finalize-on-disable completes: got state=%s, want DISABLED (not re-armed)", got)
	}
}

func TestMachine_GPSLossGraceNeverStopsRecording(t *testing.T) {
	m := NewMachine()
	endAt := runStartDwell(t, m, 0, 10)
	if err := m.OnStartResult(true, "rec-gps-loss", "", endAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	s := enabledSettings()
	// GPS goes invalid - within grace period.
	snap, action := evalAt(t, m, endAt+1, func(in *MachineInput) { in.GPS.FixValid = false })
	if snap.State != StateRecording || snap.ReasonCode != ReasonGPSLossGrace || action != ActionNone {
		t.Fatalf("within grace: got state=%s reason=%s action=%s", snap.State, snap.ReasonCode, action)
	}
	// Still invalid, well beyond the grace period - recording must still
	// never be automatically stopped merely due to GPS loss.
	beyondGrace := endAt + 1 + s.GPSLossGraceSeconds + 60
	snap, action = evalAt(t, m, beyondGrace, func(in *MachineInput) { in.GPS.FixValid = false })
	if snap.State != StateRecording || snap.ReasonCode != ReasonGPSUnavailableExtended || action != ActionNone {
		t.Fatalf("beyond grace: got state=%s reason=%s action=%s, want RECORDING/gps_unavailable_extended/none (must never auto-stop on GPS loss alone)", snap.State, snap.ReasonCode, action)
	}
	// GPS recovers with a genuinely qualifying stop sample - normal stop
	// dwell logic resumes exactly as if no loss had occurred.
	snap, _ = evalAt(t, m, beyondGrace+1, func(in *MachineInput) { in.GPS.GroundSpeedKnots = 1 })
	if snap.State != StateStopCandidate {
		t.Fatalf("after GPS recovery with qualifying stop sample: got state=%s, want STOP_CANDIDATE", snap.State)
	}
}

func TestMachine_MinimumRecordingDurationDefersStop(t *testing.T) {
	m := NewMachine()
	s := enabledSettings()
	s.MinimumRecordingDurationSeconds = 600
	in0 := baseInput(0)
	in0.Settings = s
	in0.GPS.GroundSpeedKnots = 10
	snap, _ := m.Evaluate(in0)
	if snap.State != StateStartCandidate {
		t.Fatalf("got state=%s", snap.State)
	}
	inDwell := in0
	inDwell.NowMonotonic = s.StartDwellSeconds
	snap, action := m.Evaluate(inDwell)
	if snap.State != StateStarting || action != ActionRequestStart {
		t.Fatalf("got state=%s action=%s", snap.State, action)
	}
	startedAt := s.StartDwellSeconds
	if err := m.OnStartResult(true, "rec-mindur", "", startedAt); err != nil {
		t.Fatalf("OnStartResult: %v", err)
	}
	// Begin a stop candidate, then let its dwell run to completion - well
	// before the minimum recording duration.
	inStop := in0
	inStop.Settings = s
	inStop.GPS.GroundSpeedKnots = 1
	inStop.NowMonotonic = startedAt + 1
	if snap, _ := m.Evaluate(inStop); snap.State != StateStopCandidate {
		t.Fatalf("expected stop candidate to begin, got state=%s", snap.State)
	}
	inStop.NowMonotonic = startedAt + 1 + s.StopDwellSeconds
	snap, action = m.Evaluate(inStop)
	// Dwell is satisfied but the minimum recording duration is not - the
	// machine stays in STOP_CANDIDATE (its dwell condition remains true
	// and is still worth reporting), just deferred, rather than silently
	// reverting to plain RECORDING.
	if snap.State != StateStopCandidate || action != ActionNone || snap.ReasonCode != ReasonMinimumDurationNotMet {
		t.Fatalf("stop dwell complete but under minimum duration: got state=%s action=%s reason=%s, want STOP_CANDIDATE/none/minimum_duration_not_met", snap.State, action, snap.ReasonCode)
	}
	// Once the minimum duration has elapsed, the still-satisfied stop
	// condition finalizes.
	inStop.NowMonotonic = startedAt + s.MinimumRecordingDurationSeconds + 1
	snap, action = m.Evaluate(inStop)
	if snap.State != StateFinalizing || action != ActionRequestStop {
		t.Fatalf("after minimum duration elapses: got state=%s action=%s, want FINALIZING/request_stop", snap.State, action)
	}
}

func TestMachine_OnExternalStop_WrongStateReturnsError(t *testing.T) {
	m := NewMachine()
	if err := m.OnExternalStop(0); err == nil {
		t.Fatalf("OnExternalStop while ARMED_WAITING (never RECORDING/STOP_CANDIDATE): want an error, got nil")
	}
}

func TestMachine_RepeatedEnableDisableNeverPanicsOrLeaksState(t *testing.T) {
	m := NewMachine()
	for cycle := 0; cycle < 5; cycle++ {
		base := float64(cycle) * 1000
		snap, _ := evalAt(t, m, base, func(in *MachineInput) { in.Settings.Enabled = true; in.GPS.GroundSpeedKnots = 0 })
		if snap.State != StateArmedWaiting {
			t.Fatalf("cycle %d: got state=%s, want ARMED_WAITING", cycle, snap.State)
		}
		snap, _ = evalAt(t, m, base+1, func(in *MachineInput) { in.Settings.Enabled = false })
		if snap.State != StateDisabled {
			t.Fatalf("cycle %d: got state=%s, want DISABLED", cycle, snap.State)
		}
	}
}
