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
	// trusted without review. This is also this package's holdover
	// representation: a source that has gone quiet does not lose its last
	// trusted value, it grows LastSyncSourceAge against it instead.
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
//
// Rationale for the defaults (DefaultTimeTrustConfig): a Raspberry Pi 4 has
// no RTC but a reasonably stable crystal oscillator - typical drift is a
// few parts-per-million, i.e. single-digit milliseconds even over
// MinCorrectionInterval's full 30s window. Deadband (50ms) is chosen well
// above that expected drift and above ordinary GNSS fix jitter, so routine
// noise never triggers a clock operation at all, while MinCorrectionInterval
// (30s) independently bounds how often this package will ever act on the
// clock even for a persistent, above-deadband discrepancy - together they
// are what stop the every-~100ms correction churn a naive
// "correct on every accepted sample" policy produces.
type TimeTrustConfig struct {
	MaxSampleAge        time.Duration // a sample older than this cannot be used ("data is fresh")
	RequiredConsecutive int           // consecutive agreeing accepted samples required before first trust
	AgreementTolerance  time.Duration // max allowed disagreement between consecutive samples (after accounting for elapsed time) to count as "agreeing"
	MinPlausibleUTC     time.Time     // samples before this are rejected as implausible
	MaxPlausibleUTC     time.Time     // samples after this are rejected as implausible
	LargeStepThreshold  time.Duration // a correction at/above this magnitude is a hard, once-per-boot step
	StaleAfter          time.Duration // a previously-synced source not refreshed within this window degrades to TimeDegraded (this package's holdover policy)

	// Deadband is the offset magnitude below which, once synced, no clock
	// operation is performed at all and no health event is recorded -
	// routine GNSS/oscillator noise, not a real discrepancy worth acting
	// on. See the type doc for the reasoning behind the default.
	Deadband time.Duration

	// MinCorrectionInterval is the minimum monotonic time between any two
	// non-step clock corrections. An accepted sample whose offset exceeds
	// Deadband but arrives before this interval has elapsed since the
	// last correction is deferred (ClockActionNone, with a reason saying
	// why) rather than acted on immediately - this is what bounds
	// correction frequency independent of how often samples arrive.
	MinCorrectionInterval time.Duration
}

// DefaultTimeTrustConfig returns reasonable initial thresholds. All of
// them are meant to be tuned by the deployment, not treated as final.
func DefaultTimeTrustConfig() TimeTrustConfig {
	return TimeTrustConfig{
		MaxSampleAge:          5 * time.Second,
		RequiredConsecutive:   3,
		AgreementTolerance:    2 * time.Second,
		MinPlausibleUTC:       time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		MaxPlausibleUTC:       time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC),
		LargeStepThreshold:    5 * time.Second,
		StaleAfter:            15 * time.Second,
		Deadband:              50 * time.Millisecond,
		MinCorrectionInterval: 30 * time.Second,
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
	// ClockActionNone means take no clock action - the source is not yet
	// trusted, its offset is within the deadband, a correction was
	// deferred by rate limiting, or a repeat large step was suppressed.
	// Reason always says which.
	ClockActionNone ClockAction = "none"

	// ClockActionStepOnce means immediately set the system clock to NewUTC.
	// TimeTrust only ever returns this once per LargeStepThreshold-sized
	// discrepancy per boot; a second large discrepancy after a step has
	// already been applied is reported as ClockActionNone with a reason
	// explaining that a repeat step was suppressed, not applied silently.
	ClockActionStepOnce ClockAction = "step_once"

	// ClockActionPeriodicCorrection means apply a small, forward-only
	// direct clock set toward NewUTC. This is deliberately NOT called
	// "slew": this package does not implement a genuine gradual
	// kernel-level clock discipline (e.g. adjtimex) - the caller applies
	// this the same way as ClockActionStepOnce, a direct set, just for a
	// smaller offset and, unlike the once-per-boot step, allowed to recur
	// - bounded by Deadband (small offsets never reach here at all) and
	// MinCorrectionInterval (rate-limited even when they do).
	ClockActionPeriodicCorrection ClockAction = "periodic_correction"

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
	At     time.Time // wall-clock instant this decision was made, for the event log
}

// SyncEvent is a durable record of one trusted-time decision, suitable for
// the health API's "last synchronization" fields and for inclusion in the
// diagnostic bundle.
type SyncEvent struct {
	Decision
}

// TimeHealth is the JSON shape exposed by the health API's Time field.
type TimeHealth struct {
	State  TimeState
	Source string

	// CurrentUTC is this package's best current estimate of wall-clock
	// UTC, populated only while actively synced (State is GNSS_SYNCED or
	// NETWORK_SYNCED, the same condition RecordingAllowed uses) -
	// unavailable (JSON null) otherwise, since an untrusted system clock
	// reading is not a value this package can vouch for.
	CurrentUTC OptionalTime

	// LastSyncTime is the wall-clock UTC instant of the last accepted
	// synchronization - unavailable (JSON null) if no sync has ever
	// happened this daemon lifetime.
	LastSyncTime OptionalTime

	// LastSyncSourceAgeSeconds is time elapsed, in seconds, since
	// LastSyncTime, computed on the monotonic clock - unavailable (JSON
	// null) if no sync has ever happened.
	LastSyncSourceAgeSeconds *float64
	// LastSyncSourceAge is the same quantity as a time.Duration
	// (nanoseconds when marshaled), retained for existing consumers.
	// Prefer LastSyncSourceAgeSeconds, which names its unit explicitly.
	LastSyncSourceAge time.Duration

	// EstimatedOffsetMilliseconds is the most recent source-vs-system
	// offset actually applied (step or periodic correction), in
	// milliseconds. 0 before any correction has ever been applied.
	EstimatedOffsetMilliseconds float64
	// EstimatedOffset is the same quantity as a time.Duration
	// (nanoseconds when marshaled), retained for existing consumers.
	// Prefer EstimatedOffsetMilliseconds.
	EstimatedOffset time.Duration

	RecordingAllowed bool
	LastSyncError    string
	RecentEvents     []SyncEvent
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
	lastSyncAt    time.Time // monotonic "now" at last accepted sync - for age arithmetic only, never serialized as a wall-clock value
	lastSyncUTC   time.Time // wall-clock UTC at last accepted sync - what LastSyncTime reports
	lastSyncError string
	offset        time.Duration

	consecutiveGood int
	lastGoodUTC     time.Time
	lastGoodAt      time.Time

	steppedOnce      bool
	lastCorrectionAt time.Time // monotonic "now" at the last applied (non-deadband) correction, zero if none yet
	events           []SyncEvent
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
// now is the caller's monotonic clock (for sample freshness, the
// consecutive-sample-agreement window, and rate-limiting arithmetic);
// systemClockUTC is the wall clock's current reading (what a correction
// would be relative to, and what is recorded as this Decision's At).
func (t *TimeTrust) ObserveGNSS(s GNSSTimeSample, now, systemClockUTC time.Time, recordingStarted bool) Decision {
	verdict := EvaluateSample(s, now, t.cfg)
	if !verdict.Accepted {
		t.consecutiveGood = 0
		if t.state == TimeGNSSSynced {
			t.degrade("gnss", "GNSS time source lost: "+verdict.Reason, false)
		}
		t.lastSyncError = verdict.Reason
		return Decision{Action: ClockActionNone, Reason: verdict.Reason, At: systemClockUTC}
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
		return Decision{Action: ClockActionNone, Reason: reason, At: systemClockUTC}
	}

	return t.decideAndSync("gnss", s.UTC, now, systemClockUTC, recordingStarted)
}

// ObserveNetwork processes a network (NTP) time observation. Network time
// has no consecutive-sample gate - NTP already applies its own internal
// validation - but is otherwise subject to the same step/correction/
// backward-rejection rules as GNSS, and is explicitly not required for
// correct operation (only GNSS is the mandated offline fallback).
func (t *TimeTrust) ObserveNetwork(ntpUTC, now, systemClockUTC time.Time, valid bool, recordingStarted bool) Decision {
	if !valid {
		if t.state == TimeNetworkSynced {
			t.degrade("network", "network time source lost", false)
		}
		return Decision{Action: ClockActionNone, Reason: "network time not currently valid", At: systemClockUTC}
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

	d := Decision{OldUTC: systemClockUTC, NewUTC: sourceUTC, Source: source, At: systemClockUTC}

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
		t.lastCorrectionAt = now
		t.applySync(source, sourceUTC, now, diff, d)
		return d

	case magnitude < t.cfg.Deadband:
		// Routine noise, not a real discrepancy - update trust state
		// silently (no event, no clock action): the mission requires not
		// generating a health event for every ignored sub-threshold
		// sample, and this is precisely that case.
		d.Action = ClockActionNone
		d.Reason = fmt.Sprintf("offset %s is within the %s deadband; no correction applied", magnitude, t.cfg.Deadband)
		t.applySyncQuiet(source, sourceUTC, now, diff)
		return d

	case !t.lastCorrectionAt.IsZero() && now.Sub(t.lastCorrectionAt) < t.cfg.MinCorrectionInterval:
		// Above deadband, but a correction happened too recently - defer
		// rather than act on every sample. Still update trust state
		// silently so LastSyncTime/State stay current; this deferral
		// itself IS recorded (unlike the deadband case) since it reflects
		// a real, above-threshold discrepancy the operator may want
		// visibility into if it persists.
		d.Action = ClockActionNone
		d.Reason = fmt.Sprintf("deferred correction of %s: only %s since the last correction (minimum %s)", magnitude, now.Sub(t.lastCorrectionAt), t.cfg.MinCorrectionInterval)
		t.applySyncQuiet(source, sourceUTC, now, diff)
		t.record(d)
		return d

	default:
		// Above deadband, rate limit has elapsed (or this is the first
		// correction ever): a direct clock set, honestly labeled - see
		// ClockActionPeriodicCorrection's doc comment for why this is not
		// called "slew".
		d.Action = ClockActionPeriodicCorrection
		d.Reason = fmt.Sprintf("correcting system clock by %s toward trusted %s time (direct set, not a gradual slew)", diff, source)
		t.lastCorrectionAt = now
		t.applySync(source, sourceUTC, now, diff, d)
		return d
	}
}

// applySyncQuiet updates trust state (source, last-sync time/UTC, offset)
// without recording a health event - used for in-deadband and deferred
// observations, which are real trusted samples but not events worth
// surfacing individually.
func (t *TimeTrust) applySyncQuiet(source string, sourceUTC, now time.Time, offset time.Duration) {
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
}

// applySync records a successful trust/sync and moves state to the
// synced state for source.
func (t *TimeTrust) applySync(source string, sourceUTC, now time.Time, offset time.Duration, d Decision) {
	t.applySyncQuiet(source, sourceUTC, now, offset)
	t.record(d)
}

// degrade moves state to TimeDegraded, or to TimeInvalid when invalid is
// true (reserved for a stronger signal than mere staleness - a rejected
// backward correction, not simply a lost/stale source). It records why
// without discarding the last trusted value: Snapshot still reports it,
// with growing LastSyncSourceAge, so a consumer can judge how much to
// trust it rather than losing the information entirely. This is this
// package's holdover policy: the last trusted value is held, with visibly
// growing age, rather than discarded or silently re-trusted.
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

// Snapshot returns the current health-API view of trusted time.
//
// nowMono is the caller's monotonic clock (stratuxClock.Time domain),
// used only for computing LastSyncSourceAge against lastSyncAt - both on
// the same monotonic domain, so a wall-clock step in between cannot
// perturb it.
//
// nowWall is the caller's current wall-clock UTC reading (e.g.
// time.Now().UTC()), used only to populate CurrentUTC, and only while
// actively synced - this method does not otherwise trust or validate it.
func (t *TimeTrust) Snapshot(nowMono, nowWall time.Time) TimeHealth {
	h := TimeHealth{
		State:                       t.state,
		Source:                      t.source,
		LastSyncTime:                SomeTime(t.lastSyncUTC),
		EstimatedOffsetMilliseconds: float64(t.offset) / float64(time.Millisecond),
		EstimatedOffset:             t.offset,
		LastSyncError:               t.lastSyncError,
		RecordingAllowed:            t.state == TimeGNSSSynced || t.state == TimeNetworkSynced,
	}
	if h.RecordingAllowed {
		h.CurrentUTC = SomeTime(nowWall)
	}
	if !t.lastSyncAt.IsZero() {
		age := nowMono.Sub(t.lastSyncAt)
		ageSeconds := age.Seconds()
		h.LastSyncSourceAgeSeconds = &ageSeconds
		h.LastSyncSourceAge = age
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
