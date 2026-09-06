package alerting

import (
	"testing"
	"time"
)

func newTestEvaluatorForHealth() (*Evaluator, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.MasterEnabled = true
	cfg.SystemAudioEnabled = true
	return NewEvaluator(cfg, clock.now), clock
}

func TestEvaluateHealth_StartupGraceUnknownProducesNoEvent(t *testing.T) {
	e, _ := newTestEvaluatorForHealth()
	// First-ever snapshot with everything UNKNOWN (startup grace) must not
	// alert - UNKNOWN is never itself a degradation.
	events := e.EvaluateHealth(HealthSnapshot{})
	if len(events) != 0 {
		t.Fatalf("expected no events for an all-UNKNOWN snapshot, got %+v", events)
	}
}

// TestEvaluateHealth_FirstObservationNeverAlertsEvenIfAlreadyBad
// reproduces a real bug found during development: a component whose very
// first-ever observation (transitioning from the internal Unknown
// baseline) already reports CAUTION or NOT_READY must not alert - that
// baseline transition is indistinguishable from "still within this
// component's own startup grace period," which readiness/preflight has
// already decided is not a failure. See docs/alerting.md's "Startup grace
// periods" section.
func TestEvaluateHealth_FirstObservationNeverAlertsEvenIfAlreadyBad(t *testing.T) {
	e, _ := newTestEvaluatorForHealth()
	events := e.EvaluateHealth(HealthSnapshot{GPS: ComponentNotReady, TrustedTime: ComponentCaution})
	if len(events) != 0 {
		t.Fatalf("expected no events on the first-ever observation, got %+v", events)
	}
}

func TestEvaluateHealth_ReadyToNotReadyAlertsOnce(t *testing.T) {
	e, clock := newTestEvaluatorForHealth()
	e.EvaluateHealth(HealthSnapshot{GPS: ComponentReady})
	clock.advance(time.Second)
	events := e.EvaluateHealth(HealthSnapshot{GPS: ComponentNotReady})
	if len(events) != 1 || events[0].Alert.Level != LevelSystemNotReady || events[0].Kind != "new" {
		t.Fatalf("expected one SYSTEM_NOT_READY event, got %+v", events)
	}
	clock.advance(time.Second)
	// Repeating the same NOT_READY state must not re-fire.
	repeat := e.EvaluateHealth(HealthSnapshot{GPS: ComponentNotReady})
	if len(repeat) != 0 {
		t.Fatalf("expected no repeated event for an unchanged NOT_READY state, got %+v", repeat)
	}
}

func TestEvaluateHealth_ReadyToCautionAlerts(t *testing.T) {
	e, clock := newTestEvaluatorForHealth()
	e.EvaluateHealth(HealthSnapshot{PersistentStorage: ComponentReady})
	clock.advance(time.Second)
	events := e.EvaluateHealth(HealthSnapshot{PersistentStorage: ComponentCaution})
	if len(events) != 1 || events[0].Alert.Level != LevelSystemCaution {
		t.Fatalf("expected one SYSTEM_CAUTION event, got %+v", events)
	}
}

func TestEvaluateHealth_RecoveryIsQuiet(t *testing.T) {
	e, clock := newTestEvaluatorForHealth()
	e.EvaluateHealth(HealthSnapshot{GPS: ComponentReady})
	clock.advance(time.Second)
	e.EvaluateHealth(HealthSnapshot{GPS: ComponentNotReady})
	clock.advance(time.Second)
	events := e.EvaluateHealth(HealthSnapshot{GPS: ComponentReady})
	if len(events) != 1 || events[0].Kind != "recovered" {
		t.Fatalf("expected one recovery event, got %+v", events)
	}
	if events[0].Alert.AudioEligible {
		t.Errorf("expected recovery to be silent by default (AudioEligible=false)")
	}
	if events[0].Alert.Level != LevelInformation {
		t.Errorf("Level = %q, want INFORMATION for a quiet recovery", events[0].Alert.Level)
	}
}

func TestEvaluateHealth_NoEventWithoutTransition(t *testing.T) {
	e, clock := newTestEvaluatorForHealth()
	e.EvaluateHealth(HealthSnapshot{AHRS: ComponentCaution})
	for i := 0; i < 5; i++ {
		clock.advance(time.Second)
		events := e.EvaluateHealth(HealthSnapshot{AHRS: ComponentCaution})
		if len(events) != 0 {
			t.Fatalf("iteration %d: expected no repeated event, got %+v", i, events)
		}
	}
}

func TestEvaluateHealth_MultipleComponentsIndependent(t *testing.T) {
	e, clock := newTestEvaluatorForHealth()
	e.EvaluateHealth(HealthSnapshot{GPS: ComponentReady, Baro: ComponentReady})
	clock.advance(time.Second)
	events := e.EvaluateHealth(HealthSnapshot{GPS: ComponentNotReady, Baro: ComponentReady})
	if len(events) != 1 || events[0].Alert.Component != "gps" {
		t.Fatalf("expected exactly one event for the gps component only, got %+v", events)
	}
}

func TestEvaluateHealth_AudioDisabledByDefaultSetting(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.MasterEnabled = true
	cfg.SystemAudioEnabled = false // explicit default per docs/alerting.md
	e := NewEvaluator(cfg, clock.now)
	e.EvaluateHealth(HealthSnapshot{GPS: ComponentReady})
	clock.advance(time.Second)
	events := e.EvaluateHealth(HealthSnapshot{GPS: ComponentNotReady})
	if len(events) != 1 || events[0].Alert.AudioEligible {
		t.Fatalf("expected system-health audio disabled by default, got %+v", events)
	}
}

func TestEvaluateHealth_ActiveAlertsAppearInSnapshot(t *testing.T) {
	e, clock := newTestEvaluatorForHealth()
	e.EvaluateHealth(HealthSnapshot{GPS: ComponentReady})
	clock.advance(time.Second)
	e.EvaluateHealth(HealthSnapshot{GPS: ComponentNotReady})
	snap := e.Snapshot()
	found := false
	for _, a := range snap.Active {
		if a.Component == "gps" && a.Category == CategorySystem {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an active system alert for gps, got %+v", snap.Active)
	}
}

func TestEvaluateHealth_AcknowledgeByComponentName(t *testing.T) {
	e, clock := newTestEvaluatorForHealth()
	e.EvaluateHealth(HealthSnapshot{Fan: ComponentReady})
	clock.advance(time.Second)
	e.EvaluateHealth(HealthSnapshot{Fan: ComponentCaution})
	if !e.Acknowledge("fan") {
		t.Fatal("expected Acknowledge to succeed for a known component")
	}
	if e.Acknowledge("not-a-real-component") {
		t.Fatal("expected Acknowledge to fail for an unknown id")
	}
}
