package readiness

import "testing"

func TestBuildAutoRecordHealth_DisabledIsNotInstalled(t *testing.T) {
	h := BuildAutoRecordHealth(false, "DISABLED", "disabled", "automatic recording is disabled", "")
	if h.State != StateNotInstalled {
		t.Errorf("disabled feature State = %q, want NOT_INSTALLED", h.State)
	}
}

func TestBuildAutoRecordHealth_DisabledNeverDegradesOverall(t *testing.T) {
	h := BuildAutoRecordHealth(false, "DISABLED", "disabled", "automatic recording is disabled", "")
	overall := Rollup(StateReady, StateReady, h.State)
	if overall != StateReady {
		t.Errorf("a disabled automatic-recording feature must never pull Overall down from READY, got %q", overall)
	}
}

func TestBuildAutoRecordHealth_NotYetInitializedIsUnknown(t *testing.T) {
	h := BuildAutoRecordHealth(true, "", "", "", "")
	if h.State != StateUnknown {
		t.Errorf("uninitialized State = %q, want UNKNOWN", h.State)
	}
}

func TestBuildAutoRecordHealth_ErrorIsNotReady(t *testing.T) {
	h := BuildAutoRecordHealth(true, "ERROR", "recorder_error", "recorder reported an error", "")
	if h.State != StateNotReady {
		t.Errorf("ERROR machine state = %q, want NOT_READY", h.State)
	}
}

func TestBuildAutoRecordHealth_InhibitedIsDegraded(t *testing.T) {
	h := BuildAutoRecordHealth(true, "INHIBITED", "storage_denied", "inhibited: insufficient storage capacity", "")
	if h.State != StateDegraded {
		t.Errorf("INHIBITED machine state = %q, want DEGRADED", h.State)
	}
}

func TestBuildAutoRecordHealth_RecordingIsReady(t *testing.T) {
	h := BuildAutoRecordHealth(true, "RECORDING", "recording", "automatic recording in progress", "rec-20260601T120000Z")
	if h.State != StateReady {
		t.Errorf("RECORDING machine state = %q, want READY", h.State)
	}
	if h.ActiveRecordingID == "" {
		t.Error("ActiveRecordingID should be passed through unchanged")
	}
}

func TestBuildAutoRecordHealth_ArmedWaitingIsReady(t *testing.T) {
	h := BuildAutoRecordHealth(true, "ARMED_WAITING", "waiting_for_motion", "waiting for qualifying motion", "")
	if h.State != StateReady {
		t.Errorf("ARMED_WAITING machine state = %q, want READY", h.State)
	}
}
