package fisbcache

import (
	"testing"
	"time"
)

// Freshness semantics: a cached product must never look fresher than either
// its RECEPTION age or its own (source) age allows. These tests use explicit
// monotonic seconds and wall times - nothing waits.

const minute = 60.0 // monotonic seconds

// metarAt builds a METAR entry received at monotonic recvMono / wall recvUTC
// whose trusted source time lies lag before reception (negative lag = the
// source time is AFTER reception). trusted=false models "no trustworthy
// source time".
func metarAt(recvMono float64, recvUTC time.Time, lag time.Duration, trusted bool) Entry {
	e := Entry{Key: TextKey("METAR", "KSEA"), ReceivedAtMonotonic: recvMono, ReceivedAtUTC: recvUTC, SizeBytes: 60}
	if trusted {
		e.Source = SourceTime{Trusted: true, UTC: recvUTC.Add(-lag)}
	}
	return e
}

func policyOf(e Entry) ProductPolicy { return PolicyFor(e.Key) }

var t0 = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func TestEffectiveAge_RecentSourceRecentReceptionIsFresh(t *testing.T) {
	e := metarAt(1000, t0, 2*time.Minute, true)
	age, basis := e.EffectiveAge(policyOf(e), 1000+3*minute)
	if basis != AgeBasisSource || age != 5*time.Minute {
		t.Fatalf("age=%v basis=%s, want 5m from the source", age, basis)
	}
	if got := Freshness(e, policyOf(e), 1000+3*minute); got != FreshnessCachedFresh {
		t.Fatalf("freshness = %s, want CACHED_FRESH", got)
	}
}

// THE critical regression: an old product received a moment ago must not be
// presented as fresh.
func TestFreshness_OldSourceRecentReceptionIsNotFresh(t *testing.T) {
	e := metarAt(1000, t0, 50*time.Minute, true) // issued 50 min before we heard it
	now := 1000 + 1.0                            // received one second ago
	if got := Freshness(e, policyOf(e), now); got != FreshnessCachedAging {
		t.Fatalf("a 50-minute-old METAR received a second ago is %s, want CACHED_AGING (reception-only age would have said CACHED_FRESH)", got)
	}
	age, basis := e.EffectiveAge(policyOf(e), now)
	if basis != AgeBasisSource || age < 50*time.Minute {
		t.Fatalf("effective age %v basis %s, want >= 50m from the source", age, basis)
	}
	if e.ReceptionAge(now) > 2*time.Second {
		t.Fatalf("reception age must stay separately available, got %v", e.ReceptionAge(now))
	}
	if src, ok := e.SourceAge(now); !ok || src != age {
		t.Fatalf("SourceAge = %v,%v want %v", src, ok, age)
	}
}

func TestFreshness_RecentSourceOldReceptionIsLimitedByReception(t *testing.T) {
	e := metarAt(1000, t0, 1*time.Minute, true) // source only a minute older than reception
	now := 1000 + 40*minute                     // ...but received 40 minutes ago
	age, _ := e.EffectiveAge(policyOf(e), now)
	if age < 40*time.Minute {
		t.Fatalf("effective age %v is younger than the reception age (40m)", age)
	}
	if got := Freshness(e, policyOf(e), now); got != FreshnessCachedAging {
		t.Fatalf("freshness = %s, want CACHED_AGING", got)
	}
}

func TestEffectiveAge_NoTrustedSourceFallsBackToReceptionAndSaysSo(t *testing.T) {
	e := metarAt(1000, t0, 0, false)
	age, basis := e.EffectiveAge(policyOf(e), 1000+5*minute)
	if basis != AgeBasisReception || age != 5*time.Minute {
		t.Fatalf("age=%v basis=%s, want a 5m reception-basis age", age, basis)
	}
	if _, ok := e.SourceAge(1000); ok {
		t.Fatal("a source age must not be fabricated when the source time is untrusted")
	}
	// A trusted source with no recorded (trusted) reception wall time cannot be turned into a lag either.
	e2 := metarAt(1000, time.Time{}, 0, false)
	e2.Source = SourceTime{Trusted: true, UTC: t0}
	if _, basis := e2.EffectiveAge(policyOf(e2), 1000); basis != AgeBasisReception {
		t.Fatalf("basis = %s without a trusted reception time, want reception", basis)
	}
}

func TestEffectiveAge_FutureSourceTimeNeverMakesAProductYoungerOrNegative(t *testing.T) {
	for _, ahead := range []time.Duration{1 * time.Second, 3 * time.Minute, 5 * time.Minute} {
		e := metarAt(1000, t0, -ahead, true)
		age, basis := e.EffectiveAge(policyOf(e), 1000+10*minute)
		if age < 10*time.Minute {
			t.Fatalf("source %v in the future produced age %v, younger than the reception age", ahead, age)
		}
		if age < 0 {
			t.Fatalf("negative age %v", age)
		}
		if basis != AgeBasisSource {
			t.Fatalf("a tolerated small future skew keeps the source basis, got %s", basis)
		}
	}
	// A source further in the future is rejected by the reconstruction (never trusted at all).
	ft := FISBTime{Hour: 12, Minute: 30}
	if got := ReconstructSourceTime(ft, t0, true); got.Trusted && got.UTC.After(t0.Add(maxFutureSkew)) {
		t.Fatalf("a source %v in the future was trusted", got.UTC.Sub(t0))
	}
}

func TestEffectiveAge_AbsurdPersistedLagIsBounded(t *testing.T) {
	e := metarAt(1000, t0, 1000*24*time.Hour, true) // e.g. a tampered/corrupt persisted source time
	age, _ := e.EffectiveAge(policyOf(e), 1000)
	if age > maxSourceLag+time.Second || age <= 0 {
		t.Fatalf("age %v is not bounded by %v", age, maxSourceLag)
	}
	if got := Freshness(e, policyOf(e), 1000); got != FreshnessExpired {
		t.Fatalf("freshness = %s, want EXPIRED", got)
	}
}

func TestFreshness_ExactBoundaryIsInclusive(t *testing.T) {
	p := PolicyFor(TextKey("METAR", "KSEA"))
	e := metarAt(0, t0, p.FreshLimit, true)
	if got := Freshness(e, p, 0); got != FreshnessCachedFresh {
		t.Fatalf("age exactly at the fresh limit = %s, want CACHED_FRESH", got)
	}
	e = metarAt(0, t0, p.FreshLimit+time.Second, true)
	if got := Freshness(e, p, 0); got != FreshnessCachedAging {
		t.Fatalf("one second past the fresh limit = %s, want CACHED_AGING", got)
	}
}

// Repeated reception of the SAME product (same source time) must not reset its
// source age: the retransmission's reception age is small but its lag has
// grown by exactly the time since the earlier copy.
func TestRetransmissionDoesNotResetSourceAge(t *testing.T) {
	s := NewStore()
	key := TextKey("METAR", "KSEA")
	source := t0.Add(-10 * time.Minute)
	admit := func(mono float64, wall time.Time) AdmitResult {
		return s.Admit(Entry{Key: key, ReceivedAtMonotonic: mono, ReceivedAtUTC: wall, SizeBytes: 60, Source: SourceTime{Trusted: true, UTC: source}})
	}
	if r := admit(0, t0); r != AdmitAccepted {
		t.Fatalf("first admit = %v", r)
	}
	// The tower keeps repeating it, once a minute, for 2 hours.
	var last Entry
	for m := 1; m <= 120; m++ {
		if r := admit(float64(m)*minute, t0.Add(time.Duration(m)*time.Minute)); r != AdmitSuperseded {
			t.Fatalf("retransmission %d = %v, want AdmitSuperseded", m, r)
		}
		last, _ = s.Get(key)
	}
	now := 120 * minute // just received
	src, ok := last.SourceAge(now)
	if !ok || src != 130*time.Minute {
		t.Fatalf("source age after 2h of retransmissions = %v (ok=%v), want 130m", src, ok)
	}
	if got := Freshness(last, PolicyFor(key), now); got != FreshnessStale {
		t.Fatalf("a 130-minute-old METAR that is still being rebroadcast is %s, want STALE", got)
	}
	// ...and reception-only accounting would have called it fresh: prove the test is not vacuous.
	if last.ReceptionAge(now) > time.Second {
		t.Fatalf("setup: reception age should be ~0, got %v", last.ReceptionAge(now))
	}
	// Never immortal: 4 hours in, it is EXPIRED and the retention plan removes it although it was just received.
	admit(240*minute, t0.Add(240*time.Minute))
	last, _ = s.Get(key)
	if got := Freshness(last, PolicyFor(key), 240*minute); got != FreshnessExpired {
		t.Fatalf("after 4h of rebroadcast an old METAR is %s, want EXPIRED", got)
	}
	if plan := PlanEviction(s.Snapshot(), 1<<20, 100, 240*minute); len(plan) != 1 || plan[0] != key {
		t.Fatalf("retention plan = %v, want the expired-by-source entry removed", plan)
	}
}

func TestNewerSourceProductReplacesAndResetsSourceAge(t *testing.T) {
	s := NewStore()
	key := TextKey("METAR", "KSEA")
	s.Admit(Entry{Key: key, ReceivedAtMonotonic: 0, ReceivedAtUTC: t0, SizeBytes: 60, Source: SourceTime{Trusted: true, UTC: t0.Add(-50 * time.Minute)}})
	newer := Entry{Key: key, ReceivedAtMonotonic: 10 * minute, ReceivedAtUTC: t0.Add(10 * time.Minute), SizeBytes: 60, Source: SourceTime{Trusted: true, UTC: t0.Add(5 * time.Minute)}}
	if r := s.Admit(newer); r != AdmitSuperseded {
		t.Fatalf("a newer product = %v, want AdmitSuperseded", r)
	}
	got, _ := s.Get(key)
	age, basis := got.EffectiveAge(PolicyFor(key), 10*minute)
	if basis != AgeBasisSource || age != 5*time.Minute {
		t.Fatalf("age after the newer product = %v (%s), want 5m", age, basis)
	}
	if st := Freshness(got, PolicyFor(key), 10*minute); st != FreshnessCachedFresh {
		t.Fatalf("freshness = %s", st)
	}
}

func TestOlderOutOfOrderProductDoesNotReplaceNewerOrRefreshIt(t *testing.T) {
	s := NewStore()
	key := TextKey("METAR", "KSEA")
	newer := Entry{Key: key, ReceivedAtMonotonic: 0, ReceivedAtUTC: t0, SizeBytes: 60, Source: SourceTime{Trusted: true, UTC: t0.Add(-5 * time.Minute)}}
	s.Admit(newer)
	older := Entry{Key: key, ReceivedAtMonotonic: 5 * minute, ReceivedAtUTC: t0.Add(5 * time.Minute), SizeBytes: 60, Source: SourceTime{Trusted: true, UTC: t0.Add(-40 * time.Minute)}}
	if r := s.Admit(older); r != AdmitRejectedOlder {
		t.Fatalf("an older product = %v, want AdmitRejectedOlder", r)
	}
	got, _ := s.Get(key)
	if got != newer {
		t.Fatalf("the accepted newer entry was altered: %+v", got)
	}
}

// The age advances on the monotonic clock ONLY. EffectiveAge takes no wall-clock input at all - the
// wall times inside the entry were captured together, at reception, while the clock was trusted - so a
// later wall-clock correction (GPS lock, NTP step, a jump of hours either way) cannot change it.
func TestAgeAdvancesLinearlyOnTheMonotonicClockOnly(t *testing.T) {
	e := metarAt(1000, t0, 20*time.Minute, true)
	prev := time.Duration(-1)
	for _, dm := range []float64{0, 1, 59, 60, 600, 3600, 36000} {
		age, _ := e.EffectiveAge(policyOf(e), 1000+dm)
		want := 20*time.Minute + time.Duration(dm*float64(time.Second))
		if age != want {
			t.Fatalf("at +%.0fs age = %v, want %v", dm, age, want)
		}
		if age < prev {
			t.Fatalf("age went backwards: %v after %v", age, prev)
		}
		prev = age
	}
	// A monotonic value before reception (never expected) is clamped, not negative.
	if age, _ := e.EffectiveAge(policyOf(e), 500); age != 20*time.Minute {
		t.Fatalf("age before reception = %v, want just the source lag", age)
	}
}

func TestReconstructSourceTime_RolloverAndWindowForFreshnessLabelling(t *testing.T) {
	// Hour/minute only, across UTC midnight: received 00:02, product time 23:58 -> yesterday, 4 minutes old.
	recv := time.Date(2026, 9, 25, 0, 2, 0, 0, time.UTC)
	src := ReconstructSourceTime(FISBTime{Hour: 23, Minute: 58}, recv, true)
	if !src.Trusted || recv.Sub(src.UTC) != 4*time.Minute {
		t.Fatalf("midnight rollover: %+v", src)
	}
	// Month/day across New Year.
	recv = time.Date(2027, 1, 1, 0, 3, 0, 0, time.UTC)
	src = ReconstructSourceTime(FISBTime{HasMonthDay: true, Month: 12, Day: 31, Hour: 23, Minute: 55}, recv, true)
	if !src.Trusted || recv.Sub(src.UTC) != 8*time.Minute {
		t.Fatalf("new-year rollover: %+v", src)
	}
	// A 30-hour-old TAF with a date is recognised as OLD (not rejected -> not made to look fresh).
	recv = time.Date(2026, 9, 25, 18, 0, 0, 0, time.UTC)
	src = ReconstructSourceTime(FISBTime{HasMonthDay: true, Month: 9, Day: 24, Hour: 12, Minute: 0}, recv, true)
	if !src.Trusted || recv.Sub(src.UTC) != 30*time.Hour {
		t.Fatalf("a 30h-old dated product should keep its source time, got %+v", src)
	}
	// Beyond the plausibility window it is rejected (decode error), i.e. reception-basis with an explicit label.
	src = ReconstructSourceTime(FISBTime{HasMonthDay: true, Month: 9, Day: 20, Hour: 12, Minute: 0}, recv, true)
	if src.Trusted {
		t.Fatalf("a product 5 days old must not be trusted as a source time: %+v", src)
	}
	// Hour/minute-only formats cannot express more than 24h: a 26h-old product reconstructs
	// to 2h - never older than reality, never younger than its reception age.
	recv = time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	src = ReconstructSourceTime(FISBTime{Hour: 12, Minute: 0}, recv, true)
	if !src.Trusted || recv.Sub(src.UTC) != 2*time.Hour {
		t.Fatalf("hh:mm-only aliasing: %+v", src)
	}
}

func TestUntrustedReceiveClockGivesReceptionBasis(t *testing.T) {
	src := ReconstructSourceTime(FISBTime{Hour: 11, Minute: 0}, t0, false) // wall clock not trusted at reception
	if src.Trusted {
		t.Fatal("a source time must not be reconstructed against an untrusted clock")
	}
	e := Entry{Key: TextKey("METAR", "KSEA"), ReceivedAtMonotonic: 0, Source: src} // ReceivedAtUTC not recorded either
	if _, basis := e.EffectiveAge(PolicyFor(e.Key), 30); basis != AgeBasisReception {
		t.Fatalf("basis = %s, want reception", basis)
	}
}

// Every cached product class: the same "old source, just received" scenario, with the
// product's own thresholds.
func TestFreshnessPerProductClass(t *testing.T) {
	nexrad := NexradKey(63, 0, 40, -100, 0.0667, 0.8)
	cases := []struct {
		name string
		key  Key
		lag  time.Duration
		want FreshnessState
	}{
		{"METAR 50m", TextKey("METAR", "KSEA"), 50 * time.Minute, FreshnessCachedAging},
		{"SPECI 50m", TextKey("SPECI", "KSEA"), 50 * time.Minute, FreshnessCachedAging},
		{"TAF 5h", TextKey("TAF", "KSEA"), 5 * time.Hour, FreshnessCachedAging},
		{"TAF.AMD 5h", TextKey("TAF.AMD", "KSEA"), 5 * time.Hour, FreshnessCachedAging},
		{"WINDS 10h", TextKey("WINDS", "SEA"), 10 * time.Hour, FreshnessStale},
		{"PIREP 90m", TextKey("PIREP", "SEA"), 90 * time.Minute, FreshnessStale},
		{"NEXRAD 15m", nexrad, 15 * time.Minute, FreshnessCachedAging},
		{"NEXRAD 50m", nexrad, 50 * time.Minute, FreshnessExpired},
	}
	for _, c := range cases {
		e := Entry{Key: c.key, ReceivedAtMonotonic: 0, ReceivedAtUTC: t0, Source: SourceTime{Trusted: true, UTC: t0.Add(-c.lag)}}
		if !PolicyFor(c.key).known {
			t.Fatalf("%s: no policy", c.name)
		}
		if got := Freshness(e, PolicyFor(c.key), 0); got != c.want {
			t.Errorf("%s: freshness %s, want %s", c.name, got, c.want)
		}
		if _, basis := e.EffectiveAge(PolicyFor(c.key), 0); basis != AgeBasisSource {
			t.Errorf("%s: basis %s, want source", c.name, basis)
		}
	}
}

// A policy that does not use source age (none is configured that way today) is judged on reception age alone.
func TestPolicyWithoutSourceAgeUsesReceptionOnly(t *testing.T) {
	p := PolicyFor(TextKey("METAR", "KSEA"))
	p.sourceAge = false
	e := metarAt(0, t0, 50*time.Minute, true)
	age, basis := e.EffectiveAge(p, 1)
	if basis != AgeBasisReception || age != time.Second {
		t.Fatalf("age=%v basis=%s", age, basis)
	}
}

func TestComputeStatsCountsAgeBasis(t *testing.T) {
	snap := map[Key]Entry{
		TextKey("METAR", "KSEA"): metarAt(0, t0, 5*time.Minute, true),
		TextKey("METAR", "KPDX"): {Key: TextKey("METAR", "KPDX"), ReceivedAtMonotonic: 0},
	}
	st := ComputeStats(snap, 1)
	if st.ByAgeBasis[AgeBasisSource] != 1 || st.ByAgeBasis[AgeBasisReception] != 1 {
		t.Fatalf("ByAgeBasis = %v", st.ByAgeBasis)
	}
}
