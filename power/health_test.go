package power

import (
	"testing"

	"github.com/stratux/stratux/readiness"
)

func TestEvaluate_Nominal(t *testing.T) {
	h := Evaluate(readiness.ThrottleStatus{})
	if h.Severity != SeverityOK {
		t.Errorf("expected SeverityOK, got %s (%s)", h.Severity, h.Reason)
	}
}

func TestEvaluate_UndervoltageNowIsCritical(t *testing.T) {
	h := Evaluate(readiness.ThrottleStatus{UndervoltageNow: true})
	if h.Severity != SeverityCritical {
		t.Errorf("expected SeverityCritical for current undervoltage, got %s", h.Severity)
	}
}

func TestEvaluate_ThrottledNowIsWarning(t *testing.T) {
	h := Evaluate(readiness.ThrottleStatus{ThrottledNow: true})
	if h.Severity != SeverityWarning {
		t.Errorf("expected SeverityWarning for current throttling, got %s", h.Severity)
	}
}

func TestEvaluate_HistoricalOnlyIsWarningNotCritical(t *testing.T) {
	// Occurred-but-cleared undervoltage is worth a warning, but must not
	// be conflated with an active, right-now under-voltage condition.
	h := Evaluate(readiness.ThrottleStatus{UndervoltageOccurred: true})
	if h.Severity != SeverityWarning {
		t.Errorf("expected SeverityWarning for historical-only undervoltage, got %s", h.Severity)
	}
}

func TestEvaluate_UndervoltageNowTakesPriorityOverThrottled(t *testing.T) {
	h := Evaluate(readiness.ThrottleStatus{UndervoltageNow: true, ThrottledNow: true})
	if h.Severity != SeverityCritical {
		t.Errorf("expected undervoltage to take priority (SeverityCritical), got %s", h.Severity)
	}
}

func TestEvaluate_NeverClaimsATrustedBatterySignal(t *testing.T) {
	for _, throttle := range []readiness.ThrottleStatus{
		{},
		{UndervoltageNow: true},
		{ThrottledNow: true, ThrottledOccurred: true},
	} {
		h := Evaluate(throttle)
		if h.HasTrustedBatterySignal {
			t.Errorf("HasTrustedBatterySignal must always be false on this hardware, got true for %+v", throttle)
		}
		if h.HasRuntimeEstimate {
			t.Errorf("HasRuntimeEstimate must always be false on this hardware, got true for %+v", throttle)
		}
		if len(h.Notes) == 0 {
			t.Error("Notes must never be empty - the capability-honesty text must always accompany a reading")
		}
	}
}
