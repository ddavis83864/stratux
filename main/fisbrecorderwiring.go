/*
	fisbrecorderwiring.go: Wires the fisbrecorder package (a bounded,
	opt-in field-recording and offline-replay facility for the 978 MHz
	UAT/FIS-B pipeline) into this daemon.

	See docs/fisb-field-recorder-design.md for the full design record.
	Three hook points, matching that design exactly:
	  - main/sdr.go's uatReader(): every decoded UAT frame, alongside (not
	    instead of) the existing TraceLog.Record(CONTEXT_GODUMP978, ...)
	    call.
	  - main/network.go's connectionWriter(): every outbound write to any
	    client connection, with connection.GetConnectionKey() as the
	    destination.
	  - fisbRecorderSnapshotLoop below: periodic read-only snapshots of
	    status/settings/FIS-B cache/towers/GPS-clock state, for comparison
	    against a replay.

	Start/stop follows the same convention main/trace.go's TraceLog
	already uses for globalSettings.TraceLog: a 1-second watchdog polls
	the setting and starts/stops the actual session to match
	(fisbRecorderWatchdog, mirroring traceLoggerWatchdog) rather than a
	side effect inside handleSettingsSetRequest's switch.
*/

package main

import (
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/stratux/stratux/fisbrecorder"
)

// fisbRecorder is the process-wide field-recording session controller.
// RecordFrame/RecordGDL90/RecordSnapshot are safe no-ops whenever no
// session is active, so every hot-path call site calls them
// unconditionally rather than checking globalSettings.FISBRecordingEnabled
// itself - see fisbrecorder's own package doc for why that is safe and
// intentional (it keeps the hot paths free of any lock/branch on session
// state beyond the one already inside Recorder itself).
var fisbRecorder = fisbrecorder.New(
	filepath.Join(PersistentDataPath, "fisb-recordings"),
	stratuxBuild,
	fisbrecorder.DefaultOptions(),
)

// fisbRecorderSnapshotInterval matches fisbrecorder.DefaultOptions's own
// SnapshotInterval. Kept as its own constant (rather than reading
// fisbRecorder's private opts) since main, not the package, owns the
// snapshot goroutine's schedule - see fisbrecorder.Snapshot's doc comment.
const fisbRecorderSnapshotInterval = 10 * time.Second

var fisbRecorderMu sync.Mutex

// initFISBRecorder starts a recording session immediately at boot if the
// persisted setting already says recording should be on (e.g. the
// daemon restarted mid-session) - "opt-in and off by default" describes
// the setting's own default value (false, set in defaultSettings()), not
// a rule that a restart must silently drop an operator's already-enabled
// setting.
func initFISBRecorder() {
	fisbRecorderMu.Lock()
	defer fisbRecorderMu.Unlock()
	globalSettingsMu.RLock()
	enabled := globalSettings.FISBRecordingEnabled
	globalSettingsMu.RUnlock()
	if enabled && !fisbRecorder.IsActive() {
		if sid, err := fisbRecorder.Start(); err != nil {
			log.Printf("fisbRecorder: could not start at boot: %v", err)
		} else {
			log.Printf("fisbRecorder: field-recording session %s started at boot (FISBRecordingEnabled was already true)", sid)
		}
	}
}

// fisbRecorderWatchdog polls globalSettings.FISBRecordingEnabled once a
// second and starts/stops the actual session to match - the exact same
// pattern traceLoggerWatchdog (main/trace.go) already uses for
// globalSettings.TraceLog. A failed Start/Stop is logged, never panicked:
// a recording-facility fault must never take the live decoder or GDL90
// output down with it.
func fisbRecorderWatchdog() {
	for {
		time.Sleep(1 * time.Second)

		globalSettingsMu.RLock()
		enabled := globalSettings.FISBRecordingEnabled
		globalSettingsMu.RUnlock()

		fisbRecorderMu.Lock()
		active := fisbRecorder.IsActive()
		if enabled && !active {
			if sid, err := fisbRecorder.Start(); err != nil {
				log.Printf("fisbRecorder: Start failed: %v", err)
			} else {
				log.Printf("fisbRecorder: field-recording session %s started", sid)
			}
		} else if !enabled && active {
			if m, err := fisbRecorder.Stop(); err != nil {
				log.Printf("fisbRecorder: Stop failed: %v", err)
			} else {
				log.Printf("fisbRecorder: field-recording session %s stopped (stopReason=%s, frames=%d, dropped=%d)", m.SessionID, m.StopReason, m.FrameCount, m.DroppedFrames)
			}
		}
		fisbRecorderMu.Unlock()
	}
}

// fisbRecorderSnapshotLoop takes a periodic, read-only snapshot of
// several pieces of live in-process state, for later comparison against
// a replay of the same session - see the design doc's Manifest/Snapshot
// discussion. Building each snapshot's payload is skipped entirely when
// no session is active, so this loop costs nothing when recording is off
// beyond one IsActive() check every fisbRecorderSnapshotInterval.
func fisbRecorderSnapshotLoop() {
	ticker := time.NewTicker(fisbRecorderSnapshotInterval)
	defer ticker.Stop()
	for range ticker.C {
		if !fisbRecorder.IsActive() {
			continue
		}
		recordFISBRecorderSnapshots()
	}
}

// recordFISBRecorderSnapshots gathers and records one round of
// status/settings/FIS-B cache/towers/GPS-clock snapshots. Exported as its
// own function (rather than inlined in the loop) so it can also be called
// once at session start/stop if a future change wants explicit bookend
// snapshots - not done yet, to keep this change minimal.
func recordFISBRecorderSnapshots() {
	globalSettingsMu.RLock()
	settingsCopy := globalSettings
	globalSettingsMu.RUnlock()
	fisbRecorder.RecordSnapshot("settings", settingsCopy)

	fisbRecorder.RecordSnapshot("status", globalStatus)

	fisbRecorder.RecordSnapshot("fisbCacheStatus", fisbCacheStatusSnapshot())
	fisbRecorder.RecordSnapshot("fisbCacheInventory", fisbCacheInventorySnapshot())

	ADSBTowerMutex.Lock()
	towersCopy := make(map[string]ADSBTower, len(ADSBTowers))
	for k, v := range ADSBTowers {
		towersCopy[k] = v
	}
	ADSBTowerMutex.Unlock()
	fisbRecorder.RecordSnapshot("towers", towersCopy)

	mySituation.muGPS.Lock()
	gpsClock := struct {
		GPSTime                     time.Time
		GPSLastFixSinceMidnightUTC  float32
		GPSFixQuality               uint8
		GPSLastValidNMEAMessageTime time.Time
	}{
		GPSTime:                     mySituation.GPSTime,
		GPSLastFixSinceMidnightUTC:  mySituation.GPSLastFixSinceMidnightUTC,
		GPSFixQuality:               mySituation.GPSFixQuality,
		GPSLastValidNMEAMessageTime: mySituation.GPSLastValidNMEAMessageTime,
	}
	mySituation.muGPS.Unlock()
	fisbRecorder.RecordSnapshot("gpsClock", struct {
		GPS              interface{}
		MonotonicSeconds float64
		StratuxClockTime time.Time
	}{
		GPS:              gpsClock,
		MonotonicSeconds: monotonicSeconds(),
		StratuxClockTime: stratuxClock.Time(),
	})
}
