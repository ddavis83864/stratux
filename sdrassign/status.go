package sdrassign

import "fmt"

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
	if !decoderRunning {
		return fmt.Sprintf("%s SDR assigned (index %d) but its decoder is not currently running.", a.Band, a.Device.Index)
	}
	if receiving {
		return fmt.Sprintf("%s receiving traffic.", a.Band)
	}
	return fmt.Sprintf("%s SDR active; no messages received in the last minute. This is expected when there is no nearby RF traffic.", a.Band)
}
