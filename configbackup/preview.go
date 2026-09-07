package configbackup

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/stratux/stratux/calprofile"
)

// FieldChange is one changed field, named exactly as it appears in the
// section's own JSON encoding so a displayed diff matches the document a
// technical owner could inspect themselves.
type FieldChange struct {
	Field    string      `json:"field"`
	Current  interface{} `json:"current"`
	Proposed interface{} `json:"proposed"`
}

// ProfileSummary names one calibration profile in a preview list -
// deliberately never includes the calibration vectors themselves; a
// preview is for confirming *which* profiles and *which* identity/active
// state change, not for re-deriving calibration math.
type ProfileSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// CurrentState is everything main/ gathers about the live device to diff
// against an uploaded backup. Unlike BuildInputs, this is never
// checksummed as a Document - it exists only to be compared against one.
type CurrentState struct {
	Version string
	Commit  string

	Configuration       ConfigurationSection
	CalibrationProfiles []calprofile.Profile
	ActiveProfileID     string
	AlertSettings       AlertSettingsSection
}

// Preview is Validate's companion: an accurate, human-readable account of
// exactly what applying doc would change against current, computed
// without any write. Two calls with the same (doc, current) always
// produce an identical Preview (aside from map/slice ordering, which is
// always sorted) - see ComputePreview's determinism tests.
type Preview struct {
	SchemaVersion         int    `json:"schemaVersion"`
	SourceVersion         string `json:"sourceVersion"`
	SourceCommit          string `json:"sourceCommit"`
	CompatibleWithCurrent bool   `json:"compatibleWithCurrent"`
	CurrentVersion        string `json:"currentVersion"`
	CurrentCommit         string `json:"currentCommit"`

	ConfigurationChanges []FieldChange `json:"configurationChanges,omitempty"`
	AlertSettingsChanges []FieldChange `json:"alertSettingsChanges,omitempty"`

	AddedProfiles     []ProfileSummary `json:"addedProfiles,omitempty"`
	UpdatedProfiles   []ProfileSummary `json:"updatedProfiles,omitempty"`
	UnchangedProfiles []ProfileSummary `json:"unchangedProfiles,omitempty"`

	ActiveProfileChange *FieldChange `json:"activeProfileChange,omitempty"`

	ContainsPrivacySensitiveFields bool     `json:"containsPrivacySensitiveFields"`
	PrivacySensitiveFieldNames     []string `json:"privacySensitiveFieldNames,omitempty"`

	Warnings       []string `json:"warnings,omitempty"`
	BlockingErrors []string `json:"blockingErrors,omitempty"`

	// RestartRequired is a documented heuristic, not a guarantee: true
	// whenever a radio/hardware-facing Configuration field changes,
	// since this project's existing settings API already requires a
	// restart (or equivalent re-init) for those to take effect - see
	// docs/configuration-backup-restore.md's "Apply behavior" section.
	// Alert-settings and calibration-profile changes are always applied
	// live and never set this.
	RestartRequired bool `json:"restartRequired"`

	HasChanges bool `json:"hasChanges"`
}

// configurationFieldsRequiringRestart names every ConfigurationSection
// JSON field whose live equivalent (main/managementinterface.go's
// settings handlers) is documented as requiring a restart/re-init to
// actually take effect, as opposed to a purely cosmetic/display
// preference.
var configurationFieldsRequiringRestart = map[string]bool{
	"darkMode": false, "displayTrafficSource": false,
	"radarLimits": false, "radarRange": false, "gLimits": false,
	// everything else defaults to true via fieldRequiresRestart below.
}

func fieldRequiresRestart(field string) bool {
	if requires, known := configurationFieldsRequiringRestart[field]; known {
		return requires
	}
	return true
}

// ComputePreview performs no writes and calls no other package's
// mutating APIs.
func ComputePreview(doc Document, current CurrentState) Preview {
	preview := Preview{
		SchemaVersion:         doc.SchemaVersion,
		SourceVersion:         doc.SourceVersion,
		SourceCommit:          doc.SourceCommit,
		CompatibleWithCurrent: doc.SchemaVersion >= MinimumCompatibleSchemaVersion && doc.SchemaVersion <= SchemaVersion,
		CurrentVersion:        current.Version,
		CurrentCommit:         current.Commit,
	}

	preview.ConfigurationChanges = diffJSONFields(current.Configuration, doc.Configuration)
	preview.AlertSettingsChanges = diffJSONFields(current.AlertSettings, doc.AlertSettings)

	for _, c := range preview.ConfigurationChanges {
		if fieldRequiresRestart(c.Field) {
			preview.RestartRequired = true
			break
		}
	}

	byID := make(map[string]calprofile.Profile, len(current.CalibrationProfiles))
	for _, p := range current.CalibrationProfiles {
		byID[p.ID] = p
	}
	backupIDs := make([]string, 0, len(doc.CalibrationProfiles))
	for _, p := range doc.CalibrationProfiles {
		backupIDs = append(backupIDs, p.ID)
	}
	sort.Strings(backupIDs)
	backupByID := make(map[string]calprofile.Profile, len(doc.CalibrationProfiles))
	for _, p := range doc.CalibrationProfiles {
		backupByID[p.ID] = p
	}
	for _, id := range backupIDs {
		bp := backupByID[id]
		if cur, exists := byID[id]; !exists {
			preview.AddedProfiles = append(preview.AddedProfiles, ProfileSummary{ID: bp.ID, Name: bp.Name})
		} else if !profilesEqual(cur, bp) {
			preview.UpdatedProfiles = append(preview.UpdatedProfiles, ProfileSummary{ID: bp.ID, Name: bp.Name})
		} else {
			preview.UnchangedProfiles = append(preview.UnchangedProfiles, ProfileSummary{ID: bp.ID, Name: bp.Name})
		}
	}

	if doc.ActiveCalibrationProfileID != "" && doc.ActiveCalibrationProfileID != current.ActiveProfileID {
		preview.ActiveProfileChange = &FieldChange{
			Field:    "activeCalibrationProfileId",
			Current:  current.ActiveProfileID,
			Proposed: doc.ActiveCalibrationProfileID,
		}
	}

	if !doc.Configuration.PrivacySensitive.Empty() {
		preview.ContainsPrivacySensitiveFields = true
		p := doc.Configuration.PrivacySensitive
		if p.OwnshipModeS != "" {
			preview.PrivacySensitiveFieldNames = append(preview.PrivacySensitiveFieldNames, "ownshipModeS")
		}
		if p.OGNAddr != "" {
			preview.PrivacySensitiveFieldNames = append(preview.PrivacySensitiveFieldNames, "ognAddr")
		}
		if p.OGNReg != "" {
			preview.PrivacySensitiveFieldNames = append(preview.PrivacySensitiveFieldNames, "ognReg")
		}
		if p.OGNPilot != "" {
			preview.PrivacySensitiveFieldNames = append(preview.PrivacySensitiveFieldNames, "ognPilot")
		}
		preview.Warnings = append(preview.Warnings, "This backup contains privacy-sensitive ownship-identifying fields - review privacySensitiveFieldNames before applying.")
	}

	preview.HasChanges = len(preview.ConfigurationChanges) > 0 ||
		len(preview.AlertSettingsChanges) > 0 ||
		len(preview.AddedProfiles) > 0 ||
		len(preview.UpdatedProfiles) > 0 ||
		preview.ActiveProfileChange != nil

	return preview
}

// profilesEqual compares two profiles field-for-field via their JSON
// encoding - simpler and just as exact as a manual field list, and
// automatically stays correct if calprofile.Profile ever gains a field.
func profilesEqual(a, b calprofile.Profile) bool {
	aj, err1 := json.Marshal(a)
	bj, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return bytes.Equal(aj, bj)
}

// diffJSONFields compares current and proposed (must be the same struct
// type) key-by-key via their JSON encoding, returning one FieldChange per
// differing key. This is intentionally generic (reflection via JSON,
// rather than a hand-written comparison per field) so it stays correct as
// ConfigurationSection/AlertSettingsSection gain fields, at the cost of
// reporting values as whatever json.Unmarshal produces for
// interface{} (numbers as float64) - acceptable for a display-only diff.
func diffJSONFields(current, proposed interface{}) []FieldChange {
	curBytes, err1 := json.Marshal(current)
	propBytes, err2 := json.Marshal(proposed)
	if err1 != nil || err2 != nil {
		return nil
	}
	var curMap, propMap map[string]json.RawMessage
	if json.Unmarshal(curBytes, &curMap) != nil || json.Unmarshal(propBytes, &propMap) != nil {
		return nil
	}

	keys := make([]string, 0, len(propMap))
	for k := range propMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var changes []FieldChange
	for _, k := range keys {
		curRaw, hadCur := curMap[k]
		propRaw := propMap[k]
		if hadCur && bytes.Equal(curRaw, propRaw) {
			continue
		}
		if k == "privacySensitive" {
			// Reported separately (ContainsPrivacySensitiveFields/
			// PrivacySensitiveFieldNames) - never as an undifferentiated
			// generic field diff a reviewer could skim past.
			continue
		}
		var curVal, propVal interface{}
		if hadCur {
			_ = json.Unmarshal(curRaw, &curVal)
		}
		_ = json.Unmarshal(propRaw, &propVal)
		changes = append(changes, FieldChange{Field: k, Current: curVal, Proposed: propVal})
	}
	return changes
}
