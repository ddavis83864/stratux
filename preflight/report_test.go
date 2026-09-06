package preflight

import (
	"testing"
	"time"

	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/sdrassign"
)

// healthyInput returns an Input describing a fully healthy system - every
// individual test below starts from this and degrades exactly the one
// signal it means to exercise, so a failing test clearly identifies which
// rule broke.
func healthyInput() Input {
	frameAge := 1.0
	utcNow := time.Now().UTC()
	return Input{
		UptimeSeconds: 600, // well past every grace period
		Health: readiness.HealthReport{
			Overall: readiness.StateReady,
			UAT978: readiness.RadioHealth{
				State: readiness.StateReady,
				Band: sdrassign.BandStatus{
					Enabled: true, ExternallySatisfied: true, DecoderRunning: true, Receiving: true,
				},
				LastFrameAgeSeconds:  &frameAge,
				TowerCount:           1,
				WeatherProductCounts: map[string]int{"METAR": 3},
			},
			ES1090: readiness.RadioHealth{
				State: readiness.StateReady,
				Band: sdrassign.BandStatus{
					Enabled: true, Assigned: true, DecoderRunning: true, Receiving: true, DeviceSerial: "stx:1090:0",
				},
				LastFrameAgeSeconds: &frameAge,
			},
			GPS: readiness.GPSHealth{
				State: readiness.StateReady, Present: true, DeviceType: "u-blox 8",
				FixType: "3D GPS", SatellitesInSolution: 8, AccuracyMeters: 5,
			},
			Time: readiness.TimeHealth{
				State: readiness.TimeGNSSSynced, Source: "GPS",
				LastSyncTime: readiness.SomeTime(utcNow),
			},
			GDL90: readiness.GDL90Health{
				State: readiness.StateReady, Generating: true, OutputActive: true, RecentClientCount: 2,
			},
			System: readiness.SystemHealth{State: readiness.StateReady},
			Storage: readiness.StorageHealth{
				State: readiness.StateReady, Present: true, Mounted: true, RecordingAllowed: true,
			},
			TemporaryOverlay: readiness.StorageHealth{State: readiness.StateReady, Present: true, Mounted: true},
			AHRS: readiness.AHRSHealth{
				State: readiness.StateReady, Enabled: true, Connected: true,
				Profile: readiness.AHRSProfileInfo{Available: true, ID: "profile-1", Name: "Test Aircraft", Kind: "user"},
			},
			Baro: readiness.BaroHealth{State: readiness.StateReady, Enabled: true, Connected: true},
			Fan:  readiness.FanHealth{State: readiness.StateReady, ServiceInstalled: true, ServiceActive: true, StatusAvailable: true, ControllerState: "COMMANDING"},
		},
		Profile:        ProfileSummary{Available: true, ID: "profile-1", Name: "Test Aircraft", Kind: "user", CalibrationValid: true},
		Recording:      RecordingReadiness{StorageAvailable: true, Permitted: true, State: "idle"},
		ManualAcks:     allAcked(),
		BootSessionID:  "sess-test",
		GeneratedAtUTC: &utcNow,
	}
}

// allAcked returns a ManualAcks map with every manual check acknowledged
// - the counterpart to healthyInput being fully automated-healthy.
func allAcked() map[ManualCheckID]*ManualAck {
	out := make(map[ManualCheckID]*ManualAck, len(ManualCheckDefinitions))
	now := time.Now().UTC()
	for _, d := range ManualCheckDefinitions {
		out[d.ID] = &ManualAck{CheckID: d.ID, AckedAtUTC: &now, SessionID: "sess-test"}
	}
	return out
}

func mustOverall(t *testing.T, in Input, want State) Report {
	t.Helper()
	r := BuildReport(in)
	if !r.Overall.OverallValid() {
		t.Fatalf("Overall = %q is not a valid overall state", r.Overall)
	}
	if r.Overall != want {
		t.Errorf("Overall = %q, want %q (automated=%+v)", r.Overall, want, r.Automated)
	}
	return r
}

func TestBuildReport_FullyHealthy(t *testing.T) {
	mustOverall(t, healthyInput(), StateReady)
}

func TestBuildReport_StartupGracePeriods(t *testing.T) {
	in := healthyInput()
	in.UptimeSeconds = 5 // well within every grace period
	in.Health.GPS = readiness.GPSHealth{State: readiness.StateDegraded, Present: true, FixType: "No Fix"}
	in.Health.Time = readiness.TimeHealth{State: readiness.TimeUnsynchronized}
	// Still within grace - must not be NOT_READY.
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "gps_fix" && c.State != StateUnknown {
			t.Errorf("gps_fix State = %q within grace period, want UNKNOWN", c.State)
		}
	}
}

func TestBuildReport_GraceExpiration_GPSUnavailable(t *testing.T) {
	in := healthyInput()
	in.UptimeSeconds = graceGPSAcquisitionSeconds + 1
	in.Health.GPS = readiness.GPSHealth{State: readiness.StateDegraded, Present: true, FixType: "No Fix"}
	r := mustOverall(t, in, StateCaution)
	found := false
	for _, c := range r.Automated {
		if c.CheckID == "gps_fix" {
			found = true
			if c.State != StateCaution {
				t.Errorf("gps_fix State = %q after grace expired with no fix, want CAUTION", c.State)
			}
		}
	}
	if !found {
		t.Fatal("gps_fix check not found")
	}
}

func TestBuildReport_GPSUnavailable_NoHardware(t *testing.T) {
	in := healthyInput()
	in.Health.GPS = readiness.GPSHealth{State: readiness.StateNotReady, Present: false}
	mustOverall(t, in, StateNotReady)
}

func TestBuildReport_TrustedTimeUnavailable(t *testing.T) {
	in := healthyInput()
	in.UptimeSeconds = graceGNSSTimeSeconds + 1
	in.Health.Time = readiness.TimeHealth{State: readiness.TimeUnsynchronized}
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "trusted_time" && c.Blocking {
			t.Error("trusted_time must never be blocking")
		}
	}
}

func TestBuildReport_External978ReceiverZeroFrames(t *testing.T) {
	in := healthyInput()
	in.Health.UAT978.LastFrameAgeSeconds = nil
	in.Health.UAT978.TowerCount = 0
	r := mustOverall(t, in, StateCaution) // caution from zero traffic/no tower, not blocking
	for _, c := range r.Automated {
		if c.CheckID == "978_traffic" && c.State == StateNotReady {
			t.Error("a correctly detected receiver with zero recent traffic must never be NOT_READY")
		}
	}
}

func TestBuildReport_Healthy1090NoCurrentTraffic(t *testing.T) {
	in := healthyInput()
	in.Health.ES1090.LastFrameAgeSeconds = nil
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "1090_traffic" {
			if c.State == StateNotReady {
				t.Error("healthy 1090 receiver with no current traffic must never be NOT_READY")
			}
			if c.Blocking {
				t.Error("1090_traffic must never be blocking")
			}
		}
	}
}

func TestBuildReport_FISBTowerUnavailable(t *testing.T) {
	in := healthyInput()
	in.Health.UAT978.TowerCount = 0
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "fisb_tower" {
			if c.State == StateNotReady || c.Blocking {
				t.Error("absence of a FIS-B tower must never be NOT_READY/blocking")
			}
		}
	}
}

func TestBuildReport_GDL90ActiveNoClients(t *testing.T) {
	in := healthyInput()
	in.Health.GDL90.RecentClientCount = 0
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "gdl90_clients" && c.Blocking {
			t.Error("no recent GDL90 client must never be blocking")
		}
	}
}

func TestBuildReport_GDL90Inactive(t *testing.T) {
	in := healthyInput()
	in.Health.GDL90 = readiness.GDL90Health{State: readiness.StateNotReady, Generating: false, OutputActive: false}
	mustOverall(t, in, StateNotReady)
}

func TestBuildReport_AHRSUncalibrated(t *testing.T) {
	in := healthyInput()
	in.Health.AHRS.State = readiness.StateDegraded
	in.Profile.CalibrationValid = false
	r := mustOverall(t, in, StateCaution)
	// Uncalibrated AHRS must not block or degrade unrelated components.
	for _, c := range r.Automated {
		switch c.Component {
		case "GPS", "978", "1090", "GDL90", "FISB":
			if c.State != StateReady && c.State != StateNotApplicable {
				t.Errorf("unrelated component %s/%s = %q while only AHRS was uncalibrated", c.Component, c.CheckID, c.State)
			}
		case "AHRS":
			if c.Blocking {
				t.Errorf("AHRS check %s must not be blocking merely for being uncalibrated", c.CheckID)
			}
		}
	}
}

func TestBuildReport_OptionalSensorDisabled(t *testing.T) {
	in := healthyInput()
	in.Health.AHRS = readiness.AHRSHealth{State: readiness.StateNotInstalled, Enabled: false}
	in.Health.Baro = readiness.BaroHealth{State: readiness.StateNotInstalled, Enabled: false}
	// A disabled optional sensor must not contribute to the rollup at all.
	mustOverall(t, in, StateReady)
}

func TestBuildReport_StorageWarning(t *testing.T) {
	in := healthyInput()
	in.Health.Storage.State = readiness.StateDegraded
	in.Health.Storage.Reason = "approaching warning threshold"
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "persistent_storage" && c.Blocking {
			t.Error("a storage warning (not a failure) must not be blocking")
		}
	}
}

func TestBuildReport_StorageFailure(t *testing.T) {
	in := healthyInput()
	in.Health.Storage = readiness.StorageHealth{State: readiness.StateNotReady, Present: true, Mounted: false}
	mustOverall(t, in, StateNotReady)
}

func TestBuildReport_OverlayFailure(t *testing.T) {
	in := healthyInput()
	in.Health.TemporaryOverlay = readiness.StorageHealth{State: readiness.StateNotReady, Present: false}
	mustOverall(t, in, StateNotReady)
}

func TestBuildReport_Undervoltage(t *testing.T) {
	in := healthyInput()
	in.Health.System.UndervoltageDetected = true
	mustOverall(t, in, StateNotReady)
}

func TestBuildReport_ThermalWarning(t *testing.T) {
	in := healthyInput()
	in.Health.System.Throttled = true
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "power_thermal" && c.Blocking {
			t.Error("throttling alone (without undervoltage) must be a caution, not blocking")
		}
	}
}

func TestBuildReport_FanStatusMissing(t *testing.T) {
	in := healthyInput()
	in.UptimeSeconds = graceFanControllerSeconds + 1
	in.Health.Fan = readiness.FanHealth{ServiceInstalled: true, ServiceActive: true, StatusAvailable: false}
	r := mustOverall(t, in, StateCaution)
	for _, c := range r.Automated {
		if c.CheckID == "fan_status" && c.Blocking {
			t.Error("a missing fan status file must be a caution, not blocking")
		}
	}
}

func TestBuildReport_FanStatusWithinGrace(t *testing.T) {
	in := healthyInput()
	in.UptimeSeconds = 5
	in.Health.Fan = readiness.FanHealth{ServiceInstalled: true, ServiceActive: true, StatusAvailable: false}
	r := BuildReport(in)
	for _, c := range r.Automated {
		if c.CheckID == "fan_status" && c.State != StateUnknown {
			t.Errorf("fan_status = %q within grace period, want UNKNOWN", c.State)
		}
	}
}

func TestBuildReport_UnknownComponentData(t *testing.T) {
	in := healthyInput()
	in.Health.System = readiness.SystemHealth{} // zero value: State is invalid/empty
	r := BuildReport(in)
	if r.Overall == StateReady {
		t.Error("an unrecognized/zero-value component state must never silently read as overall READY")
	}
}

func TestBuildReport_MultipleSimultaneousCautions(t *testing.T) {
	in := healthyInput()
	in.Health.GDL90.RecentClientCount = 0
	in.Health.UAT978.TowerCount = 0
	in.Health.System.Throttled = true
	r := mustOverall(t, in, StateCaution)
	if r.CautionCount < 3 {
		t.Errorf("CautionCount = %d, want at least 3", r.CautionCount)
	}
}

func TestBuildReport_BlockingOverridesCaution(t *testing.T) {
	in := healthyInput()
	in.Health.System.Throttled = true            // caution
	in.Health.System.UndervoltageDetected = true // blocking
	mustOverall(t, in, StateNotReady)
}

func TestBuildReport_ManualChecksIncomplete(t *testing.T) {
	in := healthyInput()
	in.ManualAcks = map[ManualCheckID]*ManualAck{}
	r := mustOverall(t, in, StateCaution)
	verifyCount := 0
	for _, c := range r.Manual {
		if c.State == StateVerify {
			verifyCount++
		}
		if c.Blocking {
			t.Errorf("manual check %s must never be blocking", c.CheckID)
		}
	}
	if verifyCount != len(ManualCheckDefinitions) {
		t.Errorf("expected all %d manual checks to be VERIFY, got %d", len(ManualCheckDefinitions), verifyCount)
	}
}

func TestBuildReport_ExistingCalibrationProfileCompatibility(t *testing.T) {
	in := healthyInput()
	in.Health.AHRS.Profile = readiness.AHRSProfileInfo{Available: false, Error: "profile store not initialized"}
	mustOverall(t, in, StateNotReady)
}

func TestBuildReport_Disclaimer(t *testing.T) {
	r := BuildReport(healthyInput())
	if r.Disclaimer != Disclaimer {
		t.Error("Report.Disclaimer must always be the exact required safety statement")
	}
	if r.SchemaVersion != SchemaVersion {
		t.Error("Report.SchemaVersion must match the package constant")
	}
}

func TestBuildReport_GeneratedAtNullableUntilTrusted(t *testing.T) {
	in := healthyInput()
	in.GeneratedAtUTC = nil
	r := BuildReport(in)
	if r.GeneratedAt != nil {
		t.Error("GeneratedAt must be nil when no trusted time was supplied")
	}
}
