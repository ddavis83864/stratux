/*
Package power models what this project can honestly know about the
device's power state on typical Stratux hardware, and implements a
manual, two-step controlled-shutdown flow.

Hardware reality this package is built around: a Raspberry Pi running
from an ordinary USB power bank or in-flight USB source. That hardware
gives the Pi's own firmware exactly one signal - the get_throttled
register (wrapped by readiness.ThrottleStatus) - and nothing else. There
is no battery gauge, no "minutes remaining" estimate, and no
advance-warning signal before power actually disappears.

Explicit non-goals (deliberately not implemented, and not planned for
this package):

  - Battery percentage or state-of-charge estimation. Nothing on this
    hardware path reports one.
  - Automatic/unattended shutdown triggered by a power signal. Every
    shutdown this package can perform requires an explicit, two-step
    human confirmation - see Manager in shutdown.go.
  - GPIO reservation for a power controller.
  - UPS HAT / smart-battery integration of any kind.

A future revision could add real capability here (e.g. a HAT that reports
true state-of-charge) without breaking anything in this file: Health's
HasTrustedBatterySignal/HasRuntimeEstimate fields exist precisely so a
caller can tell "no such signal exists" (today, always false) apart from
"a signal exists and reports normal," which is a distinct, stronger claim
this package must never make on the hardware it is confirmed to run on.
*/
package power

import "github.com/stratux/stratux/readiness"

// Severity is this package's own, deliberately small classification -
// distinct from readiness.ComponentState so a caller integrating this
// into preflight or readiness can make its own translation decision (see
// docs/power-shutdown-resilience.md) rather than this package silently
// picking one for them.
type Severity string

const (
	SeverityOK       Severity = "ok"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Health is one point-in-time evaluation of the device's power state.
type Health struct {
	Throttle readiness.ThrottleStatus
	Severity Severity
	Reason   string

	// HasTrustedBatterySignal/HasRuntimeEstimate are always false on the
	// hardware this package is built for - see the package doc comment.
	// They exist as explicit fields (not simply omitted) so a dashboard
	// or diagnostic bundle can display "not available on this hardware"
	// rather than silently having nothing to show, and so a future
	// hardware integration has an honest place to report true.
	HasTrustedBatterySignal bool
	HasRuntimeEstimate      bool

	// Notes are fixed, non-alarming, capability-honesty text intended for
	// direct display next to the throttle reading - see defaultNotes.
	Notes []string
}

func defaultNotes() []string {
	return []string{
		"This device has no trusted battery, runtime, or advance power-failure signal - a standard USB power bank reports nothing to the Raspberry Pi.",
		"What is shown here is only what the Raspberry Pi firmware itself can measure: under-voltage and thermal/frequency throttling, now and since this boot.",
	}
}

// Evaluate classifies a single readiness.ThrottleStatus reading. It
// performs no I/O and keeps no state - see Monitor (monitor.go) for the
// debounced, stateful version a dashboard should actually poll.
func Evaluate(t readiness.ThrottleStatus) Health {
	h := Health{
		Throttle: t,
		Notes:    defaultNotes(),
	}
	switch {
	case t.UndervoltageNow:
		h.Severity = SeverityCritical
		h.Reason = "under-voltage detected right now"
	case t.ThrottledNow:
		h.Severity = SeverityWarning
		h.Reason = "CPU is currently thermally throttled"
	case t.UndervoltageOccurred:
		h.Severity = SeverityWarning
		h.Reason = "under-voltage occurred earlier this boot"
	case t.ThrottledOccurred:
		h.Severity = SeverityWarning
		h.Reason = "thermal throttling occurred earlier this boot"
	default:
		h.Severity = SeverityOK
		h.Reason = "no throttling or under-voltage detected"
	}
	return h
}
