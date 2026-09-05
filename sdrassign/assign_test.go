package sdrassign

import (
	"strings"
	"testing"
)

func dev(index int, serial string) Device {
	return Device{Index: index, Serial: serial}
}

// permute returns every permutation of devices, used to prove the result
// does not depend on the order devices are discovered/supplied in.
func permute(devices []Device) [][]Device {
	if len(devices) <= 1 {
		return [][]Device{devices}
	}
	var out [][]Device
	for i := range devices {
		rest := make([]Device, 0, len(devices)-1)
		rest = append(rest, devices[:i]...)
		rest = append(rest, devices[i+1:]...)
		for _, p := range permute(rest) {
			perm := append([]Device{devices[i]}, p...)
			out = append(out, perm)
		}
	}
	return out
}

func assertEqualAssignment(t *testing.T, got, want Assignment, label string) {
	t.Helper()
	if got.Enabled != want.Enabled ||
		got.Detected != want.Detected ||
		got.Assigned != want.Assigned ||
		got.Device != want.Device ||
		got.Source != want.Source ||
		got.Ambiguous != want.Ambiguous ||
		got.Conflict != want.Conflict ||
		got.ExternallySatisfied != want.ExternallySatisfied ||
		got.IdentityUnstable != want.IdentityUnstable {
		t.Errorf("%s: got %+v, want %+v", label, got, want)
	}
}

// --- Determinism -----------------------------------------------------------

func TestAssign_DeterministicAcrossInputOrder(t *testing.T) {
	devices := []Device{
		dev(0, "stratux:978:0"),
		dev(1, "stratux:1090:0"),
		dev(2, ""),
	}
	var first Result
	for i, perm := range permute(devices) {
		r := Assign(perm, true, true, false, false, false)
		if i == 0 {
			first = r
			continue
		}
		assertEqualAssignment(t, r.UAT, first.UAT, "UAT")
		assertEqualAssignment(t, r.ES, first.ES, "ES")
	}
}

func TestAssign_DeterministicAcrossInputOrder_AmbiguousAnonymous(t *testing.T) {
	devices := []Device{dev(0, ""), dev(1, "")}
	var first Result
	for i, perm := range permute(devices) {
		r := Assign(perm, true, true, false, false, false)
		if i == 0 {
			first = r
			continue
		}
		assertEqualAssignment(t, r.UAT, first.UAT, "UAT")
		assertEqualAssignment(t, r.ES, first.ES, "ES")
	}
	if !first.UAT.Ambiguous || !first.ES.Ambiguous {
		t.Fatalf("expected both bands ambiguous, got UAT=%+v ES=%+v", first.UAT, first.ES)
	}
}

func TestAssign_RepeatedEvaluationIsStable(t *testing.T) {
	devices := []Device{dev(0, "stratux:978:0"), dev(1, "")}
	r1 := Assign(devices, true, true, false, false, false)
	r2 := Assign(devices, true, true, false, false, false)
	assertEqualAssignment(t, r1.UAT, r2.UAT, "UAT")
	assertEqualAssignment(t, r1.ES, r2.ES, "ES")
}

// --- Tagged assignments ------------------------------------------------------

func TestAssign_TaggedDevicesGoToDeclaredBand(t *testing.T) {
	devices := []Device{dev(0, "stratux:978:0"), dev(1, "stratux:1090:0")}
	r := Assign(devices, true, true, false, false, false)

	if !r.UAT.Assigned || r.UAT.Source != SourceTagged || r.UAT.Device.Index != 0 {
		t.Errorf("UAT: got %+v", r.UAT)
	}
	if !r.ES.Assigned || r.ES.Source != SourceTagged || r.ES.Device.Index != 1 {
		t.Errorf("ES: got %+v", r.ES)
	}
	if r.UAT.Ambiguous || r.ES.Ambiguous || r.UAT.Conflict || r.ES.Conflict {
		t.Errorf("tagged assignment should not be ambiguous or conflicted: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
}

// TestAssign_TaggedReasonQuotesSerial guards against unescaped device
// serials reaching diagnostic/log text: the EEPROM serial is user/hardware
// controlled and should always be inserted with Go-syntax quoting (%q),
// which escapes control characters, rather than raw %s.
func TestAssign_TaggedReasonQuotesSerial(t *testing.T) {
	devices := []Device{dev(0, "stratux:978:0")}
	r := Assign(devices, true, false, false, false, false)
	if !strings.Contains(r.UAT.Reason, `"stratux:978:0"`) {
		t.Errorf("expected the serial to be quoted in the reason text, got %q", r.UAT.Reason)
	}
}

func TestAssign_TaggedDevicesReversedOrderSameResult(t *testing.T) {
	forward := []Device{dev(0, "stratux:978:0"), dev(1, "stratux:1090:0")}
	reversed := []Device{dev(1, "stratux:1090:0"), dev(0, "stratux:978:0")}

	rf := Assign(forward, true, true, false, false, false)
	rr := Assign(reversed, true, true, false, false, false)

	assertEqualAssignment(t, rf.UAT, rr.UAT, "UAT")
	assertEqualAssignment(t, rf.ES, rr.ES, "ES")
}

func TestAssign_TaggedNeverCrossesToOtherBand(t *testing.T) {
	// Only a 978-tagged device is present, but ES is enabled too. The
	// 978-tagged device must never be handed to ES, tagged or anonymous.
	devices := []Device{dev(0, "stratux:978:0")}
	r := Assign(devices, true, true, false, false, false)

	if !r.UAT.Assigned || r.UAT.Device.Index != 0 {
		t.Fatalf("expected UAT assigned to tagged device: %+v", r.UAT)
	}
	if r.ES.Assigned {
		t.Fatalf("ES must not receive the 978-tagged device: %+v", r.ES)
	}
}

func TestAssign_DuplicateTagsAreConflicted(t *testing.T) {
	tests := []struct {
		name   string
		serial string
		band   func(Result) Assignment
	}{
		{"978", "stratux:978:0", func(r Result) Assignment { return r.UAT }},
		{"1090", "stratux:1090:0", func(r Result) Assignment { return r.ES }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			devices := []Device{dev(0, tc.serial), dev(1, tc.serial)}
			r := Assign(devices, true, true, false, false, false)
			a := tc.band(r)
			if !a.Conflict {
				t.Fatalf("expected conflict reported, got %+v", a)
			}
			if !a.Assigned || a.Device.Index != 0 {
				t.Fatalf("expected first (lowest-index) tagged device retained, got %+v", a)
			}
			if a.Reason == "" {
				t.Errorf("expected a human-readable conflict reason")
			}
		})
	}
}

func TestAssign_MissingTaggedHardwareIsDegraded(t *testing.T) {
	// UAT tagged device present, ES tagged device absent, and no anonymous
	// spares: ES must show enabled-but-not-detected, not a false healthy
	// state, and must not silently claim the UAT device.
	devices := []Device{dev(0, "stratux:978:0")}
	r := Assign(devices, true, true, false, false, false)

	if r.ES.Assigned || r.ES.Detected {
		t.Fatalf("ES should be undetected/unassigned when its tagged SDR is missing: %+v", r.ES)
	}
	if r.ES.Ambiguous {
		t.Fatalf("a single missing tagged receiver is a degraded state, not ambiguity: %+v", r.ES)
	}
	if r.ES.Reason == "" {
		t.Errorf("expected a diagnostic reason for the missing receiver")
	}
}

func TestAssign_UnsupportedTagIsReportedAndUnused(t *testing.T) {
	devices := []Device{dev(0, "stratux:2400:0")}
	r := Assign(devices, true, true, false, false, false)

	if r.UAT.Assigned || r.ES.Assigned {
		t.Fatalf("an unsupported tag must not be used for any band: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected a warning about the unsupported tag")
	}
}

// --- Anonymous assignments ---------------------------------------------------

func TestAssign_SingleBandSingleAnonymousDevice(t *testing.T) {
	devices := []Device{dev(0, "")}
	r := Assign(devices, true, false, false, false, false)

	if !r.UAT.Assigned || r.UAT.Source != SourceAnonymous || r.UAT.Device.Index != 0 {
		t.Fatalf("expected deterministic anonymous assignment, got %+v", r.UAT)
	}
	if r.UAT.Ambiguous {
		t.Fatalf("single band/single device must not be ambiguous: %+v", r.UAT)
	}
}

func TestAssign_TwoBandsTwoAnonymousDevicesAreAmbiguous(t *testing.T) {
	devices := []Device{dev(0, ""), dev(1, "")}
	r := Assign(devices, true, true, false, false, false)

	if r.UAT.Assigned || r.ES.Assigned {
		t.Fatalf("ambiguous devices must not be assigned: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
	if !r.UAT.Ambiguous || !r.ES.Ambiguous {
		t.Fatalf("expected both bands flagged ambiguous: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
	if !r.UAT.Detected || !r.ES.Detected {
		t.Errorf("hardware was found even though assignment is ambiguous, Detected should be true: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
	if r.UAT.Reason == "" || r.ES.Reason == "" {
		t.Errorf("expected actionable diagnostic reasons")
	}
}

func TestAssign_TwoBandsOneAnonymousDeviceIsAmbiguous(t *testing.T) {
	// A single spare cannot be known to belong to either unmet band.
	devices := []Device{dev(0, "")}
	r := Assign(devices, true, true, false, false, false)

	if r.UAT.Assigned || r.ES.Assigned {
		t.Fatalf("must not guess which band the lone spare belongs to: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
	if !r.UAT.Ambiguous || !r.ES.Ambiguous {
		t.Fatalf("expected both bands flagged ambiguous: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
}

func TestAssign_OneMissingReceiverIsDegradedNotAmbiguous(t *testing.T) {
	// Both bands enabled, no devices at all connected.
	r := Assign(nil, true, true, false, false, false)

	if r.UAT.Ambiguous || r.ES.Ambiguous {
		t.Fatalf("no candidates at all is a missing-hardware state, not ambiguity: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
	if r.UAT.Assigned || r.ES.Assigned || r.UAT.Detected || r.ES.Detected {
		t.Fatalf("expected both bands undetected/unassigned: UAT=%+v ES=%+v", r.UAT, r.ES)
	}
}

func TestAssign_DisabledBandsDoNotConsumeOrFault(t *testing.T) {
	devices := []Device{dev(0, ""), dev(1, "")}
	r := Assign(devices, true, false, false, false, false)

	if r.ES.Enabled || r.ES.Assigned || r.ES.Ambiguous || r.ES.Conflict || r.ES.Detected {
		t.Fatalf("disabled ES must be inert: %+v", r.ES)
	}
	// The disabled band's absence must not create ambiguity for UAT: one
	// enabled band, two anonymous devices -> unambiguous single pick.
	if !r.UAT.Assigned || r.UAT.Ambiguous {
		t.Fatalf("UAT should deterministically claim a spare, unaffected by the disabled band: %+v", r.UAT)
	}
}

func TestAssign_ExtraAnonymousDevicesNotAssignedArbitrarily(t *testing.T) {
	devices := []Device{dev(0, ""), dev(1, ""), dev(2, "")}
	r := Assign(devices, true, false, false, false, false)

	if !r.UAT.Assigned || r.UAT.Device.Index != 0 {
		t.Fatalf("expected the lowest-index spare used deterministically: %+v", r.UAT)
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected a warning noting the unused extra spares")
	}
	// The role (which band gets a receiver) is unambiguous - only UAT is
	// enabled - but which of the three untagged devices fills it is picked
	// by enumeration index, which is not proven stable across reboots.
	// That must be surfaced, not silently treated as equivalent to the
	// single-candidate case.
	if !r.UAT.IdentityUnstable {
		t.Fatalf("expected IdentityUnstable when more than one untagged candidate exists: %+v", r.UAT)
	}
	if r.UAT.Ambiguous {
		t.Fatalf("IdentityUnstable is not the same as Ambiguous - the role itself is not in doubt: %+v", r.UAT)
	}
	if !strings.Contains(r.UAT.Reason, "not guaranteed to be stable") {
		t.Errorf("expected the reason to call out the stability caveat, got %q", r.UAT.Reason)
	}
}

func TestAssign_SingleAnonymousCandidateIsNotIdentityUnstable(t *testing.T) {
	devices := []Device{dev(0, "")}
	r := Assign(devices, true, false, false, false, false)
	if r.UAT.IdentityUnstable {
		t.Fatalf("a single anonymous candidate has nothing to be unstable relative to: %+v", r.UAT)
	}
}

// --- External UAT satisfaction (see docs/hardware/sdr-and-bands.md) --------

func TestAssign_ExternallySatisfiedUATFreesLoneSpareForES(t *testing.T) {
	// An external low-power UAT radio already covers 978, and a single
	// untagged dongle is present with both bands enabled. Without knowing
	// UAT is externally satisfied, this would look like two enabled bands
	// competing for one anonymous device (ambiguous). Knowing it, only ES
	// is actually unmet, so the pick is unambiguous.
	devices := []Device{dev(0, "")}
	r := Assign(devices, true, true, false, false, true /* uatSatisfiedExternally */)

	if r.UAT.Assigned || !r.UAT.ExternallySatisfied || r.UAT.Source != SourceExternal {
		t.Fatalf("expected UAT externally satisfied and unassigned: %+v", r.UAT)
	}
	if r.UAT.Ambiguous {
		t.Fatalf("externally satisfied UAT must not read as ambiguous: %+v", r.UAT)
	}
	// BuildBandStatus() is where "not degraded" is actually asserted; see
	// TestBuildBandStatus_ExternallySatisfiedIsNotDegraded in status_test.go.
	if !r.ES.Assigned || r.ES.Ambiguous || r.ES.Source != SourceAnonymous || r.ES.Device.Index != 0 {
		t.Fatalf("expected ES to unambiguously claim the lone spare: %+v", r.ES)
	}
}

func TestAssign_ExternallySatisfiedUATFreesBothSparesForES(t *testing.T) {
	// Same as above, but with two untagged spares present: still only ES
	// is unmet, so this must remain an unambiguous single pick, not
	// ambiguous just because more than one device exists.
	devices := []Device{dev(0, ""), dev(1, "")}
	r := Assign(devices, true, true, false, false, true)

	if !r.ES.Assigned || r.ES.Ambiguous {
		t.Fatalf("expected ES to unambiguously claim a spare: %+v", r.ES)
	}
	if r.UAT.Assigned {
		t.Fatalf("externally satisfied UAT must not consume a device: %+v", r.UAT)
	}
}

func TestAssign_ExternallySatisfiedUATStillYieldsToExplicitTag(t *testing.T) {
	// A dongle is explicitly tagged for 978 even though an external UAT
	// radio is also connected: the user's explicit tag must still win.
	devices := []Device{dev(0, "stratux:978:0")}
	r := Assign(devices, true, false, false, false, true)

	if !r.UAT.Assigned || r.UAT.Source != SourceTagged || r.UAT.ExternallySatisfied {
		t.Fatalf("expected the explicit tag to take priority over external satisfaction: %+v", r.UAT)
	}
}

func TestAssign_ExternalSatisfactionIgnoredWhenUATDisabled(t *testing.T) {
	// UAT disabled must still behave as disabled - external satisfaction
	// is irrelevant when the band itself is off.
	r := Assign(nil, false, true, false, false, true)
	if r.UAT.Enabled || r.UAT.ExternallySatisfied || r.UAT.Source != SourceNone {
		t.Fatalf("expected UAT to remain plainly disabled: %+v", r.UAT)
	}
}

// --- Regression tests (see docs/hardware/sdr-and-bands.md) -----------------

// TestRegression_ExternalUATDoesNotBlockAnonymousES is the regression test
// for STX-P1-REV-001: a single spare SDR intended for 1090 ES must not be
// misreported as ambiguous merely because 978 UAT is enabled in settings,
// when 978 is actually already served by an external low-power UAT radio.
func TestRegression_ExternalUATDoesNotBlockAnonymousES(t *testing.T) {
	devices := []Device{dev(0, "")}
	r := Assign(devices, true, true, false, false, true)
	if r.ES.Ambiguous {
		t.Fatalf("ES must not be reported ambiguous when UAT is externally satisfied: %+v", r.ES)
	}
	if !r.ES.Assigned {
		t.Fatalf("ES must be assigned the lone spare: %+v", r.ES)
	}
}

func TestRegression_AnonymousDualReceiversCannotSwapBandsSilently(t *testing.T) {
	devices := []Device{dev(0, ""), dev(1, "")}
	seen := map[string]bool{}
	for _, perm := range permute(devices) {
		r := Assign(perm, true, true, false, false, false)
		key := r.UAT.Source.String() + "/" + r.ES.Source.String()
		seen[key] = true
		if r.UAT.Assigned || r.ES.Assigned {
			t.Fatalf("expected no silent assignment for indistinguishable anonymous devices, got UAT=%+v ES=%+v", r.UAT, r.ES)
		}
	}
	if len(seen) != 1 {
		t.Fatalf("assignment source varied across input orders: %v", seen)
	}
}

func TestRegression_EnabledBandWithoutReceiverCannotAppearHealthy(t *testing.T) {
	r := Assign(nil, true, true, false, false, false)
	for _, a := range []Assignment{r.UAT, r.ES} {
		if a.Assigned {
			t.Fatalf("must not be Assigned with zero devices present: %+v", a)
		}
	}
}

func TestRegression_TaggedDualReceiversRetainStableRoles(t *testing.T) {
	base := []Device{dev(0, "stratux:978:0"), dev(1, "stratux:1090:0")}
	for round := 0; round < 5; round++ {
		for _, perm := range permute(base) {
			r := Assign(perm, true, true, false, false, false)
			if r.UAT.Device.Serial != "stratux:978:0" || r.ES.Device.Serial != "stratux:1090:0" {
				t.Fatalf("round %d: roles did not remain stable: UAT=%+v ES=%+v", round, r.UAT, r.ES)
			}
		}
	}
}

func TestString_BandAndSource(t *testing.T) {
	if UAT.String() == "" || ES.String() == "" || OGN.String() == "" || AIS.String() == "" {
		t.Errorf("band String() must not be empty")
	}
	if SourceTagged.String() != "tagged" {
		t.Errorf("SourceTagged.String() = %q, want tagged", SourceTagged.String())
	}
	if SourceAnonymous.String() != "anonymous" {
		t.Errorf("SourceAnonymous.String() = %q, want anonymous", SourceAnonymous.String())
	}
	if SourceNone.String() != "none" {
		t.Errorf("SourceNone.String() = %q, want none", SourceNone.String())
	}
}
