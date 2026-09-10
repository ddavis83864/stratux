/*
trafficcpasettings.go: versioned, atomically-persisted settings for the
closure-rate/closest-point-of-approach (CPA) traffic-alerting enhancement -
mirrors alertsettings.go's exact temp-file/fsync/rename pattern. See
docs/traffic-cpa-alerting.md's "Settings and defaults" section.
*/
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/stratux/stratux/alerting"
	"github.com/stratux/stratux/trafficcpa"
)

// TrafficCPASettingsSchemaVersion is bumped whenever TrafficCPASettings
// gains, removes, or changes the meaning of a field a consumer should
// notice.
const TrafficCPASettingsSchemaVersion = 1

// trafficCPASettingsPath is a var, not a const, solely so tests can
// redirect it at a temp file for the duration of one test - mirrors
// alertSettingsPath's own pattern. Never reassigned in production.
var trafficCPASettingsPath = PersistentDataPath + "/traffic-cpa-settings.json"

// TrafficCPASettings is the full, user-configurable, persisted CPA
// configuration. Passive CPA calculation/display is controlled entirely
// by whether ownship/target data supports it (see
// main/trafficcpaapi.go); EscalationEnabled is the one field that gates
// whether a valid estimate may ever raise an existing alert's tier - see
// alerting.Config.CPAEscalationEnabled's own doc comment.
type TrafficCPASettings struct {
	SchemaVersion int `json:"schemaVersion"`

	// EscalationEnabled defaults to false - see
	// docs/traffic-cpa-alerting.md's "Hardware-validation checklist" for
	// why this stays off until real-hardware validation, independent of
	// this branch's own software verification.
	EscalationEnabled bool `json:"escalationEnabled"`

	// HorizonSeconds bounds the maximum useful TCPA - see
	// trafficcpa.Config.HorizonSeconds.
	HorizonSeconds float64 `json:"horizonSeconds"`

	// MinRelativeSpeedKnots is trafficcpa's own numerical-stability
	// floor - see trafficcpa.Config.MinRelativeSpeedKnots.
	MinRelativeSpeedKnots float64 `json:"minRelativeSpeedKnots"`

	// MinClosureRateKnots is the alerting-policy escalation minimum -
	// see alerting.Config.CPAMinClosureRateKnots. Deliberately a
	// separate, independently-configurable field: Validate requires it
	// be at least MinRelativeSpeedKnots (a policy minimum looser than
	// the computation's own validity floor could never bind on
	// anything - see Validate's own doc comment), but an owner may set
	// it higher for a more conservative escalation policy.
	MinClosureRateKnots float64 `json:"minClosureRateKnots"`
}

// maxTrafficCPAHorizonSeconds/maxTrafficCPASpeedKnots bound settings to a
// documented maximum, matching this project's "no arbitrary-duration/
// magnitude setting without a documented bound" convention (see
// main/alertsettings.go's maxMuteDuration).
const (
	maxTrafficCPAHorizonSeconds = 600 // 10 minutes
	maxTrafficCPASpeedKnots     = 500 // well above any realistic GA/light-jet closure speed
)

// DefaultTrafficCPASettings returns this project's documented,
// conservative defaults - matched against trafficcpa's own and
// alerting's own package defaults rather than invented independently
// here (see TestDefaultTrafficCPASettingsMatchesPackageDefaults).
func DefaultTrafficCPASettings() TrafficCPASettings {
	cpaCfg := trafficcpa.DefaultConfig()
	alertCfg := alerting.DefaultConfig()
	return TrafficCPASettings{
		SchemaVersion:         TrafficCPASettingsSchemaVersion,
		EscalationEnabled:     false,
		HorizonSeconds:        cpaCfg.HorizonSeconds,
		MinRelativeSpeedKnots: cpaCfg.MinRelativeSpeedKnots,
		MinClosureRateKnots:   alertCfg.CPAMinClosureRateKnots,
	}
}

// Validate range-checks every numeric field, rejecting NaN/Inf, zero or
// negative values, an excessive horizon/speed, and an inverted policy
// threshold (a MinClosureRateKnots below MinRelativeSpeedKnots could
// never bind - trafficcpa itself already rejects anything slower than
// MinRelativeSpeedKnots as numerically unreliable before the alerting
// policy's own, supposedly-independent minimum would ever see it).
func (s TrafficCPASettings) Validate() error {
	if err := toTrafficCPAConfig(s).Validate(); err != nil {
		return err
	}
	if s.HorizonSeconds > maxTrafficCPAHorizonSeconds {
		return fmt.Errorf("horizonSeconds exceeds the maximum of %v", maxTrafficCPAHorizonSeconds)
	}
	if s.MinRelativeSpeedKnots > maxTrafficCPASpeedKnots {
		return fmt.Errorf("minRelativeSpeedKnots exceeds the maximum of %v", maxTrafficCPASpeedKnots)
	}
	if s.MinClosureRateKnots > maxTrafficCPASpeedKnots {
		return fmt.Errorf("minClosureRateKnots exceeds the maximum of %v", maxTrafficCPASpeedKnots)
	}
	if s.MinClosureRateKnots < s.MinRelativeSpeedKnots {
		return fmt.Errorf("minClosureRateKnots must be at least minRelativeSpeedKnots (%v)", s.MinRelativeSpeedKnots)
	}
	return nil
}

// toTrafficCPAConfig converts persisted settings into a trafficcpa.Config
// - the envelope fields (MaxHorizontalSeparationMeters/
// MaxAbsoluteLatitudeDeg) are not user-configurable and always come from
// trafficcpa.DefaultConfig(), except MaxHorizontalSeparationMeters,
// which main/trafficcpaapi.go instead derives dynamically from the
// CURRENT alerting monitoring envelope at computation time (see that
// file's own doc comment for why these two envelopes must never be
// allowed to drift apart) - this function fills in the package default
// for that one field purely so a caller that only wants Validate() (which
// never reads it) has a structurally complete Config without an extra
// parameter.
func toTrafficCPAConfig(s TrafficCPASettings) trafficcpa.Config {
	cfg := trafficcpa.DefaultConfig()
	cfg.HorizonSeconds = s.HorizonSeconds
	cfg.MinRelativeSpeedKnots = s.MinRelativeSpeedKnots
	return cfg
}

var trafficCPASettingsMu sync.Mutex

// loadTrafficCPASettings reads trafficCPASettingsPath. A missing or
// corrupt file degrades safely to DefaultTrafficCPASettings() (logged,
// never fatal).
func loadTrafficCPASettings() TrafficCPASettings {
	trafficCPASettingsMu.Lock()
	defer trafficCPASettingsMu.Unlock()
	data, err := os.ReadFile(trafficCPASettingsPath)
	if err != nil {
		return DefaultTrafficCPASettings()
	}
	var s TrafficCPASettings
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("trafficcpa: settings file corrupt, using defaults: %s\n", err)
		return DefaultTrafficCPASettings()
	}
	if s.SchemaVersion != TrafficCPASettingsSchemaVersion {
		log.Printf("trafficcpa: settings file schema %d unsupported, using defaults\n", s.SchemaVersion)
		return DefaultTrafficCPASettings()
	}
	if err := s.Validate(); err != nil {
		log.Printf("trafficcpa: settings file failed validation, using defaults: %s\n", err)
		return DefaultTrafficCPASettings()
	}
	return s
}

// saveTrafficCPASettings validates and atomically persists s - temp file
// in the same directory, fsync, rename, matching alertsettings.go's
// established pattern exactly.
func saveTrafficCPASettings(s TrafficCPASettings) error {
	if err := s.Validate(); err != nil {
		return err
	}
	trafficCPASettingsMu.Lock()
	defer trafficCPASettingsMu.Unlock()

	dir := filepath.Dir(trafficCPASettingsPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("could not create settings directory: %w", err)
	}
	data, err := json.MarshalIndent(&s, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal traffic CPA settings: %w", err)
	}
	tmp := trafficCPASettingsPath + ".tmp"
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
	if err := os.Rename(tmp, trafficCPASettingsPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not finalize settings file: %w", err)
	}
	return nil
}
