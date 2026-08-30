/*
	health.go: Sentry-class readiness/health aggregation and the /getHealth
	API. Builds a readiness.HealthReport from the same signals /getStatus
	already exposes, plus the new persistent-storage certification,
	trusted-time state machine, throttling, and failed-unit checks in the
	readiness package. Does not change or remove any existing status
	field or endpoint.
*/

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/sdrassign"
)

// PersistentDataPath is where the mission's dedicated third partition is
// mounted. PersistentDataFSType is the filesystem type it must be.
const (
	PersistentDataPath   = "/var/lib/stratux-data"
	PersistentDataFSType = "ext4"
)

// timeTrust is the trusted-time state machine gps.go's RMC handler feeds.
// It is package-level (like stratuxClock, globalStatus, etc.) because it
// must accumulate state across every NMEA sentence for the life of the
// process, not be reconstructed per call.
var timeTrust = readiness.NewTimeTrust(readiness.DefaultTimeTrustConfig())

// isRecordingActive reports whether automatic flight recording is
// currently enabled and running. Workstream 7 (the recording foundation)
// deliberately does not enable automatic recording yet - this always
// returns false today. It exists now, and is already wired into the
// trusted-time backward-correction guard, specifically so enabling
// recording in a future release only requires making this reflect real
// state; no time-trust logic needs to change.
func isRecordingActive() bool {
	return false
}

var (
	globalHealth      readiness.HealthReport
	globalHealthMutex sync.Mutex
)

// bandStatusFromGlobalStatus reconstructs the sdrassign.BandStatus that
// main/sdr.go already computed for band (via sdrassign.BuildBandStatus)
// from its flattened globalStatus.<Band>_* fields - see sdr.go around
// globalStatus.UAT_Enabled/globalStatus.ES_Enabled for where those fields
// are set from the original BandStatus. This avoids introducing a second,
// parallel copy of per-band state: /getHealth's radio tiles are always
// derived from exactly the same assignment decision /getStatus reports.
func bandStatusFromGlobalStatus(enabled, detected, assigned bool, deviceSerial string, deviceIndex int, assignmentSource string, ambiguous, conflict, externallySatisfied, identityUnstable, decoderRunning, receiving, degraded bool, reason string) sdrassign.BandStatus {
	return sdrassign.BandStatus{
		Enabled:             enabled,
		Detected:            detected,
		Assigned:            assigned,
		DeviceSerial:        deviceSerial,
		DeviceIndex:         deviceIndex,
		AssignmentSource:    assignmentSource,
		Ambiguous:           ambiguous,
		Conflict:            conflict,
		ExternallySatisfied: externallySatisfied,
		IdentityUnstable:    identityUnstable,
		DecoderRunning:      decoderRunning,
		Receiving:           receiving,
		Degraded:            degraded,
		Reason:              reason,
	}
}

// ensurePersistentDataUUID resolves the filesystem UUID expected at
// PersistentDataPath.
//
// If the operator has explicitly configured globalSettings.PersistentDataUUID
// (via /setSettings, the same mechanism as every other setting), that value
// is authoritative and this function does nothing else - "configurable".
//
// Otherwise, the very first time PersistentDataPath is found mounted as a
// structurally-valid, writable ext4 filesystem (readiness.DiscoverableMount),
// its UUID is pinned into globalSettings.PersistentDataUUID and persisted via
// saveSettings() - "discoverable through a safe installation configuration".
// This never accepts an arbitrary mount: the ext4 filesystem-type gate must
// pass, and pinning only happens once, the same way a fresh install would
// certify its own hardware. Every check after pinning - whether the UUID
// came from settings or from discovery - is the same strict exact-match
// comparison in readiness.EvaluateStorage.
func ensurePersistentDataUUID() {
	if globalSettings.PersistentDataUUID != "" {
		return
	}
	mnt, err := readiness.FindMount(PersistentDataPath)
	if err != nil {
		return
	}
	_, statErr := os.Stat(PersistentDataPath)
	present := statErr == nil
	if !readiness.DiscoverableMount(mnt, present, PersistentDataFSType) {
		return
	}
	globalSettings.PersistentDataUUID = mnt.UUID
	saveSettings()
	log.Printf("readiness: pinned persistent-data filesystem UUID %s at %s (first successful discovery, no UUID was previously configured)\n", mnt.UUID, PersistentDataPath)
}

// updateHealth rebuilds globalHealth from current live signals. It is
// called periodically (see healthUpdateLoop) and is safe to call from
// tests/dev builds where vcgencmd/systemctl/findmnt may be unavailable -
// every real-hardware-touching call degrades to a reported NOT_READY/
// UNKNOWN component rather than failing the whole update.
func updateHealth() {
	now := time.Now().UTC()
	mono := stratuxClock.Time

	uatBand := bandStatusFromGlobalStatus(
		globalStatus.UAT_Enabled, globalStatus.UAT_Detected, globalStatus.UAT_Assigned,
		globalStatus.UAT_DeviceSerial, globalStatus.UAT_DeviceIndex, globalStatus.UAT_AssignmentSource,
		globalStatus.UAT_Ambiguous, globalStatus.UAT_Conflict, globalStatus.UAT_ExternallySatisfied,
		globalStatus.UAT_IdentityUnstable, globalStatus.UAT_DecoderRunning, globalStatus.UAT_Receiving,
		globalStatus.UAT_Degraded, globalStatus.UAT_DiagnosticReason,
	)
	esBand := bandStatusFromGlobalStatus(
		globalStatus.ES_Enabled, globalStatus.ES_Detected, globalStatus.ES_Assigned,
		globalStatus.ES_DeviceSerial, globalStatus.ES_DeviceIndex, globalStatus.ES_AssignmentSource,
		globalStatus.ES_Ambiguous, globalStatus.ES_Conflict, globalStatus.ES_ExternallySatisfied,
		globalStatus.ES_IdentityUnstable, globalStatus.ES_DecoderRunning, globalStatus.ES_Receiving,
		globalStatus.ES_Degraded, globalStatus.ES_DiagnosticReason,
	)
	weatherProducts := map[string]int{
		"METAR":  int(globalStatus.UAT_METAR_total),
		"TAF":    int(globalStatus.UAT_TAF_total),
		"NEXRAD": int(globalStatus.UAT_NEXRAD_total),
		"SIGMET": int(globalStatus.UAT_SIGMET_total),
		"PIREP":  int(globalStatus.UAT_PIREP_total),
		"NOTAM":  int(globalStatus.UAT_NOTAM_total),
		"OTHER":  int(globalStatus.UAT_OTHER_total),
	}
	ADSBTowerMutex.Lock()
	towerCount := len(ADSBTowers)
	ADSBTowerMutex.Unlock()
	uat := readiness.BuildRadioHealth(uatBand, globalStatus.UAT_messages_total, float64(globalStatus.UAT_messages_last_minute), float64(globalStatus.UAT_messages_max), time.Time{}, now, towerCount, weatherProducts)
	es := readiness.BuildRadioHealth(esBand, globalStatus.ES_messages_total, float64(globalStatus.ES_messages_last_minute), float64(globalStatus.ES_messages_max), time.Time{}, now, 0, nil)

	mySituation.muGPS.Lock()
	gpsPresent := globalStatus.GPS_detected_type != 0 || globalStatus.GPS_connected
	gpsLastUpdate := mySituation.GPSLastValidNMEAMessageTime
	mySituation.muGPS.Unlock()
	var gpsLastUpdateWall time.Time
	if !gpsLastUpdate.IsZero() {
		// GPSLastValidNMEAMessageTime is on stratuxClock (monotonic); convert
		// to a wall-clock-comparable age using the monotonic delta, since
		// GPSHealth's staleness math is expressed against `now`.
		gpsLastUpdateWall = now.Add(-mono.Sub(gpsLastUpdate))
	}
	gps := readiness.BuildGPSHealth(
		gpsPresent, globalStatus.GPS_solution, gpsDeviceTypeName(globalStatus.GPS_detected_type),
		globalStatus.GPS_satellites_locked, globalStatus.GPS_satellites_seen, globalStatus.GPS_satellites_tracked,
		globalStatus.GPS_position_accuracy, gpsLastUpdateWall, now, 10*time.Second, timeTrust.State(),
	)

	timeTrust.CheckStale(mono)
	timeHealth := timeTrust.Snapshot(mono)

	gdl90 := readiness.BuildGDL90Health(true, globalStatus.NetworkDataMessagesSentLastSec > 0 || globalStatus.Connected_Users > 0, int(globalStatus.Connected_Users), time.Time{}, false)

	failedUnits, _ := readiness.ListFailedUnits() // nil+nil on non-systemd dev builds; treated as "no failed units known", not an error
	throttle, _ := readiness.GetThrottled()       // zero-value (not throttled) on non-Pi dev builds
	system := readiness.BuildSystemHealth(globalStatus.Version, globalStatus.Build, time.Duration(globalStatus.Uptime)*time.Nanosecond, float64(globalStatus.CPUTemp), throttle.Throttled(), throttle.Undervoltage(), failedUnits)

	ensurePersistentDataUUID()
	storage := readiness.CertifyPersistentStorage(PersistentDataPath, globalSettings.PersistentDataUUID, readiness.DefaultPersistentStorageThresholds())
	overlay := readiness.CertifyPersistentStorage("/", "", readiness.DefaultPersistentStorageThresholds())

	ahrs := readiness.NotInstalled("AHRS board not yet installed; expected in a future hardware revision")
	baro := readiness.NotInstalled("barometer not yet installed")
	fan := readiness.NotInstalled("fan-controller integration not yet implemented in the health model")

	report := readiness.BuildHealthReport(now, uat, es, gps, gdl90, system, storage, overlay, timeHealth, timeTrust.State(), ahrs, baro, fan)

	globalHealthMutex.Lock()
	globalHealth = report
	globalHealthMutex.Unlock()
}

// gpsDeviceTypeName renders GPS_detected_type as a short human string. It
// deliberately does not attempt to enumerate every bit combination the
// existing GPS_TYPE_* constants support - just enough for the dashboard
// to show something more useful than a raw bitmask.
func gpsDeviceTypeName(detectedType uint) string {
	// GPS_detected_type packs a type nibble (compared against the
	// GPS_TYPE_* constants) together with separate protocol-detection
	// bits (e.g. GPS_PROTOCOL_NMEA) in higher bits - mask to the type
	// nibble first, matching the existing check in initGPSSerial().
	switch detectedType & 0x0f {
	case 0:
		return "none"
	case GPS_TYPE_UBX9:
		return "u-blox 9"
	case GPS_TYPE_UBX8:
		return "u-blox 8"
	case GPS_TYPE_UBX10:
		return "u-blox 10"
	case GPS_TYPE_UBX6or7:
		return "u-blox 6/7"
	case GPS_TYPE_NETWORK:
		return "network GPS"
	default:
		return "other/unknown"
	}
}

// healthUpdateInterval is how often updateHealth recomputes the report.
// This is intentionally slower than the 1s /status WebSocket tick -
// readiness is a slower-moving, higher-level judgment than raw counters.
const healthUpdateInterval = 5 * time.Second

// healthUpdateLoop runs updateHealth on a ticker for the life of the
// process. Call once from main().
func healthUpdateLoop() {
	updateHealth() // populate immediately so /getHealth never returns an empty report right after startup
	otaAdvanceLogged("startup")
	ticker := time.NewTicker(healthUpdateInterval)
	for range ticker.C {
		updateHealth()
		otaAdvanceLogged("tick")
	}
}

// otaAdvanceLogged calls otaAdvance and logs any error, so a problem
// advancing the OTA state machine is visible without making the health
// tick itself fail.
func otaAdvanceLogged(context string) {
	if err := otaAdvance(); err != nil {
		log.Printf("OTA: advance (%s) error: %s\n", context, err)
	}
}

// handleHealthRequest serves GET /getHealth: the full current
// readiness.HealthReport as JSON. Matches the existing /getStatus
// handler's headers/pattern exactly.
func handleHealthRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	globalHealthMutex.Lock()
	report := globalHealth
	globalHealthMutex.Unlock()
	healthJSON, err := json.Marshal(&report)
	if err != nil {
		http.Error(w, "failed to marshal health report", http.StatusInternalServerError)
		return
	}
	fmt.Fprintf(w, "%s\n", healthJSON)
}
