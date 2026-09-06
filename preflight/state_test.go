package preflight

import "testing"

func TestState_Valid(t *testing.T) {
	valid := []State{StateReady, StateCaution, StateNotReady, StateVerify, StateNotApplicable, StateUnknown}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("State(%q).Valid() = false, want true", s)
		}
	}
	if State("").Valid() {
		t.Error("empty State.Valid() = true, want false")
	}
	if State("bogus").Valid() {
		t.Error("State(\"bogus\").Valid() = true, want false")
	}
}

func TestState_OverallValid(t *testing.T) {
	overallOK := []State{StateReady, StateCaution, StateNotReady}
	for _, s := range overallOK {
		if !s.OverallValid() {
			t.Errorf("State(%q).OverallValid() = false, want true", s)
		}
	}
	itemOnly := []State{StateVerify, StateNotApplicable, StateUnknown}
	for _, s := range itemOnly {
		if s.OverallValid() {
			t.Errorf("State(%q).OverallValid() = true, want false - item-only states must never be an overall report state", s)
		}
	}
}
