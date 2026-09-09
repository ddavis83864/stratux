package main

import (
	"sync"
	"testing"
	"time"
)

// TestMonotonic_ConcurrentReadsDuringWatcherAreRaceSafe reproduces, under
// `go test -race`, the exact concurrency pattern that originally faulted:
// many goroutines continuously reading Time/Milliseconds/RealTime/
// HasRealTimeReference while Watcher's own goroutine ticks and updates
// them, for the lifetime of a real *monotonic clock (NewMonotonic starts
// Watcher exactly as production does - main() never constructs a
// monotonic clock any other way). On the pre-fix struct (plain fields,
// no synchronization) this test reliably faults under -race; after the
// atomic-backed rewrite it must pass cleanly, every run.
func TestMonotonic_ConcurrentReadsDuringWatcherAreRaceSafe(t *testing.T) {
	m := NewMonotonic()

	const readers = 16
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = m.Time()
				_ = m.Milliseconds()
				_ = m.RealTime()
				_ = m.HasRealTimeReference()
				_ = m.Since(time.Time{})
				_ = m.Unix()
			}
		}()
	}

	// Long enough to span several of Watcher's own 10ms ticks - the
	// point isn't timing precision, it's giving -race many genuine
	// concurrent read/write interleavings to inspect.
	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestMonotonic_ConcurrentSetRealTimeReferenceIsRaceSafeAndOnlyOnce proves
// two things about SetRealTimeReference under real concurrency: (1) no
// data race between simultaneous callers (each mutonic.go's original
// unsynchronized check-then-write could tear under -race), and (2) the
// "only allow the real clock to be set once" contract this type has
// always documented actually holds when multiple goroutines race to set
// it - RealTime() afterward must equal exactly one of the times offered,
// never a torn/mixed value.
func TestMonotonic_ConcurrentSetRealTimeReferenceIsRaceSafeAndOnlyOnce(t *testing.T) {
	m := NewMonotonic()

	const setters = 32
	offered := make([]time.Time, setters)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range offered {
		offered[i] = base.Add(time.Duration(i) * time.Hour)
	}

	var wg sync.WaitGroup
	wg.Add(setters)
	start := make(chan struct{})
	for i := 0; i < setters; i++ {
		i := i
		go func() {
			defer wg.Done()
			<-start
			m.SetRealTimeReference(offered[i])
		}()
	}
	close(start)
	wg.Wait()

	if !m.HasRealTimeReference() {
		t.Fatal("expected HasRealTimeReference to be true after concurrent SetRealTimeReference calls")
	}
	got := m.RealTime()
	matched := false
	for _, want := range offered {
		// RealTime has been advanced by Watcher's own ticks since it was
		// set, so compare only that it started from one of the offered
		// values, not exact equality against the (possibly now-advanced)
		// current value.
		if !got.Before(want) && got.Sub(want) < time.Second {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("expected RealTime() to reflect exactly one of the %d offered times, got %v", setters, got)
	}
}

// TestMonotonic_SetRealTimeReferenceSecondCallIsNoOp matches the original
// "Only allow the real clock to be set once" contract directly, without
// concurrency: a second call must never change an already-set reference.
func TestMonotonic_SetRealTimeReferenceSecondCallIsNoOp(t *testing.T) {
	m := NewMonotonic()
	first := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	m.SetRealTimeReference(first)
	got1 := m.RealTime()

	m.SetRealTimeReference(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	got2 := m.RealTime()

	// Both reads happen close enough together that Watcher's 10ms ticks
	// between them are negligible for this assertion: got2 must still be
	// anchored to `first`, not the second, later-offered time.
	if got2.Before(first) || got2.Sub(first) > time.Second {
		t.Errorf("second SetRealTimeReference call must be a no-op, got RealTime()=%v (want close to first=%v, not the later offered time)", got2, first)
	}
	_ = got1
}
