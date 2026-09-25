package fisbcache

import (
	"testing"
	"time"
)

func mustUTC(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestReconstructSourceTime_UntrustedClockNeverTrusted(t *testing.T) {
	ft := FISBTime{HasMonthDay: true, Month: 6, Day: 15, Hour: 12, Minute: 0}
	got := ReconstructSourceTime(ft, mustUTC("2026-06-15T12:00:00Z"), false)
	if got.Trusted {
		t.Fatal("expected an untrusted receive clock to never produce a trusted SourceTime")
	}
}

func TestReconstructSourceTime_ZeroReceiveTimeNeverTrusted(t *testing.T) {
	ft := FISBTime{Hour: 12, Minute: 0}
	got := ReconstructSourceTime(ft, time.Time{}, true)
	if got.Trusted {
		t.Fatal("expected a zero receive time to never produce a trusted SourceTime")
	}
}

func TestReconstructSourceTime_WithMonthDay(t *testing.T) {
	ft := FISBTime{HasMonthDay: true, Month: 6, Day: 15, Hour: 12, Minute: 30, Second: 0}
	receive := mustUTC("2026-06-15T12:31:00Z")
	got := ReconstructSourceTime(ft, receive, true)
	if !got.Trusted {
		t.Fatal("expected a trusted result")
	}
	want := mustUTC("2026-06-15T12:30:00Z")
	if !got.UTC.Equal(want) {
		t.Errorf("got %v, want %v", got.UTC, want)
	}
}

func TestReconstructSourceTime_WithoutMonthDay(t *testing.T) {
	ft := FISBTime{HasMonthDay: false, Hour: 12, Minute: 30, Second: 0}
	receive := mustUTC("2026-06-15T12:31:00Z")
	got := ReconstructSourceTime(ft, receive, true)
	if !got.Trusted {
		t.Fatal("expected a trusted result")
	}
	want := mustUTC("2026-06-15T12:30:00Z")
	if !got.UTC.Equal(want) {
		t.Errorf("got %v, want %v", got.UTC, want)
	}
}

// TestReconstructSourceTime_YearRollback proves the New Year's boundary
// case explicitly: a broadcast at Dec 31 23:59 received just after
// midnight Jan 1 must reconstruct to the PRIOR year, not the current one.
func TestReconstructSourceTime_YearRollback(t *testing.T) {
	ft := FISBTime{HasMonthDay: true, Month: 12, Day: 31, Hour: 23, Minute: 59, Second: 30}
	receive := mustUTC("2026-01-01T00:00:10Z")
	got := ReconstructSourceTime(ft, receive, true)
	if !got.Trusted {
		t.Fatal("expected a trusted result across the year boundary")
	}
	want := mustUTC("2025-12-31T23:59:30Z")
	if !got.UTC.Equal(want) {
		t.Errorf("got %v, want %v", got.UTC, want)
	}
}

// TestReconstructSourceTime_DayRollback is the no-month-day equivalent:
// a broadcast at 23:59 received just after midnight must reconstruct to
// the PRIOR day, not the current one.
func TestReconstructSourceTime_DayRollback(t *testing.T) {
	ft := FISBTime{HasMonthDay: false, Hour: 23, Minute: 59, Second: 30}
	receive := mustUTC("2026-06-16T00:00:10Z")
	got := ReconstructSourceTime(ft, receive, true)
	if !got.Trusted {
		t.Fatal("expected a trusted result across the day boundary")
	}
	want := mustUTC("2026-06-15T23:59:30Z")
	if !got.UTC.Equal(want) {
		t.Errorf("got %v, want %v", got.UTC, want)
	}
}

func TestReconstructSourceTime_ImplausiblyFarPastRejected(t *testing.T) {
	ft := FISBTime{HasMonthDay: true, Month: 1, Day: 1, Hour: 0, Minute: 0}
	receive := mustUTC("2026-06-15T12:00:00Z")
	got := ReconstructSourceTime(ft, receive, true)
	if got.Trusted {
		t.Fatal("expected a source time many months in the past to be rejected as untrustworthy")
	}
}

func TestReconstructSourceTime_InvalidFieldsRejected(t *testing.T) {
	cases := []FISBTime{
		{Hour: 24, Minute: 0},
		{Hour: 0, Minute: 60},
		{Hour: 0, Minute: 0, Second: 60},
		{HasMonthDay: true, Month: 13, Day: 1},
		{HasMonthDay: true, Month: 1, Day: 32},
		{HasMonthDay: true, Month: 0, Day: 1},
	}
	receive := mustUTC("2026-06-15T12:00:00Z")
	for _, ft := range cases {
		if got := ReconstructSourceTime(ft, receive, true); got.Trusted {
			t.Errorf("FISBTime %+v: expected rejection, got trusted %v", ft, got.UTC)
		}
	}
}
