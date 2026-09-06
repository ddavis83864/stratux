package sdrassign

import (
	"testing"
	"time"
)

// disabledAssignment / healthyAssignment / etc. build minimal Assignment
// values for exercising BuildBandStatus/DiagnosticReason directly, without
// going through Assign(). This is the coverage that was previously missing
// entirely for the status-generation logic (it used to live in main/sdr.go,
// a cgo package that can't be unit tested without a locally-built
// libdump978.so).

func TestBuildBandStatus_Disabled(t *testing.T) {
	a := Assignment{Band: UAT, Enabled: false, Reason: "978 UAT is disabled."}
	s := BuildBandStatus(a, false, false)
	if s.Degraded {
		t.Errorf("a disabled band must never be Degraded: %+v", s)
	}
	if s.Assigned || s.DecoderRunning || s.Receiving || s.Ambiguous || s.Conflict {
		t.Errorf("a disabled band must be entirely inert: %+v", s)
	}
	if s.DeviceIndex != -1 || s.DeviceSerial != "" {
		t.Errorf("expected no device identity for an unassigned band: %+v", s)
	}
}

func TestBuildBandStatus_EnabledNotDetected(t *testing.T) {
	a := Assignment{Band: ES, Enabled: true, Reason: "1090 ES is enabled but no compatible SDR was detected."}
	s := BuildBandStatus(a, false, false)
	if !s.Degraded {
		t.Errorf("enabled with no detected hardware must be Degraded: %+v", s)
	}
	if s.Assigned {
		t.Errorf("must not be Assigned: %+v", s)
	}
}

func TestBuildBandStatus_Ambiguous(t *testing.T) {
	a := Assignment{Band: ES, Enabled: true, Detected: true, Ambiguous: true, Reason: "ambiguous"}
	s := BuildBandStatus(a, true, true) // even if live signals claim otherwise
	if s.Assigned || s.DecoderRunning || s.Receiving {
		t.Fatalf("an ambiguous band must never appear assigned or operational, regardless of live signals: %+v", s)
	}
	if !s.Degraded {
		t.Errorf("ambiguous should read as degraded: %+v", s)
	}
}

// TestBuildBandStatus_ConflictNeverAppearsHealthy is the regression test for
// STX-P1-REV-003: a duplicate-tag conflict must never report DecoderRunning
// or Receiving as true, even when the retained device really is running and
// receiving, and must always be treated as Degraded.
func TestBuildBandStatus_ConflictNeverAppearsHealthy(t *testing.T) {
	a := Assignment{
		Band: UAT, Enabled: true, Detected: true, Assigned: true, Conflict: true,
		Device: Device{Index: 0, Serial: "stratux:978:0"}, Source: SourceTagged,
		Reason: "Multiple SDRs are tagged for 978 UAT.",
	}
	s := BuildBandStatus(a, true /* live decoder running */, true /* live receiving */)
	if s.DecoderRunning {
		t.Errorf("Conflict must suppress DecoderRunning even if the live signal is true: %+v", s)
	}
	if s.Receiving {
		t.Errorf("Conflict must suppress Receiving even if the live signal is true: %+v", s)
	}
	if !s.Degraded {
		t.Errorf("Conflict must always read as Degraded: %+v", s)
	}
	if s.Reason != a.Reason {
		t.Errorf("expected the conflict reason to be surfaced verbatim, got %q", s.Reason)
	}
}

func TestBuildBandStatus_AssignedDecoderStopped(t *testing.T) {
	a := Assignment{Band: ES, Enabled: true, Detected: true, Assigned: true, Source: SourceTagged,
		Device: Device{Index: 1, Serial: "stratux:1090:0"}}
	s := BuildBandStatus(a, false, false)
	if s.DecoderRunning || s.Receiving {
		t.Fatalf("decoder stopped must not report running/receiving: %+v", s)
	}
	if !s.Degraded {
		t.Errorf("assigned with a stopped decoder must be Degraded: %+v", s)
	}
}

func TestBuildBandStatus_DecoderRunningNoTrafficIsNotDegraded(t *testing.T) {
	a := Assignment{Band: UAT, Enabled: true, Detected: true, Assigned: true, Source: SourceAnonymous,
		Device: Device{Index: 0, Serial: ""}}
	s := BuildBandStatus(a, true, false)
	if s.Degraded {
		t.Errorf("no recent messages must not by itself mean degraded: %+v", s)
	}
	if s.Receiving {
		t.Errorf("must not report Receiving with no recent messages: %+v", s)
	}
	if !s.DecoderRunning {
		t.Errorf("decoder is live-running, must be reported as such: %+v", s)
	}
}

func TestBuildBandStatus_FullyHealthy(t *testing.T) {
	a := Assignment{Band: ES, Enabled: true, Detected: true, Assigned: true, Source: SourceTagged,
		Device: Device{Index: 2, Serial: "stratux:1090:0"}}
	s := BuildBandStatus(a, true, true)
	if !s.DecoderRunning || !s.Receiving {
		t.Fatalf("expected fully healthy: %+v", s)
	}
	if s.Degraded {
		t.Errorf("fully healthy must not be Degraded: %+v", s)
	}
	if s.DeviceSerial != "stratux:1090:0" || s.DeviceIndex != 2 {
		t.Errorf("expected device identity to be surfaced: %+v", s)
	}
}

// TestBuildBandStatus_ExternallySatisfiedIsNotDegraded is the regression
// test for the status side of STX-P1-REV-001: a band that doesn't need an
// SDR because it's served externally must read as healthy, not missing.
func TestBuildBandStatus_ExternallySatisfiedIsNotDegraded(t *testing.T) {
	a := Assignment{Band: UAT, Enabled: true, Source: SourceExternal, ExternallySatisfied: true,
		Reason: "978 UAT is enabled and already served by an external low-power UAT radio; no RTL-SDR is needed for this band."}
	s := BuildBandStatus(a, false, false)
	if s.Degraded {
		t.Fatalf("externally satisfied must not be reported degraded: %+v", s)
	}
	if s.Assigned || s.DecoderRunning || s.Receiving {
		t.Errorf("externally satisfied binds no RTL-SDR, so these must stay false: %+v", s)
	}
	if s.AssignmentSource != "external" {
		t.Errorf("expected AssignmentSource external, got %q", s.AssignmentSource)
	}
	if s.Reason != a.Reason {
		t.Errorf("expected the external-satisfaction reason surfaced verbatim, got %q", s.Reason)
	}
}

func TestDiagnosticReason_MatchesStructuredState(t *testing.T) {
	tests := []struct {
		name string
		a    Assignment
		dec  bool
		recv bool
		want string
	}{
		{"disabled uses assign-time reason", Assignment{Enabled: false, Reason: "x disabled"}, true, true, "x disabled"},
		{"ambiguous uses assign-time reason", Assignment{Enabled: true, Ambiguous: true, Reason: "x ambiguous"}, true, true, "x ambiguous"},
		{"conflict uses assign-time reason even if healthy", Assignment{Enabled: true, Assigned: true, Conflict: true, Reason: "x conflict"}, true, true, "x conflict"},
		{"externally satisfied uses assign-time reason", Assignment{Enabled: true, ExternallySatisfied: true, Reason: "x external"}, true, true, "x external"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DiagnosticReason(tc.a, tc.dec, tc.recv)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// --- IsReceiving -------------------------------------------------------

// TestIsReceiving_StaleMessageDoesNotSurviveReassignment is the regression
// test for the scenario where a band is reassigned to a different receiver
// (or an ambiguous state is resolved) and a message buffered from the prior
// receiver, still within the freshness window, must not make the new
// assignment appear to be receiving.
func TestIsReceiving_StaleMessageDoesNotSurviveReassignment(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	lastMessage := now.Add(-5 * time.Second) // fresh by freshness window alone
	assignedAt := now.Add(-1 * time.Second)  // reassigned after that message arrived

	if IsReceiving(lastMessage, assignedAt, now, time.Minute) {
		t.Fatalf("a message received before the current receiver was assigned must not count as receiving")
	}
}

func TestIsReceiving_NoMessageEver(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	if IsReceiving(time.Time{}, time.Time{}, now, time.Minute) {
		t.Fatalf("zero-value last-message time must never read as receiving")
	}
}

func TestIsReceiving_FreshAfterAssignment(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	assignedAt := now.Add(-30 * time.Second)
	lastMessage := now.Add(-5 * time.Second)
	if !IsReceiving(lastMessage, assignedAt, now, time.Minute) {
		t.Fatalf("a recent message after assignment must count as receiving")
	}
}

func TestIsReceiving_ExpiredFreshnessWindow(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	assignedAt := now.Add(-2 * time.Minute)
	lastMessage := now.Add(-90 * time.Second) // after assignment, but outside a 60s window
	if IsReceiving(lastMessage, assignedAt, now, time.Minute) {
		t.Fatalf("a message outside the freshness window must not count as receiving")
	}
}

func TestIsReceiving_ExactlyAtFreshnessBoundary(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	assignedAt := now.Add(-2 * time.Minute)
	lastMessage := now.Add(-time.Minute) // exactly at the boundary: not strictly before "now"
	if IsReceiving(lastMessage, assignedAt, now, time.Minute) {
		t.Fatalf("a message exactly at the freshness boundary must be treated as expired (now.Sub(t) < freshness is false when equal)")
	}
	// One nanosecond inside the window must count.
	if !IsReceiving(lastMessage.Add(time.Nanosecond), assignedAt, now, time.Minute) {
		t.Fatalf("a message one nanosecond inside the freshness window must count as receiving")
	}
}

func TestIsReceiving_AssignedExactlyAtMessageTime(t *testing.T) {
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	t0 := now.Add(-10 * time.Second)
	// A message that arrived at the exact instant of (re)assignment is not
	// "before" assignedAt, so it is not stale.
	if !IsReceiving(t0, t0, now, time.Minute) {
		t.Fatalf("a message at the exact assignment instant must not be treated as stale")
	}
}
