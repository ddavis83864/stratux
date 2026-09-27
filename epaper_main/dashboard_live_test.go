package main

import (
	"context"
	"fmt"
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

// TestLiveWatchDashboard (opt-in: EPAPER_LIVE_URL and EPAPER_LIVE_WATCH=<seconds>)
// polls the real daemon every 5 s for that long and logs every change of the
// dashboard's material state, with what changed - used to understand which
// inputs cost panel refreshes (for example during a boot). Read-only.
func TestLiveWatchDashboard(t *testing.T) {
	url := os.Getenv("EPAPER_LIVE_URL")
	secs := os.Getenv("EPAPER_LIVE_WATCH")
	if url == "" || secs == "" {
		t.Skip("EPAPER_LIVE_URL / EPAPER_LIVE_WATCH not set")
	}
	var total int
	fmt.Sscanf(secs, "%d", &total)
	src := NewDashSource(url, 2*time.Second)
	start := time.Now()
	tr := epaper.NewTracker(epaper.DefaultThresholds(), start)
	line := func(d epaper.Dashboard) string {
		return fmt.Sprintf("%s | GPS %s/%s | 1090 %s/%s | 978 %s/%s | WX %s/%s | %s | warn=%d",
			d.OverallTxt, d.GPS.Headline, d.GPS.Detail, d.ES.Headline, d.ES.Detail, d.UAT.Headline, d.UAT.Detail,
			d.FISB.Headline, d.FISB.Detail, d.Clients, len(d.Warnings))
	}
	var last string
	changes := 0
	for time.Since(start) < time.Duration(total)*time.Second {
		tr.Observe(src.Poll(context.Background(), time.Now))
		d := tr.Derive(time.Now(), time.Now())
		if k := d.MaterialKey(); k != last {
			changes++
			t.Logf("%s +%3ds #%d %s", time.Now().UTC().Format("15:04:05"), int(time.Since(start).Seconds()), changes, line(d))
			last = k
		}
		time.Sleep(5 * time.Second)
	}
}
