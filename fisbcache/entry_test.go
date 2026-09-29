package fisbcache

import "testing"

func TestFreshness_Unsupported(t *testing.T) {
	e := Entry{Key: Key{Class: ClassUnsupported, Identity: "x"}}
	if got := Freshness(e, PolicyFor(e.Key), 100); got != FreshnessUnsupported {
		t.Errorf("got %q, want UNSUPPORTED", got)
	}
}

func TestFreshness_Transitions(t *testing.T) {
	key := TextKey(TextProductMETAR, "KSEA")
	policy := PolicyFor(key)

	cases := []struct {
		ageSeconds float64
		want       FreshnessState
	}{
		{0, FreshnessCachedFresh},
		{policy.FreshLimit.Seconds() - 1, FreshnessCachedFresh},
		{policy.FreshLimit.Seconds() + 1, FreshnessCachedAging},
		{policy.StaleLimit.Seconds() + 1, FreshnessStale},
		{policy.ExpireLimit.Seconds() + 1, FreshnessExpired},
	}
	for _, c := range cases {
		e := Entry{Key: key, ReceivedAtMonotonic: 1000}
		got := Freshness(e, policy, 1000+c.ageSeconds)
		if got != c.want {
			t.Errorf("age=%.0fs: got %q, want %q", c.ageSeconds, got, c.want)
		}
	}
}

func TestEntry_Age_NeverNegative(t *testing.T) {
	e := Entry{ReceivedAtMonotonic: 1000}
	if got := e.Age(500); got != 0 {
		t.Errorf("expected 0 for a now before ReceivedAtMonotonic (clock discontinuity), got %v", got)
	}
}
