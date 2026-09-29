package uatparse

import (
	"bufio"
	"compress/gzip"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Real captured FIS-B. dump978/sample-data.txt.gz (in this repository since its initial
// commit, inherited from the upstream dump978 sources) holds 1,143 demodulated 978 MHz
// messages: 704 uplinks ('+') and 439 downlinks ('-'). The capture was made around 04:10Z on
// the 24th (month and year are not recorded; the reports are western United States). These
// tests document - from real data - what the FIS-B information-frame header time IS for each
// product the cache stores, which is what makes it usable as a "source time" for freshness.
//
// Observed (see TestRealSample_HeaderTimeIsTheProductTime):
//   - METAR / SPECI / PIREP: the header hour:minute equals the report's own observation time
//     (the ddhhmmZ token in the text) in every frame;
//   - TAF / TAF.AMD: equals the issue-time token where the text has one; the TAFs without one
//     (validity-period form) carry the start of validity or a few minutes before it (one of the
//     nine is 22:57 for a validity starting 23Z) - i.e. the issue time;
//   - WINDS: the header time is the generation time of the product, NOT the forecast valid time
//     in the text (250000Z) - i.e. the time the product was produced;
//   - NEXRAD: every one of the 200 radar frames carries the same header time (the scan/mosaic
//     time), 04:10.

func realSampleUplinks(t *testing.T) []*UATMsg {
	t.Helper()
	f, err := os.Open("../dump978/sample-data.txt.gz")
	if err != nil {
		t.Fatalf("real sample data missing: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var out []*UATMsg
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "+") {
			continue
		}
		m, err := New(line)
		if err != nil {
			t.Fatalf("a real uplink failed to parse: %v", err)
		}
		if err := m.DecodeUplink(); err != nil {
			t.Fatalf("a real uplink failed to decode: %v", err)
		}
		out = append(out, m)
	}
	return out
}

func TestRealSample_DecodesWithoutErrorAndHasTheExpectedProducts(t *testing.T) {
	msgs := realSampleUplinks(t)
	if len(msgs) != 704 {
		t.Fatalf("uplinks = %d, want 704", len(msgs))
	}
	products := map[uint32]int{}
	for _, m := range msgs {
		for _, f := range m.Frames {
			products[f.Product_id]++
		}
	}
	if products[413] != 224 || products[63] != 200 {
		t.Fatalf("products = %v, want 224 text frames and 200 NEXRAD (63) frames", products)
	}
}

func TestRealSample_HeaderTimeIsTheProductTime(t *testing.T) {
	re := regexp.MustCompile(`\b(\d{2})(\d{2})(\d{2})Z\b`)
	match := map[string]int{}
	mismatch := map[string]int{}
	nexTimes := map[[2]uint32]int{}
	tafValidityMatch, tafValidityMismatch := 0, 0
	for _, m := range realSampleUplinks(t) {
		for _, f := range m.Frames {
			if f.Product_id == 63 || f.Product_id == 64 {
				nexTimes[[2]uint32{f.FISB_hours, f.FISB_minutes}]++
				continue
			}
			if f.Product_id != 413 {
				continue
			}
			for _, line := range f.Text_data {
				fields := strings.Fields(line)
				if len(fields) < 3 {
					continue
				}
				z := re.FindStringSubmatch(line)
				if z == nil {
					// TAF in the validity-period form ("TAF KXXX ddhh/ddhh ..."): the header time is the
					// start of validity - equal to it, or a few minutes before it (issue time).
					if (fields[0] == "TAF" || fields[0] == "TAF.AMD") && len(fields) > 2 && len(fields[2]) == 9 && fields[2][4] == '/' {
						vh := uint32(fields[2][2]-'0')*10 + uint32(fields[2][3]-'0')
						// header time at, or a few minutes before, the start of validity (issue time)
						if lead := (int(vh)*60 - int(f.FISB_hours*60+f.FISB_minutes) + 1440) % 1440; lead <= 30 {
							tafValidityMatch++
						} else {
							tafValidityMismatch++
						}
					}
					continue
				}
				var hh, mm uint32
				for _, c := range z[2] {
					hh = hh*10 + uint32(c-'0')
				}
				for _, c := range z[3] {
					mm = mm*10 + uint32(c-'0')
				}
				if hh == f.FISB_hours && mm == f.FISB_minutes {
					match[fields[0]]++
				} else {
					mismatch[fields[0]]++
				}
			}
		}
	}
	for _, ty := range []string{"METAR", "SPECI", "PIREP", "TAF", "TAF.AMD"} {
		if match[ty] == 0 || mismatch[ty] != 0 {
			t.Errorf("%s: %d frames' header time equal their report time, %d differ - want all equal", ty, match[ty], mismatch[ty])
		}
	}
	if tafValidityMatch != 9 || tafValidityMismatch != 0 {
		t.Errorf("validity-form TAFs: %d header times are at/just before the start of validity, %d are not - want 9 and 0", tafValidityMatch, tafValidityMismatch)
	}
	// WINDS: the text's Z time is the forecast VALID time; the header time is the generation time.
	if mismatch["WINDS"] == 0 || match["WINDS"] != 0 {
		t.Errorf("WINDS: match=%d mismatch=%d - the header time was expected to differ from the valid time in the text", match["WINDS"], mismatch["WINDS"])
	}
	if len(nexTimes) != 1 {
		t.Errorf("NEXRAD frames carry %d distinct header times %v, want exactly one (the scan time)", len(nexTimes), nexTimes)
	}
	if nexTimes[[2]uint32{4, 10}] != 200 {
		t.Errorf("NEXRAD header times = %v, want 200 frames at 04:10", nexTimes)
	}
}
