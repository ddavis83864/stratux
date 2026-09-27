package epaper

import (
	"testing"
	"time"
)

func TestDecideDashboardFirstIsFull(t *testing.T) {
	k, st := DecideDashboard(DashPolicyState{}, "a", false, 15*time.Second, 20, t0)
	if k != RefreshFull || !st.Has {
		t.Fatalf("first = %v", k)
	}
}

func TestDecideDashboardChangeRulesAndCoalescing(t *testing.T) {
	iv := 30 * time.Second
	_, st := DecideDashboard(DashPolicyState{}, "a", false, iv, 20, t0)
	// Unchanged: nothing, however often we are asked.
	for i := 1; i < 100; i++ {
		if k, _ := DecideDashboard(st, "a", false, iv, 20, t0.Add(time.Duration(i)*time.Second)); k != RefreshNone {
			t.Fatalf("unchanged state refreshed at +%ds", i)
		}
	}
	// Changed but inside the minimum interval: held.
	if k, _ := DecideDashboard(st, "b", false, iv, 20, t0.Add(10*time.Second)); k != RefreshNone {
		t.Errorf("change inside the interval refreshed")
	}
	// Flapping inside the interval coalesces: at +30 s only the latest
	// state matters and exactly one refresh is due.
	k, st2 := DecideDashboard(st, "c", false, iv, 20, t0.Add(30*time.Second))
	if k != RefreshPartial || st2.LastKey != "c" {
		t.Errorf("at interval: %v %q", k, st2.LastKey)
	}
	// Right after, the same state needs nothing.
	if k, _ := DecideDashboard(st2, "c", false, iv, 20, t0.Add(31*time.Second)); k != RefreshNone {
		t.Error("refreshed again for the same state")
	}
}

func TestDecideDashboardHeartbeat(t *testing.T) {
	_, st := DecideDashboard(DashPolicyState{}, "a", false, 15*time.Second, 20, t0)
	if k, _ := DecideDashboard(st, "a", false, 15*time.Second, 20, t0.Add(DashboardHeartbeat-time.Second)); k != RefreshNone {
		t.Error("heartbeat too early")
	}
	k, st2 := DecideDashboard(st, "a", false, 15*time.Second, 20, t0.Add(DashboardHeartbeat))
	if k != RefreshPartial {
		t.Errorf("heartbeat = %v", k)
	}
	if k, _ := DecideDashboard(st2, "a", false, 15*time.Second, 20, t0.Add(DashboardHeartbeat+time.Minute)); k != RefreshNone {
		t.Error("heartbeat repeated too soon")
	}
}

func TestDecideDashboardFullRefreshCadence(t *testing.T) {
	iv := 15 * time.Second
	_, st := DecideDashboard(DashPolicyState{}, "k0", false, iv, 3, t0)
	now := t0
	var kinds []RefreshKind
	for i := 1; i <= 8; i++ {
		now = now.Add(200 * time.Second) // (also clear of the storm guard)
		k, n := DecideDashboard(st, "k"+string(rune('0'+i)), false, iv, 3, now)
		st = n
		kinds = append(kinds, k)
	}
	want := []RefreshKind{RefreshPartial, RefreshPartial, RefreshPartial, RefreshFull, RefreshPartial, RefreshPartial, RefreshPartial, RefreshFull}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}

func TestDecideDashboardMaxFullAgeAndInversion(t *testing.T) {
	iv := 30 * time.Second
	// A screen whose last full refresh is older than the maximum age gets a
	// full one at its next refresh, however few partials have accumulated.
	old := DashPolicyState{Has: true, LastKey: "a", LastRefreshAt: t0, LastFullAt: t0.Add(-DashboardFullMaxAge - time.Minute)}
	if k, _ := DecideDashboard(old, "a", false, iv, 200, t0.Add(DashboardHeartbeat)); k != RefreshFull {
		t.Errorf("old full refresh: %v, want FULL", k)
	}
	// Heartbeats alone (all partial) still reach a full refresh through the
	// partial cap: 5 partials, then FULL.
	_, st := DecideDashboard(DashPolicyState{}, "a", false, iv, 200, t0)
	now := t0
	var kinds []RefreshKind
	for i := 0; i < 6; i++ {
		now = now.Add(DashboardHeartbeat)
		k, n := DecideDashboard(st, "a", false, iv, 200, now)
		st = n
		kinds = append(kinds, k)
	}
	want := []RefreshKind{RefreshPartial, RefreshPartial, RefreshPartial, RefreshPartial, RefreshPartial, RefreshFull}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("heartbeat kinds = %v, want %v", kinds, want)
		}
	}
	// A polarity change of the big banner is always a full refresh.
	k, st2 := DecideDashboard(st, "fault", true, iv, 200, now.Add(time.Minute))
	if k != RefreshFull {
		t.Errorf("banner inversion = %v, want FULL", k)
	}
	k, _ = DecideDashboard(st2, "fault2", true, iv, 200, now.Add(2*time.Minute))
	if k != RefreshPartial {
		t.Errorf("change without inversion flip = %v, want PARTIAL", k)
	}
}

// A displayed age must not cause a refresh per minute: over a long quiet
// stretch the material state only moves when an age crosses a bucket.
func TestAgeBucketsKeepTheScreenStable(t *testing.T) {
	tr, _ := newRef(t)
	// weather arrives once, then nothing for 40 minutes; towers gone
	step(tr, t0.Add(5*time.Second), func(s *Sample) { s.Status.Products = ProductTotals{METAR: 1} })
	keys := map[string]int{}
	var prev string
	changes := 0
	for m := 0; m <= 40; m++ {
		at := t0.Add(time.Duration(m)*time.Minute + 10*time.Second)
		step(tr, at, func(s *Sample) { s.Status.Products = ProductTotals{METAR: 1} })
		d := tr.Derive(at, t0)
		k := d.MaterialKey()
		keys[k]++
		if k != prev {
			changes++
			prev = k
		}
	}
	// current(<2, <5) -> aging(<15) -> stale(<30, <1 HR): a handful of
	// transitions in 40 minutes, not 40.
	if changes > 7 {
		t.Errorf("%d material changes in 40 quiet minutes", changes)
	}
}

func TestDecideDashboardNotBeforeHoldsRetries(t *testing.T) {
	st := DashPolicyState{NotBefore: t0.Add(15 * time.Second)}
	if k, _ := DecideDashboard(st, "a", false, 15*time.Second, 20, t0.Add(5*time.Second)); k != RefreshNone {
		t.Errorf("retry before NotBefore = %v", k)
	}
	if k, _ := DecideDashboard(st, "a", false, 15*time.Second, 20, t0.Add(15*time.Second)); k != RefreshFull {
		t.Errorf("retry at NotBefore = %v, want the owed first FULL frame", k)
	}
}

func TestDecideDashboardPartialCapAndIntervalFloor(t *testing.T) {
	// A configured cadence of 20 partials is capped at DashboardMaxPartials.
	_, st := DecideDashboard(DashPolicyState{}, "k0", false, 15*time.Second, 20, t0)
	now := t0
	partials := 0
	for i := 1; i <= 12; i++ {
		now = now.Add(200 * time.Second)
		k, n := DecideDashboard(st, "k"+string(rune('a'+i)), false, 15*time.Second, 20, now)
		st = n
		if k == RefreshFull {
			break
		}
		partials++
	}
	if partials != DashboardMaxPartials {
		t.Errorf("%d partials before a full refresh, want %d", partials, DashboardMaxPartials)
	}
	// A configured 5 s interval is floored to 30 s.
	_, st = DecideDashboard(DashPolicyState{}, "a", false, 5*time.Second, 20, t0)
	if k, _ := DecideDashboard(st, "b", false, 5*time.Second, 20, t0.Add(20*time.Second)); k != RefreshNone {
		t.Errorf("refreshed after 20 s with a 5 s configured interval")
	}
	if k, _ := DecideDashboard(st, "b", false, 5*time.Second, 20, t0.Add(30*time.Second)); k == RefreshNone {
		t.Errorf("no refresh at the 30 s floor")
	}
}

func TestDecideDashboardStormGuardSpacesRefreshesAtVendorInterval(t *testing.T) {
	// An input that flaps constantly: a change is always pending.
	_, st := DecideDashboard(DashPolicyState{}, "s0", false, 30*time.Second, 20, t0)
	now := t0
	var times []time.Time
	for i := 1; now.Sub(t0) < 30*time.Minute; i++ {
		now = now.Add(5 * time.Second)
		k, n := DecideDashboard(st, "s"+string(rune('a'+i%26))+string(rune('a'+(i/26)%26)), false, 30*time.Second, 20, now)
		st = n
		if k != RefreshNone {
			times = append(times, now)
		}
	}
	// Once 6 refreshes have happened inside the window, gaps are >= 180 s.
	for i := DashboardStormCount - 1; i < len(times); i++ {
		if gap := times[i].Sub(times[i-1]); gap < DashboardStormSpacing {
			t.Fatalf("refresh %d came %v after the previous, want >= %v under the storm guard", i, gap, DashboardStormSpacing)
		}
	}
	if len(times) > 6+(30*60-6*30)/180+2 {
		t.Errorf("%d refreshes in 30 minutes of constant flapping", len(times))
	}
}

func TestDecideDashboardStormGuardReleasesAfterQuiet(t *testing.T) {
	_, st := DecideDashboard(DashPolicyState{}, "s0", false, 30*time.Second, 20, t0)
	now := t0
	for i := 1; i <= 12; i++ { // engage it (10 refreshes inside 10 minutes)
		now = now.Add(35 * time.Second)
		_, st = DecideDashboard(st, "s"+string(rune('a'+i)), false, 30*time.Second, 20, now)
	}
	// Quiet for 15 minutes: the window drains and the guard releases.
	now = now.Add(15 * time.Minute)
	k, st := DecideDashboard(st, "changed", false, 30*time.Second, 20, now)
	if k == RefreshNone {
		t.Fatalf("a change after a long quiet spell was held back")
	}
	if st.Storm {
		t.Error("storm guard still engaged after the window drained")
	}
	if k, _ := DecideDashboard(st, "changed-again", false, 30*time.Second, 20, now.Add(31*time.Second)); k == RefreshNone {
		t.Error("normal 30 s spacing not restored")
	}
}

func TestDecideDashboardUrgentTransitionsSkipTheStormGuard(t *testing.T) {
	// Engage the storm guard with a flapping input.
	_, st := DecideDashboard(DashPolicyState{}, "s0", false, 30*time.Second, 20, t0)
	now := t0
	for i := 1; i <= 12; i++ {
		now = now.Add(35 * time.Second)
		_, st = DecideDashboard(st, "s"+string(rune('a'+i)), false, 30*time.Second, 20, now)
	}
	if !st.Storm {
		t.Fatal("setup: storm guard should be engaged")
	}
	// An ordinary change is held...
	if k, _ := DecideDashboard(st, "ordinary", false, 30*time.Second, 20, now.Add(40*time.Second)); k != RefreshNone {
		t.Errorf("ordinary change under the guard = %v", k)
	}
	// ...but a fault / no-data banner appearing is not, and neither is it
	// clearing again (the outage and its recovery both show promptly).
	now = now.Add(40 * time.Second)
	k, st2 := DecideDashboard(st, "nodata", true, 30*time.Second, 20, now)
	if k != RefreshFull {
		t.Fatalf("entering an inverted banner under the guard = %v, want an immediate FULL", k)
	}
	now = now.Add(31 * time.Second)
	k, st3 := DecideDashboard(st2, "recovered", false, 30*time.Second, 20, now)
	if k != RefreshFull {
		t.Fatalf("clearing the inverted banner under the guard = %v, want an immediate FULL", k)
	}
	// A flapping fault cannot use this to bypass the guard indefinitely:
	// after DashboardUrgentMax flips in the window, they are held.
	held := false
	for i := 0; i < 6; i++ {
		now = now.Add(31 * time.Second)
		k, st3 = DecideDashboard(st3, "flip"+string(rune('a'+i)), i%2 == 0, 30*time.Second, 20, now)
		if k == RefreshNone {
			held = true
			break
		}
	}
	if !held {
		t.Error("an endlessly flapping fault bypassed the storm guard every time")
	}
	// The 30 s floor still applies to an urgent flip.
	_, base := DecideDashboard(DashPolicyState{}, "a", false, 30*time.Second, 20, t0)
	if k, _ := DecideDashboard(base, "b", true, 30*time.Second, 20, t0.Add(10*time.Second)); k != RefreshNone {
		t.Errorf("urgent flip inside the 30 s floor = %v", k)
	}
}

// A normal boot (a handful of refreshes as GPS, radios and clients come up)
// must not engage the guard: the next real change has to show at the floor
// interval, not minutes later.
func TestDecideDashboardNormalStartupDoesNotEngageTheStormGuard(t *testing.T) {
	iv := 30 * time.Second
	_, st := DecideDashboard(DashPolicyState{}, "boot0", false, iv, 20, t0)
	now := t0
	for i := 1; i <= 6; i++ { // six more refreshes over the first ~4 minutes
		now = now.Add(40 * time.Second)
		_, st = DecideDashboard(st, "boot"+string(rune('0'+i)), false, iv, 20, now)
	}
	if st.Storm {
		t.Fatal("normal startup engaged the storm guard")
	}
	now = now.Add(31 * time.Second)
	if k, _ := DecideDashboard(st, "client-joined", false, iv, 20, now); k == RefreshNone {
		t.Error("a change 31 s after startup settled was held back")
	}
}
