// Package readiness implements the unified component health model, trusted
// GNSS time synchronization, and persistent-storage certification used by
// the Sentry-class readiness dashboard and the /getHealth API.
//
// Like sdrassign, this package is pure and hardware/cgo-free: every
// function here takes already-gathered signals (a statfs result, a parsed
// NMEA sentence, a decoder's live counters) and returns a derived health
// judgment. Nothing in this package touches a device file, spawns a
// process, or depends on CGO, so all of it is exercised directly by go
// test without a Raspberry Pi, an SDR, or a GPS receiver attached. The
// thin glue that gathers real signals (statfs syscalls, NMEA parsing,
// decoder process state) lives in main/ and calls into this package.
package readiness

// ComponentState is the unified health classification for every monitored
// subsystem exposed via the health API and dashboard.
//
// Every component in a HealthReport carries exactly one ComponentState.
// The zero value of the type (empty string) is deliberately not one of the
// five valid states - a builder that forgets to set a component's state
// produces an invalid value that fails Valid(), rather than silently
// reading as StateUnknown. Use StateUnknown explicitly when health has
// genuinely not yet been determined.
type ComponentState string

const (
	// StateReady means the component has affirmative evidence that it is
	// working correctly right now. Dashboard: green.
	StateReady ComponentState = "READY"

	// StateDegraded means the component is at least partially working but
	// something is incomplete, stale, or otherwise worth the operator's
	// attention. Dashboard: amber.
	StateDegraded ComponentState = "DEGRADED"

	// StateNotReady means the component is expected to be available but is
	// confirmed failed, missing, or unusable right now. Dashboard: red.
	//
	// Reserve this for a confirmed problem. The mere absence of positive
	// evidence - no aircraft currently in range, no FIS-B tower currently
	// received - is not a failure; it is StateReady (radio healthy, simply
	// nothing to show right now) or at most StateDegraded, never
	// StateNotReady, and must say so in Reason.
	StateNotReady ComponentState = "NOT_READY"

	// StateNotInstalled means the underlying hardware or feature is not
	// present by design in this configuration - e.g. no AHRS board
	// installed yet, or a receiver band the operator has intentionally
	// disabled. This is not a failure. Dashboard: gray.
	StateNotInstalled ComponentState = "NOT_INSTALLED"

	// StateUnknown means health has not yet been determined, e.g. no
	// evidence has been collected since startup. Dashboard: gray, same
	// rendering as StateNotInstalled, but callers should resolve to a real
	// state as soon as evidence exists rather than leaving this in place.
	StateUnknown ComponentState = "UNKNOWN"
)

// Valid reports whether s is one of the five defined ComponentStates.
func (s ComponentState) Valid() bool {
	switch s {
	case StateReady, StateDegraded, StateNotReady, StateNotInstalled, StateUnknown:
		return true
	}
	return false
}

// Color returns the dashboard color rule for s: "green", "amber", "red", or
// "gray". An invalid ComponentState returns "gray" - the safe, attention-
// grabbing-but-not-alarming default - rather than panicking or guessing.
func (s ComponentState) Color() string {
	switch s {
	case StateReady:
		return "green"
	case StateDegraded:
		return "amber"
	case StateNotReady:
		return "red"
	case StateNotInstalled, StateUnknown:
		return "gray"
	default:
		return "gray"
	}
}

// Worse reports whether s represents a strictly less-ready condition than
// other, using the ordering an overall/aggregate readiness rollup needs:
// NOT_READY worse than DEGRADED worse than UNKNOWN/NOT_INSTALLED worse than
// READY. UNKNOWN and NOT_INSTALLED are treated as equally
// neither-good-nor-bad for aggregation purposes: neither should pull an
// otherwise-healthy system down to amber or red on its own.
func (s ComponentState) Worse(other ComponentState) bool {
	return rank(s) > rank(other)
}

func rank(s ComponentState) int {
	switch s {
	case StateNotReady:
		return 3
	case StateDegraded:
		return 2
	case StateUnknown, StateNotInstalled:
		return 1
	case StateReady:
		return 0
	default:
		// An invalid/unrecognized state is treated as worse than any
		// defined state so it cannot be silently outranked and hidden by
		// an aggregate rollup.
		return 4
	}
}

// Rollup reduces a set of component states to one overall ComponentState,
// per the dashboard's color rules: the overall state is exactly the worst
// of its components, except that StateNotInstalled/StateUnknown components
// are excluded unless *every* component is one of those two - an all-
// not-installed/unknown system reports StateUnknown overall rather than
// pretending it is StateReady on the strength of having nothing to check.
//
// An empty input reports StateUnknown.
func Rollup(states ...ComponentState) ComponentState {
	if len(states) == 0 {
		return StateUnknown
	}
	worst := ComponentState("")
	sawInformative := false
	for _, s := range states {
		if s == StateNotInstalled || s == StateUnknown {
			continue
		}
		sawInformative = true
		if worst == "" || s.Worse(worst) {
			worst = s
		}
	}
	if !sawInformative {
		return StateUnknown
	}
	return worst
}
