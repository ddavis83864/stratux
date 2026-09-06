/*
alertsettings.go: versioned, atomically-persisted settings for the
operational-alerting subsystem - mirrors calprofile.Store's established
temp-file/fsync/rename pattern (see docs/alerting.md's "Configuration and
persistence" section).
*/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stratux/stratux/alerting"
)

// AlertSettingsSchemaVersion is bumped whenever AlertSettings gains,
// removes, or changes the meaning of a field a consumer should notice.
const AlertSettingsSchemaVersion = 1

// alertSettingsPath is a var, not a const, solely so tests can redirect it
// at a temp file for the duration of one test - mirrors
// main/recordingapi.go's recordingsDir/exportsDir pattern. Never
// reassigned in production.
var alertSettingsPath = PersistentDataPath + "/alert-settings.json"

// AlertSettings is the full, user-configurable, persisted alerting
// configuration - see docs/http-api.md's alerting section for the field
// list and docs/alerting.md for the defaults' rationale.
type AlertSettings struct {
	SchemaVersion int `json:"schemaVersion"`

	MasterEnabled               bool `json:"masterEnabled"`
	VisualTrafficNoticesEnabled bool `json:"visualTrafficNoticesEnabled"`
	BrowserAudioEnabled         bool `json:"browserAudioEnabled"`
	SystemVisualEnabled         bool `json:"systemVisualEnabled"`
	SystemAudioEnabled          bool `json:"systemAudioEnabled"`

	// AudioVolume is 0.0-1.0; the dashboard maps this to a bounded,
	// conservative gain on the browser's own oscillator - never a claim
	// about physical loudness at any downstream headset.
	AudioVolume float64 `json:"audioVolume"`

	MonitoringHorizontalNM  float64 `json:"monitoringHorizontalNm"`
	MonitoringVerticalFeet  float64 `json:"monitoringVerticalFeet"`
	NoticeHorizontalNM      float64 `json:"noticeHorizontalNm"`
	NoticeVerticalFeet      float64 `json:"noticeVerticalFeet"`
	CautionHorizontalNM     float64 `json:"cautionHorizontalNm"`
	CautionVerticalFeet     float64 `json:"cautionVerticalFeet"`
	HighCautionHorizontalNM float64 `json:"highCautionHorizontalNm"`
	HighCautionVerticalFeet float64 `json:"highCautionVerticalFeet"`

	AudioCooldownNoticeSeconds   float64 `json:"audioCooldownNoticeSeconds"`
	AudioCooldownCautionSeconds  float64 `json:"audioCooldownCautionSeconds"`
	GlobalMinAudioSpacingSeconds float64 `json:"globalMinAudioSpacingSeconds"`

	SuppressGroundTraffic bool `json:"suppressGroundTraffic"`

	// Muted*/MuteUntilUnixSeconds persist the operator's mute preference
	// across a restart (distinct from active traffic-alert target state,
	// which is deliberately never persisted - see docs/alerting.md).
	// MuteUntilUnixSeconds is wall-clock (not monotonic, which resets on
	// restart) and is only meaningful when Muted is true and
	// MutedIndefinitely is false.
	Muted                bool  `json:"muted"`
	MutedIndefinitely    bool  `json:"mutedIndefinitely"`
	MuteUntilUnixSeconds int64 `json:"muteUntilUnixSeconds,omitempty"`
}

// DefaultAlertSettings returns the mission's documented, conservative
// defaults - see docs/alerting.md.
func DefaultAlertSettings() AlertSettings {
	d := alerting.DefaultConfig()
	return AlertSettings{
		SchemaVersion:                AlertSettingsSchemaVersion,
		MasterEnabled:                true,
		VisualTrafficNoticesEnabled:  true,
		BrowserAudioEnabled:          false, // requires an explicit user gesture - see docs/alerting.md
		SystemVisualEnabled:          true,
		SystemAudioEnabled:           false,
		AudioVolume:                  0.5,
		MonitoringHorizontalNM:       d.MonitoringHorizontalMeters / 1852.0,
		MonitoringVerticalFeet:       d.MonitoringVerticalFeet,
		NoticeHorizontalNM:           d.NoticeHorizontalMeters / 1852.0,
		NoticeVerticalFeet:           d.NoticeVerticalFeet,
		CautionHorizontalNM:          d.CautionHorizontalMeters / 1852.0,
		CautionVerticalFeet:          d.CautionVerticalFeet,
		HighCautionHorizontalNM:      d.HighCautionHorizontalMeters / 1852.0,
		HighCautionVerticalFeet:      d.HighCautionVerticalFeet,
		AudioCooldownNoticeSeconds:   d.AudioCooldownNoticeSeconds,
		AudioCooldownCautionSeconds:  d.AudioCooldownCautionSeconds,
		GlobalMinAudioSpacingSeconds: d.GlobalMinAudioSpacingSeconds,
		SuppressGroundTraffic:        d.SuppressGroundTraffic,
	}
}

// maxMuteDuration bounds a timed mute request to a documented maximum -
// "no arbitrary-duration mute without a documented bound."
const maxMuteDuration = 12 * time.Hour

// Validate range-checks every numeric field, rejecting NaN/Inf, negative
// distances/durations, an out-of-range volume, and an excessive mute
// duration - see docs/alerting.md and this mission's explicit requirement
// that settings persistence "reject NaN, infinity, negative distance,
// invalid duration, and excessive values."
func (s AlertSettings) Validate() error {
	numeric := []struct {
		name string
		v    float64
	}{
		{"audioVolume", s.AudioVolume},
		{"monitoringHorizontalNm", s.MonitoringHorizontalNM},
		{"monitoringVerticalFeet", s.MonitoringVerticalFeet},
		{"noticeHorizontalNm", s.NoticeHorizontalNM},
		{"noticeVerticalFeet", s.NoticeVerticalFeet},
		{"cautionHorizontalNm", s.CautionHorizontalNM},
		{"cautionVerticalFeet", s.CautionVerticalFeet},
		{"highCautionHorizontalNm", s.HighCautionHorizontalNM},
		{"highCautionVerticalFeet", s.HighCautionVerticalFeet},
		{"audioCooldownNoticeSeconds", s.AudioCooldownNoticeSeconds},
		{"audioCooldownCautionSeconds", s.AudioCooldownCautionSeconds},
		{"globalMinAudioSpacingSeconds", s.GlobalMinAudioSpacingSeconds},
	}
	for _, n := range numeric {
		if math.IsNaN(n.v) || math.IsInf(n.v, 0) {
			return fmt.Errorf("%s must be a finite number", n.name)
		}
		if n.v < 0 {
			return fmt.Errorf("%s must not be negative", n.name)
		}
	}
	if s.AudioVolume > 1.0 {
		return fmt.Errorf("audioVolume must be between 0.0 and 1.0")
	}
	if s.MuteUntilUnixSeconds < 0 {
		return fmt.Errorf("muteUntilUnixSeconds must not be negative")
	}
	if s.Muted && !s.MutedIndefinitely && s.MuteUntilUnixSeconds > 0 {
		// Deliberately compared in plain int64 seconds, never converted
		// to a nanosecond-based time.Duration first: an excessively large
		// (but still int64-representable) MuteUntilUnixSeconds would
		// overflow when multiplied up to nanoseconds, silently wrapping
		// around to a small or negative value that could wrongly pass
		// this check - caught live by
		// TestAlertSettings_ExcessiveMuteDurationRejected.
		remainingSeconds := s.MuteUntilUnixSeconds - time.Now().Unix()
		if remainingSeconds > int64(maxMuteDuration.Seconds()) {
			return fmt.Errorf("mute duration exceeds the maximum of %s", maxMuteDuration)
		}
	}
	return toAlertingConfig(s, true, true).Validate()
}

// toAlertingConfig converts the persisted, NM/feet-based settings into an
// alerting.Config (meters/feet) - the one place unit conversion happens.
func toAlertingConfig(s AlertSettings, trafficAudio, systemAudio bool) alerting.Config {
	cfg := alerting.DefaultConfig()
	cfg.MasterEnabled = s.MasterEnabled
	cfg.VisualEnabled = s.VisualTrafficNoticesEnabled
	cfg.SystemVisualEnabled = s.SystemVisualEnabled
	cfg.TrafficAudioEnabled = trafficAudio
	cfg.SystemAudioEnabled = systemAudio
	cfg.MonitoringHorizontalMeters = s.MonitoringHorizontalNM * 1852.0
	cfg.MonitoringVerticalFeet = s.MonitoringVerticalFeet
	cfg.NoticeHorizontalMeters = s.NoticeHorizontalNM * 1852.0
	cfg.NoticeVerticalFeet = s.NoticeVerticalFeet
	cfg.CautionHorizontalMeters = s.CautionHorizontalNM * 1852.0
	cfg.CautionVerticalFeet = s.CautionVerticalFeet
	cfg.HighCautionHorizontalMeters = s.HighCautionHorizontalNM * 1852.0
	cfg.HighCautionVerticalFeet = s.HighCautionVerticalFeet
	cfg.AudioCooldownNoticeSeconds = s.AudioCooldownNoticeSeconds
	cfg.AudioCooldownCautionSeconds = s.AudioCooldownCautionSeconds
	cfg.GlobalMinAudioSpacingSeconds = s.GlobalMinAudioSpacingSeconds
	cfg.SuppressGroundTraffic = s.SuppressGroundTraffic
	return cfg
}

var alertSettingsMu sync.Mutex

// loadAlertSettings reads alertSettingsPath. A missing or corrupt file
// degrades safely to DefaultAlertSettings() (logged, never fatal) - "recover
// safely from a corrupt settings file" and "never block traffic or GDL90
// processing."
func loadAlertSettings() AlertSettings {
	alertSettingsMu.Lock()
	defer alertSettingsMu.Unlock()
	data, err := os.ReadFile(alertSettingsPath)
	if err != nil {
		return DefaultAlertSettings()
	}
	var s AlertSettings
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("alerting: settings file corrupt, using defaults: %s\n", err)
		return DefaultAlertSettings()
	}
	if s.SchemaVersion != AlertSettingsSchemaVersion {
		log.Printf("alerting: settings file failed validation (schema=%d), using defaults\n", s.SchemaVersion)
		return DefaultAlertSettings()
	}
	if err := s.Validate(); err != nil {
		// A still-legitimate persisted timed mute can spuriously fail
		// the excessive-duration check if this load happens before the
		// system clock is GPS-corrected: an RTC-less board boots with a
		// stale/default wall clock until a GPS fix arrives (see the
		// "trusted-time" one-time correction in main/health.go), so
		// time.Now() here can read far earlier than it did when the
		// mute was saved, making a short real remaining duration look
		// enormous. Rather than discard every other persisted
		// preference (thresholds, audio/visual toggles) over one
		// clock-skew-sensitive field, drop only the mute state - fail
		// safe toward "not muted" (alerts stay visible) - and retry
		// before giving up on the whole file.
		unmuted := s
		unmuted.Muted = false
		unmuted.MutedIndefinitely = false
		unmuted.MuteUntilUnixSeconds = 0
		if unmuted.Validate() == nil {
			log.Printf("alerting: settings file's mute state failed validation, clearing mute and keeping other settings: %s\n", err)
			return unmuted
		}
		log.Printf("alerting: settings file failed validation (schema=%d), using defaults: %s\n", s.SchemaVersion, err)
		return DefaultAlertSettings()
	}
	return s
}

// saveAlertSettings validates and atomically persists s - temp file in the
// same directory, fsync, rename, matching calprofile.Store's established
// pattern. Never called while recMu or any traffic-relevant lock is held.
func saveAlertSettings(s AlertSettings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	alertSettingsMu.Lock()
	defer alertSettingsMu.Unlock()

	dir := filepath.Dir(alertSettingsPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create settings directory: %w", err)
	}
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal alert settings: %w", err)
	}
	tmp := alertSettingsPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("could not create temp settings file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not write temp settings file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not sync temp settings file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not close temp settings file: %w", err)
	}
	if err := os.Rename(tmp, alertSettingsPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not finalize settings file: %w", err)
	}
	return nil
}
