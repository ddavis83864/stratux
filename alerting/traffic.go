package alerting

import (
	"math"
	"time"

	"github.com/stratux/stratux/trafficcpa"
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

	// CPA is this target's closure-rate/closest-point-of-approach trend
	// estimate, computed by the caller (main/trafficcpaapi.go, via
	// trafficcpa.Compute) - nil whenever CPA calculation has not been
	// performed for this cycle (e.g. the feature is not enabled, or
	// ownship/target data did not support it). A non-nil CPA whose own
	// Valid field is false carries only diagnostic information (e.g.
	// RejectReason) and must never be treated as escalation-worthy - see
	// classifyTier's own doc comment.
	CPA *trafficcpa.Result
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
//
// CPA escalation (cpaEscalated's own return value): after the existing,
// unmodified distance/altitude classification above produces tier and
// validity, a valid, sufficiently reliable closure-rate/CPA trend
// estimate (obs.CPA) may raise tier by EXACTLY ONE LEVEL - never invent a
// tier for a target this envelope check already decided is tierNone
// (outside the monitored area, stale, or otherwise invalid), and never
// lower a tier the distance/altitude check already produced. This is
// what makes CPA a strictly ADDITIVE trend input, provably unable to
// weaken or suppress any alert the existing policy would otherwise
// produce - see docs/traffic-cpa-alerting.md's "Alert escalation policy"
// section and TestClassifyTier_CPANeverWeakensAnAlert.
func classifyTier(obs TrafficObservation, cfg Config) (tier int, validity string, cpaEscalated bool) {
	if !obs.PositionValid || !obs.DistanceValid {
		return tierNone, "position-invalid", false
	}
	if math.IsNaN(obs.DistanceMeters) || math.IsInf(obs.DistanceMeters, 0) || obs.DistanceMeters < 0 {
		return tierNone, "invalid-distance", false
	}
	if math.IsNaN(obs.AgeSeconds) || math.IsInf(obs.AgeSeconds, 0) || obs.AgeSeconds < 0 || obs.AgeSeconds > cfg.StaleAfterSeconds {
		return tierNone, "stale", false
	}
	if obs.DistanceMeters > cfg.MonitoringHorizontalMeters {
		return tierNone, "outside-envelope", false
	}

	groundCapped := cfg.SuppressGroundTraffic && obs.OnGround

	altValid := obs.RelativeAltitudeValid && !math.IsNaN(obs.RelativeAltitudeFeet) && !math.IsInf(obs.RelativeAltitudeFeet, 0)
	if !altValid {
		// Horizontal-only match: conservative NOTICE-only ceiling - never
		// a CAUTION tier without a trustworthy relative altitude, and
		// CPA never escalates past this ceiling either (cpaWarrantsEscalation
		// itself also requires obs.RelativeAltitudeValid, but returning
		// early here keeps that invariant obvious without relying on it).
		if obs.DistanceMeters <= cfg.NoticeHorizontalMeters {
			return tierNotice, "altitude-unavailable", false
		}
		return tierNone, "altitude-unavailable", false
	}

	absAlt := math.Abs(obs.RelativeAltitudeFeet)
	if absAlt > cfg.MonitoringVerticalFeet {
		return tierNone, "outside-envelope", false
	}

	tier = tierNone
	if obs.DistanceMeters <= cfg.NoticeHorizontalMeters && absAlt <= cfg.NoticeVerticalFeet {
		tier = tierNotice
	}
	if !groundCapped {
		if obs.DistanceMeters <= cfg.CautionHorizontalMeters && absAlt <= cfg.CautionVerticalFeet {
			tier = tierCaution
		}
		if obs.DistanceMeters <= cfg.HighCautionHorizontalMeters && absAlt <= cfg.HighCautionVerticalFeet {
			tier = tierHighCaution
		}
	}

	if tier > tierNone && tier < tierHighCaution {
		var nextHorizontal, nextVertical float64
		if tier == tierNotice {
			nextHorizontal, nextVertical = cfg.CautionHorizontalMeters, cfg.CautionVerticalFeet
		} else {
			nextHorizontal, nextVertical = cfg.HighCautionHorizontalMeters, cfg.HighCautionVerticalFeet
		}
		if cpaWarrantsEscalation(cfg, obs, groundCapped, nextHorizontal, nextVertical) {
			tier++
			cpaEscalated = true
		}
	}

	return tier, "fresh", cpaEscalated
}

// cpaWarrantsEscalation reports whether obs's CPA estimate justifies
// raising the tier ALREADY computed by distance/altitude alone by
// exactly one level (to a tier whose own existing horizontal/vertical
// entry thresholds are nextHorizontalMeters/nextVerticalFeet) - reusing
// the already-reviewed Notice/Caution/HighCaution thresholds rather than
// a new, separately-invented CPA-specific distance. Every condition here
// is a conservative, explicit precondition; see
// docs/traffic-cpa-alerting.md's "Alert escalation policy" section for
// the rationale behind each one.
func cpaWarrantsEscalation(cfg Config, obs TrafficObservation, groundCapped bool, nextHorizontalMeters, nextVerticalFeet float64) bool {
	if !cfg.CPAEscalationEnabled {
		return false
	}
	if obs.CPA == nil || !obs.CPA.Valid {
		return false
	}
	if groundCapped {
		return false
	}
	// An escalation is never based on a vertical PREDICTION when the
	// CURRENT relative altitude itself could not be trusted - the
	// caller's own altitude-unavailable path already caps the base tier
	// at tierNotice for exactly this reason, but this is checked
	// explicitly here too rather than relied upon implicitly.
	if !obs.RelativeAltitudeValid {
		return false
	}
	// A TCPA clamped to the configured horizon means the true closest
	// approach falls beyond it - not yet an imminent trend.
	if obs.CPA.TCPAClampedToHorizon {
		return false
	}
	if !obs.CPA.HorizontalClosureRateValid || obs.CPA.HorizontalClosureRateKnots < cfg.CPAMinClosureRateKnots {
		return false
	}
	if !obs.CPA.PredictedHorizontalSeparationValid || obs.CPA.PredictedHorizontalSeparationMeters > nextHorizontalMeters {
		return false
	}
	if obs.CPA.Confidence == trafficcpa.ConfidenceHigh {
		// A vertical prediction IS available - it must also fit the
		// next tier's own vertical threshold, or this is not a genuine
		// vertical-converging trend worth escalating for.
		if !obs.CPA.PredictedVerticalSeparationValid || math.Abs(obs.CPA.PredictedVerticalSeparationFeet) > nextVerticalFeet {
			return false
		}
	}
	// Confidence == Medium (no vertical prediction available): escalation
	// is still permitted on the horizontal trend alone, exactly one
	// tier at a time - matching classifyTier's own existing
	// altitude-unavailable conservatism elsewhere (never more than one
	// tier without a trustworthy vertical component).
	return true
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

	// cpaEscalated mirrors classifyTier's own return value from the most
	// recent update - whether a valid CPA estimate actually raised this
	// target's current tier above what distance/altitude alone produced.
	cpaEscalated bool

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
