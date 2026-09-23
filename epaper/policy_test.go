package epaper

import (
	"strconv"
	"testing"
	"time"
)

func TestDecide_FirstCallIsAlwaysFullRefresh(t *testing.T) {
	kind, state := Decide(PolicyState{}, Content{Version: "1.0"}, 10*time.Second, 20, time.Now())
	if kind != RefreshFull {
		t.Errorf("first Decide call = %v, want RefreshFull", kind)
	}
	if !state.HasRefreshedOnce {
		t.Errorf("state.HasRefreshedOnce should be true after the first call")
	}
}

func TestDecide_NoChangeMeansNoRefreshEvenAfterIntervalElapses(t *testing.T) {
	now := time.Now()
	_, state := Decide(PolicyState{}, Content{Version: "1.0"}, time.Second, 20, now)
	kind, _ := Decide(state, Content{Version: "1.0"}, time.Second, 20, now.Add(time.Hour))
	if kind != RefreshNone {
		t.Errorf("unchanged content should never trigger a refresh, got %v", kind)
	}
}

func TestDecide_ChangeBeforeIntervalElapsesIsDeferred(t *testing.T) {
	now := time.Now()
	_, state := Decide(PolicyState{}, Content{Version: "1.0"}, 10*time.Second, 20, now)
	kind, _ := Decide(state, Content{Version: "2.0"}, 10*time.Second, 20, now.Add(time.Second))
	if kind != RefreshNone {
		t.Errorf("a change within the refresh interval should be deferred, got %v", kind)
	}
}

func TestDecide_ChangeAfterIntervalElapsesIsPartial(t *testing.T) {
	now := time.Now()
	_, state := Decide(PolicyState{}, Content{Version: "1.0"}, 10*time.Second, 20, now)
	kind, _ := Decide(state, Content{Version: "2.0"}, 10*time.Second, 20, now.Add(11*time.Second))
	if kind != RefreshPartial {
		t.Errorf("a change after the interval elapsed should be RefreshPartial, got %v", kind)
	}
}

func TestDecide_ForcesFullRefreshAfterConfiguredPartialCount(t *testing.T) {
	now := time.Now()
	_, state := Decide(PolicyState{}, Content{Version: "0"}, time.Second, 2, now)
	for i := 1; i <= 2; i++ {
		now = now.Add(2 * time.Second)
		var kind RefreshKind
		kind, state = Decide(state, Content{Version: strconv.Itoa(i)}, time.Second, 2, now)
		if kind != RefreshPartial {
			t.Fatalf("iteration %d: expected RefreshPartial, got %v", i, kind)
		}
	}
	now = now.Add(2 * time.Second)
	kind, _ := Decide(state, Content{Version: "final"}, time.Second, 2, now)
	if kind != RefreshFull {
		t.Errorf("expected a forced full refresh after fullRefreshEvery partials, got %v", kind)
	}
}

func TestDecide_TimestampAloneNeverTriggersARefresh(t *testing.T) {
	now := time.Now()
	c := Content{Version: "1.0", SampledAt: now}
	_, state := Decide(PolicyState{}, c, time.Second, 20, now)
	c2 := Content{Version: "1.0", SampledAt: now.Add(time.Hour)} // only SampledAt differs
	kind, _ := Decide(state, c2, time.Second, 20, now.Add(2*time.Second))
	if kind != RefreshNone {
		t.Errorf("a SampledAt-only change must not trigger a refresh, got %v", kind)
	}
}

func TestShouldShowStale(t *testing.T) {
	if ShouldShowStale(StaleDataThresholdSeconds*time.Second - time.Second) {
		t.Errorf("data just under the threshold should not be stale")
	}
	if !ShouldShowStale(StaleDataThresholdSeconds*time.Second + time.Second) {
		t.Errorf("data just over the threshold should be stale")
	}
}
