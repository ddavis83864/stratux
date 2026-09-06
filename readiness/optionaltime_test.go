package readiness

import (
	"encoding/json"
	"testing"
	"time"
)

func TestOptionalTime_ZeroTimeMarshalsNull(t *testing.T) {
	o := SomeTime(time.Time{})
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Errorf("got %s, want null", b)
	}
}

func TestOptionalTime_NoTimeMarshalsNull(t *testing.T) {
	b, err := json.Marshal(NoTime())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Errorf("got %s, want null", b)
	}
}

func TestOptionalTime_RealValueMarshalsSameAsPlainTime(t *testing.T) {
	real := time.Date(2026, 8, 31, 3, 11, 21, 0, time.UTC)
	o := SomeTime(real)
	got, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(real)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("OptionalTime marshaling diverges from plain time.Time: got %s, want %s", got, want)
	}
}

func TestOptionalTime_NoYearOneSubstring(t *testing.T) {
	b, _ := json.Marshal(NoTime())
	if string(b) != "null" {
		t.Errorf("unavailable OptionalTime must never contain a year-1 timestamp, got %s", b)
	}
}

func TestOptionalTime_RoundTrip(t *testing.T) {
	real := time.Date(2026, 8, 31, 3, 11, 21, 123000000, time.UTC)
	o := SomeTime(real)
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var back OptionalTime
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Valid || !back.Time.Equal(real) {
		t.Errorf("round-trip mismatch: got %+v, want %+v", back, o)
	}
}

func TestOptionalTime_UnmarshalNull(t *testing.T) {
	var o OptionalTime
	o.Valid = true // pre-set to confirm null actually resets it
	if err := json.Unmarshal([]byte("null"), &o); err != nil {
		t.Fatal(err)
	}
	if o.Valid {
		t.Error("unmarshaling null must produce an unavailable OptionalTime")
	}
}

func TestOptionalTime_IsZero(t *testing.T) {
	if !NoTime().IsZero() {
		t.Error("NoTime() must report IsZero() true")
	}
	if SomeTime(time.Now()).IsZero() {
		t.Error("a real value must report IsZero() false")
	}
}

func TestOptionalTime_EmbeddedInStruct(t *testing.T) {
	type wrapper struct {
		LastSeen OptionalTime
	}
	w := wrapper{LastSeen: NoTime()}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"LastSeen":null}` {
		t.Errorf("got %s, want {\"LastSeen\":null}", b)
	}
}
