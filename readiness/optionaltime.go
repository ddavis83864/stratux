package readiness

import (
	"encoding/json"
	"time"
)

// OptionalTime is a wall-clock UTC instant that may not yet be available -
// "no frame received this daemon lifetime," "never synchronized," "no
// application-layer client evidence exists." It marshals as JSON `null`
// when unavailable, never as the `"0001-01-01T00:00:00Z"` a bare
// time.Time's zero value would otherwise produce - which reads as a real,
// if implausible, date rather than "this value does not exist."
//
// For any value that IS present, the JSON representation is byte-for-byte
// identical to what a plain time.Time field already produced (RFC3339Nano),
// so an existing consumer that only ever saw non-zero values is unaffected;
// only a consumer that was silently treating the old zero-value sentinel as
// meaningful needs to change, which is the correct fix on their side too.
type OptionalTime struct {
	Time  time.Time
	Valid bool
}

// SomeTime returns an OptionalTime wrapping t, or an unavailable OptionalTime
// if t is the zero time.Time - the one caller-facing constructor, so "is
// this actually a value" is decided in exactly one place.
func SomeTime(t time.Time) OptionalTime {
	if t.IsZero() {
		return OptionalTime{}
	}
	return OptionalTime{Time: t, Valid: true}
}

// NoTime is the explicit "unavailable" value, for call sites where writing
// SomeTime(time.Time{}) would obscure the intent.
func NoTime() OptionalTime { return OptionalTime{} }

// IsZero reports whether the value is unavailable.
func (o OptionalTime) IsZero() bool { return !o.Valid }

func (o OptionalTime) MarshalJSON() ([]byte, error) {
	if !o.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(o.Time)
}

func (o *OptionalTime) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*o = OptionalTime{}
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(data, &t); err != nil {
		return err
	}
	*o = OptionalTime{Time: t, Valid: true}
	return nil
}
