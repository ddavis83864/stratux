package epaper

import "testing"

func TestNormalize_DisabledIsAlwaysValidRegardlessOfOtherFields(t *testing.T) {
	c := Config{Enabled: false, Panel: "nonsense", Page: "nonsense", Rotation: 45, RefreshIntervalSeconds: -5}
	got, err := Normalize(c)
	if err != nil {
		t.Fatalf("a disabled config must never fail validation, got: %v", err)
	}
	if got.Enabled {
		t.Errorf("Normalize must not flip Enabled")
	}
}

func TestNormalize_ZeroValueFillsSafeDefaults(t *testing.T) {
	got, err := Normalize(Config{Enabled: true})
	if err != nil {
		t.Fatalf("zero-value-but-enabled config should normalize to valid defaults, got: %v", err)
	}
	if got.Panel != PanelWaveshare37 {
		t.Errorf("Panel default = %q, want %q", got.Panel, PanelWaveshare37)
	}
	if got.Page != PageOverview {
		t.Errorf("Page default = %q, want %q", got.Page, PageOverview)
	}
	if got.RefreshIntervalSeconds != DefaultRefreshIntervalSeconds {
		t.Errorf("RefreshIntervalSeconds default = %d, want %d", got.RefreshIntervalSeconds, DefaultRefreshIntervalSeconds)
	}
	if got.FullRefreshEvery != DefaultFullRefreshEvery {
		t.Errorf("FullRefreshEvery default = %d, want %d", got.FullRefreshEvery, DefaultFullRefreshEvery)
	}
	if got.GPIO != DefaultGPIOMapping() {
		t.Errorf("GPIO default = %+v, want %+v", got.GPIO, DefaultGPIOMapping())
	}
}

func TestNormalize_RejectsUnsupportedValuesWhenEnabled(t *testing.T) {
	cases := []Config{
		{Enabled: true, Panel: "some-other-panel"},
		{Enabled: true, Page: "some-other-page"},
		{Enabled: true, Rotation: 45},
		{Enabled: true, RefreshIntervalSeconds: 1},
		{Enabled: true, FullRefreshEvery: MaxFullRefreshEvery + 1},
	}
	for i, c := range cases {
		if _, err := Normalize(c); err == nil {
			t.Errorf("case %d: expected an error, got none (%+v)", i, c)
		}
	}
}

func TestNormalize_RejectsConflictingGPIOMapping(t *testing.T) {
	c := Config{Enabled: true, GPIO: GPIOMapping{DC: 18, Busy: 24, Rst: 27, Pwr: 22}} // 18 = fan PWM
	if _, err := Normalize(c); err == nil {
		t.Errorf("expected rejection of a GPIO mapping that reuses the fan's pin")
	}
}

func TestDefaultGPIOMapping_NeverUsesExcludedPhysicalPins(t *testing.T) {
	// This is a documentation-level cross-check: pins 1 and 6 aren't BCM
	// numbers, so this just asserts PhysicalPinExcluded's own two known
	// values are exactly {1,6} and nothing else, protecting against a
	// future accidental edit widening or narrowing the exclusion set.
	for p := 1; p <= 40; p++ {
		want := p == 1 || p == 6
		if got := PhysicalPinExcluded(p); got != want {
			t.Errorf("PhysicalPinExcluded(%d) = %v, want %v", p, got, want)
		}
	}
}
