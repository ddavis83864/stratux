package storagelifecycle

import (
	"testing"

	"github.com/stratux/stratux/readiness"
)

func testThresholds() readiness.StorageThresholds {
	return readiness.DefaultPersistentStorageThresholds() // 80/90/95 - see readiness/storage.go
}

func TestFilesystemPressure_AllBands(t *testing.T) {
	th := testThresholds()
	cases := []struct {
		pct  float64
		want PressureState
	}{
		{0, PressureNormal},
		{79.9, PressureNormal},
		{80, PressureElevated},
		{89.9, PressureElevated},
		{90, PressureHigh},
		{94.9, PressureHigh},
		{95, PressureCritical},
		{100, PressureCritical},
	}
	for _, c := range cases {
		got := FilesystemPressure(c.pct, th)
		if got != c.want {
			t.Errorf("FilesystemPressure(%v) = %s, want %s", c.pct, got, c.want)
		}
	}
}

func TestNamespaceQuotaPressure_Unconfigured(t *testing.T) {
	if got := NamespaceQuotaPressure(1<<40, Quota{}); got != PressureNormal {
		t.Errorf("expected PressureNormal for an unconfigured quota regardless of usage, got %s", got)
	}
}

func TestNamespaceQuotaPressure_Bands(t *testing.T) {
	quota := Quota{MaxBytes: 1000}
	cases := []struct {
		used int64
		want PressureState
	}{
		{0, PressureNormal},
		{749, PressureNormal},
		{750, PressureElevated},
		{899, PressureElevated},
		{900, PressureHigh},
		{999, PressureHigh},
		{1000, PressureCritical},
		{1500, PressureCritical},
	}
	for _, c := range cases {
		got := NamespaceQuotaPressure(c.used, quota)
		if got != c.want {
			t.Errorf("NamespaceQuotaPressure(%d) = %s, want %s", c.used, got, c.want)
		}
	}
}

func TestPressureRank_WorseOfTwo(t *testing.T) {
	if worse(PressureNormal, PressureHigh) != PressureHigh {
		t.Error("expected HIGH to win over NORMAL")
	}
	if worse(PressureCritical, PressureUnknown) != PressureCritical {
		t.Error("expected CRITICAL to win over UNKNOWN")
	}
	if worse(PressureNormal, PressureUnknown) != PressureUnknown {
		t.Error("expected UNKNOWN to be treated as worse than NORMAL")
	}
}

func TestMonitor_StartsUnknown(t *testing.T) {
	m := NewMonitor(3)
	if m.Current() != PressureUnknown {
		t.Errorf("expected PressureUnknown before any samples, got %s", m.Current())
	}
}

func TestMonitor_RequiresConsecutiveSamples(t *testing.T) {
	m := NewMonitor(3)
	if got := m.Observe(PressureCritical); got != PressureUnknown {
		t.Errorf("1st sample: expected still debounced (UNKNOWN), got %s", got)
	}
	if got := m.Observe(PressureCritical); got != PressureUnknown {
		t.Errorf("2nd sample: expected still debounced (UNKNOWN), got %s", got)
	}
	if got := m.Observe(PressureCritical); got != PressureCritical {
		t.Errorf("3rd consecutive sample: expected CRITICAL, got %s", got)
	}
}

func TestMonitor_StreakResetsOnDisagreement(t *testing.T) {
	m := NewMonitor(2)
	m.Observe(PressureHigh)
	m.Observe(PressureNormal) // breaks the streak
	if got := m.Observe(PressureHigh); got != PressureUnknown {
		t.Errorf("streak should have reset, got %s", got)
	}
}

func TestMonitor_RecoversAfterConsecutiveGoodSamples(t *testing.T) {
	m := NewMonitor(2)
	m.Observe(PressureHigh)
	m.Observe(PressureHigh)
	if m.Current() != PressureHigh {
		t.Fatalf("setup: expected confirmed HIGH, got %s", m.Current())
	}
	m.Observe(PressureNormal)
	if got := m.Observe(PressureNormal); got != PressureNormal {
		t.Errorf("expected recovery to NORMAL after 2 consecutive good samples, got %s", got)
	}
}

func TestMonitor_NoFlappingOnAlternatingSamples(t *testing.T) {
	// This is the concrete anti-flapping guarantee: alternating
	// HIGH/NORMAL samples one at a time, with RequiredConsecutive=3,
	// must never confirm either state (the streak keeps resetting).
	m := NewMonitor(3)
	for i := 0; i < 10; i++ {
		if i%2 == 0 {
			m.Observe(PressureHigh)
		} else {
			m.Observe(PressureNormal)
		}
	}
	if m.Current() != PressureUnknown {
		t.Errorf("alternating samples must never confirm a state, got %s", m.Current())
	}
}
