package alerting

import (
	"math"
	"time"
)

// TrafficObservation is one target's freshly-recomputed state for the
// current evaluation cycle, built by the caller (main/alertingapi.go) from
// the existing main.TrafficInfo - see docs/alerting.md's "Traffic data
// inputs" section for exactly which existing fields feed each value here.
type TrafficObservation struct {
	// TargetID is a sanitized, already-public identifier (e.g. the same
	// hex ICAO address string already broadcast in the clear and already
	// shown on the existing Traffic page).
	TargetID string

	// IsOwnship must be true when the caller's own isOwnshipTrafficInfo
	// check identified this observation as ownship - checked again here,
	// defense in depth (see docs/alerting.md's "Ownship-exclusion
	// behavior").
	IsOwnship bool

	PositionValid  bool
	DistanceValid  bool
	DistanceMeters float64

	RelativeAltitudeValid bool
	RelativeAltitudeFeet  float64

	// AgeSeconds is the same monotonic age already computed by
	// sendTrafficUpdates() for this target (ti.Age).
	AgeSeconds float64

	OnGround bool

	ClockDirectionValid bool
	ClockDirection      ClockDirection
}

// tier is the internal 0-3 classification: 0 = not eligible/below notice,
// 1 = notice, 2 = caution, 3 = high caution. Tiers 2 and 3 both surface as
// the single public LevelTrafficCaution (see docs/alerting.md) - tier is
// retained internally only to drive hysteresis/cooldown/message wording.
const (
	tierNone        = 0
	tierNotice      = 1
	tierCaution     = 2
	tierHighCaution = 3
)

func levelForTier(t int) Level {
	switch {
	case t >= tierCaution:
		return LevelTrafficCaution
	case t == tierNotice:
		return LevelTrafficNotice
	default:
		return LevelInformation
	}
}

// classifyTier is a pure function of one observation and a threshold
// configuration - deterministic and exhaustively unit-tested in
// traffic_test.go. It never mutates any state and has no memory: hysteresis
// is applied by the caller (targetTrafficState.update) by calling this
// twice, once with cfg and once with a widened ("exit") copy of cfg.
func classifyTier(obs TrafficObservation, cfg Config) (tier int, validity string) {
	if !obs.PositionValid || !obs.DistanceValid {
		return tierNone, "position-invalid"
	}
	if math.IsNaN(obs.DistanceMeters) || math.IsInf(obs.DistanceMeters, 0) || obs.DistanceMeters < 0 {
		return tierNone, "invalid-distance"
	}
	if math.IsNaN(obs.AgeSeconds) || math.IsInf(obs.AgeSeconds, 0) || obs.AgeSeconds < 0 || obs.AgeSeconds > cfg.StaleAfterSeconds {
		return tierNone, "stale"
	}
	if obs.DistanceMeters > cfg.MonitoringHorizontalMeters {
		return tierNone, "outside-envelope"
	}

	altValid := obs.RelativeAltitudeValid && !math.IsNaN(obs.RelativeAltitudeFeet) && !math.IsInf(obs.RelativeAltitudeFeet, 0)
	if !altValid {
		// Horizontal-only match: conservative NOTICE-only ceiling - never
		// a CAUTION tier without a trustworthy relative altitude.
		if obs.DistanceMeters <= cfg.NoticeHorizontalMeters {
			return tierNotice, "altitude-unavailable"
		}
		return tierNone, "altitude-unavailable"
	}

	absAlt := math.Abs(obs.RelativeAltitudeFeet)
	if absAlt > cfg.MonitoringVerticalFeet {
		return tierNone, "outside-envelope"
	}

	tier = tierNone
	if obs.DistanceMeters <= cfg.NoticeHorizontalMeters && absAlt <= cfg.NoticeVerticalFeet {
		tier = tierNotice
	}
	groundCapped := cfg.SuppressGroundTraffic && obs.OnGround
	if !groundCapped {
		if obs.DistanceMeters <= cfg.CautionHorizontalMeters && absAlt <= cfg.CautionVerticalFeet {
			tier = tierCaution
		}
		if obs.DistanceMeters <= cfg.HighCautionHorizontalMeters && absAlt <= cfg.HighCautionVerticalFeet {
			tier = tierHighCaution
		}
	}
	return tier, "fresh"
}

// widen returns a copy of cfg with every entry threshold multiplied by
// ExitHysteresisFactor - used only to evaluate whether a target already at
// a tier should still be considered "inside" that tier for de-escalation
// purposes.
func (c Config) widen() Config {
	w := c
	f := c.ExitHysteresisFactor
	w.NoticeHorizontalMeters *= f
	w.NoticeVerticalFeet *= f
	w.CautionHorizontalMeters *= f
	w.CautionVerticalFeet *= f
	w.HighCautionHorizontalMeters *= f
	w.HighCautionVerticalFeet *= f
	// Monitoring envelope also widens, consistent with every other bound,
	// so a target already being tracked isn't dropped outright by a small
	// noisy excursion just past the monitoring edge.
	w.MonitoringHorizontalMeters *= f
	w.MonitoringVerticalFeet *= f
	return w
}

// targetTrafficState is the per-target memory the Evaluator keeps between
// cycles - never persisted to disk (see docs/alerting.md: "reset on daemon
// restart").
type targetTrafficState struct {
	id string

	currentTier int
	validity    string

	// belowExitSince is zero (IsZero()) while the target still qualifies
	// under the widened exit thresholds for currentTier; otherwise it is
	// the monotonic instant the target first dropped below the exit
	// band, used to enforce DeescalateDwellSeconds.
	belowExitSince time.Time

	firstSeenAt   time.Time
	lastUpdatedAt time.Time

	acknowledged bool
	ackTier      int

	// lastAudioAt[tier] is the monotonic instant of the last
	// audio-eligible event for this target at this tier (only tierNotice
	// and tierCaution/tierHighCaution buckets are used - the latter two
	// share one cooldown bucket since they're the same public Level).
	lastAudioAt map[int]time.Time

	last TrafficObservation
}

func audioCooldownBucket(tier int) int {
	if tier >= tierCaution {
		return tierCaution
	}
	return tierNotice
}

func (c Config) audioCooldownForBucket(bucket int) time.Duration {
	if bucket == tierCaution {
		return time.Duration(c.AudioCooldownCautionSeconds * float64(time.Second))
	}
	return time.Duration(c.AudioCooldownNoticeSeconds * float64(time.Second))
}
