package alerting

import (
	"fmt"
	"sync"
	"time"
)

// Counters is a bounded, sanitized set of operational counters - safe to
// embed directly in a diagnostic bundle (see docs/alerting.md's
// "Diagnostics" section).
type Counters struct {
	SuppressedEvents      int `json:"suppressedEvents"`
	DroppedEvaluations    int `json:"droppedEvaluations"`
	StaleTargetRejections int `json:"staleTargetRejections"`
}

// Snapshot is the full, sanitized, bounded state returned by
// Evaluator.Snapshot - the basis for GET /getAlerts and the diagnostics
// summary.
type Snapshot struct {
	SchemaVersion int      `json:"schemaVersion"`
	Active        []Alert  `json:"active"`
	RecentHistory []Event  `json:"recentHistory"`
	Counters      Counters `json:"counters"`
	Muted         bool     `json:"muted"`
	// MuteRemainingSeconds is how many seconds remain until an active
	// timed mute automatically clears - nil for an indefinite mute (or
	// when not muted).
	MuteRemainingSeconds *float64 `json:"muteRemainingSeconds,omitempty"`
	TrackedTargetCount   int      `json:"trackedTargetCount"`
	Disclaimer           string   `json:"disclaimer"`
}

// Evaluator holds all in-memory alerting state. Safe for concurrent use.
// Never persisted - see docs/alerting.md's "reset on daemon restart" rule.
type Evaluator struct {
	mu  sync.Mutex
	cfg Config
	now func() time.Time

	targets          map[string]*targetTrafficState
	healthComponents map[string]*healthComponentState

	history []Event

	mutedActive bool
	muteUntil   time.Time // zero = no auto-expiry while mutedActive

	lastGlobalAudioAt time.Time

	counters Counters

	nextSeq int64
}

// NewEvaluator constructs an Evaluator. nowFunc mirrors
// preflight.NewManualAckStore's existing clock-injection convention.
func NewEvaluator(cfg Config, nowFunc func() time.Time) *Evaluator {
	return &Evaluator{
		cfg:              cfg,
		now:              nowFunc,
		targets:          make(map[string]*targetTrafficState),
		healthComponents: make(map[string]*healthComponentState),
	}
}

// SetConfig atomically replaces the active configuration. Rejects an
// invalid config (see Config.Validate) without disturbing the previous,
// still-valid one.
func (e *Evaluator) SetConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = cfg
	return nil
}

// Config returns a copy of the currently active configuration.
func (e *Evaluator) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// SetMuted arms or clears mute. untilMono is the zero time.Time for an
// indefinite mute (cleared only by an explicit Unmute); otherwise mute
// automatically clears once the evaluator's clock passes it - callers
// should compute untilMono using the same monotonic clock family as
// nowFunc.
func (e *Evaluator) SetMuted(muted bool, untilMono time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mutedActive = muted
	e.muteUntil = untilMono
}

func (e *Evaluator) expireMuteLocked(now time.Time) {
	if e.mutedActive && !e.muteUntil.IsZero() && !now.Before(e.muteUntil) {
		e.mutedActive = false
		e.muteUntil = time.Time{}
	}
}

func (e *Evaluator) mutedLocked(now time.Time) bool {
	e.expireMuteLocked(now)
	return e.mutedActive
}

// IsMuted reports current mute state.
func (e *Evaluator) IsMuted() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.mutedLocked(e.now())
}

// Acknowledge marks the given traffic target or health component id as
// acknowledged. Idempotent; returns false only if id names nothing
// currently tracked (never an error - an unknown/already-expired id is not
// exceptional).
func (e *Evaluator) Acknowledge(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st, ok := e.targets[id]; ok {
		st.acknowledged = true
		st.ackTier = st.currentTier
		return true
	}
	if st, ok := e.healthComponents[id]; ok {
		st.acknowledged = true
		return true
	}
	return false
}

// RecordDroppedEvaluation increments the dropped-evaluation counter - called
// by the caller when a bounded channel between traffic ingestion and this
// evaluator overflows (see docs/alerting.md's "Failure isolation" section).
// Never blocks, never panics.
func (e *Evaluator) RecordDroppedEvaluation() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.counters.DroppedEvaluations++
}

// EvaluateTraffic processes one cycle's worth of already-filtered traffic
// observations (the caller has already excluded targets
// isOwnshipTrafficInfo identifies as ownship or tells the caller to
// ignore) and returns any newly-produced events. ownshipValid must be the
// caller's isGPSValid() result for this cycle - when false, NO alert is
// ever produced (see docs/alerting.md's "no alert without valid ownship
// data"), though already-tracked targets are still aged out normally.
func (e *Evaluator) EvaluateTraffic(observations []TrafficObservation, ownshipValid bool) []Event {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireMuteLocked(now)

	var events []Event
	seen := make(map[string]bool, len(observations))

	if ownshipValid && e.cfg.MasterEnabled {
		for _, obs := range observations {
			if obs.IsOwnship || obs.TargetID == "" {
				continue
			}
			seen[obs.TargetID] = true
			st, exists := e.targets[obs.TargetID]
			if !exists {
				if len(e.targets) >= e.cfg.MaxTrackedTargets {
					e.evictOldestLocked()
				}
				st = &targetTrafficState{id: obs.TargetID, firstSeenAt: now, lastAudioAt: map[int]time.Time{}}
				e.targets[obs.TargetID] = st
			}
			if ev := e.updateTargetLocked(st, obs, now); ev != nil {
				events = append(events, *ev)
			}
			if st.validity == "stale" {
				e.counters.StaleTargetRejections++
			}
		}
	}

	for id, st := range e.targets {
		if seen[id] {
			continue
		}
		if now.Sub(st.lastUpdatedAt).Seconds() >= e.cfg.ExpireAfterSeconds {
			if st.currentTier != tierNone {
				expiredEv := e.buildEventLocked(st, "expired", false, "target-expired", now)
				expiredEv.Seq = e.nextSeqLocked()
				events = append(events, expiredEv)
			}
			delete(e.targets, id)
		}
	}

	for _, ev := range events {
		e.appendHistoryLocked(ev)
	}
	return events
}

func (e *Evaluator) evictOldestLocked() {
	var oldestID string
	var oldestAt time.Time
	for id, st := range e.targets {
		if oldestID == "" || st.lastUpdatedAt.Before(oldestAt) {
			oldestID = id
			oldestAt = st.lastUpdatedAt
		}
	}
	if oldestID != "" {
		delete(e.targets, oldestID)
	}
}

func (e *Evaluator) updateTargetLocked(st *targetTrafficState, obs TrafficObservation, now time.Time) *Event {
	entryTier, validity, entryCPAEscalated := classifyTier(obs, e.cfg)
	prevTier := st.currentTier

	// cpaEscalated tracks, for whichever tier ends up assigned to
	// newTier below, whether THAT tier's own most recent classification
	// was CPA-escalated - defaults to holding the previously-known value
	// (st.cpaEscalated) for every branch that merely holds prevTier
	// unchanged (hysteresis/dwell), and is only overwritten when newTier
	// is freshly assigned from entryTier.
	var newTier int
	cpaEscalated := st.cpaEscalated
	switch {
	case entryTier >= prevTier:
		newTier = entryTier
		cpaEscalated = entryCPAEscalated
		st.belowExitSince = time.Time{}
	default:
		exitTier, _, _ := classifyTier(obs, e.cfg.widen())
		if exitTier >= prevTier {
			newTier = prevTier
			st.belowExitSince = time.Time{}
		} else {
			if st.belowExitSince.IsZero() {
				st.belowExitSince = now
			}
			if now.Sub(st.belowExitSince).Seconds() >= e.cfg.DeescalateDwellSeconds {
				newTier = entryTier
				cpaEscalated = entryCPAEscalated
				st.belowExitSince = time.Time{}
			} else {
				newTier = prevTier
			}
		}
	}

	st.validity = validity
	st.cpaEscalated = cpaEscalated
	st.lastUpdatedAt = now
	st.last = obs

	if newTier == prevTier {
		st.currentTier = newTier
		return nil // duplicate suppression: no event for an unchanged tier
	}

	var kind string
	switch {
	case prevTier == tierNone:
		kind = "new"
	case newTier > prevTier:
		kind = "escalated"
	case newTier == tierNone:
		kind = "cleared"
	default:
		kind = "deescalated"
	}

	if kind == "cleared" {
		// The target has genuinely left the envelope (confirmed by the
		// full de-escalation dwell, not a boundary flap) - clear all
		// per-target memory so a later re-entry is treated as a wholly
		// new encounter: unacknowledged, and not rate-limited by a
		// cooldown timestamp from the earlier encounter. See
		// docs/alerting.md's "re-entry" rule.
		st.acknowledged = false
		st.ackTier = tierNone
		st.lastAudioAt = map[int]time.Time{}
		st.cpaEscalated = false
	} else if newTier > st.ackTier {
		st.acknowledged = false
	}
	st.currentTier = newTier

	audioEligible, suppressionReason := false, "quiet-by-default"
	if kind == "new" || kind == "escalated" {
		audioEligible, suppressionReason = e.trafficAudioEligibilityLocked(st, newTier, now)
	}

	ev := e.buildEventLocked(st, kind, audioEligible, suppressionReason, now)
	ev.Seq = e.nextSeqLocked()
	return &ev
}

func (e *Evaluator) trafficAudioEligibilityLocked(st *targetTrafficState, tier int, now time.Time) (bool, string) {
	if !e.cfg.MasterEnabled {
		return false, "alerting-disabled"
	}
	if !e.cfg.TrafficAudioEnabled {
		return false, "audio-disabled"
	}
	if e.mutedLocked(now) {
		return false, "muted"
	}
	bucket := audioCooldownBucket(tier)
	if last, ok := st.lastAudioAt[bucket]; ok {
		if now.Sub(last) < e.cfg.audioCooldownForBucket(bucket) {
			e.counters.SuppressedEvents++
			return false, "per-target-cooldown"
		}
	}
	if !e.lastGlobalAudioAt.IsZero() && now.Sub(e.lastGlobalAudioAt) < time.Duration(e.cfg.GlobalMinAudioSpacingSeconds*float64(time.Second)) {
		e.counters.SuppressedEvents++
		return false, "global-min-spacing"
	}
	st.lastAudioAt[bucket] = now
	e.lastGlobalAudioAt = now
	return true, ""
}

func (e *Evaluator) buildEventLocked(st *targetTrafficState, kind string, audioEligible bool, suppressionReason string, now time.Time) Event {
	level := levelForTier(st.currentTier)
	if kind == "cleared" || kind == "expired" {
		level = LevelInformation
	}
	msg := trafficMessage(kind, st.currentTier, st.last)
	a := Alert{
		ID:                    st.id,
		Category:              CategoryTraffic,
		Level:                 level,
		TargetID:              st.id,
		Message:               msg,
		DistanceMeters:        st.last.DistanceMeters,
		DistanceValid:         st.last.DistanceValid,
		RelativeAltitudeFeet:  st.last.RelativeAltitudeFeet,
		RelativeAltitudeValid: st.last.RelativeAltitudeValid,
		ClockDirection:        st.last.ClockDirection,
		ClockDirectionValid:   st.last.ClockDirectionValid,
		FirstSeenAgeSeconds:   now.Sub(st.firstSeenAt).Seconds(),
		LastUpdatedAgeSeconds: now.Sub(st.lastUpdatedAt).Seconds(),
		Acknowledged:          st.acknowledged,
		AudioEligible:         audioEligible,
		SuppressionReason:     suppressionReason,
		DataValidity:          st.validity,
		Disclaimer:            Disclaimer,
		CPAEscalated:          st.cpaEscalated,
	}
	if cpa := st.last.CPA; cpa != nil {
		a.CPAValid = cpa.Valid
		a.CPAConfidence = string(cpa.Confidence)
		a.CPARejectReason = string(cpa.RejectReason)
		a.CPAClosureRateKnots = cpa.HorizontalClosureRateKnots
		a.CPAClosureRateValid = cpa.HorizontalClosureRateValid
		a.CPATCPASeconds = cpa.TCPASeconds
		a.CPATCPAValid = cpa.TCPAValid
		a.CPATCPAClampedToHorizon = cpa.TCPAClampedToHorizon
		a.CPAPredictedHorizontalMeters = cpa.PredictedHorizontalSeparationMeters
		a.CPAPredictedHorizontalValid = cpa.PredictedHorizontalSeparationValid
		a.CPAPredictedVerticalFeet = cpa.PredictedVerticalSeparationFeet
		a.CPAPredictedVerticalValid = cpa.PredictedVerticalSeparationValid
		a.CPATrend = string(cpa.Trend)
	}
	return Event{Alert: a, Kind: kind}
}

func trafficMessage(kind string, tier int, obs TrafficObservation) string {
	if kind == "cleared" || kind == "expired" {
		return "Traffic clear - target no longer within the monitored area."
	}
	base := "Traffic nearby - check position"
	if tier >= tierHighCaution {
		base = "Traffic close - check position"
	}
	detail := ""
	if obs.ClockDirectionValid {
		detail += fmt.Sprintf(" (%s", obs.ClockDirection)
	}
	if obs.DistanceValid {
		nm := obs.DistanceMeters / 1852.0
		if detail == "" {
			detail += fmt.Sprintf(" (approx %.1f NM", nm)
		} else {
			detail += fmt.Sprintf(", approx %.1f NM", nm)
		}
	}
	if obs.RelativeAltitudeValid {
		rel := "level"
		if obs.RelativeAltitudeFeet > 100 {
			rel = "above"
		} else if obs.RelativeAltitudeFeet < -100 {
			rel = "below"
		}
		if detail == "" {
			detail += fmt.Sprintf(" (approx %.0f ft %s", abs(obs.RelativeAltitudeFeet), rel)
		} else {
			detail += fmt.Sprintf(", approx %.0f ft %s", abs(obs.RelativeAltitudeFeet), rel)
		}
	}
	if detail != "" {
		detail += ")"
	}
	return base + detail + "."
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// nextSeqLocked assigns the next monotonically-increasing sequence number -
// called exactly once per genuine history-bound event, never for a
// read-only Snapshot()/Active-list view (see Event.Seq's doc comment).
func (e *Evaluator) nextSeqLocked() int64 {
	e.nextSeq++
	return e.nextSeq
}

func (e *Evaluator) appendHistoryLocked(ev Event) {
	e.history = append(e.history, ev)
	if len(e.history) > e.cfg.MaxEventHistory {
		e.history = e.history[len(e.history)-e.cfg.MaxEventHistory:]
	}
}

// Snapshot returns the current, bounded, sanitized state.
func (e *Evaluator) Snapshot() Snapshot {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireMuteLocked(now)

	var active []Alert
	for _, st := range e.targets {
		if st.currentTier == tierNone {
			continue
		}
		ev := e.buildEventLocked(st, "current", false, "quiet-by-default", now)
		active = append(active, ev.Alert)
	}
	for _, st := range e.healthComponents {
		if st.level.rank() < ComponentCaution.rank() {
			continue
		}
		active = append(active, healthAlert(st, now))
	}

	hist := make([]Event, len(e.history))
	copy(hist, e.history)

	var muteRemaining *float64
	if e.mutedActive && !e.muteUntil.IsZero() {
		v := e.muteUntil.Sub(now).Seconds()
		muteRemaining = &v
	}

	return Snapshot{
		SchemaVersion:        SchemaVersion,
		Active:               active,
		RecentHistory:        hist,
		Counters:             e.counters,
		Muted:                e.mutedActive,
		MuteRemainingSeconds: muteRemaining,
		TrackedTargetCount:   len(e.targets),
		Disclaimer:           Disclaimer,
	}
}
