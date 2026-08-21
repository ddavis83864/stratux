/*
Package sdrassign implements deterministic RTL-SDR dongle-to-band
assignment for Stratux's primary receiver bands.

Historically this decision was made by ranging over a Go map of
not-yet-assigned dongles ("unusedIDs" in main/sdr.go). Go intentionally
randomizes map iteration order, so when two or more untagged
("anonymous") dongles were present, which physical dongle ended up
serving which band could change from one process start to the next.

Assign() replaces that with a pure function: given the same set of
discovered devices and the same enabled bands, it always produces the
same result, regardless of the order devices are supplied in. It has
no dependency on RTL-SDR hardware, cgo, or any Stratux global state,
so it can be exercised entirely with Go's standard testing package.

Where a stable assignment cannot be derived - two or more enabled
bands competing for two or more indistinguishable, untagged dongles -
Assign() reports the affected bands as ambiguous rather than guessing.
A device's USB enumeration index is not proven stable across reboots
(it depends on kernel/libusb enumeration order), so it is used only
as a tie-breaker when exactly one candidate remains for exactly one
unfulfilled band; it is never used to arbitrate between two or more
candidates for two or more bands.
*/
package sdrassign

import (
	"fmt"
	"regexp"
	"sort"
)

// Band identifies one of the frequencies Stratux can assign an SDR to.
type Band int

const (
	UAT Band = iota // 978 MHz
	ES              // 1090 MHz
	OGN             // 868 MHz
	AIS             // 162 MHz
)

// String returns the conventional short name used in logs and status text.
func (b Band) String() string {
	switch b {
	case UAT:
		return "978 UAT"
	case ES:
		return "1090 ES"
	case OGN:
		return "868 OGN"
	case AIS:
		return "162 AIS"
	default:
		return "unknown band"
	}
}

// Source describes how a band ended up with the receiver it has.
type Source int

const (
	// SourceNone means no receiver is currently assigned to the band.
	SourceNone Source = iota
	// SourceTagged means the assigned device carried the band's own
	// "stratux:<freq>" EEPROM serial tag.
	SourceTagged
	// SourceAnonymous means the assigned device carried no recognized
	// Stratux tag and was assigned because it was the sole untagged
	// candidate for the sole unfulfilled enabled band.
	SourceAnonymous
)

// String renders the source using the vocabulary from the SDR/status docs.
func (s Source) String() string {
	switch s {
	case SourceTagged:
		return "tagged"
	case SourceAnonymous:
		return "anonymous"
	default:
		return "none"
	}
}

// Device describes one RTL-SDR dongle as discovered via libusb enumeration.
//
// Index is the transient, process-local enumeration index (as returned by
// rtl.GetDeviceCount/GetDeviceUsbStrings). It is NOT guaranteed to be
// stable across reboots or replugs, so callers must not treat it as a
// persistent device identity - it is used here only to produce a
// deterministic tie-break among otherwise-identical anonymous candidates,
// and for display purposes.
type Device struct {
	Index  int
	Serial string
}

// Assignment is the outcome for a single band.
type Assignment struct {
	Band Band

	// Enabled reports whether the band is configured to operate.
	Enabled bool

	// Detected reports whether at least one compatible SDR was found
	// for this band: either a device tagged for it, or (when enabled)
	// an untagged device available to serve it.
	Detected bool

	// Assigned reports whether a receiver was bound to this band.
	Assigned bool

	// Device is the receiver bound to this band. Zero value if Assigned
	// is false.
	Device Device

	// Source describes how Device came to be assigned.
	Source Source

	// Ambiguous is true when two or more enabled bands were competing
	// for two or more indistinguishable untagged devices, so no safe
	// automatic assignment could be made for this band.
	Ambiguous bool

	// Conflict is true when more than one device carried this band's
	// own tag. The first (lowest-index) tagged device, if any, is still
	// used; the rest are ignored rather than reassigned elsewhere.
	Conflict bool

	// Reason is a human-readable explanation of the current state,
	// intended for display in the status UI.
	Reason string
}

// Result is the full assignment outcome for one evaluation.
type Result struct {
	UAT Assignment
	ES  Assignment
	OGN Assignment
	AIS Assignment

	// Warnings holds cross-cutting issues not tied to exactly one band,
	// such as a device carrying an unrecognized "stratux:" tag.
	Warnings []string
}

// ByBand returns the Assignment for the given band.
func (r Result) ByBand(b Band) Assignment {
	switch b {
	case UAT:
		return r.UAT
	case ES:
		return r.ES
	case OGN:
		return r.OGN
	default:
		return r.AIS
	}
}

// allBands is the fixed priority order used only to decide, deterministically,
// which unfulfilled band gets the lone candidate when exactly one anonymous
// device is available for exactly one remaining enabled band. It has no
// effect once two or more bands are simultaneously competing for devices.
var allBands = []Band{UAT, ES, OGN, AIS}

var tagPattern = regexp.MustCompile(`str?a?t?u?x:(\d+)`)

var bandTagFreq = map[Band]string{
	UAT: "978",
	ES:  "1090",
	OGN: "868",
	AIS: "162",
}

var supportedFreqs = map[string]bool{
	"978":  true,
	"1090": true,
	"868":  true,
	"162":  true,
}

// genericFreq is the "stx:0:<ppm>" tag documented in
// docs/hardware/sdr-and-bands.md as a spare/fallback marker. No band-specific
// fallback behavior for it is implemented anywhere in this codebase today
// (see the tech-debt notes), so it is treated as carrying no band-specific
// tag at all: a "stx:0" device is left eligible for anonymous assignment,
// matching the code's actual pre-existing behavior, rather than being
// reported as an invalid/unsupported tag.
const genericFreq = "0"

// matchedBand returns the band a serial's Stratux tag names, if any, and
// whether the serial carries a recognized Stratux tag at all (tagged may be
// true while band is not one of the known constants, for an unsupported
// frequency tag).
func matchedBand(serial string) (band Band, tagged bool, supported bool) {
	m := tagPattern.FindStringSubmatch(serial)
	if m == nil {
		return 0, false, false
	}
	freq := m[1]
	if freq == genericFreq {
		return 0, false, false
	}
	if !supportedFreqs[freq] {
		return 0, true, false
	}
	for b, f := range bandTagFreq {
		if f == freq {
			return b, true, true
		}
	}
	return 0, true, false
}

// Assign computes SDR band assignment for the given devices. It is a pure
// function: the result depends only on the (Index, Serial) pairs present in
// devices and on which bands are enabled, never on the order devices are
// supplied in and never on any package-level state.
func Assign(devices []Device, uatEnabled, esEnabled, ognEnabled, aisEnabled bool) Result {
	enabled := map[Band]bool{UAT: uatEnabled, ES: esEnabled, OGN: ognEnabled, AIS: aisEnabled}

	// Work on a copy sorted by index so the two passes below are
	// reproducible even though callers may supply devices in any order
	// (e.g. as returned by an unordered enumeration).
	sorted := make([]Device, len(devices))
	copy(sorted, devices)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Index < sorted[j].Index })

	result := Result{
		UAT: Assignment{Band: UAT, Enabled: uatEnabled},
		ES:  Assignment{Band: ES, Enabled: esEnabled},
		OGN: Assignment{Band: OGN, Enabled: ognEnabled},
		AIS: Assignment{Band: AIS, Enabled: aisEnabled},
	}

	// Pass 1: bind every recognized Stratux tag to its declared band, in
	// ascending device-index order. A device carrying any recognized (or
	// unsupported) Stratux tag is never treated as anonymous, even for
	// its own band if that band is disabled, and even if its own band's
	// slot is already filled or its tag is unsupported.
	tagged := make(map[int]bool) // device index -> excluded from anonymous pool
	firstTaggedIndex := map[Band]int{}
	for _, d := range sorted {
		band, isTagged, supported := matchedBand(d.Serial)
		if !isTagged {
			continue
		}
		tagged[d.Index] = true
		if !supported {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"SDR at index %d has an unrecognized Stratux tag (%q); it will not be used for any band. Re-tag it with debian/sdr-tool.sh.",
				d.Index, d.Serial))
			continue
		}
		a := result.forBand(band)
		if !a.Assigned {
			a.Assigned = true
			a.Detected = true
			a.Device = d
			a.Source = SourceTagged
			firstTaggedIndex[band] = d.Index
			a.Reason = fmt.Sprintf("%s assigned by tag (%s) to SDR index %d.", band, d.Serial, d.Index)
		} else {
			a.Conflict = true
			a.Reason = fmt.Sprintf(
				"Multiple SDRs are tagged for %s. Using tagged SDR index %d; ignoring duplicate tag on index %d. Re-tag the extra device.",
				band, firstTaggedIndex[band], d.Index)
		}
	}

	// Pass 2: assign untagged (anonymous) devices to whichever enabled
	// bands are still unfulfilled.
	var anonymous []Device
	for _, d := range sorted {
		if !tagged[d.Index] {
			anonymous = append(anonymous, d)
		}
	}

	var unmet []Band
	for _, b := range allBands {
		a := result.forBand(b)
		if enabled[b] && !a.Assigned {
			unmet = append(unmet, b)
		}
	}

	switch {
	case len(unmet) == 0:
		// Nothing left to do.
	case len(anonymous) == 0:
		for _, b := range unmet {
			a := result.forBand(b)
			a.Reason = fmt.Sprintf("%s is enabled but no compatible SDR was detected. Connect an SDR and tag it (or leave it untagged if it is the only spare device).", b)
		}
	case len(unmet) == 1:
		b := unmet[0]
		a := result.forBand(b)
		d := anonymous[0] // lowest index; sole candidate for the sole unmet band
		a.Assigned = true
		a.Detected = true
		a.Source = SourceAnonymous
		a.Device = d
		a.Reason = fmt.Sprintf("%s assigned to the only untagged SDR present (index %d). Tag it with debian/sdr-tool.sh to make this assignment stable.", b, d.Index)
		if len(anonymous) > 1 {
			result.Warnings = append(result.Warnings, fmt.Sprintf(
				"%d extra untagged SDR(s) are connected but not needed; they were left unassigned.", len(anonymous)-1))
		}
	default:
		// Two or more enabled bands still need a receiver and one or
		// more untagged, indistinguishable candidates exist: assigning
		// any of them to any band would be a guess. Report every unmet
		// band as ambiguous instead of silently picking one.
		for _, b := range unmet {
			a := result.forBand(b)
			a.Detected = len(anonymous) > 0
			a.Ambiguous = true
			a.Reason = fmt.Sprintf(
				"%d untagged SDR(s) are connected but %d enabled bands (including %s) need one; Stratux cannot tell them apart. Tag each SDR with debian/sdr-tool.sh (stratux:978 / stratux:1090 / ...) so assignment is unambiguous.",
				len(anonymous), len(unmet), b)
		}
	}

	// Bands that are disabled report a clean, non-faulted state and never
	// consume a device.
	for _, b := range allBands {
		a := result.forBand(b)
		if !enabled[b] {
			*a = Assignment{Band: b, Enabled: false, Reason: fmt.Sprintf("%s is disabled.", b)}
		}
	}

	return result
}

// forBand returns a pointer to the Assignment for b so callers can mutate
// it in place while building up Result.
func (r *Result) forBand(b Band) *Assignment {
	switch b {
	case UAT:
		return &r.UAT
	case ES:
		return &r.ES
	case OGN:
		return &r.OGN
	default:
		return &r.AIS
	}
}
