package readiness

import "testing"
import "time"

func TestBuildEpaperHealth_DisabledIsNotInstalledNotFailed(t *testing.T) {
	h := BuildEpaperHealth(false, false, false, false, false, "", false, "", "", 0, 0, 0, 0, time.Time{}, time.Now(), 30*time.Second)
	if h.State != StateNotInstalled {
		t.Errorf("disabled epaper State = %q, want NOT_INSTALLED", h.State)
	}
	if h.State.Color() != "gray" {
		t.Errorf("disabled epaper should render gray, not %s", h.State.Color())
	}
}

func TestBuildEpaperHealth_ServiceNotInstalledIsNotInstalledEvenIfEnabled(t *testing.T) {
	// Enabled in settings, but this build/platform never installed the
	// systemd unit at all - a platform fact, not a runtime failure.
	h := BuildEpaperHealth(true, false, false, false, false, "", false, "", "", 0, 0, 0, 0, time.Time{}, time.Now(), 30*time.Second)
	if h.State != StateNotInstalled {
		t.Errorf("enabled-but-uninstalled epaper State = %q, want NOT_INSTALLED", h.State)
	}
}

func TestBuildEpaperHealth_EnabledInstalledButInactiveIsNotReady(t *testing.T) {
	h := BuildEpaperHealth(true, true, false, false, false, "", false, "", "", 0, 0, 0, 0, time.Time{}, time.Now(), 30*time.Second)
	if h.State != StateNotReady {
		t.Errorf("enabled+installed but inactive epaper State = %q, want NOT_READY", h.State)
	}
}

func TestBuildEpaperHealth_ActiveButNoStatusYetIsDegraded(t *testing.T) {
	h := BuildEpaperHealth(true, true, true, false, false, "", false, "", "", 0, 0, 0, 0, time.Time{}, time.Now(), 30*time.Second)
	if h.State != StateDegraded {
		t.Errorf("active-but-no-status epaper State = %q, want DEGRADED", h.State)
	}
}

func TestBuildEpaperHealth_MalformedStatusIsDegraded(t *testing.T) {
	h := BuildEpaperHealth(true, true, true, true, true, "", false, "", "", 0, 0, 0, 0, time.Now(), time.Now(), 30*time.Second)
	if h.State != StateDegraded {
		t.Errorf("malformed epaper status State = %q, want DEGRADED", h.State)
	}
}

func TestBuildEpaperHealth_PanelNotDetectedIsDegradedNotFailed(t *testing.T) {
	// No physical panel wired up yet is routine (this feature ships
	// disconnected-by-default-safe) and must never read as red/NOT_READY,
	// which could be mistaken for a core subsystem failure.
	now := time.Now()
	h := BuildEpaperHealth(true, true, true, true, false, "NOT_DETECTED", false, "", "myPanel", 0, 0, 0, 0, now, now, 30*time.Second)
	if h.State != StateDegraded {
		t.Errorf("panel-not-detected epaper State = %q, want DEGRADED", h.State)
	}
}

func TestBuildEpaperHealth_ReportedErrorIsDegradedNotFailed(t *testing.T) {
	now := time.Now()
	h := BuildEpaperHealth(true, true, true, true, false, "ERROR", true, "SPI_WRITE_FAILED", "myPanel", 3, 5, 1, 2, now, now, 30*time.Second)
	if h.State != StateDegraded {
		t.Errorf("driver-error epaper State = %q, want DEGRADED (never NOT_READY - this hardware is purely supplemental)", h.State)
	}
	if h.LastErrorCategory != "SPI_WRITE_FAILED" {
		t.Errorf("LastErrorCategory = %q, want SPI_WRITE_FAILED", h.LastErrorCategory)
	}
}

func TestBuildEpaperHealth_StaleStatusIsDegraded(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	h := BuildEpaperHealth(true, true, true, true, false, "RUNNING", true, "", "myPanel", 1, 1, 0, 0, old, time.Now(), 30*time.Second)
	if h.State != StateDegraded {
		t.Errorf("stale-status epaper State = %q, want DEGRADED", h.State)
	}
	if !h.Stale {
		t.Error("Stale must be true when the status age exceeds staleAfter")
	}
}

func TestBuildEpaperHealth_RunningNominalIsReady(t *testing.T) {
	now := time.Now()
	h := BuildEpaperHealth(true, true, true, true, false, "RUNNING", true, "", "myPanel", 4, 10, 0, 0, now, now, 30*time.Second)
	if h.State != StateReady {
		t.Errorf("nominal running epaper State = %q, want READY: %s", h.State, h.Reason)
	}
	if h.Stale {
		t.Error("a fresh measurement must not be stale")
	}
}

func TestBuildEpaperHealth_DisabledIsExcludedFromRollup(t *testing.T) {
	epaperHealth := BuildEpaperHealth(false, false, false, false, false, "", false, "", "", 0, 0, 0, 0, time.Time{}, time.Now(), 30*time.Second)
	overall := Rollup(StateReady, StateReady, epaperHealth.State)
	if overall != StateReady {
		t.Errorf("disabled epaper must not drag Overall down from READY, got %q", overall)
	}
}

func TestBuildEpaperHealth_FailingEnabledDisplayCanSurfaceInRollupWithoutExceedingDegraded(t *testing.T) {
	now := time.Now()
	epaperHealth := BuildEpaperHealth(true, true, true, true, false, "ERROR", true, "SPI_WRITE_FAILED", "myPanel", 0, 0, 0, 3, now, now, 30*time.Second)
	overall := Rollup(StateReady, StateReady, epaperHealth.State)
	if overall != StateDegraded {
		t.Errorf("a failing but enabled epaper display should surface as DEGRADED overall (not READY, not NOT_READY/worse), got %q", overall)
	}
}
