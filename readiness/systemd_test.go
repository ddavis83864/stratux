package readiness

import "testing"

func TestParseFailedUnits_Empty(t *testing.T) {
	if got := ParseFailedUnits(""); got != nil {
		t.Errorf("empty output should parse to nil/no units, got %v", got)
	}
}

func TestParseFailedUnits_OneUnit(t *testing.T) {
	out := "stratux.service                   loaded failed failed Stratux\n"
	got := ParseFailedUnits(out)
	if len(got) != 1 || got[0] != "stratux.service" {
		t.Errorf("ParseFailedUnits(%q) = %v, want [\"stratux.service\"]", out, got)
	}
}

func TestParseFailedUnits_Multiple(t *testing.T) {
	out := "stratux.service loaded failed failed Stratux\nfancontrol.service loaded failed failed Fan\n"
	got := ParseFailedUnits(out)
	if len(got) != 2 {
		t.Errorf("expected 2 failed units, got %v", got)
	}
}
