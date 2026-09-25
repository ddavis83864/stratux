package epaper

import (
	"strings"
	"testing"
)

func TestDimensions_RotationBounds(t *testing.T) {
	cases := []struct {
		panel          string
		rotation, w, h int
	}{
		{PanelWaveshare37, 0, PanelWidth, PanelHeight},
		{PanelWaveshare37, 90, PanelHeight, PanelWidth},
		{PanelWaveshare37, 180, PanelWidth, PanelHeight},
		{PanelWaveshare37, 270, PanelHeight, PanelWidth},
		{PanelWaveshare42V2, 0, Panel42V2Width, Panel42V2Height},
		{PanelWaveshare42V2, 90, Panel42V2Height, Panel42V2Width},
		{PanelWaveshare42V2, 180, Panel42V2Width, Panel42V2Height},
		{PanelWaveshare42V2, 270, Panel42V2Height, Panel42V2Width},
	}
	for _, c := range cases {
		w, h := Dimensions(c.panel, c.rotation)
		if w != c.w || h != c.h {
			t.Errorf("Dimensions(%q, %d) = (%d,%d), want (%d,%d)", c.panel, c.rotation, w, h, c.w, c.h)
		}
	}
}

// TestNativeDimensions_NeverSwapsRegardlessOfRotation is a direct
// regression test for a real hardware-validation finding: a PanelDriver
// must always be constructed with the panel's fixed native dimensions,
// never Dimensions's rotation-swapped ones - feeding a swapped
// width/height into a driver's own RAM-window/gate-count programming
// would misconfigure the physical controller, since those are fixed
// properties of the silicon, not something a software rotation setting
// can reprogram. NativeDimensions takes no rotation parameter at all
// (by design, so it cannot be misused the way the pre-fix code misused
// Dimensions for driver construction).
func TestNativeDimensions_NeverSwapsRegardlessOfRotation(t *testing.T) {
	cases := []struct {
		panel string
		w, h  int
	}{
		{PanelWaveshare37, PanelWidth, PanelHeight},
		{PanelWaveshare42V2, Panel42V2Width, Panel42V2Height},
		{"some-future-panel", PanelWidth, PanelHeight}, // unrecognized falls back to 3.7in
	}
	for _, c := range cases {
		w, h := NativeDimensions(c.panel)
		if w != c.w || h != c.h {
			t.Errorf("NativeDimensions(%q) = (%d,%d), want (%d,%d)", c.panel, w, h, c.w, c.h)
		}
	}
}

// TestDimensions_UnrecognizedPanelFallsBackTo37in mirrors Normalize's own
// "empty/unrecognized falls back to the shipped default" convention -
// Dimensions must never panic or silently return zero for an unexpected
// panel string, since a value from a future/newer config could reach
// this function before validation runs.
func TestDimensions_UnrecognizedPanelFallsBackTo37in(t *testing.T) {
	w, h := Dimensions("some-future-panel", 0)
	if w != PanelWidth || h != PanelHeight {
		t.Errorf("Dimensions(unrecognized, 0) = (%d,%d), want the 3.7in fallback (%d,%d)", w, h, PanelWidth, PanelHeight)
	}
}

func TestShortBuild_TruncatesToStandardShortHashLength(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"abc", "abc"},
		{"1234567", "1234567"},
		{"339b84f16954263d590a8c476f5deeff58f797fa", "339b84f"},
	}
	for _, c := range cases {
		if got := shortBuild(c.in); got != c.want {
			t.Errorf("shortBuild(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLayout_HeaderLineFitsPanelWidth is a regression test for a real
// hardware-validation finding: the header line was sized for this
// project's original, incorrect 480px-wide assumption. On the panel's
// actual native 280px-wide portrait canvas, a full 40-character commit
// hash pushed the line well past the right edge, clipping it - even with
// the worst-case (longest) real build string this project ever produces
// (a full git SHA), the header must fit within PanelWidth's actual
// character budget for the font this driver uses (7px/char, 4px left
// margin - see epaper_main/render.go).
func TestLayout_HeaderLineFitsPanelWidth(t *testing.T) {
	const charWidthPx = 7
	const leftMarginPx = 4
	maxChars := (PanelWidth - leftMarginPx) / charWidthPx

	c := Content{Version: "2.0.0~rc2", Build: "339b84f16954263d590a8c476f5deeff58f797fa"}
	lines := Layout(c, Config{Page: PageOverview}, false)
	if len(lines) == 0 {
		t.Fatal("expected at least a header line")
	}
	header := lines[0].Text
	if len(header) > maxChars {
		t.Errorf("header line %q is %d characters, want at most %d to fit PanelWidth (%dpx)", header, len(header), maxChars, PanelWidth)
	}
}

func TestLayout_StaleIndicatorAppearsOnlyWhenStale(t *testing.T) {
	c := Content{Version: "2.0.0", Build: "abcdef12"}
	fresh := Layout(c, Config{Page: PageOverview}, false)
	stale := Layout(c, Config{Page: PageOverview}, true)
	if containsStaleMarker(fresh) {
		t.Errorf("a fresh render must not show the stale marker")
	}
	if !containsStaleMarker(stale) {
		t.Errorf("a stale render must show the stale marker")
	}
}

func containsStaleMarker(lines []Line) bool {
	for _, l := range lines {
		if strings.Contains(l.Text, "STALE") {
			return true
		}
	}
	return false
}

func TestLayout_NeverIncludesCoordinatesOrCredentials(t *testing.T) {
	c := Content{
		Version: "2.0.0", Build: "abcdef12",
		OverallReady: "READY", AHRSState: "READY", BaroState: "READY", FanState: "READY",
	}
	for _, page := range []string{PageOverview, PageReceivers, PageHealth} {
		for _, l := range Layout(c, Config{Page: page}, false) {
			lower := strings.ToLower(l.Text)
			for _, forbidden := range []string{"lat", "lon", "password", "passphrase", "ssid", "token"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("page %q line %q contains forbidden substring %q", page, l.Text, forbidden)
				}
			}
		}
	}
}

func TestLayout_EveryPageProducesAtLeastOneLine(t *testing.T) {
	c := Content{Version: "2.0.0", Build: "abcdef12"}
	for _, page := range []string{PageOverview, PageReceivers, PageHealth, "unknown-falls-back"} {
		lines := Layout(c, Config{Page: page}, false)
		if len(lines) == 0 {
			t.Errorf("page %q produced zero lines", page)
		}
	}
}

func TestShutdownAndStartupLines_AreNonEmptyAndStatic(t *testing.T) {
	if len(ShutdownLines()) == 0 {
		t.Errorf("ShutdownLines must not be empty")
	}
	if len(StartupLines()) == 0 {
		t.Errorf("StartupLines must not be empty")
	}
}
