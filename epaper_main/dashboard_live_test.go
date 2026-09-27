package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stratux/stratux/epaper"
)

// TestLiveDashboardFromDevice is an opt-in, read-only check used during
// physical validation: with EPAPER_LIVE_URL set (for example
// http://192.168.10.1) it polls the real daemon with the same source and
// tracker the service uses and writes what the panel should be showing
// (EPAPER_LIVE_OUT, default the working directory). It only ever issues
// GETs and is skipped otherwise.
func TestLiveDashboardFromDevice(t *testing.T) {
	url := os.Getenv("EPAPER_LIVE_URL")
	if url == "" {
		t.Skip("EPAPER_LIVE_URL not set")
	}
	src := NewDashSource(url, 3*time.Second)
	tr := epaper.NewTracker(epaper.DefaultThresholds(), time.Now())
	for i := 0; i < 2; i++ {
		s := src.Poll(context.Background(), time.Now)
		tr.Observe(s)
		if i == 0 {
			time.Sleep(6 * time.Second)
		}
	}
	d := tr.Derive(time.Now(), time.Now())
	t.Logf("overall=%s %q | %s", d.Overall, d.OverallTxt, d.Subtitle)
	for _, tile := range []epaper.Tile{d.GPS, d.ES, d.UAT, d.FISB} {
		t.Logf("%-10s %-14s | %s (level %d)", tile.Label, tile.Headline, tile.Detail, tile.Level)
	}
	t.Logf("footer: %s | %s | warnings=%v", d.Clients, d.Footer.Text, d.Warnings)
	img, _ := renderDashboardImage(d)
	out := os.Getenv("EPAPER_LIVE_OUT")
	if out == "" {
		out = "."
	}
	if err := os.WriteFile(filepath.Join(out, "live.png"), pngBytes(t, img), 0o644); err != nil {
		t.Fatal(err)
	}
}
