package preflight

import "testing"

func TestWifiAdminChecks_IdleOrEmptyIsReady(t *testing.T) {
	for _, stage := range []string{"", "idle"} {
		results := wifiAdminChecks(Input{WifiAdminStage: stage})
		if len(results) != 1 {
			t.Fatalf("stage=%q: got %d results, want 1", stage, len(results))
		}
		if results[0].State != StateReady || results[0].Severity != SeverityInfo {
			t.Errorf("stage=%q: %+v, want READY/Info", stage, results[0])
		}
	}
}

func TestWifiAdminChecks_PendingStagesAreCautionNeverBlocking(t *testing.T) {
	for _, stage := range []string{"previewed", "applying", "awaiting_reconnection", "rolling_back"} {
		results := wifiAdminChecks(Input{WifiAdminStage: stage})
		if results[0].State != StateCaution || results[0].Severity != SeverityCaution {
			t.Errorf("stage=%q: %+v, want CAUTION/Caution", stage, results[0])
		}
		if results[0].Severity == SeverityBlocking {
			t.Errorf("stage=%q: a pending Wi-Fi transaction must never be SeverityBlocking - it is administrative, never a flight-readiness condition", stage)
		}
	}
}

func TestWifiAdminChecks_RecoveryRequiredIsCautionNeverBlocking(t *testing.T) {
	results := wifiAdminChecks(Input{WifiAdminStage: "recovery_required"})
	if results[0].State != StateCaution || results[0].Severity != SeverityCaution {
		t.Errorf("%+v, want CAUTION/Caution", results[0])
	}
	if results[0].Severity == SeverityBlocking {
		t.Error("recovery_required must never be SeverityBlocking - see wifiAdminChecks' own doc comment")
	}
}

func TestWifiAdminChecks_FailedIsCaution(t *testing.T) {
	results := wifiAdminChecks(Input{WifiAdminStage: "failed"})
	if results[0].State != StateCaution {
		t.Errorf("%+v, want CAUTION", results[0])
	}
}

func TestWifiAdminChecks_UnrecognizedStageIsUnknownNeverBlocking(t *testing.T) {
	results := wifiAdminChecks(Input{WifiAdminStage: "some-future-stage"})
	if results[0].State != StateUnknown {
		t.Errorf("%+v, want UNKNOWN for an unrecognized stage string", results[0])
	}
	if results[0].Severity == SeverityBlocking {
		t.Error("an unrecognized stage must never be SeverityBlocking")
	}
}
