package alerting

import (
	"testing"
	"time"

	"github.com/stratux/stratux/trafficcpa"
)

// cpaFixture builds a minimal, otherwise-conservative trafficcpa.Result
// for policy testing - callers override only the fields their specific
// test cares about.
func cpaFixture(valid bool, closureKt, tcpaSec, predHorizM float64, clamped bool, trend trafficcpa.Trend) *trafficcpa.Result {
	return &trafficcpa.Result{
		Valid:                               valid,
		Confidence:                          trafficcpa.ConfidenceMedium,
		HorizontalClosureRateValid:          true,
		HorizontalClosureRateKnots:          closureKt,
		TCPAValid:                           true,
		TCPASeconds:                         tcpaSec,
		TCPAClampedToHorizon:                clamped,
		PredictedHorizontalSeparationValid:  true,
		PredictedHorizontalSeparationMeters: predHorizM,
		Trend:                               trend,
	}
}

func cpaEnabledConfig() Config {
	cfg := DefaultConfig()
	cfg.CPAEscalationEnabled = true
	cfg.CPAMinClosureRateKnots = 30
	return cfg
}

// TestClassifyTier_CPANeverWeakensAnAlert is the mandatory proof from
// docs/traffic-cpa-alerting.md / this mission's own Phase 6: for every
// combination of base tier and CPA state (nil, invalid, diverging, or
// even a strongly converging one), classifyTier's result is never LOWER
// than what the exact same observation would produce with CPA escalation
// disabled entirely.
func TestClassifyTier_CPANeverWeakensAnAlert(t *testing.T) {
	baseCfg := DefaultConfig()
	cpaCfg := cpaEnabledConfig()

	obsAt := func(dist, alt float64, cpa *trafficcpa.Result) TrafficObservation {
		o := freshObs("A1", dist, alt)
		o.CPA = cpa
		return o
	}

	cpaStates := []*trafficcpa.Result{
		nil,
		cpaFixture(false, 0, 0, 0, false, trafficcpa.TrendUnknown),        // invalid
		cpaFixture(true, -50, 30, 5000, false, trafficcpa.TrendDiverging), // diverging
		cpaFixture(true, 100, 10, 10, false, trafficcpa.TrendConverging),  // strongly converging
		cpaFixture(true, 100, 10, 10, true, trafficcpa.TrendConverging),   // converging but clamped to horizon
	}

	distances := []float64{
		baseCfg.NoticeHorizontalMeters - 1,
		baseCfg.CautionHorizontalMeters - 1,
		baseCfg.HighCautionHorizontalMeters - 1,
		baseCfg.MonitoringHorizontalMeters + 1, // outside envelope entirely
	}

	for _, dist := range distances {
		withoutCPA, _, _ := classifyTier(obsAt(dist, 0, nil), baseCfg)
		for i, cpa := range cpaStates {
			withCPA, _, _ := classifyTier(obsAt(dist, 0, cpa), cpaCfg)
			if withCPA < withoutCPA {
				t.Errorf("dist=%v cpaState=%d: tier weakened by CPA - without=%d with=%d", dist, i, withoutCPA, withCPA)
			}
		}
	}
}

func TestClassifyTier_CPAEscalatesNoticeToCautionByExactlyOneTier(t *testing.T) {
	cfg := cpaEnabledConfig()
	// Base distance inside Notice, outside Caution.
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	obs.CPA = cpaFixture(true, cfg.CPAMinClosureRateKnots+10, 30, cfg.CautionHorizontalMeters-1, false, trafficcpa.TrendConverging)

	baseTier, _, _ := classifyTier(obs, DefaultConfig())
	if baseTier != tierNotice {
		t.Fatalf("test precondition failed: expected base tier Notice, got %d", baseTier)
	}

	tier, _, escalated := classifyTier(obs, cfg)
	if tier != tierCaution {
		t.Errorf("expected escalation to exactly Caution, got tier=%d", tier)
	}
	if !escalated {
		t.Error("expected cpaEscalated=true")
	}
}

func TestClassifyTier_CPADisabledByDefaultNeverEscalates(t *testing.T) {
	cfg := DefaultConfig() // CPAEscalationEnabled left at its default
	if cfg.CPAEscalationEnabled {
		t.Fatal("test precondition failed: expected CPA escalation disabled by default")
	}
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	obs.CPA = cpaFixture(true, 1000, 5, 1, false, trafficcpa.TrendConverging) // as strong a signal as possible

	tier, _, escalated := classifyTier(obs, cfg)
	if tier != tierNotice || escalated {
		t.Errorf("expected no escalation with the default (disabled) config, got tier=%d escalated=%v", tier, escalated)
	}
}

func TestClassifyTier_CPANeverEscalatesPastHighCaution(t *testing.T) {
	cfg := cpaEnabledConfig()
	dist := cfg.HighCautionHorizontalMeters - 1
	obs := freshObs("A1", dist, 0)
	obs.CPA = cpaFixture(true, 1000, 5, 1, false, trafficcpa.TrendConverging)

	tier, _, escalated := classifyTier(obs, cfg)
	if tier != tierHighCaution {
		t.Errorf("expected tier to remain HighCaution (already the maximum), got %d", tier)
	}
	if escalated {
		t.Error("expected cpaEscalated=false - there is no tier above HighCaution to escalate to")
	}
}

func TestClassifyTier_CPAGroundCappedNeverEscalates(t *testing.T) {
	cfg := cpaEnabledConfig()
	cfg.SuppressGroundTraffic = true
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	obs.OnGround = true
	obs.CPA = cpaFixture(true, 1000, 5, 1, false, trafficcpa.TrendConverging)

	tier, _, escalated := classifyTier(obs, cfg)
	if tier != tierNotice || escalated {
		t.Errorf("expected ground-capped target to never escalate via CPA, got tier=%d escalated=%v", tier, escalated)
	}
}

func TestClassifyTier_CPATCPAClampedToHorizonNeverEscalates(t *testing.T) {
	cfg := cpaEnabledConfig()
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	obs.CPA = cpaFixture(true, cfg.CPAMinClosureRateKnots+10, cfg.CautionHorizontalMeters, cfg.CautionHorizontalMeters-1, true /* clamped */, trafficcpa.TrendConverging)

	tier, _, escalated := classifyTier(obs, cfg)
	if tier != tierNotice || escalated {
		t.Errorf("expected a horizon-clamped TCPA to never escalate, got tier=%d escalated=%v", tier, escalated)
	}
}

func TestClassifyTier_CPABelowMinClosureRateNeverEscalates(t *testing.T) {
	cfg := cpaEnabledConfig()
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2

	below := freshObs("A1", dist, 0)
	below.CPA = cpaFixture(true, cfg.CPAMinClosureRateKnots-1, 30, cfg.CautionHorizontalMeters-1, false, trafficcpa.TrendConverging)
	if tier, _, escalated := classifyTier(below, cfg); tier != tierNotice || escalated {
		t.Errorf("expected no escalation just below the minimum closure rate, got tier=%d escalated=%v", tier, escalated)
	}

	above := freshObs("A1", dist, 0)
	above.CPA = cpaFixture(true, cfg.CPAMinClosureRateKnots+1, 30, cfg.CautionHorizontalMeters-1, false, trafficcpa.TrendConverging)
	if tier, _, escalated := classifyTier(above, cfg); tier != tierCaution || !escalated {
		t.Errorf("expected escalation just above the minimum closure rate, got tier=%d escalated=%v", tier, escalated)
	}
}

func TestClassifyTier_CPAPredictedSeparationBeyondNextTierNeverEscalates(t *testing.T) {
	cfg := cpaEnabledConfig()
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	// Converging, fast, and within horizon - but the PREDICTED separation
	// at CPA still doesn't fit inside Caution's own horizontal threshold.
	obs.CPA = cpaFixture(true, cfg.CPAMinClosureRateKnots+50, 30, cfg.CautionHorizontalMeters+1, false, trafficcpa.TrendConverging)

	tier, _, escalated := classifyTier(obs, cfg)
	if tier != tierNotice || escalated {
		t.Errorf("expected no escalation when the predicted separation doesn't fit the next tier, got tier=%d escalated=%v", tier, escalated)
	}
}

func TestClassifyTier_CPAHighConfidenceRequiresVerticalFitToo(t *testing.T) {
	cfg := cpaEnabledConfig()
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	cpa := cpaFixture(true, cfg.CPAMinClosureRateKnots+10, 30, cfg.CautionHorizontalMeters-1, false, trafficcpa.TrendConverging)
	cpa.Confidence = trafficcpa.ConfidenceHigh
	cpa.PredictedVerticalSeparationValid = true
	cpa.PredictedVerticalSeparationFeet = cfg.CautionVerticalFeet + 1 // does not fit
	obs.CPA = cpa

	tier, _, escalated := classifyTier(obs, cfg)
	if tier != tierNotice || escalated {
		t.Errorf("expected no escalation when the high-confidence vertical prediction doesn't fit the next tier, got tier=%d escalated=%v", tier, escalated)
	}

	cpa.PredictedVerticalSeparationFeet = cfg.CautionVerticalFeet - 1 // now fits
	obs.CPA = cpa
	tier, _, escalated = classifyTier(obs, cfg)
	if tier != tierCaution || !escalated {
		t.Errorf("expected escalation once the vertical prediction also fits, got tier=%d escalated=%v", tier, escalated)
	}
}

func TestClassifyTier_CPARequiresValidCurrentRelativeAltitude(t *testing.T) {
	cfg := cpaEnabledConfig()
	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	obs.RelativeAltitudeValid = false // current altitude itself is untrustworthy
	obs.CPA = cpaFixture(true, 1000, 5, 1, false, trafficcpa.TrendConverging)

	tier, validity, escalated := classifyTier(obs, cfg)
	if tier != tierNotice || validity != "altitude-unavailable" || escalated {
		t.Errorf("expected the existing altitude-unavailable ceiling to hold regardless of CPA, got tier=%d validity=%q escalated=%v", tier, validity, escalated)
	}
}

// TestEvaluateTraffic_CPAEscalationUsesExistingFramework proves CPA
// escalation flows through the SAME hysteresis/audio/history machinery
// every other tier change already uses - never a second, parallel alert
// for the same target.
func TestEvaluateTraffic_CPAEscalationUsesExistingFramework(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cfg := cpaEnabledConfig()
	cfg.MasterEnabled = true
	cfg.TrafficAudioEnabled = true
	e := NewEvaluator(cfg, clock.now)

	dist := (cfg.NoticeHorizontalMeters + cfg.CautionHorizontalMeters) / 2
	obs := freshObs("A1", dist, 0)
	obs.CPA = cpaFixture(true, cfg.CPAMinClosureRateKnots+10, 30, cfg.CautionHorizontalMeters-1, false, trafficcpa.TrendConverging)

	events := e.EvaluateTraffic([]TrafficObservation{obs}, true)
	if len(events) != 1 {
		t.Fatalf("expected exactly one event (no parallel alert), got %d: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Kind != "new" || ev.Alert.Level != LevelTrafficCaution {
		t.Fatalf("expected a single new TRAFFIC_CAUTION event (CPA-escalated straight from first sight), got %+v", ev)
	}
	if !ev.Alert.CPAEscalated {
		t.Error("expected Alert.CPAEscalated=true")
	}
	if !ev.Alert.CPAValid || ev.Alert.CPATrend != string(trafficcpa.TrendConverging) {
		t.Errorf("expected the Alert to carry the CPA fields, got %+v", ev.Alert)
	}

	snap := e.Snapshot()
	if len(snap.Active) != 1 {
		t.Fatalf("expected exactly one active alert for this target, got %d", len(snap.Active))
	}

	// A later cycle with an unchanged tier must not re-alert (duplicate
	// suppression still applies exactly as before).
	clock.advance(time.Second)
	events = e.EvaluateTraffic([]TrafficObservation{obs}, true)
	if len(events) != 0 {
		t.Errorf("expected duplicate suppression to still apply after a CPA-escalated event, got %+v", events)
	}
}
