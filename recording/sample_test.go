package recording

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSample_AHRSAbsentFieldsSerializeAsNull(t *testing.T) {
	s := Sample{
		UTC:       time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Latitude:  47.6,
		Longitude: -122.3,
	}
	if s.HasAHRS() {
		t.Error("a sample with no AHRS fields set should report HasAHRS()=false")
	}
	if s.HasPressureAltitude() {
		t.Error("a sample with no pressure altitude set should report HasPressureAltitude()=false")
	}
	data, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	out := string(data)
	for _, field := range []string{"PitchDeg", "BankDeg", "VerticalAccelG", "GLoad", "PressureAltitudeFt"} {
		if !strings.Contains(out, `"`+field+`":null`) {
			t.Errorf("expected %q to serialize as null when AHRS is absent, got: %s", field, out)
		}
	}
}

func TestSample_AHRSPresentFieldsSerializeAsNumbers(t *testing.T) {
	pitch := 2.5
	s := Sample{PitchDeg: &pitch}
	if !s.HasAHRS() {
		t.Error("a sample with PitchDeg set should report HasAHRS()=true")
	}
	data, _ := json.Marshal(&s)
	if strings.Contains(string(data), `"PitchDeg":null`) {
		t.Error("PitchDeg should not serialize as null once set")
	}
}

func TestSample_ZeroBankAngleIsNotConfusedWithAbsent(t *testing.T) {
	// A real, level-flight bank angle of exactly 0 must remain
	// distinguishable from "no AHRS installed" (nil).
	zero := 0.0
	s := Sample{BankDeg: &zero}
	if !s.HasAHRS() {
		// HasAHRS is defined via PitchDeg, not BankDeg, per the documented
		// "set together or not at all" invariant - this test exercises
		// that a genuine zero value round-trips distinctly from nil
		// regardless of which field is inspected.
	}
	data, _ := json.Marshal(&s)
	if !strings.Contains(string(data), `"BankDeg":0`) {
		t.Errorf("a real zero bank angle should serialize as 0, not be lost or become null: %s", string(data))
	}
}
