package main

import "testing"

// TestValidateSettingsValue_EpaperPanel mirrors epaper.Normalize's own
// rule: empty string means "use the default panel" (always valid); any
// other value must be a real supported panel identifier.
func TestValidateSettingsValue_EpaperPanel(t *testing.T) {
	cases := []struct {
		val     interface{}
		wantErr bool
	}{
		{"waveshare-3.7in", false},
		{"", false},
		{"not-a-real-panel", true},
		{123, true}, // wrong type entirely
	}
	for _, c := range cases {
		err := validateSettingsValue("EpaperPanel", c.val)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSettingsValue(EpaperPanel, %v) error = %v, wantErr %v", c.val, err, c.wantErr)
		}
	}
}

// TestValidateSettingsValue_EpaperPage mirrors epaper.Normalize's own
// rule: empty string means "use the default page" (always valid); any
// other value must be one of the three real supported pages.
func TestValidateSettingsValue_EpaperPage(t *testing.T) {
	cases := []struct {
		val     interface{}
		wantErr bool
	}{
		{"overview", false},
		{"receivers", false},
		{"health", false},
		{"", false},
		{"not-a-real-page", true},
	}
	for _, c := range cases {
		err := validateSettingsValue("EpaperPage", c.val)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSettingsValue(EpaperPage, %v) error = %v, wantErr %v", c.val, err, c.wantErr)
		}
	}
}

// TestValidateSettingsValue_EpaperRotation mirrors epaper.Normalize's own
// rule exactly: only 0, 90, 180, or 270 are valid - unlike Panel/Page,
// there is no "empty means default" case, since 0 is itself a genuine,
// valid rotation.
func TestValidateSettingsValue_EpaperRotation(t *testing.T) {
	cases := []struct {
		val     float64
		wantErr bool
	}{
		{0, false},
		{90, false},
		{180, false},
		{270, false},
		{45, true},
		{360, true},
		{-90, true},
		{999, true},
	}
	for _, c := range cases {
		err := validateSettingsValue("EpaperRotation", c.val)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSettingsValue(EpaperRotation, %v) error = %v, wantErr %v", c.val, err, c.wantErr)
		}
	}
}

// TestValidateSettingsValue_EpaperRefreshIntervalSeconds mirrors
// epaper.Normalize's own rule: zero or negative falls back to the
// default (always valid); only the dead zone strictly between 0 and the
// minimum (5) is rejected - this is the exact hardware-validation
// finding this test guards against regressing: before this fix, an
// out-of-range value here was silently persisted and then silently
// never applied by epaperd.
func TestValidateSettingsValue_EpaperRefreshIntervalSeconds(t *testing.T) {
	cases := []struct {
		val     float64
		wantErr bool
	}{
		{0, false},
		{-5, false},
		{5, false},
		{15, false},
		{1, true},
		{2, true},
		{4, true},
	}
	for _, c := range cases {
		err := validateSettingsValue("EpaperRefreshIntervalSeconds", c.val)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSettingsValue(EpaperRefreshIntervalSeconds, %v) error = %v, wantErr %v", c.val, err, c.wantErr)
		}
	}
}

// TestValidateSettingsValue_EpaperFullRefreshEvery mirrors
// epaper.Normalize's own rule: zero or negative falls back to the
// default (always valid); only exceeding the maximum (200) is rejected.
func TestValidateSettingsValue_EpaperFullRefreshEvery(t *testing.T) {
	cases := []struct {
		val     float64
		wantErr bool
	}{
		{0, false},
		{-1, false},
		{1, false},
		{20, false},
		{200, false},
		{201, true},
		{1000, true},
	}
	for _, c := range cases {
		err := validateSettingsValue("EpaperFullRefreshEvery", c.val)
		if (err != nil) != c.wantErr {
			t.Errorf("validateSettingsValue(EpaperFullRefreshEvery, %v) error = %v, wantErr %v", c.val, err, c.wantErr)
		}
	}
}

// TestValidateSettingsValue_EpaperEnabled confirms the plain bool type
// check still applies - the new range/enum checks above are additive,
// never a replacement for the existing type validation.
func TestValidateSettingsValue_EpaperEnabled(t *testing.T) {
	if err := validateSettingsValue("EpaperEnabled", true); err != nil {
		t.Errorf("validateSettingsValue(EpaperEnabled, true) unexpected error: %v", err)
	}
	if err := validateSettingsValue("EpaperEnabled", "true"); err == nil {
		t.Error("validateSettingsValue(EpaperEnabled, \"true\") expected a type error, got nil")
	}
}
