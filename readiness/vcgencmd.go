package readiness

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Raspberry Pi firmware throttled-status bit meanings (vcgencmd
// get_throttled). Bits 0-3 are the live/current condition; bits 16-19
// record whether the condition has occurred at any point since boot.
const (
	bitUndervoltageNow      = 1 << 0
	bitThrottledNow         = 1 << 2
	bitUndervoltageOccurred = 1 << 16
	bitThrottledOccurred    = 1 << 18
)

// ThrottleStatus is the parsed result of `vcgencmd get_throttled`.
type ThrottleStatus struct {
	Raw uint32

	UndervoltageNow      bool
	ThrottledNow         bool
	UndervoltageOccurred bool
	ThrottledOccurred    bool
}

// Throttled reports true if throttling is active now or has occurred since
// boot - the health API and dashboard care about "has this happened",
// not only "is it happening in this exact instant", since a brief
// under-voltage event during a power-supply hiccup is exactly the kind of
// thing an operator needs to see even after it has passed.
func (s ThrottleStatus) Throttled() bool {
	return s.ThrottledNow || s.ThrottledOccurred
}

// Undervoltage reports true if under-voltage is active now or has
// occurred since boot. See Throttled for why "has occurred" counts.
func (s ThrottleStatus) Undervoltage() bool {
	return s.UndervoltageNow || s.UndervoltageOccurred
}

// ParseThrottled parses vcgencmd get_throttled's output, e.g.
// "throttled=0x50005". It performs no I/O and is exercised directly by
// tests against real firmware output strings.
func ParseThrottled(output string) (ThrottleStatus, error) {
	s := strings.TrimSpace(output)
	s = strings.TrimPrefix(s, "throttled=")
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return ThrottleStatus{}, fmt.Errorf("empty vcgencmd get_throttled output")
	}
	raw, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return ThrottleStatus{}, fmt.Errorf("could not parse throttled value %q: %w", output, err)
	}
	v := uint32(raw)
	return ThrottleStatus{
		Raw:                  v,
		UndervoltageNow:      v&bitUndervoltageNow != 0,
		ThrottledNow:         v&bitThrottledNow != 0,
		UndervoltageOccurred: v&bitUndervoltageOccurred != 0,
		ThrottledOccurred:    v&bitThrottledOccurred != 0,
	}, nil
}

// GetThrottled runs vcgencmd get_throttled and parses its output. This is
// the one real-hardware-touching half; ParseThrottled above is what tests
// exercise. It returns a zero-value, not-throttled ThrottleStatus and a
// non-nil error on any platform/build where vcgencmd is unavailable (e.g.
// a desktop dev machine) - callers should treat that as "unknown", not
// "definitely fine".
func GetThrottled() (ThrottleStatus, error) {
	out, err := exec.Command("vcgencmd", "get_throttled").Output()
	if err != nil {
		return ThrottleStatus{}, fmt.Errorf("vcgencmd get_throttled: %w", err)
	}
	return ParseThrottled(string(out))
}
