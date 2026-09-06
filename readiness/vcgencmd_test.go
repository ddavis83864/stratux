package readiness

import "testing"

func TestParseThrottled_Nominal(t *testing.T) {
	s, err := ParseThrottled("throttled=0x0\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Throttled() || s.Undervoltage() {
		t.Errorf("0x0 should report no throttling and no undervoltage, got %+v", s)
	}
}

func TestParseThrottled_CurrentUndervoltage(t *testing.T) {
	s, err := ParseThrottled("throttled=0x1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !s.UndervoltageNow || !s.Undervoltage() {
		t.Errorf("0x1 should report current undervoltage, got %+v", s)
	}
	if s.Throttled() {
		t.Errorf("0x1 should not report throttling, got %+v", s)
	}
}

func TestParseThrottled_HistoricalOnly(t *testing.T) {
	// 0x50005 is a real-world example: bits 0,2 (currently under-voltage
	// and throttled) plus bits 16,18 (both have occurred since boot).
	s, err := ParseThrottled("throttled=0x50005")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !s.UndervoltageNow || !s.ThrottledNow || !s.UndervoltageOccurred || !s.ThrottledOccurred {
		t.Errorf("0x50005 should set all four flags, got %+v", s)
	}
}

func TestParseThrottled_OccurredButNotCurrentlyThrottled(t *testing.T) {
	// bit 16 only: undervoltage happened once since boot but has cleared.
	s, err := ParseThrottled("throttled=0x10000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.UndervoltageNow {
		t.Error("bit 16 alone should not mean undervoltage is happening right now")
	}
	if !s.Undervoltage() {
		t.Error("Undervoltage() should still report true - it has occurred and that matters for diagnostics")
	}
}

func TestParseThrottled_EmptyIsError(t *testing.T) {
	if _, err := ParseThrottled(""); err == nil {
		t.Error("empty input should be an error, not a silently-nominal result")
	}
}

func TestParseThrottled_GarbageIsError(t *testing.T) {
	if _, err := ParseThrottled("not a valid line"); err == nil {
		t.Error("unparseable input should be an error")
	}
}
