/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	Modifications (c) 2016 AvSquirrel (https://github.com/AvSquirrel)
	monotonic.go: Create monotonic clock using time.Timer - necessary because of real time clock changes on RPi.
*/

package main

import (
	"sync/atomic"
	"time"

	humanize "github.com/dustin/go-humanize"
)

// Timer (since start).
//
// Concurrency: stratuxClock is a single package-level instance, assigned
// exactly once in main() before any goroutine that could read it starts
// (see NewMonotonic - the Watcher goroutine itself is started before
// NewMonotonic returns the pointer, so a caller can never observe a
// stratuxClock with a nil ticker or an unstarted Watcher). After that one
// assignment, stratuxClock itself is never reassigned - only Watcher's
// own per-tick updates to the clock's *contents* happen afterward, and
// those contents are read continuously from effectively every goroutine
// in this daemon (traffic processing, GPS, network heartbeats, datalog,
// the HTTP handlers, ...). This is why Time/Milliseconds/RealTime are
// atomics rather than plain fields with a mutex: a mutex here would put
// a lock acquisition on this project's single hottest read path, for
// state that changes in a single, simple way (Watcher's own 10ms tick)
// - an atomic load/store is both correct and cheaper. See Time(),
// Milliseconds(), and Watcher's own doc comments for the exact contract.
type monotonic struct {
	milliseconds atomic.Uint64
	t            atomic.Pointer[time.Time]
	ticker       *time.Ticker
	realTimeSet  atomic.Bool
	realTime     atomic.Pointer[time.Time]
}

// Watcher is stratuxClock's one and only writer for Milliseconds/Time/
// RealTime - started once from NewMonotonic and never stopped for the
// life of the process (matching this type's original design; this fix
// changes only how the update is published, not when or how often).
// Every field it touches is an atomic - readers elsewhere never
// observe a torn/partial update, and never need a lock of their own.
func (m *monotonic) Watcher() {
	for {
		<-m.ticker.C
		m.milliseconds.Add(10)
		next := m.t.Load().Add(10 * time.Millisecond)
		m.t.Store(&next)
		if m.realTimeSet.Load() {
			nextReal := m.realTime.Load().Add(10 * time.Millisecond)
			m.realTime.Store(&nextReal)
		}
	}
}

// Time returns the current monotonic-domain time. Safe to call from any
// goroutine, including concurrently with Watcher's own updates - see
// this type's doc comment.
func (m *monotonic) Time() time.Time { return *m.t.Load() }

// Milliseconds returns the current monotonic-domain millisecond counter.
// Safe to call from any goroutine, including concurrently with Watcher's
// own updates - see this type's doc comment.
func (m *monotonic) Milliseconds() uint64 { return m.milliseconds.Load() }

func (m *monotonic) Since(t time.Time) time.Duration {
	return m.Time().Sub(t)
}

func (m *monotonic) HumanizeTime(t time.Time) string {
	return humanize.RelTime(t, m.Time(), "ago", "from now")
}

func (m *monotonic) Unix() int64 {
	return int64(m.Since(time.Time{}).Seconds())
}

func (m *monotonic) HasRealTimeReference() bool {
	return m.realTimeSet.Load()
}

// SetRealTimeReference records t as the wall-clock reference RealTime
// advances from, exactly once - a second call is a silent no-op, exactly
// as the original "Only allow the real clock to be set once" comment
// documented. CompareAndSwap makes the "only once" guarantee itself
// atomic (the original plain `if !m.realTimeSet { ... }` had its own,
// separate, narrower race between the check and the two writes that
// follow it - fixed here as a direct consequence of making this safe for
// concurrent use at all, not a new behavior).
func (m *monotonic) SetRealTimeReference(t time.Time) {
	if m.realTimeSet.CompareAndSwap(false, true) {
		m.realTime.Store(&t)
	}
}

// RealTime returns the current wall-clock reference, advanced by
// Watcher() the same way Time is. The zero time.Time{} before
// SetRealTimeReference has ever been called - matching the original
// field's own zero-value behavior.
func (m *monotonic) RealTime() time.Time { return *m.realTime.Load() }

// NewMonotonic returns a monotonic clock already ticking - the Watcher
// goroutine is started before this function returns, so a caller can
// never observe a partially-initialized clock (no nil atomic.Pointer
// contents: t/realTime are seeded with the zero time.Time before
// Watcher starts, exactly matching the original zero-value Time/RealTime
// struct fields).
func NewMonotonic() *monotonic {
	m := &monotonic{ticker: time.NewTicker(10 * time.Millisecond)}
	zero := time.Time{}
	m.t.Store(&zero)
	m.realTime.Store(&zero)
	go m.Watcher()
	return m
}
