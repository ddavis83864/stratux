package preflight

import "testing"

func TestTrafficCPAChecks_DisabledIsInformationalOnly(t *testing.T) {
	results := trafficCPAChecks(Input{TrafficCPAEnabled: false})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.State != StateNotApplicable {
		t.Errorf("disabled State = %q, want NOT_APPLICABLE", r.State)
	}
	if r.Severity != SeverityInfo {
		t.Errorf("disabled Severity = %q, want Info - a deliberate opt-out must never be CAUTION/NOT_READY", r.Severity)
	}
}

func TestTrafficCPAChecks_InvalidSettingsIsCautionNeverBlocking(t *testing.T) {
	results := trafficCPAChecks(Input{TrafficCPAEnabled: true, TrafficCPASettingsValid: false})
	if results[0].State != StateCaution || results[0].Severity != SeverityCaution {
		t.Errorf("invalid settings = %+v, want CAUTION/Caution", results[0])
	}
	if results[0].Severity == SeverityBlocking {
		t.Error("traffic CPA's own settings failure must never be SeverityBlocking - it is a supplemental trend input, never authoritative for flight readiness")
	}
}

func TestTrafficCPAChecks_EnabledAndValidIsReady(t *testing.T) {
	results := trafficCPAChecks(Input{TrafficCPAEnabled: true, TrafficCPASettingsValid: true})
	if results[0].State != StateReady {
		t.Errorf("enabled+valid = %+v, want READY", results[0])
	}
}
