package fisbcache

import "time"

// FISBTime is the raw FIS-B APDU broadcast-time encoding, exactly as
// uatparse.UATFrame.decodeTimeFormat already decodes it (FISB_month/day/
// hours/minutes/seconds) - never a full time.Time, because the FIS-B
// encoding itself never includes a year, and two of its four time-format
// options (t_opt 0 and 1) omit month/day entirely (see
// uatparse/uatparse.go's own decodeTimeFormat and its "make a new FISB
// Time structure" TODO). HasMonthDay distinguishes those two cases so
// ReconstructSourceTime never silently treats a genuinely-absent
// month/day as "January 0".
type FISBTime struct {
	HasMonthDay          bool
	Month, Day           uint32
	Hour, Minute, Second uint32
}

// SourceTime is one cache entry's reconstructed best estimate of when
// its product was actually issued/broadcast, plus how much that estimate
// can be trusted - never a claim of exact precision beyond what FIS-B
// itself encodes (seconds are frequently absent - see FISBTime).
type SourceTime struct {
	// Trusted is false whenever ReconstructSourceTime could not combine
	// FISBTime with a genuinely trusted wall clock (see
	// ReconstructSourceTime) - an untrusted SourceTime must never be
	// persisted as if it were reliable (see docs/fisb-weather-cache.md's
	// time-model section) and a caller must treat it as display-only,
	// never as an input to expiration or supersession-by-time decisions.
	Trusted bool
	// UTC is the reconstructed absolute time - the zero value when
	// !Trusted.
	UTC time.Time
}

// ReconstructSourceTime combines a FIS-B frame's own partial broadcast
// time with a trusted receive-time reference to produce a best-effort
// absolute UTC SourceTime. This is the ONLY place this package ever
// invents a year (FIS-B never encodes one) or a month/day (two of the
// four FIS-B time-format options never encode them) - and it always does
// so conservatively:
//
//   - receiveUTCTrusted must be true (the caller's own wall clock is
//     currently trusted - see readiness.TimeTrust in main/'s glue) or
//     the result is always !Trusted with a zero UTC. This package never
//     guesses a year from an untrusted clock.
//   - If ft.HasMonthDay is true, the reconstructed date uses ft's own
//     month/day with receiveUTC's year, then rolls the year back by one
//     if that would place the reconstructed time more than
//     maxFutureSkew ahead of receiveUTC (the classic "broadcast just
//     before, received just after, a New Year's Eve/Day boundary" case)
//   - this is the only correction ever applied; a result still more
//     than maxFutureSkew in the future after that one rollback, or more
//     than maxPastSkew behind receiveUTC, is treated as untrustworthy
//     (!Trusted) rather than accepted at face value, since a
//     genuinely-corrupt or misdecoded FISBTime is far more likely than a
//     real FIS-B broadcast being that far from the receive time.
//   - If ft.HasMonthDay is false, receiveUTC's own year/month/day are
//     used with ft's hour/minute/second - the same maxFutureSkew/
//     maxPastSkew bounds still apply (this catches, e.g., an hour value
//     that would place the result implausibly far from receive time due
//     to a decode error), with a day rollback (not year) as the one
//     correction attempted for a time-of-day that appears to be just
//     before receive-time's own UTC midnight.
func ReconstructSourceTime(ft FISBTime, receiveUTC time.Time, receiveUTCTrusted bool) SourceTime {
	if !receiveUTCTrusted || receiveUTC.IsZero() {
		return SourceTime{}
	}
	if ft.Hour > 23 || ft.Minute > 59 || ft.Second > 59 {
		return SourceTime{}
	}

	var candidate time.Time
	if ft.HasMonthDay {
		if ft.Month < 1 || ft.Month > 12 || ft.Day < 1 || ft.Day > 31 {
			return SourceTime{}
		}
		candidate = time.Date(receiveUTC.Year(), time.Month(ft.Month), int(ft.Day), int(ft.Hour), int(ft.Minute), int(ft.Second), 0, time.UTC)
		if candidate.Sub(receiveUTC) > maxFutureSkew {
			candidate = time.Date(receiveUTC.Year()-1, time.Month(ft.Month), int(ft.Day), int(ft.Hour), int(ft.Minute), int(ft.Second), 0, time.UTC)
		}
	} else {
		candidate = time.Date(receiveUTC.Year(), receiveUTC.Month(), receiveUTC.Day(), int(ft.Hour), int(ft.Minute), int(ft.Second), 0, time.UTC)
		if candidate.Sub(receiveUTC) > maxFutureSkew {
			candidate = candidate.AddDate(0, 0, -1)
		}
	}

	skew := candidate.Sub(receiveUTC)
	if skew > maxFutureSkew || skew < -maxPastSkew {
		return SourceTime{}
	}
	return SourceTime{Trusted: true, UTC: candidate}
}

const (
	// maxFutureSkew bounds how far a reconstructed source time may lie
	// ahead of the trusted receive time before being rejected as
	// untrustworthy - a real broadcast is never received before it was
	// sent; a small allowance covers only clock-domain rounding, not a
	// genuinely different day.
	maxFutureSkew = 5 * time.Minute
	// maxPastSkew bounds how far behind receive time a reconstructed
	// source time may lie - generous enough for a legitimately delayed/
	// rebroadcast product (FIS-B ground stations rebroadcast the same
	// uplink message repeatedly for some time), conservative enough to
	// catch a decode error landing on the wrong day/month.
	maxPastSkew = 6 * time.Hour
)
