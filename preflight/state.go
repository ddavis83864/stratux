// Package preflight implements a simplified, supplemental preflight
// checklist built on top of the existing readiness health model. It does
// not measure hardware or duplicate any component-health logic itself -
// see BuildReport, which consumes an already-built readiness.HealthReport
// (plus a small amount of additional context: the active calibration
// profile, manual acknowledgements, and boot/session timing) and applies
// an explicit, documented decision policy on top of it.
//
// Like readiness/recording/ota/calprofile, this package is pure and
// hardware/cgo-free: every function here takes already-gathered values and
// returns a derived judgment, so all of it is exercised directly by go
// test. The thin glue that gathers those values (reading globalHealth,
// the active calprofile.Profile, wall/monotonic clocks) lives in
// main/preflightapi.go.
//
// This is explicitly a supplemental, non-certified preflight aid - see
// docs/preflight-readiness.md. It never controls the aircraft or modifies
// ADS-B/GDL90 operation, and a failure inside this package must never
// interrupt any other Stratux subsystem (see BuildReport's recover-based
// failure isolation in main/preflightapi.go).
package preflight

// State is the health/readiness classification used throughout the
// preflight report - deliberately a distinct type from
// readiness.ComponentState, because the two vocabularies answer different
// questions. readiness.ComponentState says "is this component working
// right now" (five values: READY/DEGRADED/NOT_READY/NOT_INSTALLED/
// UNKNOWN). State says "is this item ready for the pilot to rely on
// before flight," which needs two additional ideas readiness has no use
// for: VERIFY (a human must look at this - software cannot prove it) and
// the same NOT_APPLICABLE/UNKNOWN pair readiness already has, kept as
// separate constants here so this package never has to import readiness
// just to compare states.
//
// The zero value (empty string) is deliberately invalid, matching
// readiness.ComponentState's convention - see Valid().
type State string

const (
	// StateReady: all required automated checks pass and all required
	// manual checks are acknowledged. Dashboard: green.
	StateReady State = "READY"

	// StateCaution: flight-support functions remain available, but an
	// optional/degraded item, an environmental condition, or an
	// incomplete (but not required-and-overdue) manual check needs
	// attention. Dashboard: amber.
	StateCaution State = "CAUTION"

	// StateNotReady: a required technical function is unavailable or
	// unsafe for the intended operation. Reserve this for a confirmed,
	// blocking problem - see the Severity/Blocking documentation on
	// CheckResult and the rollup rules in decision.go. Dashboard: red.
	StateNotReady State = "NOT_READY"

	// StateVerify: this item requires direct human observation - software
	// has no way to confirm it (e.g. "physical fans spinning"). Distinct
	// from an unacknowledged manual check only in framing: VERIFY is a
	// per-item state a check can report; whether an unacknowledged VERIFY
	// item is itself a caution is a decision-policy question, not a
	// property of the state value.
	StateVerify State = "VERIFY"

	// StateNotApplicable: the underlying feature is intentionally
	// disabled or not installed - not a failure, and (per the decision
	// policy) never contributes to the overall rollup, matching
	// readiness.ComponentState's StateNotInstalled convention.
	StateNotApplicable State = "NOT_APPLICABLE"

	// StateUnknown: insufficient trustworthy information exists to judge
	// this item yet (e.g. still within a startup grace period with no
	// evidence either way). Never silently treated as StateReady - see
	// decision.go's rollup, which folds an UNKNOWN required check into
	// CAUTION rather than dropping it.
	StateUnknown State = "UNKNOWN"
)

// Valid reports whether s is one of the six defined preflight states.
func (s State) Valid() bool {
	switch s {
	case StateReady, StateCaution, StateNotReady, StateVerify, StateNotApplicable, StateUnknown:
		return true
	}
	return false
}

// OverallValid reports whether s is one of the three states an *overall*
// report is allowed to carry. Individual checks may use any of the six
// State values; Report.Overall must always be one of these three - see
// BuildReport's rollup, which never returns anything else.
func (s State) OverallValid() bool {
	switch s {
	case StateReady, StateCaution, StateNotReady:
		return true
	}
	return false
}

// Severity classifies how a non-ready CheckResult should affect the
// overall rollup - a separate axis from State, because the same State can
// mean different things for different checks (e.g. an UNKNOWN GPS fix
// during the acquisition grace period is not yet actionable, while an
// UNKNOWN storage check because statfs failed is worth a caution).
type Severity string

const (
	// SeverityBlocking: if this check's State is StateNotReady, the
	// overall report must be StateNotReady. See decision.go.
	SeverityBlocking Severity = "BLOCKING"
	// SeverityCaution: if this check's State is anything other than
	// StateReady or StateNotApplicable, it pulls an otherwise-ready
	// overall report to at least StateCaution, but never to
	// StateNotReady on its own.
	SeverityCaution Severity = "CAUTION"
	// SeverityInfo: never affects the overall rollup by itself -
	// informational only (e.g. FIS-B tower count, message rates).
	SeverityInfo Severity = "INFO"
)
