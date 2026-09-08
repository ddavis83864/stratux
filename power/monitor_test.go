package power

import (
	"testing"

	"github.com/stratux/stratux/readiness"
)

func TestMonitor_StartsAtOKWithNoSamples(t *testing.T) {
	m := NewMonitor(3)
	if m.Current().Severity != SeverityOK {
		t.Errorf("expected SeverityOK before any samples, got %s", m.Current().Severity)
	}
}

func TestMonitor_RequiresConsecutiveSamplesBeforeChanging(t *testing.T) {
	m := NewMonitor(3)
	bad := readiness.ThrottleStatus{UndervoltageNow: true}

	if h := m.Observe(bad); h.Severity != SeverityOK {
		t.Fatalf("1st bad sample: expected still SeverityOK (debounced), got %s", h.Severity)
	}
	if h := m.Observe(bad); h.Severity != SeverityOK {
		t.Fatalf("2nd bad sample: expected still SeverityOK (debounced), got %s", h.Severity)
	}
	if h := m.Observe(bad); h.Severity != SeverityCritical {
		t.Fatalf("3rd consecutive bad sample: expected SeverityCritical, got %s", h.Severity)
	}
}

func TestMonitor_ResetsStreakOnDisagreement(t *testing.T) {
	m := NewMonitor(3)
	bad := readiness.ThrottleStatus{UndervoltageNow: true}
	good := readiness.ThrottleStatus{}

	m.Observe(bad)
	m.Observe(bad)
	m.Observe(good) // breaks the streak
	if h := m.Observe(bad); h.Severity != SeverityOK {
		t.Fatalf("streak should have reset after the disagreeing sample, got %s", h.Severity)
	}
	m.Observe(bad)
	if h := m.Observe(bad); h.Severity != SeverityCritical {
		t.Fatalf("3 consecutive bad samples after the reset should confirm, got %s", h.Severity)
	}
}

func TestMonitor_RequiredConsecutiveClampedToOne(t *testing.T) {
	m := NewMonitor(0)
	h := m.Observe(readiness.ThrottleStatus{UndervoltageNow: true})
	if h.Severity != SeverityCritical {
		t.Errorf("RequiredConsecutive<1 should behave like 1 (confirm immediately), got %s", h.Severity)
	}
}

func TestMonitor_RecoversAfterConsecutiveGoodSamples(t *testing.T) {
	m := NewMonitor(2)
	bad := readiness.ThrottleStatus{ThrottledNow: true}
	good := readiness.ThrottleStatus{}

	m.Observe(bad)
	m.Observe(bad)
	if m.Current().Severity != SeverityWarning {
		t.Fatalf("expected confirmed SeverityWarning, got %s", m.Current().Severity)
	}
	m.Observe(good)
	if h := m.Observe(good); h.Severity != SeverityOK {
		t.Errorf("expected recovery to SeverityOK after 2 consecutive good samples, got %s", h.Severity)
	}
}
