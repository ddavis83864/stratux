package preflight

import "testing"

func TestAutoRecordChecks_DisabledIsInformationalOnly(t *testing.T) {
	results := autoRecordChecks(Input{AutoRecordEnabled: false})
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

func TestAutoRecordChecks_NotYetInitializedIsInformational(t *testing.T) {
	results := autoRecordChecks(Input{AutoRecordEnabled: true, AutoRecordMachineState: ""})
	if results[0].State != StateNotApplicable || results[0].Severity != SeverityInfo {
		t.Errorf("uninitialized = %+v, want NOT_APPLICABLE/Info", results[0])
	}
}

func TestAutoRecordChecks_ErrorNeverBlocksOverall(t *testing.T) {
	results := autoRecordChecks(Input{AutoRecordEnabled: true, AutoRecordMachineState: "ERROR", AutoRecordReason: "recorder reported an error"})
	if results[0].Severity == SeverityBlocking {
		t.Error("automatic recording's own ERROR must never be SeverityBlocking - it is a supplemental feature, never authoritative for flight readiness")
	}
}

func TestAutoRecordChecks_InhibitedIsCaution(t *testing.T) {
	results := autoRecordChecks(Input{AutoRecordEnabled: true, AutoRecordMachineState: "INHIBITED", AutoRecordReason: "inhibited: insufficient storage capacity"})
	if results[0].State != StateCaution || results[0].Severity != SeverityCaution {
		t.Errorf("INHIBITED = %+v, want CAUTION/Caution", results[0])
	}
}

func TestAutoRecordChecks_ArmedIsReady(t *testing.T) {
	results := autoRecordChecks(Input{AutoRecordEnabled: true, AutoRecordMachineState: "ARMED_WAITING"})
	if results[0].State != StateReady {
		t.Errorf("ARMED_WAITING = %+v, want READY", results[0])
	}
}
