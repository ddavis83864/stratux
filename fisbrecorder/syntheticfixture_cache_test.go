package fisbrecorder

import (
	"testing"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/uatparse"
)

// TestSyntheticFixture_TextProductsAreCacheAdmissible proves - against the
// real fisbcache package, not by inspection - that the fixture's own text
// products decode to a (productType, location) pair fisbcache.PolicyFor
// actually recognizes, so fisbcache.Store.Admit accepts them rather than
// returning AdmitRejectedUnsupported.
//
// This exists because an earlier revision of this fixture used the
// leading token "SYNTHETIC" (an unrecognized report type) for both text
// frames: they decoded fine and derived tower identity fine, but
// main.fisbCacheCaptureTextFrame's own admission call
// (fisbCacheEnqueue -> Store.Admit) silently rejected both, and nothing
// in this package's own tests caught it - only a real daemon-level bench
// session did (see docs/fisb-field-recorder-design.md's cache-admission
// section). This test is what should have caught it, and now would.
func TestSyntheticFixture_TextProductsAreCacheAdmissible(t *testing.T) {
	cases := []struct {
		name string
		buf  string
	}{
		{"towerA_METAR", SyntheticTowerAFrame1},
		{"towerB_PIREP", SyntheticTowerBFrame3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, err := uatparse.New(c.buf)
			if err != nil {
				t.Fatalf("uatparse.New: %v", err)
			}
			if err := msg.DecodeUplink(); err != nil {
				t.Fatalf("DecodeUplink: %v", err)
			}
			texts, err := msg.GetTextReports()
			if err != nil {
				t.Fatalf("GetTextReports: %v", err)
			}
			if len(texts) != 1 {
				t.Fatalf("got %d text reports, want 1", len(texts))
			}
			// Mirrors main/fisbcachecapture.go's own
			// fisbParseTextReportHeader exactly: split on spaces,
			// fields[0]=type, fields[1]=location.
			fields := splitOnSpaceForTest(texts[0])
			if len(fields) < 5 {
				t.Fatalf("text report has %d fields, want >=5 (main's own fisbParseTextReportHeader requires this to even attempt admission): %q", len(fields), texts[0])
			}
			productType, location := fields[0], fields[1]

			store := fisbcache.NewStore()
			result := store.Admit(fisbcache.Entry{
				Key:                 fisbcache.TextKey(productType, location),
				ReceivedAtMonotonic: 1.0,
				SizeBytes:           int64(len(texts[0])),
			})
			if result == fisbcache.AdmitRejectedUnsupported {
				t.Fatalf("Store.Admit rejected productType=%q location=%q as unsupported - fisbcache/policy.go's textPolicies does not recognize %q; pick a leading token it does (see textPolicies in fisbcache/policy.go: METAR, SPECI, TAF, TAF.AMD, WINDS, PIREP)", productType, location, productType)
			}
			if result != fisbcache.AdmitAccepted {
				t.Fatalf("Store.Admit = %q, want %q", result, fisbcache.AdmitAccepted)
			}
		})
	}
}

// splitOnSpaceForTest mirrors main/fisbcachecapture.go's own
// splitOnSpace/fisbParseTextReportHeader exactly (duplicated rather than
// imported - that function lives in package main, which this package
// cannot import without pulling in cgo).
func splitOnSpaceForTest(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}
