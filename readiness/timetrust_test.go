package readiness

import (
	"testing"
	"time"
)

func cfg() TimeTrustConfig {
	return DefaultTimeTrustConfig()
}

func goodSample(utc, receivedAt time.Time) GNSSTimeSample {
	return GNSSTimeSample{
		HardwarePresent: true,
		ChecksumValid:   true,
		StatusValid:     true,
		Parseable:       true,
		AcceptableFix:   true,
		UTC:             utc,
		ReceivedAt:      receivedAt,
	}
}

// --- EvaluateSample: one gate at a time ---

func TestEvaluateSample_HardwareAbsent(t *testing.T) {
	s := goodSample(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Now())
	s.HardwarePresent = false
	v := EvaluateSample(s, s.ReceivedAt, cfg())
	if v.Accepted {
		t.Error("sample with no hardware present must be rejected")
	}
}

func TestEvaluateSample_InvalidChecksum(t *testing.T) {
	s := goodSample(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Now())
	s.ChecksumValid = false
	v := EvaluateSample(s, s.ReceivedAt, cfg())
	if v.Accepted {
		t.Error("sample with invalid checksum must be rejected")
	}
}

func TestEvaluateSample_InvalidStatus(t *testing.T) {
	s := goodSample(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Now())
	s.StatusValid = false
	v := EvaluateSample(s, s.ReceivedAt, cfg())
	if v.Accepted {
		t.Error("sample with invalid sentence status must be rejected")
	}
}

func TestEvaluateSample_MalformedDateTime(t *testing.T) {
	s := goodSample(time.Time{}, time.Now())
	s.Parseable = false
	v := EvaluateSample(s, s.ReceivedAt, cfg())
	if v.Accepted {
		t.Error("unparseable date/time must be rejected")
	}
}

func TestEvaluateSample_ImplausibleYear(t *testing.T) {
	s := goodSample(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), time.Now())
	v := EvaluateSample(s, s.ReceivedAt, cfg())
	if v.Accepted {
		t.Error("an implausible year (1999) must be rejected")
	}
	s2 := goodSample(time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), time.Now())
	v2 := EvaluateSample(s2, s2.ReceivedAt, cfg())
	if v2.Accepted {
		t.Error("an implausible far-future year (2099) must be rejected")
	}
}

func TestEvaluateSample_StaleData(t *testing.T) {
	receivedAt := time.Now()
	s := goodSample(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), receivedAt)
	tooLate := receivedAt.Add(cfg().MaxSampleAge + time.Second)
	v := EvaluateSample(s, tooLate, cfg())
	if v.Accepted {
		t.Error("a stale sample must be rejected")
	}
}

func TestEvaluateSample_UnacceptableFix(t *testing.T) {
	s := goodSample(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Now())
	s.AcceptableFix = false
	v := EvaluateSample(s, s.ReceivedAt, cfg())
	if v.Accepted {
		t.Error("an unacceptable fix quality must be rejected")
	}
}

func TestEvaluateSample_ValidSampleAccepted(t *testing.T) {
	s := goodSample(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC), time.Now())
	v := EvaluateSample(s, s.ReceivedAt, cfg())
	if !v.Accepted {
		t.Errorf("a fully valid sample must be accepted, got reason: %s", v.Reason)
	}
}

// --- Consecutive-sample gate ---

func TestObserveGNSS_ConsecutiveSampleGate(t *testing.T) {
	tt := NewTimeTrust(cfg())
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := time.Now()
	sysClock := base.Add(-time.Hour) // system clock is stale by an hour

	// First RequiredConsecutive-1 samples must not yet establish trust.
	for i := 0; i < cfg().RequiredConsecutive-1; i++ {
		d := tt.ObserveGNSS(goodSample(base.Add(time.Duration(i)*time.Second), now.Add(time.Duration(i)*time.Second)), now.Add(time.Duration(i)*time.Second), sysClock, false)
		if d.Action != ClockActionNone {
			t.Fatalf("sample %d: expected no clock action before consecutive gate satisfied, got %s", i, d.Action)
		}
		if tt.State() == TimeGNSSSynced {
			t.Fatalf("sample %d: should not be GNSS_SYNCED yet", i)
		}
	}

	// The Nth agreeing sample should establish trust and step the clock.
	n := cfg().RequiredConsecutive - 1
	finalNow := now.Add(time.Duration(n) * time.Second)
	finalUTC := base.Add(time.Duration(n) * time.Second)
	d := tt.ObserveGNSS(goodSample(finalUTC, finalNow), finalNow, sysClock, false)
	if tt.State() != TimeGNSSSynced {
		t.Fatalf("after %d consecutive agreeing samples, expected GNSS_SYNCED, got %s", cfg().RequiredConsecutive, tt.State())
	}
	if d.Action != ClockActionStepOnce {
		t.Errorf("expected a step once trust is established with a stale clock, got %s (%s)", d.Action, d.Reason)
	}
}

func TestObserveGNSS_DisagreeingSampleResetsConsecutiveCount(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	sysClock := base

	tt.ObserveGNSS(goodSample(base, now), now, sysClock, false)
	// A wildly different UTC value on the very next sample - simulating a
	// glitch - must not count toward the consecutive streak.
	glitchNow := now.Add(time.Second)
	glitch := goodSample(base.Add(10*time.Hour), glitchNow)
	tt.ObserveGNSS(glitch, glitchNow, sysClock, false)
	if tt.consecutiveGood != 1 {
		t.Errorf("a disagreeing sample should reset the streak to 1 (itself), got %d", tt.consecutiveGood)
	}
}

// --- Stale build-time clock -> first valid trust -> one-time step ---

func TestObserveGNSS_StaleBuildClockIsSteppedOnce(t *testing.T) {
	tt := NewTimeTrust(cfg())
	realUTC := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	staleSystemClock := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // a typical unset-RTC build-time clock
	now := time.Now()

	var last Decision
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		last = tt.ObserveGNSS(goodSample(realUTC.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, staleSystemClock, false)
	}
	if last.Action != ClockActionStepOnce {
		t.Fatalf("expected ClockActionStepOnce for a stale build-time clock, got %s: %s", last.Action, last.Reason)
	}
	if !last.NewUTC.Equal(realUTC.Add(time.Duration(cfg().RequiredConsecutive-1) * time.Second)) {
		t.Errorf("NewUTC = %v, want the trusted GNSS time", last.NewUTC)
	}

	// A second large discrepancy in the same session must not step again.
	again := tt.ObserveGNSS(goodSample(realUTC.Add(time.Hour), now.Add(100*time.Second)), now.Add(100*time.Second), staleSystemClock, false)
	if again.Action == ClockActionStepOnce {
		t.Error("the clock must not be stepped a second time in the same session")
	}
}

// --- Backward-time rejection after recording begins ---

func TestObserveGNSS_RejectsBackwardCorrectionAfterRecordingStarted(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	// System clock is ahead of the (correct) GNSS time - a backward
	// correction - and recording has already begun.
	systemClock := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	behindUTC := systemClock.Add(-time.Hour)

	var last Decision
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		last = tt.ObserveGNSS(goodSample(behindUTC.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, systemClock, true /* recording started */)
	}
	if last.Action != ClockActionRejectBackward {
		t.Fatalf("expected ClockActionRejectBackward, got %s: %s", last.Action, last.Reason)
	}
	if tt.State() != TimeInvalid {
		t.Errorf("state after a rejected backward correction during recording should be INVALID, got %s", tt.State())
	}
}

func TestObserveGNSS_ForwardCorrectionAllowedEvenAfterRecordingStarted(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	systemClock := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	aheadUTC := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	var last Decision
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		last = tt.ObserveGNSS(goodSample(aheadUTC.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, systemClock, true)
	}
	if last.Action != ClockActionStepOnce {
		t.Errorf("a large forward correction should still be applied even if recording has started, got %s: %s", last.Action, last.Reason)
	}
	if tt.State() != TimeGNSSSynced {
		t.Errorf("state should be GNSS_SYNCED after a successful forward correction, got %s", tt.State())
	}
}

// --- Large forward step vs small slew ---

func TestObserveGNSS_SmallForwardCorrectionSlews(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	systemClock := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	slightlyAhead := systemClock.Add(2 * time.Second) // below LargeStepThreshold (5s)

	var last Decision
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		last = tt.ObserveGNSS(goodSample(slightlyAhead, sampleNow), sampleNow, systemClock, false)
	}
	if last.Action != ClockActionSlew {
		t.Errorf("a small forward correction should slew, got %s: %s", last.Action, last.Reason)
	}
}

// --- GNSS loss after synchronization ---

func TestObserveGNSS_LossAfterSyncDegrades(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	utc := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, utc, false)
	}
	if tt.State() != TimeGNSSSynced {
		t.Fatalf("setup failed: expected GNSS_SYNCED, got %s", tt.State())
	}

	// Now GNSS produces an invalid sample (e.g. checksum failure - loss of
	// a good fix).
	lossNow := now.Add(10 * time.Second)
	bad := goodSample(utc, lossNow)
	bad.ChecksumValid = false
	tt.ObserveGNSS(bad, lossNow, utc, false)
	if tt.State() != TimeDegraded {
		t.Errorf("state after GNSS loss following a successful sync should be DEGRADED, got %s", tt.State())
	}
}

func TestCheckStale_DegradesSilentSource(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	utc := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, utc, false)
	}
	// No further samples arrive at all (not even a rejected one) - only
	// CheckStale should catch this. The margin comfortably exceeds
	// StaleAfter measured from lastGoodAt, which the setup loop above has
	// already advanced a few seconds past `now`.
	tt.CheckStale(now.Add(cfg().StaleAfter + time.Minute))
	if tt.State() != TimeDegraded {
		t.Errorf("a source that has gone silent past StaleAfter should degrade, got %s", tt.State())
	}
}

func TestCheckStale_DoesNothingWhileFresh(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	utc := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, utc, false)
	}
	tt.CheckStale(now.Add(time.Second))
	if tt.State() != TimeGNSSSynced {
		t.Errorf("a source refreshed well within StaleAfter should remain synced, got %s", tt.State())
	}
}

// --- Network-to-GNSS fallback ---

func TestNetworkToGNSSFallback(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	networkUTC := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	systemClock := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	tt.ObserveNetwork(networkUTC, now, systemClock, true, false)
	if tt.State() != TimeNetworkSynced {
		t.Fatalf("expected NETWORK_SYNCED after a valid network observation, got %s", tt.State())
	}

	// Network is lost.
	tt.ObserveNetwork(time.Time{}, now.Add(time.Second), networkUTC, false, false)
	if tt.State() != TimeDegraded {
		t.Fatalf("expected DEGRADED after network loss, got %s", tt.State())
	}

	// GNSS now becomes available and, once trusted, must take over as the
	// offline fallback.
	var last Decision
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i+2) * time.Second)
		gnssUTC := networkUTC.Add(time.Duration(i+2) * time.Second)
		last = tt.ObserveGNSS(goodSample(gnssUTC, sampleNow), sampleNow, networkUTC.Add(time.Duration(i+2)*time.Second-time.Millisecond), false)
	}
	if tt.State() != TimeGNSSSynced {
		t.Fatalf("expected GNSS_SYNCED after GNSS becomes available following network loss, got %s (last decision: %s)", tt.State(), last.Reason)
	}
}

// --- Snapshot / RecordingAllowed ---

func TestSnapshot_RecordingAllowedOnlyWhenSynced(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	if tt.Snapshot(now).RecordingAllowed {
		t.Error("recording must not be allowed before any sync")
	}
	utc := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, utc, false)
	}
	if !tt.Snapshot(now).RecordingAllowed {
		t.Error("recording should be allowed once GNSS_SYNCED")
	}
}

func TestSnapshot_EventsAreBounded(t *testing.T) {
	tt := NewTimeTrust(cfg())
	now := time.Now()
	utc := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Establish trust once, then feed enough distinct large corrections to
	// generate more than maxRetainedEvents events (each accepted-but-not-
	// re-stepped GNSS sample past the first still records a "none, already
	// stepped" event).
	for i := 0; i < cfg().RequiredConsecutive; i++ {
		sampleNow := now.Add(time.Duration(i) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Duration(i)*time.Second), sampleNow), sampleNow, utc, false)
	}
	for i := 0; i < maxRetainedEvents+10; i++ {
		sampleNow := now.Add(time.Duration(i+100) * time.Second)
		tt.ObserveGNSS(goodSample(utc.Add(time.Hour), sampleNow), sampleNow, utc, false)
	}
	if got := len(tt.Snapshot(now).RecentEvents); got > maxRetainedEvents {
		t.Errorf("event log grew to %d, want capped at %d", got, maxRetainedEvents)
	}
}
