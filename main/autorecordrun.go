/*
autorecordrun.go: main/'s glue that actually drives the autorecord.Machine
- the detection tick, and carrying out the Machine's ActionRequestStart/
ActionRequestStop through the existing recording subsystem
(startRecordingLocked/stopActiveRecording - never a parallel start/stop
implementation), reporting the real outcome back via
OnStartResult/OnStopResult.

Lock order (must never be inverted anywhere in this package):
autoRecordMu may be held while acquiring/releasing recMu; recMu must
never be held while acquiring autoRecordMu. Every function in this file
that takes recMu does so only briefly and releases it before returning to
its autoRecordMu-holding caller, or (like
autoRecordNotifyManualStopIfOwned) takes recMu first and fully releases
it before separately acquiring autoRecordMu - never both held at once in
the reverse order.

Automatic Flight Recording is disabled by default (see
autorecord.DefaultSettings) and never deletes an existing recording to
make room - see docs/automatic-flight-recording.md.
*/
package main

import (
	"log"
	"sync"
	"time"

	"github.com/stratux/stratux/autorecord"
	"github.com/stratux/stratux/recording"
)

// autoRecordTickInterval matches this project's ~1 Hz GPS update rate
// (see main/gps.go) - detecting motion at a finer grain would not see any
// new GPS information, and coarser would add needless start/stop-dwell
// latency.
const autoRecordTickInterval = 1 * time.Second

var (
	autoRecordMu            sync.Mutex
	autoRecordMachine       *autorecord.Machine
	autoRecordSettingsCache autorecord.Settings
	autoRecordStopCh        chan struct{}
)

// initAutoRecord loads persisted settings, constructs the Machine, and
// starts its dedicated detection-tick goroutine - must run after
// initStorageLifecycle/initPower/initPreflight (this feature reads all
// three's live status) and after recMu/recCurrent are usable (always
// true - they are plain package vars). Never starts or stops a recording
// itself; the very first tick, one second later, is the earliest any
// automatic action can occur, and only if Settings.Enabled is already
// true and a qualifying dwell then completes.
func initAutoRecord() {
	autoRecordMu.Lock()
	autoRecordSettingsCache = loadAutoRecordSettings()
	autoRecordMachine = autorecord.NewMachine()
	autoRecordStopCh = make(chan struct{})
	stopCh := autoRecordStopCh
	autoRecordMu.Unlock()

	// Bounded, read-only-except-for-one-sidecar recovery pass - see
	// main/autorecordrecovery.go. Runs before the detection loop starts,
	// but never itself starts or stops a recording.
	autoRecordScanForInterrupted()

	go autoRecordLoop(stopCh)
	log.Printf("autorecord: initialized (enabled=%v)\n", autoRecordSettingsCache.Enabled)
}

// autoRecordLoop is this feature's one dedicated goroutine - it never
// blocks ADS-B/GPS/GDL90/AHRS/alerting/the main daemon loop, and nothing
// else blocks on it (its own tick handling is the only thing serialized
// through autoRecordMu, which every other caller in this package holds
// only briefly).
func autoRecordLoop(stopCh chan struct{}) {
	ticker := time.NewTicker(autoRecordTickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			autoRecordTick()
		}
	}
}

// autoRecordTick is one full evaluate-then-act cycle - called by
// autoRecordLoop every autoRecordTickInterval, and once more, directly
// (not via the ticker), by autoRecordHandleShutdown so a confirmed
// shutdown finalizes an active automatic recording immediately rather
// than waiting up to one tick interval.
func autoRecordTick() {
	autoRecordMu.Lock()
	defer autoRecordMu.Unlock()
	autoRecordTickLocked()
}

// autoRecordTickLocked is autoRecordTick's body - split out so
// autoRecordHandleShutdown (which must run inside the SAME critical
// section as the rest of a normal tick, not a second, separately-locked
// call) can invoke it directly. Callers must hold autoRecordMu.
func autoRecordTickLocked() {
	if autoRecordMachine == nil {
		return
	}
	now := monotonicSeconds()
	in := autorecord.MachineInput{
		NowMonotonic:          now,
		Settings:              autoRecordSettingsCache,
		GPS:                   readAutoRecordGPSSample(),
		TrustedTime:           autoRecordTrustedTime(),
		ManualRecordingActive: autoRecordOtherRecordingActive(),
		Conflicts:             autoRecordConflicts(),
		Storage:               autoRecordStorageDecision(),
	}
	_, action := autoRecordMachine.Evaluate(in)
	switch action {
	case autorecord.ActionRequestStart:
		autoRecordPerformStart()
	case autorecord.ActionRequestStop:
		autoRecordPerformStop()
	}
}

// autoRecordPerformStart carries out one ActionRequestStart through the
// existing recording subsystem, exactly the same sequence
// handleStartRecordingRequest uses for a manual start - buildPreflightReport()
// happens outside recMu (see its own doc comment: it transitively locks
// recMu itself, and sync.Mutex is not reentrant), then startRecordingLocked,
// then the initial metadata write, also outside recMu. Called with
// autoRecordMu already held; buildPreflightReport/startRecordingLocked
// each take and release recMu internally, never held across this call.
func autoRecordPerformStart() {
	now := monotonicSeconds()
	machineSnap := autoRecordMachine.Snapshot(now)
	autoCtx := &autoRecordSessionContext{
		TriggerReasonCode:     string(machineSnap.ReasonCode),
		PolicySchemaVersion:   autoRecordSettingsCache.SchemaVersion,
		StartGroundspeedKnots: autoRecordSettingsCache.StartGroundspeedKnots,
		StartDwellSeconds:     autoRecordSettingsCache.StartDwellSeconds,
		StopGroundspeedKnots:  autoRecordSettingsCache.StopGroundspeedKnots,
		StopDwellSeconds:      autoRecordSettingsCache.StopDwellSeconds,
		StorageDecision:       machineSnap.StorageDecision,
		// Only ever set for the first automatic start within
		// autoRecordContinuationWindowSeconds of this boot, and only if
		// an interrupted prior automatic recording was actually found -
		// see main/autorecordrecovery.go. Not yet cleared here: cleared
		// only once this start attempt actually succeeds, below.
		ContinuationOfRecordingID: autoRecordConsumeContinuation(now),
	}

	preflightSnapshot := buildPreflightReport()
	outcome := startRecordingLocked(preflightSnapshot)

	if outcome.Session == nil {
		errMsg := "recording subsystem refused the automatic start request"
		if outcome.HTTPBody != nil {
			if e, ok := outcome.HTTPBody["error"].(string); ok && e != "" {
				errMsg = e
			}
		}
		log.Printf("autorecord: automatic start refused: %s\n", errMsg)
		if err := autoRecordMachine.OnStartResult(false, "", errMsg, now); err != nil {
			log.Printf("autorecord: OnStartResult: %s\n", err)
		}
		return
	}

	recMu.Lock()
	if recCurrent == outcome.Session {
		outcome.Session.autoRecordInitiated = true
	}
	recMu.Unlock()
	if autoCtx.ContinuationOfRecordingID != "" {
		autoRecordClearContinuation() // consumed - never offered to a later start too
	}

	snapshot := buildSessionSnapshot(preflightSnapshot, outcome.Session, autoCtx)
	if err := recording.WriteInitialMetadata(outcome.Session.dir, outcome.Session.ID, snapshot); err != nil {
		log.Printf("autorecord: could not write initial metadata for session %s: %s\n", outcome.Session.ID, err)
		recMu.Lock()
		if recCurrent == outcome.Session {
			outcome.Session.MetadataError = err.Error()
		}
		recMu.Unlock()
	}

	log.Printf("autorecord: started automatic recording %s\n", outcome.Session.ID)
	if err := autoRecordMachine.OnStartResult(true, outcome.Session.ID, "", now); err != nil {
		log.Printf("autorecord: OnStartResult: %s\n", err)
	}
}

// autoRecordStopModeForTrigger maps the Machine's TriggerSource (why THIS
// stop was requested) onto recording.SessionFinalization.AutoRecordStopMode's
// own vocabulary - kept as a single small function so the two vocabularies
// (autorecord.TriggerSource and the finalization's plain string) are never
// allowed to silently drift apart from each other.
func autoRecordStopModeForTrigger(t autorecord.TriggerSource) string {
	switch t {
	case autorecord.TriggerShutdown:
		return "shutdown"
	case autorecord.TriggerFeatureDisabled:
		return "feature_disabled"
	default:
		return "automatic"
	}
}

// autoRecordPerformStop carries out one ActionRequestStop through the
// existing recording subsystem - stopActiveRecording() is the same
// function the manual /stopRecording endpoint uses, and is safe/idempotent
// to call here. stopActiveRecording never reports failure directly (a
// metadata-finalization problem is logged and recorded on the session,
// never treated as the stop itself failing - see its own doc comment), so
// this always reports success to the Machine; the one condition that
// would make a stop meaningless (nothing was actually active) cannot
// occur here because the Machine only ever requests a stop for a
// recording it just confirmed it started.
func autoRecordPerformStop() {
	now := monotonicSeconds()
	mode := autoRecordStopModeForTrigger(autoRecordMachine.Snapshot(now).TriggerSource)
	stopActiveRecording(mode)
	if err := autoRecordMachine.OnStopResult(true, "", now); err != nil {
		log.Printf("autorecord: OnStopResult: %s\n", err)
	}
}

// autoRecordNotifyManualStopIfOwned tells the Machine its active
// recording was just stopped by the owner's manual /stopRecording call,
// not by the Machine's own ActionRequestStop - see
// autorecord.Machine.OnExternalStop's doc comment. Called from
// handleStopRecordingRequest immediately after stopActiveRecording();
// a no-op whenever no automatic recording was actually the one just
// stopped (including: nothing was active, or a manual recording was
// active). Takes recMu first and fully releases it before separately
// acquiring autoRecordMu - see this file's lock-order note.
func autoRecordNotifyManualStopIfOwned() {
	recMu.Lock()
	var stoppedID string
	if recCurrent != nil {
		stoppedID = recCurrent.ID
	}
	recMu.Unlock()
	if stoppedID == "" {
		return
	}

	autoRecordMu.Lock()
	defer autoRecordMu.Unlock()
	if autoRecordMachine == nil {
		return
	}
	now := monotonicSeconds()
	snap := autoRecordMachine.Snapshot(now)
	if snap.ActiveRecordingID != stoppedID {
		return // not the automatic recording - nothing to reconcile
	}
	if snap.State != autorecord.StateRecording && snap.State != autorecord.StateStopCandidate {
		return
	}
	if err := autoRecordMachine.OnExternalStop(now); err != nil {
		log.Printf("autorecord: OnExternalStop: %s\n", err)
	}
}

// autoRecordHandleShutdown forces one immediate, synchronous detection
// tick with the shutdown conflict already visible, so a confirmed
// controlled shutdown finalizes an active automatic recording right away
// rather than waiting up to one tick interval for the periodic loop to
// notice - called from gracefulShutdown, strictly before the existing,
// unconditional stopRecordingForShutdown() call (which remains as the
// correct, unchanged path for a manual recording, and is a harmless
// idempotent no-op if this function already stopped the only active
// recording).
func autoRecordHandleShutdown() {
	autoRecordMu.Lock()
	defer autoRecordMu.Unlock()
	if autoRecordMachine == nil {
		return
	}
	autoRecordTickLocked()
}

// stopAutoRecord stops the detection-tick goroutine - not currently
// called anywhere in production (this daemon has no general subsystem-
// shutdown path short of process exit), provided for test symmetry and
// any future orderly-shutdown path.
func stopAutoRecord() {
	autoRecordMu.Lock()
	stopCh := autoRecordStopCh
	autoRecordStopCh = nil
	autoRecordMu.Unlock()
	if stopCh != nil {
		close(stopCh)
	}
}
