package readiness

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// --- AHRSHealth ------------------------------------------------------

// availableProfileFixture is the "profile subsystem is fine, some
// ordinary profile is active" case every pre-existing AHRS fixture uses,
// so a profile-subsystem problem never affects tests that predate the
// calibration-profile feature and are not exercising it.
func availableProfileFixture() AHRSProfileInfo {
	return AHRSProfileInfo{
		Available: true,
		ID:        "profile-0000000000000000",
		Name:      "Current Installation",
		Kind:      "user",
	}
}

func TestBuildAHRSHealth_Ready(t *testing.T) {
	mono := time.Now()
	pitch, roll, gload := 1.5, -2.5, 1.02
	h := BuildAHRSHealth(true, true, 0x1F, &pitch, &roll, &gload, mono, mono, SomeTime(time.Now()), true, true, [2]int{-1, 0}, 2*time.Second, availableProfileFixture())
	if h.State != StateReady {
		t.Errorf("State = %q, want READY: %s", h.State, h.Reason)
	}
	if !h.PitchAvailable || !h.RollAvailable || !h.GLoadAvailable {
		t.Error("pitch/roll/g-load must be available when non-nil values are supplied")
	}
	if h.Stale {
		t.Error("a measurement taken at nowMono must not be stale")
	}
}

func TestBuildAHRSHealth_DisabledByConfigurationIsNotInstalledNotFailed(t *testing.T) {
	h := BuildAHRSHealth(false, false, 0, nil, nil, nil, time.Time{}, time.Now(), NoTime(), false, false, [2]int{}, 2*time.Second, availableProfileFixture())
	if h.State != StateNotInstalled {
		t.Errorf("disabled-by-configuration AHRS State = %q, want NOT_INSTALLED", h.State)
	}
	if h.State.Color() != "gray" {
		t.Errorf("disabled AHRS should render gray, not %s", h.State.Color())
	}
}

func TestBuildAHRSHealth_EnabledButMissingIsNotReady(t *testing.T) {
	// Enabled by configuration but the IMU has never connected - a real
	// problem for hardware the baseline requires, distinct from a
	// deliberate disable.
	h := BuildAHRSHealth(true, false, 0, nil, nil, nil, time.Time{}, time.Now(), NoTime(), false, false, [2]int{}, 2*time.Second, availableProfileFixture())
	if h.State != StateNotReady {
		t.Errorf("enabled-but-disconnected AHRS State = %q, want NOT_READY", h.State)
	}
}

func TestBuildAHRSHealth_StaleMeasurementIsDegraded(t *testing.T) {
	mono := time.Now()
	later := mono.Add(10 * time.Second) // staleAfter is 2s
	pitch, roll := 0.0, 0.0
	h := BuildAHRSHealth(true, true, 0x1F, &pitch, &roll, nil, mono, later, SomeTime(mono), true, true, [2]int{-1, 0}, 2*time.Second, availableProfileFixture())
	if h.State != StateDegraded {
		t.Errorf("stale AHRS State = %q, want DEGRADED", h.State)
	}
	if !h.Stale {
		t.Error("Stale must be true once age exceeds staleAfter")
	}
	if h.LastMeasurementAgeSeconds == nil || *h.LastMeasurementAgeSeconds != 10 {
		t.Errorf("LastMeasurementAgeSeconds = %v, want 10", h.LastMeasurementAgeSeconds)
	}
}

func TestBuildAHRSHealth_InvalidSentinelValuesAreUnavailableNotZero(t *testing.T) {
	// The caller (main/) is responsible for converting the AHRS library's
	// sentinel (ahrs.Invalid, ~3276.7) to nil before calling this
	// function - this test documents that once nil, the value must never
	// be treated as a real 0/valid reading, and health degrades rather
	// than reads READY.
	mono := time.Now()
	h := BuildAHRSHealth(true, true, 0x03, nil, nil, nil, mono, mono, SomeTime(time.Now()), true, true, [2]int{-1, 0}, 2*time.Second, availableProfileFixture())
	if h.PitchAvailable || h.RollAvailable || h.GLoadAvailable {
		t.Error("nil pitch/roll/g-load must report as unavailable, never as a fabricated zero value")
	}
	if h.State != StateDegraded {
		t.Errorf("AHRS connected/recent but with invalid pitch/roll State = %q, want DEGRADED", h.State)
	}
}

func TestBuildAHRSHealth_UnavailableHeadingNeverAffectsReadiness(t *testing.T) {
	// BuildAHRSHealth deliberately has no heading parameter at all - this
	// test documents that HeadingSupported is always false and that a
	// fully-ready AHRS report never depends on it.
	mono := time.Now()
	pitch, roll, gload := 0.1, 0.2, 1.0
	h := BuildAHRSHealth(true, true, 0x1F, &pitch, &roll, &gload, mono, mono, SomeTime(time.Now()), true, true, [2]int{-1, 0}, 2*time.Second, availableProfileFixture())
	if h.HeadingSupported {
		t.Error("HeadingSupported must always be false - magnetic heading is not calibrated in this build")
	}
	if h.State != StateReady {
		t.Errorf("State = %q, want READY - heading must never gate readiness", h.State)
	}
}

func TestBuildAHRSHealth_ReconnectingHasNoMeasurementYet(t *testing.T) {
	// IMU reports connected again after a drop, but no attitude solution
	// has been produced since reconnecting (AHRSLastAttitudeTime reset to
	// the zero value by main/sensors.go on the prior failure) - this must
	// read NOT_READY, not a stale-but-otherwise-normal DEGRADED, and must
	// never fabricate an age.
	h := BuildAHRSHealth(true, true, 0x02, nil, nil, nil, time.Time{}, time.Now(), NoTime(), true, true, [2]int{-1, 0}, 2*time.Second, availableProfileFixture())
	if h.State != StateNotReady {
		t.Errorf("reconnecting AHRS (no solution yet) State = %q, want NOT_READY", h.State)
	}
	if h.LastMeasurementAgeSeconds != nil {
		t.Error("LastMeasurementAgeSeconds must be nil, not a fabricated age, before any solution has been produced")
	}
}

func TestBuildAHRSHealth_UncalibratedIsDegraded(t *testing.T) {
	mono := time.Now()
	pitch, roll := 0.0, 0.0
	h := BuildAHRSHealth(true, true, 0x03, &pitch, &roll, nil, mono, mono, SomeTime(time.Now()), false, false, [2]int{}, 2*time.Second, availableProfileFixture())
	if h.State != StateDegraded {
		t.Errorf("no level reference set State = %q, want DEGRADED", h.State)
	}
}

func TestBuildAHRSHealth_GyroUncalibratedIsDegraded(t *testing.T) {
	// Level reference set, gyro zero-drift not - this is "incomplete"
	// calibration and must read DEGRADED, not READY.
	mono := time.Now()
	pitch, roll := 0.0, 0.0
	h := BuildAHRSHealth(true, true, 0x03, &pitch, &roll, nil, mono, mono, SomeTime(time.Now()), true, false, [2]int{-1, 0}, 2*time.Second, availableProfileFixture())
	if h.State != StateDegraded {
		t.Errorf("level set but gyro uncalibrated State = %q, want DEGRADED", h.State)
	}
}

func TestBuildAHRSHealth_ProfileUnavailableDowngradesReadyToDegraded(t *testing.T) {
	// Hardware is perfectly healthy - connected, fresh, calibrated - but
	// the profile subsystem itself has a problem (missing/corrupt
	// store). This must read DEGRADED, honestly, never a silently
	// ignored READY and never a false NOT_READY implying the hardware
	// itself failed.
	mono := time.Now()
	pitch, roll, gload := 0.5, -0.3, 1.0
	unavailable := AHRSProfileInfo{Available: false, Error: "profile store corrupt: unexpected end of JSON input"}
	h := BuildAHRSHealth(true, true, 0x1F, &pitch, &roll, &gload, mono, mono, SomeTime(time.Now()), true, true, [2]int{-1, 0}, 2*time.Second, unavailable)
	if h.State != StateDegraded {
		t.Errorf("profile-unavailable State = %q, want DEGRADED", h.State)
	}
	if !strings.Contains(h.Reason, "profile") {
		t.Errorf("Reason should mention the profile subsystem problem, got %q", h.Reason)
	}
	if h.Profile.Available {
		t.Error("Profile.Available should reflect the unavailable input")
	}
}

func TestBuildAHRSHealth_ProfileUnavailableDoesNotMaskHardwareFailure(t *testing.T) {
	// A genuinely disconnected IMU must stay NOT_READY - a profile
	// problem on top of it must never look milder than the hardware
	// failure alone would.
	unavailable := AHRSProfileInfo{Available: false, Error: "profile store missing"}
	h := BuildAHRSHealth(true, false, 0, nil, nil, nil, time.Time{}, time.Now(), NoTime(), false, false, [2]int{}, 2*time.Second, unavailable)
	if h.State != StateNotReady {
		t.Errorf("disconnected IMU with an unavailable profile subsystem State = %q, want NOT_READY (hardware failure must not be masked)", h.State)
	}
}

func TestBuildAHRSHealth_ProfileFieldsPopulated(t *testing.T) {
	mono := time.Now()
	pitch, roll, gload := 0.1, 0.2, 1.0
	then := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	profile := AHRSProfileInfo{
		Available:        true,
		ID:               "profile-0123456789abcdef",
		Name:             "Cherokee Six",
		Kind:             "user",
		LastCalibratedAt: SomeTime(then),
	}
	h := BuildAHRSHealth(true, true, 0x1F, &pitch, &roll, &gload, mono, mono, SomeTime(time.Now()), true, true, [2]int{-1, 0}, 2*time.Second, profile)
	if h.Profile.ID != "profile-0123456789abcdef" || h.Profile.Name != "Cherokee Six" || h.Profile.Kind != "user" {
		t.Errorf("Profile fields not passed through: %+v", h.Profile)
	}
	if h.Profile.LastCalibratedAt.IsZero() || !h.Profile.LastCalibratedAt.Time.Equal(then) {
		t.Errorf("Profile.LastCalibratedAt = %v, want %v", h.Profile.LastCalibratedAt, then)
	}
}

func TestBuildAHRSHealth_JSONNeverContainsYearOne(t *testing.T) {
	h := BuildAHRSHealth(true, false, 0, nil, nil, nil, time.Time{}, time.Now(), NoTime(), false, false, [2]int{}, 2*time.Second, availableProfileFixture())
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("AHRSHealth JSON must never contain a year-1 timestamp: %s", b)
	}
}

// --- BaroHealth --------------------------------------------------------

func TestBuildBaroHealth_Ready(t *testing.T) {
	mono := time.Now()
	temp, alt, vs := 22.0, 1200.0, 150.0
	h := BuildBaroHealth(true, true, &temp, &alt, &vs, BaroSourceTypeName(1), mono, mono, SomeTime(time.Now()), 15*time.Second)
	if h.State != StateReady {
		t.Errorf("State = %q, want READY: %s", h.State, h.Reason)
	}
}

func TestBuildBaroHealth_DisabledByConfigurationIsNotInstalled(t *testing.T) {
	h := BuildBaroHealth(false, false, nil, nil, nil, "none", time.Time{}, time.Now(), NoTime(), 15*time.Second)
	if h.State != StateNotInstalled {
		t.Errorf("disabled barometer State = %q, want NOT_INSTALLED", h.State)
	}
}

func TestBuildBaroHealth_EnabledButMissingIsNotReady(t *testing.T) {
	h := BuildBaroHealth(true, false, nil, nil, nil, "none", time.Time{}, time.Now(), NoTime(), 15*time.Second)
	if h.State != StateNotReady {
		t.Errorf("enabled-but-disconnected barometer State = %q, want NOT_READY", h.State)
	}
}

func TestBuildBaroHealth_StaleIsDegraded(t *testing.T) {
	mono := time.Now()
	later := mono.Add(30 * time.Second)
	temp, alt := 20.0, 1000.0
	h := BuildBaroHealth(true, true, &temp, &alt, nil, "BMP280/BMP388 (onboard)", mono, later, SomeTime(mono), 15*time.Second)
	if h.State != StateDegraded {
		t.Errorf("stale barometer State = %q, want DEGRADED", h.State)
	}
	if h.LastMeasurementAgeSeconds == nil || *h.LastMeasurementAgeSeconds != 30 {
		t.Errorf("LastMeasurementAgeSeconds = %v, want 30", h.LastMeasurementAgeSeconds)
	}
}

func TestBuildBaroHealth_NonFiniteIsNotReady(t *testing.T) {
	mono := time.Now()
	nan := math.NaN()
	h := BuildBaroHealth(true, true, nil, &nan, nil, "BMP280/BMP388 (onboard)", mono, mono, SomeTime(time.Now()), 15*time.Second)
	if h.State != StateNotReady {
		t.Errorf("non-finite pressure altitude State = %q, want NOT_READY", h.State)
	}
	if !h.NonFinite {
		t.Error("NonFinite must be true for a NaN reading")
	}
}

func TestBuildBaroHealth_ImplausibleIsDegradedNotFailed(t *testing.T) {
	// Structurally implausible (e.g. 90000ft) but finite - a hardware
	// oddity worth flagging, not a certain failure, and never confused
	// with ordinary cabin-pressure variation which stays well inside the
	// bound.
	mono := time.Now()
	alt := 90000.0
	h := BuildBaroHealth(true, true, nil, &alt, nil, "BMP280/BMP388 (onboard)", mono, mono, SomeTime(time.Now()), 15*time.Second)
	if h.State != StateDegraded {
		t.Errorf("implausible-altitude barometer State = %q, want DEGRADED", h.State)
	}
	if !h.Implausible {
		t.Error("Implausible must be true for a structurally out-of-range altitude")
	}
}

func TestBuildBaroHealth_OrdinaryCabinPressureVariationIsNotImplausible(t *testing.T) {
	mono := time.Now()
	alt := 8000.0 // routine light-aircraft cabin/field pressure altitude
	temp := 15.0
	h := BuildBaroHealth(true, true, &temp, &alt, nil, "BMP280/BMP388 (onboard)", mono, mono, SomeTime(time.Now()), 15*time.Second)
	if h.Implausible {
		t.Error("ordinary cabin-pressure-altitude variation must never be flagged implausible")
	}
	if h.State != StateReady {
		t.Errorf("State = %q, want READY", h.State)
	}
}

func TestBuildBaroHealth_Reconnection(t *testing.T) {
	h := BuildBaroHealth(true, true, nil, nil, nil, "none", time.Time{}, time.Now(), NoTime(), 15*time.Second)
	if h.State != StateNotReady {
		t.Errorf("reconnecting barometer (no measurement yet) State = %q, want NOT_READY", h.State)
	}
}

func TestBuildBaroHealth_JSONNeverContainsYearOne(t *testing.T) {
	h := BuildBaroHealth(true, false, nil, nil, nil, "none", time.Time{}, time.Now(), NoTime(), 15*time.Second)
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("BaroHealth JSON must never contain a year-1 timestamp: %s", b)
	}
}

// --- FanHealth -----------------------------------------------------------

func TestBuildFanHealth_ServiceAbsentIsNotInstalled(t *testing.T) {
	h := BuildFanHealth(false, false, false, false, "", "", nil, nil, nil, nil, nil, time.Time{}, time.Now(), 10*time.Second)
	if h.State != StateNotInstalled {
		t.Errorf("service-not-installed Fan State = %q, want NOT_INSTALLED", h.State)
	}
}

func TestBuildFanHealth_ServiceInstalledButInactiveIsNotReady(t *testing.T) {
	h := BuildFanHealth(true, false, false, false, "", "", nil, nil, nil, nil, nil, time.Time{}, time.Now(), 10*time.Second)
	if h.State != StateNotReady {
		t.Errorf("installed-but-inactive Fan State = %q, want NOT_READY", h.State)
	}
}

func TestBuildFanHealth_Idle(t *testing.T) {
	now := time.Now()
	temp, target := 42.0, 50.0
	dutyMin, duty, freq := uint32(0), uint32(1), uint32(64000)
	h := BuildFanHealth(true, true, true, false, "IDLE", "", &temp, &target, &dutyMin, &duty, &freq, now, now, 10*time.Second)
	if h.State != StateReady {
		t.Errorf("idle-but-healthy Fan State = %q, want READY: %s", h.State, h.Reason)
	}
	if h.TachometerSupported {
		t.Error("TachometerSupported must always be false - no physical rotation feedback exists")
	}
	if !strings.Contains(h.Reason, "rotation feedback unavailable") {
		t.Errorf("Reason must explicitly say rotation feedback is unavailable, got %q", h.Reason)
	}
}

func TestBuildFanHealth_Commanding(t *testing.T) {
	now := time.Now()
	temp, target := 58.0, 50.0
	dutyMin, duty, freq := uint32(20), uint32(65), uint32(64000)
	h := BuildFanHealth(true, true, true, false, "COMMANDING", "", &temp, &target, &dutyMin, &duty, &freq, now, now, 10*time.Second)
	if h.State != StateReady {
		t.Errorf("commanding Fan State = %q, want READY", h.State)
	}
	if h.RequestedDutyPercent == nil || *h.RequestedDutyPercent != 65 {
		t.Errorf("RequestedDutyPercent = %v, want 65", h.RequestedDutyPercent)
	}
}

func TestBuildFanHealth_StaleStatusIsDegraded(t *testing.T) {
	last := time.Now().Add(-time.Minute)
	now := time.Now()
	h := BuildFanHealth(true, true, true, false, "COMMANDING", "", nil, nil, nil, nil, nil, last, now, 10*time.Second)
	if h.State != StateDegraded {
		t.Errorf("stale fan status State = %q, want DEGRADED", h.State)
	}
	if !h.Stale {
		t.Error("Stale must be true once age exceeds staleAfter")
	}
}

func TestBuildFanHealth_MalformedStatusIsDegraded(t *testing.T) {
	h := BuildFanHealth(true, true, true, true, "", "", nil, nil, nil, nil, nil, time.Time{}, time.Now(), 10*time.Second)
	if h.State != StateDegraded {
		t.Errorf("malformed status Fan State = %q, want DEGRADED", h.State)
	}
}

func TestBuildFanHealth_ControllerErrorIsNotReady(t *testing.T) {
	now := time.Now()
	h := BuildFanHealth(true, true, true, false, "ERROR", "GPIO open failed: permission denied", nil, nil, nil, nil, nil, now, now, 10*time.Second)
	if h.State != StateNotReady {
		t.Errorf("controller-reported-error Fan State = %q, want NOT_READY", h.State)
	}
}

func TestBuildFanHealth_NeverClaimsOperationalWithoutTachometer(t *testing.T) {
	// Even in the best-case READY/COMMANDING scenario, the reason text
	// must not claim the physical fan is confirmed spinning.
	now := time.Now()
	dutyMin, duty := uint32(0), uint32(100)
	h := BuildFanHealth(true, true, true, false, "COMMANDING", "", nil, nil, &dutyMin, &duty, nil, now, now, 10*time.Second)
	for _, claim := range []string{"fan is spinning", "fan spinning", "confirmed operational"} {
		if strings.Contains(strings.ToLower(h.Reason), claim) {
			t.Errorf("Reason must never claim physical rotation: %q", h.Reason)
		}
	}
}

func TestBuildFanHealth_JSONNeverContainsYearOne(t *testing.T) {
	h := BuildFanHealth(false, false, false, false, "", "", nil, nil, nil, nil, nil, time.Time{}, time.Now(), 10*time.Second)
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "0001-01-01") {
		t.Errorf("FanHealth JSON must never contain a year-1 timestamp: %s", b)
	}
}

// --- systemd unit-state parsing ------------------------------------------

func TestParseUnitActiveState(t *testing.T) {
	cases := map[string]string{
		"active\n":     "active",
		"inactive\n":   "inactive",
		"failed\n":     "failed",
		"activating\n": "activating",
		"unknown\n":    "unknown",
		"":             "",
	}
	for in, want := range cases {
		if got := ParseUnitActiveState(in); got != want {
			t.Errorf("ParseUnitActiveState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnitInstalled(t *testing.T) {
	installed := []string{"active", "inactive", "failed", "activating", "deactivating"}
	for _, s := range installed {
		if !UnitInstalled(s) {
			t.Errorf("UnitInstalled(%q) = false, want true", s)
		}
	}
	notInstalled := []string{"unknown", ""}
	for _, s := range notInstalled {
		if UnitInstalled(s) {
			t.Errorf("UnitInstalled(%q) = true, want false", s)
		}
	}
}
