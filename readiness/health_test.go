package readiness

import (
	"testing"
	"time"

	"github.com/stratux/stratux/sdrassign"
)

// --- Fixtures: healthy, no-signal, degraded, missing-hardware ---
// These four scenarios are the mission-required fixture set for the health
// API's schema/transition tests (workstream 4) and are exercised here at
// the aggregator level; main/'s handler tests (once wired in) reuse the
// same shapes.

func healthyBand(band string, deviceIndex int) sdrassign.BandStatus {
	return sdrassign.BandStatus{
		Enabled:          true,
		Detected:         true,
		Assigned:         true,
		DeviceSerial:     "stx:" + band + ":0",
		DeviceIndex:      deviceIndex,
		AssignmentSource: "tagged",
		DecoderRunning:   true,
		Receiving:        true,
		Degraded:         false,
		Reason:           band + " receiving traffic.",
	}
}

func noSignalBand(band string, deviceIndex int) sdrassign.BandStatus {
	// Decoder running, receiver healthy, simply nothing in range right
	// now - sdrassign.BuildBandStatus deliberately does not set Degraded
	// for this case (see sdrassign.TestBuildBandStatus_DecoderRunningNoTrafficIsNotDegraded).
	return sdrassign.BandStatus{
		Enabled:          true,
		Detected:         true,
		Assigned:         true,
		DeviceSerial:     "stx:" + band + ":0",
		DeviceIndex:      deviceIndex,
		AssignmentSource: "tagged",
		DecoderRunning:   true,
		Receiving:        false,
		Degraded:         false,
		Reason:           band + " SDR active; no messages received in the last minute. This is expected when there is no nearby RF traffic.",
	}
}

func healthyGPS(now time.Time) GPSHealth {
	return BuildGPSHealth(true, "3D GPS", "u-blox 8", 10, 12, 12, 3.0, now, now, 10*time.Second, TimeGNSSSynced)
}

func healthyStorage() StorageHealth {
	return EvaluateStorage(true, true, false, "/dev/mmcblk0p3", "u", "u", statOfPercent(20), time.Now(), true, nil, DefaultPersistentStorageThresholds())
}

func healthyTime() (TimeHealth, TimeState) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	utc := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, utc, false)
	}
	return tt.Snapshot(now), tt.State()
}

func noHardwareFixtures() (ahrs, baro, fan FutureHardwareHealth) {
	return NotInstalled("AHRS board not yet installed"),
		NotInstalled("barometer not yet installed"),
		NotInstalled("fan-controller integration not yet implemented")
}

func TestFixture_HealthyDualBand(t *testing.T) {
	now := time.Now()
	uat := BuildRadioHealth(healthyBand("978", 0), 500, 12, 20, now, now, 3, map[string]int{"METAR": 40, "TAF": 5})
	es := BuildRadioHealth(healthyBand("1090", 1), 900, 30, 45, now, now, 0, nil)
	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 1, now, false)
	system := BuildSystemHealth("2.0-pre5", "d3ac9396", time.Hour, 55.0, false, false, nil)
	storage := healthyStorage()
	overlay := EvaluateStorage(true, true, false, "tmpfs", "", "", statOfPercent(30), now, true, nil, DefaultPersistentStorageThresholds())
	timeHealth, timeState := healthyTime()
	ahrs, baro, fan := noHardwareFixtures()

	r := BuildHealthReport(now, uat, es, gps, gdl90, system, storage, overlay, timeHealth, timeState, ahrs, baro, fan)

	if r.Overall != StateReady {
		t.Errorf("healthy dual-band report Overall = %q, want READY", r.Overall)
	}
	for name, s := range map[string]ComponentState{
		"UAT978": r.UAT978.State, "ES1090": r.ES1090.State, "GPS": r.GPS.State,
		"GDL90": r.GDL90.State, "System": r.System.State, "Storage": r.Storage.State,
	} {
		if s != StateReady {
			t.Errorf("%s State = %q, want READY", name, s)
		}
	}
}

func TestFixture_NoSignalIsNotAFailure(t *testing.T) {
	// Both radios healthy, decoders running, but zero traffic in range -
	// must never render red or amber. This is the dashboard's most
	// important color-rule requirement.
	now := time.Now()
	uat := BuildRadioHealth(noSignalBand("978", 0), 0, 0, 0, time.Time{}, now, 0, nil)
	es := BuildRadioHealth(noSignalBand("1090", 1), 0, 0, 0, time.Time{}, now, 0, nil)

	if uat.State != StateReady {
		t.Errorf("no-signal UAT State = %q (color %s), want READY/green - absence of traffic must never read as a failure",
			uat.State, uat.State.Color())
	}
	if es.State != StateReady {
		t.Errorf("no-signal ES State = %q (color %s), want READY/green", es.State, es.State.Color())
	}

	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 0, time.Time{}, false)
	system := BuildSystemHealth("2.0-pre5", "d3ac9396", time.Hour, 50.0, false, false, nil)
	storage := healthyStorage()
	overlay := healthyStorage()
	timeHealth, timeState := healthyTime()
	ahrs, baro, fan := noHardwareFixtures()
	r := BuildHealthReport(now, uat, es, gps, gdl90, system, storage, overlay, timeHealth, timeState, ahrs, baro, fan)
	if r.Overall != StateReady {
		t.Errorf("a fully healthy system with simply no current RF traffic must roll up to READY, got %q", r.Overall)
	}
}

func TestFixture_Degraded(t *testing.T) {
	now := time.Now()
	uat := BuildRadioHealth(healthyBand("978", 0), 500, 12, 20, now, now, 3, nil)
	es := BuildRadioHealth(healthyBand("1090", 1), 900, 30, 45, now, now, 0, nil)
	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 1, now, false)
	system := BuildSystemHealth("2.0-pre5", "d3ac9396", time.Hour, 55.0, false, false, nil)
	// Storage at 92% used - above critical (90%) but below the
	// recording-prohibited line (95%): DEGRADED, not NOT_READY.
	storage := EvaluateStorage(true, true, false, "/dev/mmcblk0p3", "u", "u", statOfPercent(92), now, true, nil, DefaultPersistentStorageThresholds())
	overlay := healthyStorage()
	timeHealth, timeState := healthyTime()
	ahrs, baro, fan := noHardwareFixtures()

	r := BuildHealthReport(now, uat, es, gps, gdl90, system, storage, overlay, timeHealth, timeState, ahrs, baro, fan)

	if r.Storage.State != StateDegraded {
		t.Errorf("92%%-full storage State = %q, want DEGRADED", r.Storage.State)
	}
	if r.Overall != StateDegraded {
		t.Errorf("a system with one degraded component should roll up Overall to DEGRADED, got %q", r.Overall)
	}
	if !r.Storage.RecordingAllowed {
		t.Error("storage at 92 percent used should still permit recording, only the 95 percent line prohibits it")
	}
}

func TestFixture_MissingHardwareDoesNotFailOrExposeFields(t *testing.T) {
	now := time.Now()
	uat := BuildRadioHealth(healthyBand("978", 0), 500, 12, 20, now, now, 3, nil)
	es := BuildRadioHealth(healthyBand("1090", 1), 900, 30, 45, now, now, 0, nil)
	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 1, now, false)
	system := BuildSystemHealth("2.0-pre5", "d3ac9396", time.Hour, 55.0, false, false, nil)
	storage := healthyStorage()
	overlay := healthyStorage()
	timeHealth, timeState := healthyTime()
	ahrs, baro, fan := noHardwareFixtures()

	r := BuildHealthReport(now, uat, es, gps, gdl90, system, storage, overlay, timeHealth, timeState, ahrs, baro, fan)

	for name, h := range map[string]FutureHardwareHealth{"AHRS": r.AHRS, "Baro": r.Baro, "Fan": r.Fan} {
		if h.State != StateNotInstalled {
			t.Errorf("%s State = %q, want NOT_INSTALLED", name, h.State)
		}
		if h.State.Color() != "gray" {
			t.Errorf("%s should render gray, not %s", name, h.State.Color())
		}
	}
	if r.Overall != StateReady {
		t.Errorf("missing future hardware must not prevent an otherwise-healthy Overall of READY, got %q", r.Overall)
	}
}

func TestBuildRadioHealth_DisabledBandIsNotInstalledNotFailed(t *testing.T) {
	disabled := sdrassign.BandStatus{Enabled: false, Reason: "978 UAT disabled by configuration."}
	h := BuildRadioHealth(disabled, 0, 0, 0, time.Time{}, time.Now(), 0, nil)
	if h.State != StateNotInstalled {
		t.Errorf("a user-disabled band should read NOT_INSTALLED (gray), not a failure, got %q", h.State)
	}
}

func TestBuildRadioHealth_ConflictIsNotReady(t *testing.T) {
	conflict := sdrassign.BandStatus{Enabled: true, Assigned: true, Conflict: true, Reason: "duplicate tag conflict"}
	h := BuildRadioHealth(conflict, 0, 0, 0, time.Time{}, time.Now(), 0, nil)
	if h.State != StateNotReady {
		t.Errorf("a conflicted band must read NOT_READY, got %q", h.State)
	}
}

func TestBuildRadioHealth_MissingReceiverIsNotReady(t *testing.T) {
	missing := sdrassign.BandStatus{Enabled: true, Assigned: false, Reason: "978 UAT enabled but no receiver assigned"}
	h := BuildRadioHealth(missing, 0, 0, 0, time.Time{}, time.Now(), 0, nil)
	if h.State != StateNotReady {
		t.Errorf("an enabled-but-unassigned band (missing radio) must read NOT_READY, got %q", h.State)
	}
}

func TestBuildGPSHealth_PresentWithoutFixIsDegradedNotFailed(t *testing.T) {
	now := time.Now()
	h := BuildGPSHealth(true, "No Fix", "u-blox 8", 0, 4, 4, 0, now, now, 10*time.Second, TimeUnsynchronized)
	if h.State != StateDegraded {
		t.Errorf("GPS present, searching for a fix, State = %q, want DEGRADED", h.State)
	}
}

func TestBuildGPSHealth_MissingDeviceIsNotReady(t *testing.T) {
	h := BuildGPSHealth(false, "", "", 0, 0, 0, 0, time.Time{}, time.Now(), 10*time.Second, TimeUnsynchronized)
	if h.State != StateNotReady {
		t.Errorf("a genuinely missing GPS device State = %q, want NOT_READY", h.State)
	}
}

func TestBuildGDL90Health_NoClientIsStillReady(t *testing.T) {
	// GDL90 generating and available with zero clients connected (e.g. on
	// the bench, no EFB open) must not read as a failure.
	h := BuildGDL90Health(true, true, 0, time.Time{}, false)
	if h.State != StateReady {
		t.Errorf("GDL90 active with no client State = %q, want READY", h.State)
	}
}

func TestBuildGDL90Health_DoesNotClaimForeFlightWithoutEvidence(t *testing.T) {
	h := BuildGDL90Health(true, true, 1, time.Now(), false)
	if h.ForeFlightClientDetected {
		t.Error("ForeFlightClientDetected must be false unless explicitly set from real identifying evidence")
	}
}
