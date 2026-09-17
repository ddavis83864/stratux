package epaper

import (
	"strings"
	"testing"
)

func TestDimensions_RotationBounds(t *testing.T) {
	cases := []struct {
		rotation, w, h int
	}{
		{0, PanelWidth, PanelHeight},
		{90, PanelHeight, PanelWidth},
		{180, PanelWidth, PanelHeight},
		{270, PanelHeight, PanelWidth},
	}
	for _, c := range cases {
		w, h := Dimensions(c.rotation)
		if w != c.w || h != c.h {
			t.Errorf("Dimensions(%d) = (%d,%d), want (%d,%d)", c.rotation, w, h, c.w, c.h)
		}
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
