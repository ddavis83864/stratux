package readiness

import "testing"

func TestValid(t *testing.T) {
	valid := []ComponentState{StateReady, StateDegraded, StateNotReady, StateNotInstalled, StateUnknown}
	for _, s := range valid {
		if !s.Valid() {
			t.Errorf("%q should be valid", s)
		}
	}
	if ComponentState("").Valid() {
		t.Error("zero value must not be valid")
	}
	if ComponentState("bogus").Valid() {
		t.Error("unrecognized string must not be valid")
	}
}

func TestColor(t *testing.T) {
	cases := map[ComponentState]string{
		StateReady:            "green",
		StateDegraded:         "amber",
		StateNotReady:         "red",
		StateNotInstalled:     "gray",
		StateUnknown:          "gray",
		ComponentState(""):    "gray",
		ComponentState("bad"): "gray",
	}
	for s, want := range cases {
		if got := s.Color(); got != want {
			t.Errorf("%q.Color() = %q, want %q", s, got, want)
		}
	}
}

func TestWorse(t *testing.T) {
	if !StateNotReady.Worse(StateDegraded) {
		t.Error("NOT_READY should be worse than DEGRADED")
	}
	if !StateDegraded.Worse(StateReady) {
		t.Error("DEGRADED should be worse than READY")
	}
	if StateReady.Worse(StateDegraded) {
		t.Error("READY should not be worse than DEGRADED")
	}
	if !StateNotInstalled.Worse(StateReady) {
		t.Error("NOT_INSTALLED should rank below READY in the general ordering (Rollup, not Worse, is what keeps a NOT_INSTALLED tile from dragging down an otherwise-healthy system)")
	}
	if StateNotInstalled.Worse(StateDegraded) {
		t.Error("NOT_INSTALLED should not be worse than DEGRADED")
	}
	if StateReady.Worse(StateReady) {
		t.Error("a state should not be worse than itself")
	}
}

func TestRollup_Empty(t *testing.T) {
	if got := Rollup(); got != StateUnknown {
		t.Errorf("Rollup() with no components = %q, want UNKNOWN", got)
	}
}

func TestRollup_AllReady(t *testing.T) {
	if got := Rollup(StateReady, StateReady, StateReady); got != StateReady {
		t.Errorf("Rollup(all READY) = %q, want READY", got)
	}
}

func TestRollup_WorstWins(t *testing.T) {
	if got := Rollup(StateReady, StateDegraded, StateReady); got != StateDegraded {
		t.Errorf("Rollup = %q, want DEGRADED", got)
	}
	if got := Rollup(StateReady, StateDegraded, StateNotReady); got != StateNotReady {
		t.Errorf("Rollup = %q, want NOT_READY", got)
	}
}

func TestRollup_NotInstalledDoesNotDragDownHealthySystem(t *testing.T) {
	// A system with working 978/1090/GPS/time/storage but no AHRS board
	// yet must roll up to READY overall, not UNKNOWN/DEGRADED, purely
	// because one future-hardware tile is NOT_INSTALLED.
	got := Rollup(StateReady, StateReady, StateReady, StateNotInstalled)
	if got != StateReady {
		t.Errorf("Rollup(mostly READY + one NOT_INSTALLED) = %q, want READY", got)
	}
}

func TestRollup_AllNotInstalledOrUnknownIsUnknown(t *testing.T) {
	got := Rollup(StateNotInstalled, StateUnknown, StateNotInstalled)
	if got != StateUnknown {
		t.Errorf("Rollup(all NOT_INSTALLED/UNKNOWN) = %q, want UNKNOWN", got)
	}
}

func TestRollup_NotInstalledDoesNotHideAFailure(t *testing.T) {
	got := Rollup(StateNotInstalled, StateNotReady)
	if got != StateNotReady {
		t.Errorf("Rollup(NOT_INSTALLED + NOT_READY) = %q, want NOT_READY", got)
	}
}
