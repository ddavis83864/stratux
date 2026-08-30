package readiness

import (
	"fmt"
	"time"
)

// TimeState is the trusted-time synchronization state exposed via the
// health API and dashboard.
type TimeState string

const (
	// TimeUnsynchronized means no trusted source (network or GNSS) has
	// ever been confirmed since startup. The system clock's value is of
	// unknown accuracy.
	TimeUnsynchronized TimeState = "UNSYNCHRONIZED"

	// TimeNetworkSynced means the system clock is currently trusted via
	// network time (NTP). Not required in flight - see TimeGNSSSynced.
	TimeNetworkSynced TimeState = "NETWORK_SYNCED"

	// TimeGNSSSynced means the system clock is currently trusted via a
	// GNSS receiver's own UTC time, validated against all of the trust
	// gates in EvaluateSample. This is the offline, in-flight fallback and
	// is required before automatic flight recording is enabled.
	TimeGNSSSynced TimeState = "GNSS_SYNCED"

	// TimeDegraded means a trusted source was previously established but
	// has since become stale, lost, or has produced a discrepancy (e.g. a
	// would-be backward correction after recording began) that was not
	// applied. The last trusted value is still the best available
	// estimate, with growing uncertainty, but new data cannot currently be
	// trusted without review.
	TimeDegraded TimeState = "DEGRADED"

	// TimeInvalid means current time tracking has concrete evidence of
	// being wrong (e.g. a rejected backward correction after recording had
	// already started, or a source that has been continuously exceeding
	// its plausible bounds), not merely stale or unconfirmed. Recording
	// must not be allowed to start while in this state.
	TimeInvalid TimeState = "INVALID"
)

// GNSSTimeSample is one candidate UTC time reading derived from a GNSS
// receiver sentence (today, an RMC sentence's date+time fields; main/gps.go
// is responsible for populating this from whatever it parses - this
// package never touches NMEA text itself).
type GNSSTimeSample struct {
	HardwarePresent bool      // a GNSS receiver is attached/detected at all
	ChecksumValid   bool      // the source sentence's NMEA checksum validated
	StatusValid     bool      // the sentence's own status field indicates a valid navigation fix (e.g. RMC field A, not V)
	Parseable       bool      // the date/time fields could be parsed at all
	AcceptableFix   bool      // the receiver's current fix is good enough to trust for time (caller's threshold)
	UTC             time.Time // the parsed UTC value; meaningless unless Parseable
	ReceivedAt      time.Time // when this sample was captured, on the caller's monotonic clock (not wall time - see IsReceiving in sdrassign for why)
}

// TimeTrustConfig controls how strictly GNSS time samples are trusted and
// how the system clock is corrected. All fields are configurable per the
// mission requirement that thresholds not be hardcoded.
type TimeTrustConfig struct {
	MaxSampleAge        time.Duration // a sample older than this cannot be used ("data is fresh")
	RequiredConsecutive int           // consecutive agreeing accepted samples required before first trust
	AgreementTolerance  time.Duration // max allowed disagreement between consecutive samples (after accounting for elapsed time) to count as "agreeing"
	MinPlausibleUTC     time.Time     // samples before this are rejected as implausible
	MaxPlausibleUTC     time.Time     // samples after this are rejected as implausible
	LargeStepThreshold  time.Duration // a correction at/above this magnitude is a hard step; below it, a slew is preferred
	StaleAfter          time.Duration // a previously-synced source not refreshed within this window degrades to TimeDegraded
}

// DefaultTimeTrustConfig returns reasonable initial thresholds. All of
// them are meant to be tuned by the deployment, not treated as final.
func DefaultTimeTrustConfig() TimeTrustConfig {
	return TimeTrustConfig{
		MaxSampleAge:        5 * time.Second,
		RequiredConsecutive: 3,
		AgreementTolerance:  2 * time.Second,
		MinPlausibleUTC:     time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		MaxPlausibleUTC:     time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
		LargeStepThreshold:  5 * time.Second,
		StaleAfter:          15 * time.Second,
	}
}

// SampleVerdict is the result of gating one GNSSTimeSample against every
// trust condition, independent of any prior state.
type SampleVerdict struct {
	Accepted bool
	Reason   string
}

// EvaluateSample checks one sample against every trust gate: hardware
// presence, checksum, sentence status, parseability, plausible bounds, and
// freshness (evaluated against now, the caller's monotonic clock reading -
// not wall time, since wall time is exactly what may not yet be trusted).
// It does not check the consecutive-sample-agreement gate, which needs
// history and lives on TimeTrust instead.
//
// This function touches no global state and is exercised directly by
// tests with synthetic samples.
func EvaluateSample(s GNSSTimeSample, now time.Time, cfg TimeTrustConfig) SampleVerdict {
	if !s.HardwarePresent {
		return SampleVerdict{false, "GNSS hardware not present"}
	}
	if !s.ChecksumValid {
		return SampleVerdict{false, "NMEA checksum invalid"}
	}
	if !s.Parseable {
		return SampleVerdict{false, "date/time fields could not be parsed"}
	}
	if !s.StatusValid {
		return SampleVerdict{false, "sentence status does not indicate a valid navigation fix"}
	}
	if s.UTC.Before(cfg.MinPlausibleUTC) || s.UTC.After(cfg.MaxPlausibleUTC) {
		return SampleVerdict{false, fmt.Sprintf("UTC value %s is outside the plausible range [%s, %s]",
			s.UTC.Format(time.RFC3339), cfg.MinPlausibleUTC.Format(time.RFC3339), cfg.MaxPlausibleUTC.Format(time.RFC3339))}
	}
	if !s.AcceptableFix {
		return SampleVerdict{false, "fix quality is not acceptable for time trust"}
	}
	if s.ReceivedAt.IsZero() || now.Sub(s.ReceivedAt) > cfg.MaxSampleAge {
		return SampleVerdict{false, fmt.Sprintf("sample is stale (age %s exceeds %s)", now.Sub(s.ReceivedAt), cfg.MaxSampleAge)}
	}
	return SampleVerdict{true, "sample passed all trust gates"}
}

// ClockAction is what the caller (main/gps.go) should actually do to the
// system clock in response to a Decision.
type ClockAction string

const (
	// ClockActionNone means take no clock action (e.g. the current source
	// is not yet trusted, or its value already agrees with the system
	// clock closely enough that a correction is not worth applying).
	ClockActionNone ClockAction = "none"

	// ClockActionStepOnce means immediately set the system clock to NewUTC.
	// TimeTrust only ever returns this once per LargeStepThreshold-sized
	// discrepancy per boot; a second large discrepancy after a step has
	// already been applied is reported as ClockActionNone with a reason
	// explaining that a repeat step was suppressed, not applied silently.
	ClockActionStepOnce ClockAction = "step_once"

	// ClockActionSlew means apply a small, forward-only correction toward
	// NewUTC. Unlike ClockActionStepOnce, this may recur - small ongoing
	// discipline is not the disruptive event a hard step is.
	//
	// Note: this package only decides that a slew is the right response;
	// it deliberately does not implement gradual sub-second clock
	// discipline (e.g. via adjtimex) itself. Today's caller applies a
	// ClockActionSlew the same way as a step (a direct clock set), which
	// is simple and safe but not a true gradual slew - see the time-trust
	// documentation for this known limitation.
	ClockActionSlew ClockAction = "slew"

	// ClockActionRejectBackward means NewUTC is behind the current system
	// clock and recording has already begun, so it was deliberately not
	// applied. The discrepancy is recorded; time moves to TimeDegraded.
	ClockActionRejectBackward ClockAction = "reject_backward"
)

// Decision is the outcome of evaluating one trusted-time observation
// against the current system clock and trust state.
type Decision struct {
	Action ClockAction
	OldUTC time.Time
	NewUTC time.Time
	Source string // "gnss" or "network"
	Reason string
	At     time.Time // when this decision was made (wall clock, for the event log)
}

// SyncEvent is a durable record of one trusted-time decision, suitable for
// the health API's "last synchronization" fields and for inclusion in the
// diagnostic bundle.
type SyncEvent struct {
	Decision
}

// TimeHealth is the JSON shape exposed by the health API's Time field.
type TimeHealth struct {
	State             TimeState
	Source            string
	CurrentUTC        time.Time
	LastSyncTime      time.Time
	LastSyncSourceAge time.Duration
	EstimatedOffset   time.Duration
	RecordingAllowed  bool
	LastSyncError     string
	RecentEvents      []SyncEvent
}

// maxRetainedEvents bounds the in-memory sync-event log so a
// misbehaving/flapping time source cannot grow it without limit.
const maxRetainedEvents = 20

// TimeTrust tracks trusted-time state across repeated observations. Unlike
// the rest of this package it is necessarily stateful - "three consecutive
// agreeing samples" and "never step twice" are properties of a sequence of
// observations, not of any single one - but every state transition is
// still driven entirely by explicit inputs (samples, the current system
// clock reading, whether recording has started) with no direct hardware
// or OS access, so it is fully unit-testable with a fake clock.
type TimeTrust struct {
	cfg TimeTrustConfig

	state         TimeState
	source        string
	lastSyncAt    time.Time // monotonic-ish "now" at last accepted sync
	lastSyncUTC   time.Time
	lastSyncError string
	offset        time.Duration

	consecutiveGood int
	lastGoodUTC     time.Time
	lastGoodAt      time.Time

	steppedOnce bool
	events      []SyncEvent
}

// NewTimeTrust creates a tracker starting in TimeUnsynchronized.
func NewTimeTrust(cfg TimeTrustConfig) *TimeTrust {
	return &TimeTrust{cfg: cfg, state: TimeUnsynchronized}
}

// ObserveGNSS processes one GNSS time sample: it applies EvaluateSample,
// enforces the consecutive-agreement gate, and - once trusted - decides
// what clock action (if any) the caller should take, given the system
// clock's current reading and whether recording has already started.
//
// now is the caller's monotonic clock (for sample freshness and the
// consecutive-sample-agreement window); systemClockUTC is the wall clock's
// current reading (what a correction would be relative to).
func (t *TimeTrust) ObserveGNSS(s GNSSTimeSample, now, systemClockUTC time.Time, recordingStarted bool) Decision {
	verdict := EvaluateSample(s, now, t.cfg)
	if !verdict.Accepted {
		t.consecutiveGood = 0
		if t.state == TimeGNSSSynced {
			t.degrade("gnss", "GNSS time source lost: "+verdict.Reason, false)
		}
		t.lastSyncError = verdict.Reason
		return Decision{Action: ClockActionNone, Reason: verdict.Reason, At: now}
	}

	if t.consecutiveGood > 0 && !t.lastGoodAt.IsZero() {
		elapsed := now.Sub(t.lastGoodAt)
		implied := t.lastGoodUTC.Add(elapsed)
		disagreement := s.UTC.Sub(implied)
		if disagreement < 0 {
			disagreement = -disagreement
		}
		if disagreement > t.cfg.AgreementTolerance {
			// A sample that itself passed every individual gate but
			// disagrees with the run so far is treated as a fresh run of
			// one, not an error - a single noisy/glitched fix should not
			// need a full StaleAfter timeout to recover from.
			t.consecutiveGood = 0
		}
	}
	t.consecutiveGood++
	t.lastGoodUTC = s.UTC
	t.lastGoodAt = now

	if t.consecutiveGood < t.cfg.RequiredConsecutive {
		reason := fmt.Sprintf("gnss sample accepted (%d/%d consecutive needed before trust)", t.consecutiveGood, t.cfg.RequiredConsecutive)
		return Decision{Action: ClockActionNone, Reason: reason, At: now}
	}

	return t.decideAndSync("gnss", s.UTC, now, systemClockUTC, recordingStarted)
}

// ObserveNetwork processes a network (NTP) time observation. Network time
// has no consecutive-sample gate - NTP already applies its own internal
// validation - but is otherwise subject to the same step/slew/backward-
// rejection rules as GNSS, and is explicitly not required for correct
// operation (only GNSS is the mandated offline fallback).
func (t *TimeTrust) ObserveNetwork(ntpUTC, now, systemClockUTC time.Time, valid bool, recordingStarted bool) Decision {
	if !valid {
		if t.state == TimeNetworkSynced {
			t.degrade("network", "network time source lost", false)
		}
		return Decision{Action: ClockActionNone, Reason: "network time not currently valid", At: now}
	}
	return t.decideAndSync("network", ntpUTC, now, systemClockUTC, recordingStarted)
}

func (t *TimeTrust) decideAndSync(source string, sourceUTC, now, systemClockUTC time.Time, recordingStarted bool) Decision {
	diff := sourceUTC.Sub(systemClockUTC)
	backward := diff < 0
	magnitude := diff
	if backward {
		magnitude = -magnitude
	}

	d := Decision{OldUTC: systemClockUTC, NewUTC: sourceUTC, Source: source, At: now}

	switch {
	case backward && recordingStarted:
		d.Action = ClockActionRejectBackward
		d.Reason = fmt.Sprintf("rejected %s backward correction of %s after recording had already started - time not moved, marking degraded", source, magnitude)
		t.degrade(source, d.Reason, true)
		t.record(d)
		return d

	case magnitude >= t.cfg.LargeStepThreshold && t.steppedOnce:
		d.Action = ClockActionNone
		d.Reason = fmt.Sprintf("suppressed a second large (%s) clock step this session; only one automatic step is applied per boot", magnitude)
		t.record(d)
		return d

	case magnitude >= t.cfg.LargeStepThreshold:
		d.Action = ClockActionStepOnce
		d.Reason = fmt.Sprintf("stepping system clock by %s from trusted %s time (one-time correction)", magnitude, source)
		t.steppedOnce = true
		t.applySync(source, sourceUTC, now, diff, d)
		return d

	case magnitude == 0:
		d.Action = ClockActionNone
		d.Reason = fmt.Sprintf("system clock already agrees with trusted %s time", source)
		t.applySync(source, sourceUTC, now, diff, d)
		return d

	default: // small forward (or already-negligible-and-not-caught-above) correction
		d.Action = ClockActionSlew
		d.Reason = fmt.Sprintf("slewing system clock by %s toward trusted %s time", diff, source)
		t.applySync(source, sourceUTC, now, diff, d)
		return d
	}
}

// applySync records a successful trust/sync and moves state to the
// synced state for source.
func (t *TimeTrust) applySync(source string, sourceUTC, now time.Time, offset time.Duration, d Decision) {
	t.lastSyncAt = now
	t.lastSyncUTC = sourceUTC
	t.lastSyncError = ""
	t.offset = offset
	if source == "gnss" {
		t.state = TimeGNSSSynced
	} else {
		t.state = TimeNetworkSynced
	}
	t.source = source
	t.record(d)
}

// degrade moves state to TimeDegraded, or to TimeInvalid when invalid is
// true (reserved for a stronger signal than mere staleness - a rejected
// backward correction, not simply a lost/stale source). It records why
// without discarding the last trusted value: Snapshot still reports it,
// with growing LastSyncSourceAge, so a consumer can judge how much to
// trust it rather than losing the information entirely.
func (t *TimeTrust) degrade(source, reason string, invalid bool) {
	switch {
	case invalid:
		t.state = TimeInvalid
	case t.state == TimeUnsynchronized:
		// Never having synced at all is not "degraded" - that word implies
		// a regression from a previously-good state.
		t.state = TimeUnsynchronized
	default:
		t.state = TimeDegraded
	}
	t.lastSyncError = reason
}

func (t *TimeTrust) record(d Decision) {
	t.events = append(t.events, SyncEvent{d})
	if len(t.events) > maxRetainedEvents {
		t.events = t.events[len(t.events)-maxRetainedEvents:]
	}
}

// CheckStale re-evaluates whether a previously-synced source has gone
// stale (no accepted sample within StaleAfter). Callers should invoke this
// periodically (e.g. alongside the health-report tick) even when no new
// sample has arrived, since silence itself is the signal here.
func (t *TimeTrust) CheckStale(now time.Time) {
	if t.state != TimeGNSSSynced && t.state != TimeNetworkSynced {
		return
	}
	if t.lastGoodAt.IsZero() && t.lastSyncAt.IsZero() {
		return
	}
	reference := t.lastGoodAt
	if t.source == "network" {
		reference = t.lastSyncAt
	}
	if now.Sub(reference) > t.cfg.StaleAfter {
		t.degrade(t.source, fmt.Sprintf("%s time source stale: no accepted sample in %s (limit %s)", t.source, now.Sub(reference), t.cfg.StaleAfter), false)
	}
}

// Snapshot returns the current health-API view of trusted time. currentUTC
// is the caller's present idea of wall-clock UTC (used only to compute
// RecordingAllowed's implicit freshness relative to Now for display; it is
// not otherwise trusted or validated by this method).
func (t *TimeTrust) Snapshot(now time.Time) TimeHealth {
	h := TimeHealth{
		State:            t.state,
		Source:           t.source,
		LastSyncTime:     t.lastSyncAt,
		EstimatedOffset:  t.offset,
		LastSyncError:    t.lastSyncError,
		RecordingAllowed: t.state == TimeGNSSSynced || t.state == TimeNetworkSynced,
	}
	if !t.lastSyncAt.IsZero() {
		h.LastSyncSourceAge = now.Sub(t.lastSyncAt)
	}
	events := make([]SyncEvent, len(t.events))
	copy(events, t.events)
	h.RecentEvents = events
	return h
}

// State returns the current TimeState directly, for callers (e.g. the
// unified health aggregator) that only need the enum, not the full
// Snapshot.
func (t *TimeTrust) State() TimeState {
	return t.state
}
