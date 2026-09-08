package storagelifecycle

import "github.com/stratux/stratux/readiness"

// PressureState is this package's storage-pressure classification. It
// deliberately reuses readiness.StorageThresholds' already-validated
// percentages (see FilesystemPressure below) for the whole-filesystem
// dimension rather than inventing new numbers - this package adds a
// second, new dimension (per-namespace quota pressure, since readiness
// has no concept of a namespace) and reports the worse of the two.
type PressureState string

const (
	PressureNormal   PressureState = "NORMAL"
	PressureElevated PressureState = "ELEVATED"
	PressureHigh     PressureState = "HIGH"
	PressureCritical PressureState = "CRITICAL"
	// PressureUnknown means no valid sample exists yet, or the most
	// recent scan failed - never reported as PressureNormal just because
	// there is nothing to report (see docs/storage-lifecycle.md).
	PressureUnknown PressureState = "UNKNOWN"
)

// rank orders severity for "worse of two states" comparisons -
// PressureUnknown is treated as more severe than PressureNormal (an
// inventory failure must never be silently reported as "fine") but less
// severe than a confirmed HIGH/CRITICAL reading, so a namespace that is
// definitely fine cannot mask a filesystem that is definitely not, or
// vice versa.
func (p PressureState) rank() int {
	switch p {
	case PressureNormal:
		return 0
	case PressureUnknown:
		return 1
	case PressureElevated:
		return 2
	case PressureHigh:
		return 3
	case PressureCritical:
		return 4
	default:
		return 4
	}
}

// worse returns whichever of a, b ranks more severe.
func worse(a, b PressureState) PressureState {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// FilesystemPressure classifies a whole-filesystem utilization percentage
// using readiness's own already-validated thresholds (Warn/Critical -
// see readiness.DefaultPersistentStorageThresholds), so this package
// never re-derives or second-guesses that threshold policy. Percentages
// above CriticalPercent but below RecordingProhibitedPercent are HIGH;
// at or above RecordingProhibitedPercent (the point readiness itself
// already refuses new recordings) is CRITICAL.
func FilesystemPressure(utilizationPercent float64, thresholds readiness.StorageThresholds) PressureState {
	switch {
	case utilizationPercent >= thresholds.RecordingProhibitedPercent:
		return PressureCritical
	case utilizationPercent >= thresholds.CriticalPercent:
		return PressureHigh
	case utilizationPercent >= thresholds.WarnPercent:
		return PressureElevated
	default:
		return PressureNormal
	}
}

// NamespaceQuotaPressure classifies one namespace's usage against its own
// Quota using the same percentage bands as FilesystemPressure, so a
// namespace-quota reading and a filesystem reading combine meaningfully.
// A zero/unset quota (MaxBytes == 0) always reports PressureNormal - an
// unconfigured quota is "no limit," not "already exceeded."
func NamespaceQuotaPressure(usedBytes int64, quota Quota) PressureState {
	if quota.MaxBytes <= 0 {
		return PressureNormal
	}
	pct := float64(usedBytes) / float64(quota.MaxBytes) * 100
	switch {
	case pct >= 100:
		return PressureCritical
	case pct >= 90:
		return PressureHigh
	case pct >= 75:
		return PressureElevated
	default:
		return PressureNormal
	}
}

// Monitor debounces a stream of raw PressureState samples against
// RequiredConsecutive agreeing samples before its reported state changes
// - the same pattern as power.Monitor (see that package's doc comment for
// the full rationale), reused here for consistency across the codebase
// rather than inventing a different hysteresis mechanism for storage.
type Monitor struct {
	RequiredConsecutive int

	current   PressureState
	candidate PressureState
	streak    int
}

// NewMonitor returns a Monitor reporting PressureUnknown until it has
// seen requiredConsecutive agreeing samples.
func NewMonitor(requiredConsecutive int) *Monitor {
	if requiredConsecutive < 1 {
		requiredConsecutive = 1
	}
	return &Monitor{RequiredConsecutive: requiredConsecutive, current: PressureUnknown}
}

// Observe evaluates one new raw sample and returns the Monitor's current
// (debounced) PressureState.
func (m *Monitor) Observe(raw PressureState) PressureState {
	if raw == m.candidate {
		m.streak++
	} else {
		m.candidate = raw
		m.streak = 1
	}
	if m.streak >= m.RequiredConsecutive {
		m.current = raw
	}
	return m.current
}

// Current returns the Monitor's last-reported (debounced) state without
// taking a new sample.
func (m *Monitor) Current() PressureState { return m.current }
