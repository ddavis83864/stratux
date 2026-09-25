package alerting

import (
	"math"
	"testing"
	"time"
)

// fakeClock is a simple controllable clock for deterministic tests.
type fakeClock struct{ t time.Time }

func (f *fakeClock) now() time.Time          { return f.t }
func (f *fakeClock) advance(d time.Duration) { f.t = f.t.Add(d) }

func newTestEvaluator() (*Evaluator, *fakeClock) {
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.MasterEnabled = true
	cfg.TrafficAudioEnabled = true
	cfg.SystemAudioEnabled = true
	return NewEvaluator(cfg, clock.now), clock
}

func freshObs(id string, distMeters, relAlt float64) TrafficObservation {
	return TrafficObservation{
		TargetID:              id,
		PositionValid:         true,
		DistanceValid:         true,
		DistanceMeters:        distMeters,
		RelativeAltitudeValid: true,
		RelativeAltitudeFeet:  relAlt,
		AgeSeconds:            1,
	}
}

func TestClassifyTier_InvalidOwnshipDataProducesNoAlert(t *testing.T) {
	e, _ := newTestEvaluator()
	obs := freshObs("A1", 500, 100) // well within high-caution range
	events := e.EvaluateTraffic([]TrafficObservation{obs}, false /* ownshipValid */)
	if len(events) != 0 {
		t.Fatalf("expected no events with invalid ownship data, got %+v", events)
	}
	if len(e.Snapshot().Active) != 0 {
		t.Fatalf("expected no active alerts with invalid ownship data")
	}
}

func TestClassifyTier_StaleTargetRejected(t *testing.T) {
	cfg := DefaultConfig()
	obs := freshObs("A1", 500, 100)
	obs.AgeSeconds = cfg.StaleAfterSeconds + 1
	tier, validity, _ := classifyTier(obs, cfg)
	if tier != tierNone || validity != "stale" {
		t.Errorf("tier=%d validity=%q, want tierNone/stale", tier, validity)
	}
}

func TestClassifyTier_InvalidPositionRejected(t *testing.T) {
	cfg := DefaultConfig()
	obs := freshObs("A1", 500, 100)
	obs.PositionValid = false
	tier, validity, _ := classifyTier(obs, cfg)
	if tier != tierNone || validity != "position-invalid" {
		t.Errorf("tier=%d validity=%q, want tierNone/position-invalid", tier, validity)
	}
}

func TestClassifyTier_NonFiniteValuesRejected(t *testing.T) {
	cfg := DefaultConfig()
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		obs := freshObs("A1", bad, 100)
		tier, _, _ := classifyTier(obs, cfg)
		if tier != tierNone {
			t.Errorf("distance=%v: tier=%d, want tierNone", bad, tier)
		}
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		obs := freshObs("A1", 500, bad)
		tier, _, _ := classifyTier(obs, cfg)
		if tier > tierNotice {
			t.Errorf("relAlt=%v: tier=%d, want <= tierNotice (altitude untrustworthy)", bad, tier)
		}
	}
}

func TestClassifyTier_OwnshipIdentifiedIsExcluded(t *testing.T) {
	e, _ := newTestEvaluator()
	obs := freshObs("OWN", 100, 50)
	obs.IsOwnship = true
	events := e.EvaluateTraffic([]TrafficObservation{obs}, true)
	if len(events) != 0 {
		t.Fatalf("expected no events for an ownship-identified observation, got %+v", events)
	}
}

func TestClassifyTier_MissingAltitudeCapsAtNotice(t *testing.T) {
	cfg := DefaultConfig()
	obs := freshObs("A1", 1000, 0) // well within caution/high-caution range
	obs.RelativeAltitudeValid = false
	tier, validity, _ := classifyTier(obs, cfg)
	if tier != tierNotice || validity != "altitude-unavailable" {
		t.Errorf("tier=%d validity=%q, want tierNotice/altitude-unavailable", tier, validity)
	}
}

func TestClassifyTier_OutsideMonitoringEnvelope(t *testing.T) {
	cfg := DefaultConfig()
	obs := freshObs("A1", cfg.MonitoringHorizontalMeters+1, 0)
	tier, validity, _ := classifyTier(obs, cfg)
	if tier != tierNone || validity != "outside-envelope" {
		t.Errorf("tier=%d validity=%q, want tierNone/outside-envelope", tier, validity)
	}
	obs2 := freshObs("A2", 100, cfg.MonitoringVerticalFeet+1)
	tier2, validity2, _ := classifyTier(obs2, cfg)
	if tier2 != tierNone || validity2 != "outside-envelope" {
		t.Errorf("tier=%d validity=%q, want tierNone/outside-envelope (vertical)", tier2, validity2)
	}
}

func TestClassifyTier_ThresholdEntryExactBoundaries(t *testing.T) {
	cfg := DefaultConfig()
	cases := []struct {
		name      string
		dist, alt float64
		wantTier  int
	}{
		{"exactly at notice", cfg.NoticeHorizontalMeters, cfg.NoticeVerticalFeet, tierNotice},
		{"just inside notice", cfg.NoticeHorizontalMeters - 1, cfg.NoticeVerticalFeet - 1, tierNotice},
		{"just outside notice", cfg.NoticeHorizontalMeters + 1, cfg.NoticeVerticalFeet - 1, tierNone},
		{"exactly at caution", cfg.CautionHorizontalMeters, cfg.CautionVerticalFeet, tierCaution},
		{"exactly at high caution", cfg.HighCautionHorizontalMeters, cfg.HighCautionVerticalFeet, tierHighCaution},
	}
	for _, c := range cases {
		obs := freshObs("A1", c.dist, c.alt)
		tier, _, _ := classifyTier(obs, cfg)
		if tier != c.wantTier {
			t.Errorf("%s: tier=%d, want %d", c.name, tier, c.wantTier)
		}
	}
}

func TestEvaluateTraffic_NoticeEntryProducesEvent(t *testing.T) {
	e, _ := newTestEvaluator()
	obs := freshObs("A1", 4000, 1000) // inside notice, outside caution
	events := e.EvaluateTraffic([]TrafficObservation{obs}, true)
	if len(events) != 1 || events[0].Kind != "new" || events[0].Alert.Level != LevelTrafficNotice {
		t.Fatalf("expected one new TRAFFIC_NOTICE event, got %+v", events)
	}
}

func TestEvaluateTraffic_CautionEscalation(t *testing.T) {
	e, clock := newTestEvaluator()
	// Enter at notice range first.
	e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 4000, 1000)}, true)
	clock.advance(time.Second)
	// Now escalate to caution range immediately (same cycle proof).
	events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 3000, 700)}, true)
	if len(events) != 1 || events[0].Kind != "escalated" || events[0].Alert.Level != LevelTrafficCaution {
		t.Fatalf("expected immediate escalation to TRAFFIC_CAUTION, got %+v", events)
	}
}

func TestEvaluateTraffic_DuplicateSuppression(t *testing.T) {
	e, clock := newTestEvaluator()
	obs := freshObs("A1", 3000, 700)
	e.EvaluateTraffic([]TrafficObservation{obs}, true)
	clock.advance(time.Second)
	events := e.EvaluateTraffic([]TrafficObservation{obs}, true) // same tier again
	if len(events) != 0 {
		t.Fatalf("expected duplicate suppression (no event for unchanged tier), got %+v", events)
	}
}

func TestEvaluateTraffic_HysteresisPreventsFlapping(t *testing.T) {
	e, clock := newTestEvaluator()
	cfg := e.Config()
	// Enter caution.
	e.EvaluateTraffic([]TrafficObservation{freshObs("A1", cfg.CautionHorizontalMeters-1, 0)}, true)
	clock.advance(time.Second)
	// Drift just outside entry threshold but still inside the widened exit band - must NOT de-escalate.
	justOutside := cfg.CautionHorizontalMeters + 1
	events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", justOutside, 0)}, true)
	if len(events) != 0 {
		t.Fatalf("expected no de-escalation while still inside the widened exit band, got %+v", events)
	}
	snap := e.Snapshot()
	if len(snap.Active) != 1 || snap.Active[0].Level != LevelTrafficCaution {
		t.Fatalf("expected target to remain at TRAFFIC_CAUTION during hysteresis band, got %+v", snap.Active)
	}
}

func TestEvaluateTraffic_DelayedDeescalation(t *testing.T) {
	e, clock := newTestEvaluator()
	cfg := e.Config()
	e.EvaluateTraffic([]TrafficObservation{freshObs("A1", cfg.CautionHorizontalMeters-1, 0)}, true)
	clock.advance(time.Second)
	// Move well outside even the widened exit band.
	farOutside := cfg.CautionHorizontalMeters * cfg.ExitHysteresisFactor * 2
	events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", farOutside, 0)}, true)
	if len(events) != 0 {
		t.Fatalf("expected no immediate de-escalation before dwell elapses, got %+v", events)
	}
	// Advance past the dwell period.
	clock.advance(time.Duration(cfg.DeescalateDwellSeconds+1) * time.Second)
	events = e.EvaluateTraffic([]TrafficObservation{freshObs("A1", farOutside, 0)}, true)
	if len(events) != 1 || events[0].Kind != "cleared" {
		t.Fatalf("expected de-escalation to clear after dwell elapses, got %+v", events)
	}
}

// TestEvaluateTraffic_AudioCooldownExpires exercises the realistic case the
// per-target cooldown protects against: a target hovering near a tier
// boundary, de-escalating (without ever fully clearing to tier 0 - see
// TestEvaluateTraffic_HysteresisPreventsFlapping/DelayedDeescalation for
// that separate, cooldown-resetting path) and then re-escalating to the
// same tier bucket again soon after.
func TestEvaluateTraffic_AudioCooldownExpires(t *testing.T) {
	e, clock := newTestEvaluator()
	cfg := e.Config()

	// Enter caution - first caution-bucket audio should be eligible.
	first := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", cfg.CautionHorizontalMeters-1, 0)}, true)
	if len(first) != 1 || !first[0].Alert.AudioEligible {
		t.Fatalf("expected first caution event to be audio-eligible, got %+v", first)
	}
	clock.advance(time.Second)

	// De-escalate to notice-only range (still nonzero - not a full clear)
	// and let the dwell period elapse so the tier actually drops to 1.
	farOutsideCaution := cfg.CautionHorizontalMeters * cfg.ExitHysteresisFactor * 2
	if farOutsideCaution >= cfg.NoticeHorizontalMeters {
		farOutsideCaution = cfg.NoticeHorizontalMeters - 1
	}
	e.EvaluateTraffic([]TrafficObservation{freshObs("A1", farOutsideCaution, 0)}, true)
	clock.advance(time.Duration(cfg.DeescalateDwellSeconds+1) * time.Second)
	deescalated := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", farOutsideCaution, 0)}, true)
	if len(deescalated) != 1 || deescalated[0].Kind != "deescalated" {
		t.Fatalf("expected a deescalated (not cleared) event, got %+v", deescalated)
	}
	clock.advance(time.Second)

	// Re-escalate to caution again, well within the original cooldown
	// window - must be a new "escalated" event, but audio suppressed.
	reescalated := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", cfg.CautionHorizontalMeters-1, 0)}, true)
	if len(reescalated) != 1 || reescalated[0].Kind != "escalated" {
		t.Fatalf("expected an escalated event, got %+v", reescalated)
	}
	if reescalated[0].Alert.AudioEligible {
		t.Fatalf("expected audio suppressed by per-target cooldown on quick re-escalation")
	}
	if reescalated[0].Alert.SuppressionReason != "per-target-cooldown" {
		t.Errorf("SuppressionReason = %q, want per-target-cooldown", reescalated[0].Alert.SuppressionReason)
	}
}

func TestEvaluateTraffic_GlobalMinAudioSpacing(t *testing.T) {
	e, _ := newTestEvaluator()
	events := e.EvaluateTraffic([]TrafficObservation{
		freshObs("A1", 4000, 1000),
		freshObs("A2", 4000, 1000),
	}, true)
	if len(events) != 2 {
		t.Fatalf("expected 2 new events, got %d", len(events))
	}
	audible := 0
	for _, ev := range events {
		if ev.Alert.AudioEligible {
			audible++
		}
	}
	if audible != 1 {
		t.Errorf("expected exactly 1 of 2 simultaneous events to be audio-eligible (global spacing), got %d", audible)
	}
}

func TestEvaluateTraffic_TargetExpiresAndCanReenter(t *testing.T) {
	e, clock := newTestEvaluator()
	cfg := e.Config()
	e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 4000, 1000)}, true)
	e.Acknowledge("A1")
	clock.advance(time.Duration(cfg.ExpireAfterSeconds+1) * time.Second)
	events := e.EvaluateTraffic(nil, true) // A1 no longer observed
	if len(events) != 1 || events[0].Kind != "expired" {
		t.Fatalf("expected expired event, got %+v", events)
	}
	if len(e.Snapshot().Active) != 0 {
		t.Fatalf("expected no active alerts after expiration")
	}
	// Re-entry must produce a fresh, unacknowledged "new" event.
	reentry := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 4000, 1000)}, true)
	if len(reentry) != 1 || reentry[0].Kind != "new" || reentry[0].Alert.Acknowledged {
		t.Fatalf("expected fresh unacknowledged event on re-entry, got %+v", reentry)
	}
}

func TestEvaluateTraffic_AcknowledgeClearedByEscalation(t *testing.T) {
	e, clock := newTestEvaluator()
	e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 4000, 1000)}, true)
	e.Acknowledge("A1")
	clock.advance(time.Second)
	events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 3000, 700)}, true) // escalate to caution
	if len(events) != 1 || events[0].Alert.Acknowledged {
		t.Fatalf("expected escalation past acknowledged tier to clear ack, got %+v", events)
	}
}

func TestEvaluateTraffic_MaxTrackedTargetsBounded(t *testing.T) {
	e, _ := newTestEvaluator()
	cfg := e.Config()
	cfg.MaxTrackedTargets = 3
	if err := e.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	var obs []TrafficObservation
	for i := 0; i < 10; i++ {
		obs = append(obs, freshObs(string(rune('A'+i)), 4000, 1000))
	}
	e.EvaluateTraffic(obs, true)
	if got := e.Snapshot().TrackedTargetCount; got > 3 {
		t.Errorf("TrackedTargetCount = %d, want <= 3 (MaxTrackedTargets)", got)
	}
}

func TestEvaluateTraffic_ChannelOverflowCounterIndependent(t *testing.T) {
	e, _ := newTestEvaluator()
	e.RecordDroppedEvaluation()
	e.RecordDroppedEvaluation()
	if e.Snapshot().Counters.DroppedEvaluations != 2 {
		t.Errorf("DroppedEvaluations = %d, want 2", e.Snapshot().Counters.DroppedEvaluations)
	}
}

func TestEvaluateTraffic_MuteSuppressesAudioNotVisual(t *testing.T) {
	e, _ := newTestEvaluator()
	untilFar := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	e.SetMuted(true, untilFar)
	events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 4000, 1000)}, true)
	if len(events) != 1 {
		t.Fatalf("expected a visual event even while muted, got %+v", events)
	}
	if events[0].Alert.AudioEligible {
		t.Errorf("expected AudioEligible=false while muted")
	}
	if events[0].Alert.SuppressionReason != "muted" {
		t.Errorf("SuppressionReason = %q, want muted", events[0].Alert.SuppressionReason)
	}
	if len(e.Snapshot().Active) != 1 {
		t.Errorf("expected visual alert to remain active while muted")
	}
}

func TestEvaluateTraffic_UnmuteRestoresAudioEligibility(t *testing.T) {
	e, clock := newTestEvaluator()
	e.SetMuted(true, time.Time{})
	if !e.IsMuted() {
		t.Fatal("expected muted")
	}
	e.SetMuted(false, time.Time{})
	if e.IsMuted() {
		t.Fatal("expected unmuted")
	}
	clock.advance(time.Second)
	events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 4000, 1000)}, true)
	if !events[0].Alert.AudioEligible {
		t.Errorf("expected audio eligible after unmute")
	}
}

func TestEvaluateTraffic_TimedMuteExpires(t *testing.T) {
	e, clock := newTestEvaluator()
	e.SetMuted(true, clock.now().Add(5*time.Second))
	clock.advance(6 * time.Second)
	if e.IsMuted() {
		t.Fatal("expected mute to have expired")
	}
}

func TestEvaluateTraffic_MasterDisabledProducesNoAlerts(t *testing.T) {
	e, _ := newTestEvaluator()
	cfg := e.Config()
	cfg.MasterEnabled = false
	if err := e.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", 500, 100)}, true)
	if len(events) != 0 {
		t.Fatalf("expected no events with MasterEnabled=false, got %+v", events)
	}
}

func TestEvaluateTraffic_GroundSuppression(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SuppressGroundTraffic = true
	obs := freshObs("A1", cfg.HighCautionHorizontalMeters-1, 0)
	obs.OnGround = true
	tier, _, _ := classifyTier(obs, cfg)
	if tier > tierNotice {
		t.Errorf("ground-suppressed target: tier=%d, want capped at tierNotice", tier)
	}
}

func TestConfig_ValidationRejectsNaNAndInf(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CautionHorizontalMeters = math.NaN()
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for NaN threshold")
	}
	cfg = DefaultConfig()
	cfg.NoticeVerticalFeet = math.Inf(1)
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for +Inf threshold")
	}
}

func TestConfig_ValidationRejectsNegativeAndBadOrdering(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CautionHorizontalMeters = -1
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for negative distance")
	}
	cfg = DefaultConfig()
	cfg.ExitHysteresisFactor = 0.5
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for ExitHysteresisFactor <= 1.0")
	}
	cfg = DefaultConfig()
	cfg.NoticeHorizontalMeters = cfg.MonitoringHorizontalMeters + 1
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for notice threshold wider than monitoring envelope")
	}
}

func TestConfig_DefaultIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("DefaultConfig() failed validation: %v", err)
	}
}

func TestEvaluateTraffic_EventSeqMonotonicAndUnique(t *testing.T) {
	e, clock := newTestEvaluator()
	seen := map[int64]bool{}
	var last int64
	for i := 0; i < 5; i++ {
		events := e.EvaluateTraffic([]TrafficObservation{freshObs("A1", float64(4000-i*100), 1000)}, true)
		for _, ev := range events {
			if ev.Seq <= last {
				t.Errorf("Seq did not increase monotonically: got %d after %d", ev.Seq, last)
			}
			if seen[ev.Seq] {
				t.Errorf("Seq %d reused", ev.Seq)
			}
			seen[ev.Seq] = true
			last = ev.Seq
		}
		clock.advance(time.Second)
	}
	if len(seen) == 0 {
		t.Fatal("expected at least one event with a nonzero Seq")
	}
}

func TestClockDirectionFromRelativeBearing(t *testing.T) {
	cases := map[float64]ClockDirection{
		0:   "12 o'clock",
		90:  "3 o'clock",
		180: "6 o'clock",
		270: "9 o'clock",
		-90: "9 o'clock",
		360: "12 o'clock",
	}
	for bearing, want := range cases {
		if got := ClockDirectionFromRelativeBearing(bearing); got != want {
			t.Errorf("bearing=%v: got %q, want %q", bearing, got, want)
		}
	}
}
