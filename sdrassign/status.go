package sdrassign

import (
	"fmt"
	"time"
)

// BandStatus is the fully-derived display status for one band: an
// Assignment combined with the live decoder-running and message-freshness
// signals the caller measured (main/sdr.go has no way to derive those
// itself without touching real device state, so they're passed in rather
// than recomputed here).
//
// It is a pure, hardware-free, cgo-free data structure. BuildBandStatus and
// DiagnosticReason - the functions that produce it - can therefore be
// exercised entirely with Go's standard testing package, unlike the rest of
// the main package (which requires cgo and a locally-built libdump978.so to
// even compile).
type BandStatus struct {
	Enabled             bool
	Detected            bool
	Assigned            bool
	DeviceSerial        string
	DeviceIndex         int
	AssignmentSource    string
	Ambiguous           bool
	Conflict            bool
	ExternallySatisfied bool
	IdentityUnstable    bool
	DecoderRunning      bool
	Receiving           bool
	Degraded            bool
	Reason              string
}

// BuildBandStatus derives one band's full display status from its
// Assignment plus the live decoder-running and message-freshness signals.
//
// A conflicted assignment (Conflict) never reports DecoderRunning or
// Receiving as true, even if the retained device's decoder genuinely is
// running and receiving: a duplicate-tag conflict must never read as fully
// healthy over the wire, only as something that needs the user's attention.
func BuildBandStatus(a Assignment, liveDecoderRunning, liveReceiving bool) BandStatus {
	healthySignal := a.Assigned && !a.Conflict
	s := BandStatus{
		Enabled:             a.Enabled,
		Detected:            a.Detected,
		Assigned:            a.Assigned,
		DeviceIndex:         -1,
		AssignmentSource:    a.Source.String(),
		Ambiguous:           a.Ambiguous,
		Conflict:            a.Conflict,
		ExternallySatisfied: a.ExternallySatisfied,
		IdentityUnstable:    a.IdentityUnstable,
		DecoderRunning:      healthySignal && liveDecoderRunning,
		Receiving:           healthySignal && liveDecoderRunning && liveReceiving,
		Degraded:            a.Enabled && !a.ExternallySatisfied && (!a.Assigned || a.Conflict || !liveDecoderRunning),
		Reason:              DiagnosticReason(a, liveDecoderRunning, liveReceiving),
	}
	if a.Assigned {
		s.DeviceSerial = a.Device.Serial
		s.DeviceIndex = a.Device.Index
	}
	return s
}

// IsReceiving reports whether a band should currently be considered to be
// receiving traffic, given the time its most recent message arrived, the
// time its currently-bound receiver was (re)assigned, the current time, and
// the freshness window.
//
// A message that arrived before the current receiver was assigned - e.g.
// buffered activity from a predecessor device that has since been replaced,
// or from before an ambiguous state was resolved by tagging - does not
// count. Without this, a freshly (re)assigned receiver could read as
// "receiving" for up to one freshness window on the strength of a
// receiver it doesn't share any hardware with.
//
// All three times should come from the same monotonic clock (the caller's
// stratuxClock, not wall time) so a real-time clock adjustment can't distort
// the freshness comparison.
func IsReceiving(lastMessageTime, assignedAt, now time.Time, freshness time.Duration) bool {
	if lastMessageTime.IsZero() {
		return false
	}
	if lastMessageTime.Before(assignedAt) {
		return false
	}
	return now.Sub(lastMessageTime) < freshness
}

// DiagnosticReason builds the human-readable status line shown in the web
// UI for one band. A disabled, externally-satisfied, unassigned, ambiguous
// or conflicted band already has a complete explanation from Assign() at
// assignment time; only a cleanly assigned band needs the live
// decoder/receiving state layered on top, since that can change every
// second without a reassignment happening.
func DiagnosticReason(a Assignment, decoderRunning, receiving bool) string {
	if !a.Enabled || a.ExternallySatisfied || !a.Assigned || a.Ambiguous || a.Conflict {
		return a.Reason
	}
	// IdentityUnstable bands are otherwise healthy - the live decoder/
	// receiving state below is still meaningful and shown - but the
	// device-identity caveat is important enough to always be visible
	// alongside it, not just at assignment time.
	var suffix string
	if a.IdentityUnstable {
		suffix = " Which SDR fills this role is not guaranteed to be stable across reboots; tag your SDRs with debian/sdr-tool.sh to fix that."
	}
	if !decoderRunning {
		return fmt.Sprintf("%s SDR assigned (index %d) but its decoder is not currently running.%s", a.Band, a.Device.Index, suffix)
	}
	if receiving {
		return fmt.Sprintf("%s receiving traffic.%s", a.Band, suffix)
	}
	return fmt.Sprintf("%s SDR active; no messages received in the last minute. This is expected when there is no nearby RF traffic.%s", a.Band, suffix)
}
