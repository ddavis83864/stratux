package configbackup

import "testing"

// The FIS-B cache's maximum entry count is 10,000 (main.FISBCacheMaxEntriesLimit; main's tests
// keep the two constants equal). Configuration Backup validation must honor it: a backup can
// never smuggle in a larger cache than the settings API would accept.
func TestValidate_FISBCacheMaxEntriesBoundary(t *testing.T) {
	cases := []struct {
		entries int
		ok      bool
	}{
		{1, true}, {2000, true}, {9999, true}, {10000, true},
		{0, false}, {-1, false}, {10001, false}, {100000, false}, {1000000, false}, {1 << 40, false},
	}
	for _, c := range cases {
		in := testBuildInputs()
		in.FISBCacheSettings = FISBCacheSettingsSection{Enabled: true, PersistenceEnabled: true, MaxCacheBytes: 16 << 20, MaxEntries: c.entries}
		doc, err := BuildDocument(in)
		if err != nil {
			t.Fatalf("BuildDocument: %v", err)
		}
		res := Validate(doc, mustMarshalLen(t, doc))
		if res.OK() != c.ok {
			t.Errorf("maxEntries=%d: OK=%v, want %v (errors %v)", c.entries, res.OK(), c.ok, res.Errors)
		}
	}
	if FISBCacheMaxEntries != 10000 {
		t.Fatalf("FISBCacheMaxEntries = %d, want 10000", FISBCacheMaxEntries)
	}
}
