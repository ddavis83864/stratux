/*
autorecordinput.go: reads the live daemon state Automatic Flight
Recording's pure Machine needs for one detection tick - GPS sample,
conflicting subsystem states, trusted-time, and whether a recording this
Machine did not itself start is already active. Every function here is a
read-only observation; none of them mutate anything.
*/
package main

import (
	"time"

	"github.com/stratux/stratux/autorecord"
	"github.com/stratux/stratux/power"
	"github.com/stratux/stratux/readiness"
)

// autoRecordMaxGPSSampleAge mirrors isGPSValid()'s own 3-second staleness
// bound - this package deliberately does not import or call isGPSValid()
// itself (see readAutoRecordGPSSample's doc comment), but the underlying
// GPS subsystem's own definition of "fresh" is exactly this value, and
// re-deriving a different one here would be an unexplained, undocumented
// divergence from it.
const autoRecordMaxGPSSampleAge = 3 * time.Second

// readAutoRecordGPSSample builds an autorecord.GPSSample directly from
// mySituation under its own mutex - deliberately NOT calling isGPSValid(),
// which resets several mySituation fields to sentinel values as a side
// effect whenever it finds no valid fix (see main/gps.go). That behavior
// is established, existing policy for isGPSValid()'s own callers; this
// package must not trigger it purely as a side effect of reading GPS
// state for its own, independent, read-only purposes. The validity
// criteria themselves are intentionally identical to isGPSValid()'s (see
// autoRecordMaxGPSSampleAge) - this is an independent re-derivation of
// the same policy, not a different one.
func readAutoRecordGPSSample() autorecord.GPSSample {
	mySituation.muGPS.Lock()
	fixQuality := mySituation.GPSFixQuality
	lastFix := mySituation.GPSLastFixLocalTime
	groundSpeed := mySituation.GPSGroundSpeed
	mySituation.muGPS.Unlock()

	// globalStatus.GPS_connected is read without its own lock, matching
	// every other existing read site in this codebase (see isGPSValid()
	// itself, and main/gen_gdl90.go) - not a new convention introduced by
	// this feature.
	connected := globalStatus.GPS_connected

	age := stratuxClock.Since(lastFix)
	valid := connected && fixQuality > 0 && age < autoRecordMaxGPSSampleAge

	return autorecord.GPSSample{
		FixValid:         valid,
		GroundSpeedKnots: groundSpeed,
		SampleAgeSeconds: age.Seconds(),
	}
}

// autoRecordTrustedTime reports whether the system clock is currently
// backed by a trusted source (GNSS or network) - required before
// Automatic Flight Recording will start a new recording (see
// readiness.TimeGNSSSynced's own doc comment). DEGRADED/INVALID/
// UNSYNCHRONIZED all count as untrusted: a holdover or discredited clock
// must not gate a persisted, timestamped recording's start.
func autoRecordTrustedTime() bool {
	switch timeTrust.State() {
	case readiness.TimeGNSSSynced, readiness.TimeNetworkSynced:
		return true
	default:
		return false
	}
}

// autoRecordOtherRecordingActive reports whether a recording is currently
// active that this feature's own Machine did not itself start. Evaluated
// fresh every tick; harmless (simply unused) on ticks where the Machine
// is not in a state that would consult it.
//
// Any recCurrent found active here is, by construction, never this
// Machine's own recording: main/'s glue only asks the Machine's
// startBlocked policy about ManualRecordingActive while the Machine is in
// ARMED_WAITING/START_CANDIDATE/INHIBITED, and in every one of those
// states the Machine's own ActiveRecordingID is always empty (it is only
// ever set by OnStartResult, which also moves the Machine out of all
// three of those states) - so there is no case where this function could
// mistake the Machine's own automatic recording for someone else's.
func autoRecordOtherRecordingActive() bool {
	recMu.Lock()
	defer recMu.Unlock()
	return recCurrent != nil && recCurrent.State == recordingStateActive
}

// autoRecordConflicts reads the daemon-wide states that must block a new
// automatic start, or force an immediate stop - each from that
// subsystem's own existing status, never re-derived independently.
func autoRecordConflicts() autorecord.Conflicts {
	return autorecord.Conflicts{
		OTABusy:           otaNotBusyPrecondition() != nil,
		ConfigRestoreBusy: configBackupNotBusyPrecondition() != nil,
		ShutdownRequested: autoRecordShutdownConfirmed(),
	}
}

// autoRecordShutdownConfirmed reports true only once the owner has
// actually confirmed a controlled shutdown and it is underway - not
// merely requested-and-awaiting-confirmation (power.StageConfirmationRequired
// is still fully revocable/expirable, and must not itself force-stop an
// active automatic recording - see power.Manager's own Confirm/token
// semantics).
func autoRecordShutdownConfirmed() bool {
	if shutdownManager == nil {
		return false
	}
	stage, _ := shutdownManager.Status()
	switch stage {
	case power.StageShutdownRequested, power.StageFlushing, power.StageReadyToPowerOff, power.StageCommandIssued:
		return true
	default:
		return false
	}
}
