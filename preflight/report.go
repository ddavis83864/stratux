package preflight

import (
	"strconv"
	"time"

	"github.com/stratux/stratux/readiness"
)

// SchemaVersion is bumped whenever CheckResult or Report gains, removes,
// or changes the meaning of a field in a way a consumer should notice.
const SchemaVersion = 1

// CheckResult is one line item in a preflight report - either an
// automated check derived from existing health data, or a manual check
// reflecting (or requesting) a human acknowledgement. The schema is
// deliberately identical in shape for both kinds; Source is what tells
// them apart.
type CheckResult struct {
	Component string   `json:"component"`
	CheckID   string   `json:"checkId"`
	Label     string   `json:"label"`
	State     State    `json:"state"`
	Severity  Severity `json:"severity"`
	// Blocking mirrors Severity == SeverityBlocking, exposed as its own
	// field so a consumer never has to know the Severity enum just to
	// answer "can this by itself force the overall report to NOT_READY."
	Blocking          bool       `json:"blocking"`
	Reason            string     `json:"reason"`
	RecommendedAction string     `json:"recommendedAction,omitempty"`
	Source            string     `json:"source"` // "automated" or "manual"
	ObservedAt        *time.Time `json:"observedAt,omitempty"`
	// ExpiresAt is set only for an acknowledged manual check - when the
	// acknowledgement stops counting as current. See ManualAckStore.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

func newCheck(component, id, label string, state State, sev Severity, reason string) CheckResult {
	return CheckResult{
		Component: component,
		CheckID:   id,
		Label:     label,
		State:     state,
		Severity:  sev,
		Blocking:  sev == SeverityBlocking,
		Reason:    reason,
		Source:    "automated",
	}
}

// ProfileSummary is the small, sanitized slice of the active calibration
// profile a preflight report needs - deliberately not the full
// calprofile.Profile (which this package never imports; see the package
// comment), and not even readiness.AHRSProfileInfo verbatim, so this
// package's public surface does not change if either of those does.
type ProfileSummary struct {
	Available        bool   `json:"available"`
	ID               string `json:"id,omitempty"`
	Name             string `json:"name,omitempty"`
	Kind             string `json:"kind,omitempty"`
	CalibrationValid bool   `json:"calibrationValid"`
}

// RecordingReadiness is the small recording-subsystem slice a preflight
// report needs - main/preflightapi.go supplies this from the existing
// recording subsystem rather than this package importing it.
type RecordingReadiness struct {
	StorageAvailable bool   `json:"storageAvailable"`
	Permitted        bool   `json:"permitted"`
	State            string `json:"state"` // "idle", "active", etc - empty if unknown
}

// Report is the complete preflight result - see docs/preflight-readiness.md
// for the full field-by-field contract.
type Report struct {
	SchemaVersion int `json:"schemaVersion"`

	// GeneratedAt is nullable until trusted wall-clock time exists - see
	// readiness.OptionalTime's rationale, reused here via a *time.Time
	// for the same reason (never a Go zero-time sentinel).
	GeneratedAt *time.Time `json:"generatedAt"`
	// GeneratedAtMonoSeconds is always present (monotonic time is always
	// available), so a caller with no trusted wall clock yet can still
	// tell reports apart and compute elapsed time between two of them.
	GeneratedAtMonoSeconds float64 `json:"generatedAtMonoSeconds"`

	// BootSessionID identifies the current daemon boot/session - the
	// same identity ManualAckStore acknowledgements are bound to. Safe
	// to expose: it is an opaque random token, not a filesystem path or
	// hardware identifier.
	BootSessionID string `json:"bootSessionId"`

	Overall State  `json:"overall"`
	Summary string `json:"summary"`

	RequiredActionCount int `json:"requiredActionCount"`
	CautionCount        int `json:"cautionCount"`

	Automated []CheckResult `json:"automated"`
	Manual    []CheckResult `json:"manual"`

	Profile   ProfileSummary     `json:"profile"`
	Recording RecordingReadiness `json:"recording"`

	// Disclaimer is included in every report (not just the dashboard
	// copy) so any consumer - including a diagnostic bundle or a future
	// integration - carries the same safety framing verbatim.
	Disclaimer string `json:"disclaimer"`
}

// Disclaimer is the exact required safety statement - see B3 in the
// mission brief / docs/preflight-readiness.md. Defined once so the
// dashboard, the API, and diagnostics can never drift from each other.
const Disclaimer = "Supplemental, non-certified preflight aid. The pilot remains responsible for verifying the aircraft, equipment, weather and flight conditions."

// Input is everything BuildReport needs beyond the fixed check
// definitions - gathered by main/preflightapi.go from already-existing
// state (globalHealth, the active calibration profile, the recording
// subsystem, manual acknowledgements) and passed in verbatim. BuildReport
// itself touches no global state and performs no I/O, so it is entirely
// deterministic and safe to unit test with fabricated inputs.
type Input struct {
	Health readiness.HealthReport

	// UptimeSeconds is monotonic seconds since this daemon process
	// started (not since the last calibration or the last reboot of a
	// prior process necessarily - "this process" is what grace periods
	// are measured against, matching ManualAckStore's session model).
	UptimeSeconds float64

	Profile   ProfileSummary
	Recording RecordingReadiness

	// ManualAcks is a snapshot from ManualAckStore.Snapshot - already
	// filtered to only currently-valid (right session, not expired)
	// acknowledgements; a nil map entry or missing key both mean "not
	// currently acknowledged."
	ManualAcks map[ManualCheckID]*ManualAck

	BootSessionID  string
	GeneratedAtUTC *time.Time // nil when time is not currently trusted

	// PreviousSessionAvailable/PreviousSessionEndedCleanly/
	// PreviousSessionNote mirror power.PreviousSessionAssessment (see the
	// power package) without this package importing it, the same
	// import-avoidance pattern readiness.CalibrationProfileSummary uses
	// for calprofile. PreviousSessionAvailable false means no assessment
	// could be made (e.g. first boot with this feature) - not evidence of
	// anything, and never treated as a caution.
	PreviousSessionAvailable    bool
	PreviousSessionEndedCleanly bool
	PreviousSessionNote         string
}

// Grace periods - see docs/preflight-readiness.md "Startup grace
// periods" for the rationale behind each duration. All are measured
// against Input.UptimeSeconds (monotonic), never wall-clock time, so a
// GNSS/NTP clock step immediately after boot cannot shorten or lengthen
// one - see docs/readiness-and-time-trust.md.
const (
	// graceSDRDiscoverySeconds is 120s, not a shorter guess, because it
	// mirrors a real, pre-existing constraint: main/sdr.go's sdrWatcher()
	// deliberately delays configuring any SDR device until GPS acquires a
	// fix or 120s elapses, whichever comes first (to reduce RF noise
	// during GPS acquisition) - confirmed live on hardware with no GPS
	// fix, where readiness.RadioHealth.Band.Enabled read false for the
	// full ~120s. An earlier, shorter value here caused 978/1090 to
	// misreport NOT_APPLICABLE ("deliberately disabled") during that
	// window instead of UNKNOWN ("not yet determined") - see checks.go's
	// radioChecks()/fisbChecks().
	graceSDRDiscoverySeconds   = 120.0
	graceGPSAcquisitionSeconds = 90.0
	graceGNSSTimeSeconds       = 90.0
	graceNetworkClientSeconds  = 60.0
	graceFanControllerSeconds  = 30.0
)

// BuildReport applies the documented decision policy (see checks.go for
// each individual rule, and this function's rollup at the end) to in,
// producing a complete, deterministic Report. It never panics on its own
// - every field read from in.Health defaults to Go's zero value if a
// caller supplies a partially-populated readiness.HealthReport, which
// simply surfaces as StateUnknown/"insufficient data" checks rather than
// a crash. main/preflightapi.go additionally wraps the call to BuildReport
// itself in a recover(), per the failure-isolation requirement - see its
// package comment.
func BuildReport(in Input) Report {
	var automated []CheckResult
	automated = append(automated, coreSystemChecks(in)...)
	automated = append(automated, gpsTimeChecks(in)...)
	automated = append(automated, adsbChecks(in)...)
	automated = append(automated, gdl90Checks(in)...)
	automated = append(automated, ahrsChecks(in)...)
	automated = append(automated, baroCheck(in)...)
	automated = append(automated, fanChecks(in)...)
	automated = append(automated, recordingChecks(in)...)
	automated = append(automated, fisbChecks(in)...)
	automated = append(automated, powerSessionChecks(in)...)

	manual := manualCheckResults(in)

	overall, required, caution := rollup(automated, manual)

	return Report{
		SchemaVersion:          SchemaVersion,
		GeneratedAt:            in.GeneratedAtUTC,
		GeneratedAtMonoSeconds: in.UptimeSeconds,
		BootSessionID:          in.BootSessionID,
		Overall:                overall,
		Summary:                summarize(overall, required, caution),
		RequiredActionCount:    required,
		CautionCount:           caution,
		Automated:              automated,
		Manual:                 manual,
		Profile:                in.Profile,
		Recording:              in.Recording,
		Disclaimer:             Disclaimer,
	}
}

// rollup implements the overall decision policy: NOT_READY if any
// BLOCKING check is not in a "fine" state; else CAUTION if any check
// (blocking or not) is not in a "fine" state; else READY. "Fine" means
// StateReady or StateNotApplicable - StateNotApplicable never affects the
// rollup (matching readiness.Rollup's treatment of StateNotInstalled),
// and StateUnknown/StateVerify/StateCaution/StateNotReady all count as
// "not fine" so an UNKNOWN required check is never silently promoted to
// READY (see State's doc comment).
func rollup(automated, manual []CheckResult) (overall State, requiredActions, cautions int) {
	all := make([]CheckResult, 0, len(automated)+len(manual))
	all = append(all, automated...)
	all = append(all, manual...)

	sawBlockingProblem := false
	sawAnyProblem := false
	for _, c := range all {
		if c.State == StateReady || c.State == StateNotApplicable {
			continue
		}
		if c.Severity == SeverityInfo {
			// Informational checks (e.g. the fixed "fan rotation is never
			// electronically confirmed" VERIFY notice, or the GDL90
			// client-identification disclosure) never affect the rollup
			// on their own, regardless of their State - see Severity's
			// doc comment.
			continue
		}
		sawAnyProblem = true
		if c.Blocking {
			sawBlockingProblem = true
			requiredActions++
		} else {
			cautions++
		}
	}

	switch {
	case sawBlockingProblem:
		return StateNotReady, requiredActions, cautions
	case sawAnyProblem:
		return StateCaution, requiredActions, cautions
	default:
		return StateReady, requiredActions, cautions
	}
}

func summarize(overall State, required, caution int) string {
	switch overall {
	case StateNotReady:
		if required == 1 {
			return "1 required item needs attention before flight."
		}
		return strconv.Itoa(required) + " required items need attention before flight."
	case StateCaution:
		if caution == 1 {
			return "1 item needs attention - flight-support functions remain available."
		}
		return strconv.Itoa(caution) + " items need attention - flight-support functions remain available."
	case StateReady:
		return "All required automated checks pass and all required manual checks are acknowledged."
	default:
		return "Preflight state could not be determined."
	}
}
