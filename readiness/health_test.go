package readiness

import (
	"encoding/json"
	"strings"
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
		wall := utc.Add(time.Duration(i) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, wall, false)
	}
	return tt.Snapshot(now, utc.Add(time.Duration(cfg().RequiredConsecutive)*time.Second)), tt.State()
}

func noHardwareFixtures() (ahrs, baro, fan FutureHardwareHealth) {
	return NotInstalled("AHRS board not yet installed"),
		NotInstalled("barometer not yet installed"),
		NotInstalled("fan-controller integration not yet implemented")
}

func TestFixture_HealthyDualBand(t *testing.T) {
	now := time.Now()
	uat := BuildRadioHealth(healthyBand("978", 0), 500, 12, 20, now, now, SomeTime(now), 3, map[string]int{"METAR": 40, "TAF": 5})
	es := BuildRadioHealth(healthyBand("1090", 1), 900, 30, 45, now, now, SomeTime(now), 0, nil)
	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 1, SomeTime(now))
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
	uat := BuildRadioHealth(noSignalBand("978", 0), 0, 0, 0, time.Time{}, now, NoTime(), 0, nil)
	es := BuildRadioHealth(noSignalBand("1090", 1), 0, 0, 0, time.Time{}, now, NoTime(), 0, nil)

	if uat.State != StateReady {
		t.Errorf("no-signal UAT State = %q (color %s), want READY/green - absence of traffic must never read as a failure",
			uat.State, uat.State.Color())
	}
	if es.State != StateReady {
		t.Errorf("no-signal ES State = %q (color %s), want READY/green", es.State, es.State.Color())
	}
	if uat.LastFrameAgeSeconds != nil {
		t.Error("a band with no frame ever received must have a nil LastFrameAgeSeconds, not a fabricated age")
	}
	if !uat.LastFrameTime.IsZero() {
		t.Error("a band with no frame ever received must have an unavailable LastFrameTime")
	}

	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 0, NoTime())
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
	uat := BuildRadioHealth(healthyBand("978", 0), 500, 12, 20, now, now, SomeTime(now), 3, nil)
	es := BuildRadioHealth(healthyBand("1090", 1), 900, 30, 45, now, now, SomeTime(now), 0, nil)
	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 1, SomeTime(now))
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
	uat := BuildRadioHealth(healthyBand("978", 0), 500, 12, 20, now, now, SomeTime(now), 3, nil)
	es := BuildRadioHealth(healthyBand("1090", 1), 900, 30, 45, now, now, SomeTime(now), 0, nil)
	gps := healthyGPS(now)
	gdl90 := BuildGDL90Health(true, true, 1, SomeTime(now))
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
	h := BuildRadioHealth(disabled, 0, 0, 0, time.Time{}, time.Now(), NoTime(), 0, nil)
	if h.State != StateNotInstalled {
		t.Errorf("a user-disabled band should read NOT_INSTALLED (gray), not a failure, got %q", h.State)
	}
}

func TestBuildRadioHealth_ExternallySatisfiedUnassignedBandIsReady(t *testing.T) {
	// An external low-power 978 UAT radio serves the band directly - it is
	// never SDR-assigned, so Assigned is false, but this must not read as
	// missing/NOT_READY. Mirrors sdrassign's own BuildBandStatus, which
	// excludes ExternallySatisfied bands from Degraded for the same reason.
	external := sdrassign.BandStatus{
		Enabled:             true,
		Assigned:            false,
		ExternallySatisfied: true,
		AssignmentSource:    "external",
		Reason:              "978 UAT served by an external low-power radio.",
	}
	now := time.Now()
	h := BuildRadioHealth(external, 1200, 40, 60, now, now, SomeTime(now), 2, map[string]int{"METAR": 10})
	if h.State != StateReady {
		t.Errorf("an externally-satisfied, SDR-unassigned band must read READY, got %q (color %s)", h.State, h.State.Color())
	}
}

func TestBuildRadioHealth_ExternallySatisfiedOverridesConflict(t *testing.T) {
	// ExternallySatisfied is checked before Ambiguous/Conflict/Assigned in
	// StateFromBandStatus, matching sdrassign's own precedence.
	b := sdrassign.BandStatus{Enabled: true, ExternallySatisfied: true, Conflict: true, Ambiguous: true, Assigned: false}
	if got := StateFromBandStatus(b); got != StateReady {
		t.Errorf("StateFromBandStatus with ExternallySatisfied=true = %q, want READY", got)
	}
}

func TestBuildRadioHealth_ConflictIsNotReady(t *testing.T) {
	conflict := sdrassign.BandStatus{Enabled: true, Assigned: true, Conflict: true, Reason: "duplicate tag conflict"}
	h := BuildRadioHealth(conflict, 0, 0, 0, time.Time{}, time.Now(), NoTime(), 0, nil)
	if h.State != StateNotReady {
		t.Errorf("a conflicted band must read NOT_READY, got %q", h.State)
	}
}

func TestBuildRadioHealth_MissingReceiverIsNotReady(t *testing.T) {
	missing := sdrassign.BandStatus{Enabled: true, Assigned: false, Reason: "978 UAT enabled but no receiver assigned"}
	h := BuildRadioHealth(missing, 0, 0, 0, time.Time{}, time.Now(), NoTime(), 0, nil)
	if h.State != StateNotReady {
		t.Errorf("an enabled-but-unassigned band (missing radio) must read NOT_READY, got %q", h.State)
	}
}

// --- Receiver last-frame timestamp/age semantics ---

func TestBuildRadioHealth_NeverReceivedIsUnavailableNotZeroAge(t *testing.T) {
	now := time.Now()
	h := BuildRadioHealth(healthyBand("978", 0), 0, 0, 0, time.Time{}, now, NoTime(), 0, nil)
	if h.LastFrameAgeSeconds != nil {
		t.Error("LastFrameAgeSeconds must be nil (unavailable), not a fabricated 0, when no frame has ever been received")
	}
	if !h.LastFrameTime.IsZero() {
		t.Error("LastFrameTime must be unavailable when no frame has ever been received")
	}
}

func TestBuildRadioHealth_FirstFrameSetsAgeNearZero(t *testing.T) {
	mono := time.Now()
	wall := time.Date(2026, 8, 31, 3, 0, 0, 0, time.UTC)
	h := BuildRadioHealth(healthyBand("978", 0), 1, 5, 5, mono, mono, SomeTime(wall), 0, nil)
	if h.LastFrameAgeSeconds == nil {
		t.Fatal("LastFrameAgeSeconds must be populated once a frame has been received")
	}
	if *h.LastFrameAgeSeconds != 0 {
		t.Errorf("age immediately after the frame = %v, want 0", *h.LastFrameAgeSeconds)
	}
	if h.LastFrameTime.IsZero() || !h.LastFrameTime.Time.Equal(wall) {
		t.Errorf("LastFrameTime = %v, want %v", h.LastFrameTime, wall)
	}
}

func TestBuildRadioHealth_AgeGrowsWithMonotonicElapsed(t *testing.T) {
	mono := time.Now()
	later := mono.Add(30 * time.Second)
	h := BuildRadioHealth(healthyBand("978", 0), 1, 0, 5, mono, later, SomeTime(mono), 0, nil)
	if h.LastFrameAgeSeconds == nil || *h.LastFrameAgeSeconds != 30 {
		t.Errorf("age = %v, want 30s", h.LastFrameAgeSeconds)
	}
}

func TestBuildRadioHealth_WallClockStepDoesNotAffectAge(t *testing.T) {
	// The whole point of computing age on the monotonic clock: a wall-clock
	// correction between the frame and "now" must not perturb the age at
	// all, since BuildRadioHealth is never even given the wall-clock
	// values for this calculation - only lastFrameMono/nowMono.
	mono := time.Now()
	laterMono := mono.Add(10 * time.Second)
	// lastFrameWall is deliberately a wildly different, even backward,
	// wall-clock value - it must have zero effect on the age.
	backwardWall := time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)
	h := BuildRadioHealth(healthyBand("978", 0), 1, 0, 5, mono, laterMono, SomeTime(backwardWall), 0, nil)
	if h.LastFrameAgeSeconds == nil || *h.LastFrameAgeSeconds != 10 {
		t.Errorf("age = %v, want 10s regardless of the wall-clock display value", h.LastFrameAgeSeconds)
	}
}

func TestBuildRadioHealth_ExternalUATNeverReceivedIsStillReadyNotFailed(t *testing.T) {
	// Mission-critical distinction: external 978 with zero rate/no frame
	// ever received must still be READY (externally satisfied), never
	// downgraded merely for lacking a last-frame timestamp.
	external := sdrassign.BandStatus{
		Enabled:             true,
		ExternallySatisfied: true,
		AssignmentSource:    "external",
		Reason:              "978 UAT served by an external low-power radio.",
	}
	h := BuildRadioHealth(external, 0, 0, 0, time.Time{}, time.Now(), NoTime(), 0, nil)
	if h.State != StateReady {
		t.Errorf("external UAT with no frame yet received must still be READY, got %q", h.State)
	}
}

func TestBuildRadioHealth_JSONNeverContainsYearOne(t *testing.T) {
	h := BuildRadioHealth(healthyBand("978", 0), 0, 0, 0, time.Time{}, time.Now(), NoTime(), 0, nil)
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("RadioHealth JSON must never contain a year-1 timestamp for an unavailable LastFrameTime: %s", b)
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
	h := BuildGDL90Health(true, true, 0, NoTime())
	if h.State != StateReady {
		t.Errorf("GDL90 active with no client State = %q, want READY", h.State)
	}
}

func TestBuildGDL90Health_DoesNotClaimForeFlightWithoutEvidence(t *testing.T) {
	h := BuildGDL90Health(true, true, 1, SomeTime(time.Now()))
	if h.ForeFlightClientDetected {
		t.Error("ForeFlightClientDetected must be false unless explicitly set from real identifying evidence")
	}
}

func TestBuildGDL90Health_LastNetworkClientActivityUnavailableWhenNeverSeen(t *testing.T) {
	h := BuildGDL90Health(true, true, 0, NoTime())
	if !h.LastNetworkClientActivity.IsZero() {
		t.Error("LastNetworkClientActivity must be unavailable when no client activity has ever been observed")
	}
	if !h.LastClientActivity.IsZero() {
		t.Error("the legacy LastClientActivity field must agree with LastNetworkClientActivity")
	}
}

func TestBuildGDL90Health_LastNetworkClientActivityPopulatedWhenSeen(t *testing.T) {
	now := time.Now()
	h := BuildGDL90Health(true, true, 2, SomeTime(now))
	if h.LastNetworkClientActivity.IsZero() || !h.LastNetworkClientActivity.Time.Equal(now) {
		t.Errorf("LastNetworkClientActivity = %v, want %v", h.LastNetworkClientActivity, now)
	}
	if h.LastClientActivity.IsZero() || !h.LastClientActivity.Time.Equal(now) {
		t.Error("legacy LastClientActivity must equal LastNetworkClientActivity")
	}
}

func TestBuildGDL90Health_JSONNeverContainsYearOne(t *testing.T) {
	h := BuildGDL90Health(true, true, 0, NoTime())
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("GDL90Health JSON must never contain a year-1 timestamp: %s", b)
	}
}

func TestBuildForeFlightDetection_OutputInactiveIsUnknown(t *testing.T) {
	// If GDL90 isn't even generating, there is no basis to say anything
	// about client presence - this must not be conflated with "confirmed
	// absent" (NOT_DETECTED), which is a stronger claim.
	c := BuildForeFlightDetection(false, false)
	if c.State != ClientUnknown {
		t.Errorf("State = %q, want UNKNOWN", c.State)
	}
	if c.DetectionBasis == "" {
		t.Error("DetectionBasis must always be populated, regardless of State")
	}
}

func TestBuildForeFlightDetection_OutputInactiveIsUnknownEvenIfClientsClaimedAssociated(t *testing.T) {
	// Output-active gates everything: if GDL90 itself isn't running, a
	// caller-asserted "clients associated" is not trustworthy evidence of
	// anything - stay UNKNOWN rather than NOT_DETECTED or UNSUPPORTED.
	c := BuildForeFlightDetection(false, true)
	if c.State != ClientUnknown {
		t.Errorf("State = %q, want UNKNOWN", c.State)
	}
}

func TestBuildForeFlightDetection_NoClientsIsConfidentlyNotDetected(t *testing.T) {
	// Zero clients associated is real, sufficient evidence that ForeFlight
	// specifically cannot be among them - this is the one case allowed to
	// reach a confident negative without per-app identification.
	c := BuildForeFlightDetection(true, false)
	if c.State != ClientNotDetected {
		t.Errorf("State = %q, want NOT_DETECTED", c.State)
	}
}

func TestBuildForeFlightDetection_ClientsPresentIsUnsupportedNotDetected(t *testing.T) {
	// This is the key honesty check: one or more clients being present
	// must NOT be reported as ForeFlight being detected - the protocol
	// gives no application-layer identification, so the correct answer is
	// "can't tell", not a guess in either direction.
	c := BuildForeFlightDetection(true, true)
	if c.State != ClientUnsupported {
		t.Errorf("State = %q, want UNSUPPORTED", c.State)
	}
	if c.State == ClientDetected {
		t.Fatal("must never report DETECTED without real application-layer evidence")
	}
}

func TestBuildForeFlightDetection_LastSeenAlwaysUnavailable(t *testing.T) {
	// ForeFlight-specific LastSeen must never be populated: no application-
	// layer evidence identifying ForeFlight exists anywhere in this
	// project, in any input combination, so there is never a real instant
	// to report - conflating generic network activity with "ForeFlight was
	// seen" is exactly the overclaim this model exists to avoid.
	for _, outputActive := range []bool{false, true} {
		for _, clients := range []bool{false, true} {
			c := BuildForeFlightDetection(outputActive, clients)
			if !c.LastSeen.IsZero() {
				t.Errorf("outputActive=%v clients=%v: LastSeen must be unavailable, got %v", outputActive, clients, c.LastSeen)
			}
		}
	}
}

func TestBuildForeFlightDetection_NeverReachesDetectedToday(t *testing.T) {
	// Documents the current, honest limit: with no application-layer
	// evidence source implemented anywhere in this project, DETECTED must
	// be unreachable from any input combination today. This test is
	// expected to need updating (not silently pass) the day real evidence
	// is wired in.
	for _, outputActive := range []bool{false, true} {
		for _, clients := range []bool{false, true} {
			c := BuildForeFlightDetection(outputActive, clients)
			if c.State == ClientDetected {
				t.Errorf("outputActive=%v clients=%v produced ClientDetected with no evidence source implemented", outputActive, clients)
			}
		}
	}
}

func TestBuildForeFlightDetection_JSONNeverContainsYearOne(t *testing.T) {
	c := BuildForeFlightDetection(true, true)
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("ClientObservability JSON must never contain a year-1 timestamp: %s", b)
	}
}

func TestBuildGDL90Health_ForeFlightDetectionIsPopulatedAndConsistent(t *testing.T) {
	h := BuildGDL90Health(true, true, 2, SomeTime(time.Now()))
	if h.ForeFlightDetection.State != ClientUnsupported {
		t.Errorf("ForeFlightDetection.State = %q, want UNSUPPORTED with clients present", h.ForeFlightDetection.State)
	}
	// The legacy bool field must always agree with the new model's notion
	// of a confirmed-positive result.
	if h.ForeFlightClientDetected != (h.ForeFlightDetection.State == ClientDetected) {
		t.Error("ForeFlightClientDetected must equal (ForeFlightDetection.State == ClientDetected)")
	}

	h2 := BuildGDL90Health(true, true, 0, NoTime())
	if h2.ForeFlightDetection.State != ClientNotDetected {
		t.Errorf("ForeFlightDetection.State = %q, want NOT_DETECTED with zero clients", h2.ForeFlightDetection.State)
	}

	h3 := BuildGDL90Health(false, false, 0, NoTime())
	if h3.ForeFlightDetection.State != ClientUnknown {
		t.Errorf("ForeFlightDetection.State = %q, want UNKNOWN with GDL90 inactive", h3.ForeFlightDetection.State)
	}
}
