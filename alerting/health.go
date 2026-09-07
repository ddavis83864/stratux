package alerting

import "time"

// ComponentLevel is a coarse, already-derived health level for one
// monitored component - never computed here. The caller
// (main/alertinghealth.go) derives this from the existing, already
// grace-aware readiness.HealthReport/preflight.Report; this package only
// ever compares consecutive ComponentLevel values to detect transitions.
type ComponentLevel string

const (
	ComponentUnknown  ComponentLevel = "UNKNOWN"
	ComponentReady    ComponentLevel = "READY"
	ComponentCaution  ComponentLevel = "CAUTION"
	ComponentNotReady ComponentLevel = "NOT_READY"
)

// worse reports whether b is a strictly worse state than a, using the
// fixed ordering Unknown < Ready < Caution < NotReady. Unknown is
// deliberately treated as better than Caution/NotReady for transition
// purposes - a component newly reporting UNKNOWN during its own startup
// grace period (already handled upstream by readiness/preflight) must
// never itself look like a degradation here.
func (a ComponentLevel) rank() int {
	switch a {
	case ComponentReady:
		return 1
	case ComponentCaution:
		return 2
	case ComponentNotReady:
		return 3
	default: // ComponentUnknown or anything unrecognized
		return 0
	}
}

func worse(from, to ComponentLevel) bool {
	if from.rank() == ComponentUnknown.rank() {
		// A transition OUT of Unknown (including the daemon's very first
		// health observation, before any real baseline exists) is never
		// itself a degradation - this is what makes startup-grace
		// (already-Unknown) periods honest rather than alarming. See
		// docs/alerting.md's "Startup grace periods" section.
		return false
	}
	return to.rank() > from.rank() && to.rank() >= ComponentCaution.rank()
}

func recovered(from, to ComponentLevel) bool {
	return from.rank() >= ComponentCaution.rank() && to.rank() < from.rank() && to == ComponentReady
}

// HealthSnapshot is one point-in-time set of already-derived component
// levels, built by main/alertinghealth.go from readiness.HealthReport and
// preflight.Report. Field names match the mission's eligible-transition
// list; every field defaults to ComponentUnknown (never evaluated as a
// degradation) if the caller has nothing to report for it.
type HealthSnapshot struct {
	Overall            ComponentLevel
	PersistentStorage  ComponentLevel
	TemporaryOverlay   ComponentLevel
	Thermal            ComponentLevel
	UAT978             ComponentLevel
	ES1090             ComponentLevel
	GPS                ComponentLevel
	TrustedTime        ComponentLevel
	GDL90Clients       ComponentLevel
	AHRS               ComponentLevel
	Baro               ComponentLevel
	Fan                ComponentLevel
	CalibrationProfile ComponentLevel
}

// componentEntry pairs a HealthSnapshot field's current value with its
// stable name/message, for iteration.
type componentEntry struct {
	name    string
	message string
	level   ComponentLevel
}

// healthComponentState is the per-component memory the Evaluator keeps
// between health-evaluation cycles.
type healthComponentState struct {
	name             string
	message          string
	level            ComponentLevel
	lastTransitionAt time.Time
	acknowledged     bool
	lastAudioAt      time.Time
}

func (h HealthSnapshot) entries() []componentEntry {
	return []componentEntry{
		{"overall", "Overall Stratux readiness", h.Overall},
		{"persistent_storage", "Persistent storage", h.PersistentStorage},
		{"temporary_overlay", "Protected root overlay", h.TemporaryOverlay},
		{"thermal", "Power and thermal", h.Thermal},
		{"978_receiver", "978 UAT receiver", h.UAT978},
		{"1090_receiver", "1090 ES receiver", h.ES1090},
		{"gps", "GPS", h.GPS},
		{"trusted_time", "Trusted time", h.TrustedTime},
		{"gdl90_clients", "GDL90 client connection", h.GDL90Clients},
		{"ahrs", "AHRS", h.AHRS},
		{"baro", "Barometer", h.Baro},
		{"fan", "Fan controller", h.Fan},
		{"calibration_profile", "Active calibration profile", h.CalibrationProfile},
	}
}

// EvaluateHealth compares snap against the previously-seen level for each
// component and returns events only for actual transitions - see
// docs/alerting.md's "Health-transition behavior": no event fires merely
// because a component continues reporting the same CAUTION/NOT_READY state,
// and a transition into ComponentUnknown never itself counts as a
// degradation (grace periods are already handled upstream).
func (e *Evaluator) EvaluateHealth(snap HealthSnapshot) []Event {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireMuteLocked(now)

	var events []Event
	for _, entry := range snap.entries() {
		st, exists := e.healthComponents[entry.name]
		if !exists {
			st = &healthComponentState{name: entry.name, message: entry.message, level: ComponentUnknown}
			e.healthComponents[entry.name] = st
		}
		prev := st.level
		// entry.level.rank() == 0 covers both the explicit ComponentUnknown
		// constant and the zero-value ComponentLevel("") a caller gets by
		// simply not setting a field - both must be treated identically
		// (never a degradation) rather than looking like a real transition.
		if entry.level == prev || entry.level.rank() == ComponentUnknown.rank() {
			continue
		}
		st.level = entry.level
		st.message = entry.message
		st.lastTransitionAt = now

		if prev.rank() == ComponentUnknown.rank() {
			// Establishing this component's first-ever real baseline
			// (including recovering from a later Unknown blip) is never
			// itself reportable, worse() or not - see its own doc comment.
			continue
		}

		var ev Event
		switch {
		case worse(prev, entry.level):
			st.acknowledged = false
			audioEligible, reason := e.healthAudioEligibilityLocked(st, entry.level, now)
			ev = healthEvent(st, audioEligible, reason, now)
			if entry.level == ComponentNotReady {
				ev.Alert.Level = LevelSystemNotReady
			}
			ev.Kind = "new"
		case entry.level == ComponentReady && prev.rank() >= ComponentCaution.rank():
			ev = healthEvent(st, false, "recovery-silent-by-default", now)
			ev.Alert.Level = LevelInformation
			ev.Kind = "recovered"
		default:
			// An improving-but-not-fully-recovered transition (e.g.
			// NOT_READY -> CAUTION) - a quiet, visual-only note, no audio.
			ev = healthEvent(st, false, "quiet-by-default", now)
			ev.Alert.Level = LevelInformation
			ev.Kind = "deescalated"
		}
		ev.Seq = e.nextSeqLocked()
		events = append(events, ev)
		e.appendHistoryLocked(ev)
	}
	return events
}

func (e *Evaluator) healthAudioEligibilityLocked(st *healthComponentState, level ComponentLevel, now time.Time) (bool, string) {
	if !e.cfg.MasterEnabled {
		return false, "alerting-disabled"
	}
	if !e.cfg.SystemAudioEnabled {
		return false, "audio-disabled"
	}
	if e.mutedLocked(now) {
		return false, "muted"
	}
	if !st.lastAudioAt.IsZero() && now.Sub(st.lastAudioAt) < e.cfg.audioCooldownForBucket(tierCaution) {
		e.counters.SuppressedEvents++
		return false, "per-target-cooldown"
	}
	if !e.lastGlobalAudioAt.IsZero() && now.Sub(e.lastGlobalAudioAt) < time.Duration(e.cfg.GlobalMinAudioSpacingSeconds*float64(time.Second)) {
		e.counters.SuppressedEvents++
		return false, "global-min-spacing"
	}
	st.lastAudioAt = now
	e.lastGlobalAudioAt = now
	return true, ""
}

func healthEvent(st *healthComponentState, audioEligible bool, reason string, now time.Time) Event {
	return Event{
		Alert: Alert{
			ID:                    st.name,
			Category:              CategorySystem,
			Level:                 LevelSystemCaution,
			Component:             st.name,
			Message:               st.message + ": " + string(st.level),
			LastUpdatedAgeSeconds: now.Sub(st.lastTransitionAt).Seconds(),
			FirstSeenAgeSeconds:   now.Sub(st.lastTransitionAt).Seconds(),
			Acknowledged:          st.acknowledged,
			AudioEligible:         audioEligible,
			SuppressionReason:     reason,
			DataValidity:          "fresh",
			Disclaimer:            Disclaimer,
		},
	}
}

func healthAlert(st *healthComponentState, now time.Time) Alert {
	level := LevelSystemCaution
	if st.level == ComponentNotReady {
		level = LevelSystemNotReady
	}
	return Alert{
		ID:                    st.name,
		Category:              CategorySystem,
		Level:                 level,
		Component:             st.name,
		Message:               st.message + ": " + string(st.level),
		LastUpdatedAgeSeconds: now.Sub(st.lastTransitionAt).Seconds(),
		FirstSeenAgeSeconds:   now.Sub(st.lastTransitionAt).Seconds(),
		Acknowledged:          st.acknowledged,
		AudioEligible:         false,
		DataValidity:          "fresh",
		Disclaimer:            Disclaimer,
	}
}
