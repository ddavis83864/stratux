package power

import "github.com/stratux/stratux/readiness"

// Monitor turns a stream of raw readiness.ThrottleStatus samples into a
// debounced Health a dashboard or preflight check can poll without
// flapping between severities on a single noisy reading. It has no timer
// or goroutine of its own - every transition happens synchronously inside
// Observe, driven entirely by whatever calls it (main/'s periodic health
// sampler in production, a fixed sequence of fake samples in tests) - so
// its behavior is fully deterministic and requires no real hardware or
// wall-clock delay to test.
type Monitor struct {
	// RequiredConsecutive is how many consecutive samples must agree on
	// the same Severity/Reason before Monitor's reported Health changes
	// to match them. Clamped to at least 1 by NewMonitor.
	RequiredConsecutive int

	current   Health
	candidate Health
	streak    int
}

// NewMonitor returns a Monitor that reports SeverityOK ("no samples yet")
// until it has seen requiredConsecutive agreeing samples.
func NewMonitor(requiredConsecutive int) *Monitor {
	if requiredConsecutive < 1 {
		requiredConsecutive = 1
	}
	return &Monitor{
		RequiredConsecutive: requiredConsecutive,
		current:             Health{Severity: SeverityOK, Reason: "no samples yet", Notes: defaultNotes()},
	}
}

// Observe evaluates one new raw sample and returns the Monitor's current
// (debounced) Health - which may or may not have just changed to match
// this sample, depending on RequiredConsecutive.
func (m *Monitor) Observe(t readiness.ThrottleStatus) Health {
	h := Evaluate(t)
	if h.Severity == m.candidate.Severity && h.Reason == m.candidate.Reason {
		m.streak++
	} else {
		m.candidate = h
		m.streak = 1
	}
	if m.streak >= m.RequiredConsecutive {
		m.current = h
	}
	return m.current
}

// Current returns the Monitor's last-reported (debounced) Health without
// taking a new sample.
func (m *Monitor) Current() Health {
	return m.current
}
