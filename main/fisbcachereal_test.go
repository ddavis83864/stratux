package main

import (
	"bufio"
	"compress/gzip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/uatparse"
)

// Real captured FIS-B (dump978/sample-data.txt.gz - see uatparse/real_sample_test.go for its
// provenance and what it shows about header times) run through the cache's capture path against
// a fixed receive time, so the result is deterministic. The capture was made about 04:10Z on the
// 24th; the receive time is set 2 minutes after the radar scan and ~17 minutes after the
// observations, i.e. shortly after the capture instant.

func realSampleUplinkLines(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("../dump978/sample-data.txt.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var out []string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "+") {
			out = append(out, sc.Text())
		}
	}
	return out
}

func withFixedFISBReceiveTime(t *testing.T, at time.Time) {
	t.Helper()
	orig := fisbCacheWallClock
	fisbCacheWallClock = func() time.Time { return at }
	t.Cleanup(func() { fisbCacheWallClock = orig })
}

func feedRealSample(t *testing.T) {
	t.Helper()
	for _, line := range realSampleUplinkLines(t) {
		receiveUplink(t, line)
	}
}

func TestFISBRealCapture_ThroughTheCacheWithConservativeFreshness(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	withFixedFISBReceiveTime(t, time.Date(2026, 9, 24, 4, 40, 0, 0, time.UTC))
	enableFISBCacheForTest(t, false)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache.MaxEntries = 20000
	fisbCacheSettingsCache.MaxCacheBytes = 64 << 20
	fisbCacheMu.Unlock()

	feedRealSample(t)
	first := fisbCacheStore.Len()
	if first < 100 {
		t.Fatalf("only %d entries cached from 704 real uplinks", first)
	}
	// Replaying the whole capture again (a ground station repeats everything) adds nothing.
	feedRealSample(t)
	if n := fisbCacheStore.Len(); n != first {
		t.Fatalf("a second pass over the same capture changed the cache from %d to %d entries", first, n)
	}

	// Independent oracle: the newest observation time per METAR/SPECI station, read from the report
	// text itself (not from the frame header the cache uses), against the fixed receive time.
	receive := time.Date(2026, 9, 24, 4, 40, 0, 0, time.UTC)
	newest := map[string]time.Time{}
	for _, line := range realSampleUplinkLines(t) {
		msg, _ := uatparse.New(line)
		msg.DecodeUplink()
		for _, f := range msg.Frames {
			for _, l := range f.Text_data {
				fs := strings.Fields(l)
				if len(fs) < 3 || (fs[0] != "METAR" && fs[0] != "SPECI") {
					continue
				}
				z := fs[2] // ddhhmmZ
				if len(z) != 7 || z[6] != 'Z' {
					continue
				}
				d, _ := strconv.Atoi(z[0:2])
				h, _ := strconv.Atoi(z[2:4])
				m, _ := strconv.Atoi(z[4:6])
				obs := time.Date(2026, 9, d, h, m, 0, 0, time.UTC) // the report's own day-of-month
				if k := fs[0] + " " + fs[1]; obs.After(newest[k]) {
					newest[k] = obs
				}
			}
		}
	}

	perType := map[string]int{}
	metarChecked := 0
	for _, it := range fisbInventoryForTest(t) {
		ty := "NEXRAD"
		if it.ProductClass == string(fisbcache.ClassText) {
			ty = strings.Fields(it.Identity)[0]
		}
		perType[ty]++
		if it.AgeSeconds < it.ReceptionAgeSeconds {
			t.Errorf("%s %s: effective age %.0f is younger than the reception age %.0f", ty, it.Identity, it.AgeSeconds, it.ReceptionAgeSeconds)
		}
		if it.AgeSeconds < 0 || it.ReceptionAgeSeconds < 0 {
			t.Errorf("%s %s: negative age", ty, it.Identity)
		}
		switch ty {
		case "METAR", "SPECI":
			obs, ok := newest[it.Identity]
			if !ok {
				continue
			}
			metarChecked++
			want := receive.Sub(obs)
			if want < 0 {
				want = 0 // a small clock-domain skew ahead: never younger than reception
			}
			if it.AgeBasis != "source" {
				t.Errorf("%s: basis %s, want source", it.Identity, it.AgeBasis)
			}
			if d := it.AgeSeconds - want.Seconds(); d < -5 || d > 5 {
				t.Errorf("%s: effective age %.0fs, but its report says it is %.0fs old", it.Identity, it.AgeSeconds, want.Seconds())
			}
			// Reception-only accounting calls everything just received CACHED_FRESH; an observation
			// older than the 15-minute window must not be.
			if want > 15*time.Minute+5*time.Second && it.Freshness == string(fisbcache.FreshnessCachedFresh) {
				t.Errorf("%s: a %.0fs-old observation is CACHED_FRESH", it.Identity, want.Seconds())
			}
		case "WINDS":
			// Header time is the generation time 02:06Z: 2h34m old at 04:40Z, inside the 3h fresh window.
			if it.AgeBasis != "source" || it.AgeSeconds < 2*3600+33*60 || it.AgeSeconds > 2*3600+35*60 {
				t.Errorf("%s: effective age %.0fs (%s), want 2h34m from the source", it.Identity, it.AgeSeconds, it.AgeBasis)
			}
		case "NEXRAD":
			// Scan time 04:10Z: 30 minutes old at 04:40Z - past the 20-minute aging limit, so STALE. Reception-only
			// accounting would have called a tile received a moment ago CACHED_FRESH.
			if it.AgeBasis != "source" || it.AgeSeconds < 29*60 || it.AgeSeconds > 31*60 || it.Freshness != string(fisbcache.FreshnessStale) {
				t.Errorf("radar tile: age %.0fs (%s) freshness %s, want ~30 min STALE", it.AgeSeconds, it.AgeBasis, it.Freshness)
			}
		}
	}
	if metarChecked < 100 {
		t.Errorf("only %d METAR stations were cross-checked against their own report text", metarChecked)
	}
	for _, ty := range []string{"METAR", "TAF", "WINDS", "PIREP", "NEXRAD"} {
		if perType[ty] == 0 {
			t.Errorf("no %s entry was cached from the real capture (%v)", ty, perType)
		}
	}
	t.Logf("real capture -> %d cached products: %v", first, perType)
}

// The same real capture received ONE DAY later would leave the reception-only model showing
// everything fresh again after a signal gap; here it is judged by its own age and (for the
// short-lived products) never cached at all.
func TestFISBRealCapture_OldCaptureIsNotCachedAsFresh(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	// Received 6 hours after the observations: METAR/PIREP (expire 3h/2h) and radar (45 min) are already
	// expired by their own time; TAF and winds are not.
	withFixedFISBReceiveTime(t, time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC))
	enableFISBCacheForTest(t, false)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache.MaxEntries = 20000
	fisbCacheSettingsCache.MaxCacheBytes = 64 << 20
	fisbCacheMu.Unlock()
	feedRealSample(t)
	for _, it := range fisbInventoryForTest(t) {
		ty := "NEXRAD"
		if it.ProductClass == string(fisbcache.ClassText) {
			ty = strings.Fields(it.Identity)[0]
		}
		switch ty {
		case "METAR", "SPECI", "PIREP", "NEXRAD":
			t.Errorf("%s %s was cached although its own time is hours past its expiry", ty, it.Identity)
		}
	}
	if fisbCacheStore.Len() == 0 {
		t.Fatal("TAFs and winds (long-lived) should still be cached")
	}
}

var _ = uatparse.UPLINK_FRAME_DATA_BYTES
