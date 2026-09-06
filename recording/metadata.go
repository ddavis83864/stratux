package recording

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/stratux/stratux/preflight"
)

// MetadataSchemaVersion is bumped whenever SessionMetadata, SessionSnapshot,
// or SessionFinalization gains, removes, or changes the meaning of a field a
// consumer should notice - mirroring preflight.SchemaVersion's convention.
const MetadataSchemaVersion = 1

// metadataFileName is the fixed sidecar filename inside one recording's own
// directory - not a timestamped name like the rotated *.jsonl sample files,
// since there is exactly one metadata record per recording. A fixed name
// makes "does this recording have metadata" a single stat, not a directory
// scan, and keeps the temp-file-and-rename atomic-write path trivial (see
// WriteInitialMetadata/finalizeMetadataLocked).
const metadataFileName = "metadata.json"

// SessionMetadata is the durable, versioned sidecar record for one
// recording - see docs/recording.md for the full design rationale. It is
// written once at recording start (Snapshot) and updated exactly once more,
// at a normal stop, touching only Finalization.
type SessionMetadata struct {
	SchemaVersion int    `json:"schemaVersion"`
	RecordingID   string `json:"recordingId"`

	// Snapshot is captured exactly once, when the recording starts, and is
	// never modified afterward - see SessionSnapshot's own doc comment.
	Snapshot SessionSnapshot `json:"snapshot"`

	// Finalization holds only the fields that legitimately change after
	// the recording starts - see SessionFinalization's own doc comment.
	// Zero value (Complete: false) until the recording actually finishes
	// normally.
	Finalization SessionFinalization `json:"finalization"`
}

// SessionSnapshot is the immutable point-in-time state captured once, when
// a recording starts. Nothing in this struct is ever modified after
// WriteInitialMetadata returns - a later recording that wants updated
// values gets its own, separate SessionMetadata instead. This is
// recording-start state only, not the live/current Preflight state: no API
// response or dashboard rendering of this struct may present it as
// "current."
//
// Every field here is already safe to persist and to expose: the
// preflight.CheckResult/ProfileSummary-shaped fields carry no GPS
// coordinates, MAC addresses, or credentials by that type's own existing
// design (the same type GET /getPreflightReport already returns), and the
// remaining fields are plain scalar identifiers.
type SessionSnapshot struct {
	// CapturedAtUTC is nil if trusted wall-clock time was not yet
	// available when the recording started - never a fabricated time.
	CapturedAtUTC         *time.Time `json:"capturedAtUtc,omitempty"`
	CapturedAtMonoSeconds float64    `json:"capturedAtMonoSeconds"`

	StratuxVersion string `json:"stratuxVersion,omitempty"`
	StratuxCommit  string `json:"stratuxCommit,omitempty"`

	PreflightBootSessionID          string     `json:"preflightBootSessionId,omitempty"`
	PreflightGeneratedAt            *time.Time `json:"preflightGeneratedAt,omitempty"`
	PreflightGeneratedAtMonoSeconds float64    `json:"preflightGeneratedAtMonoSeconds"`
	PreflightOverallState           string     `json:"preflightOverallState,omitempty"`
	PreflightRequiredActionCount    int        `json:"preflightRequiredActionCount"`
	PreflightCautionCount           int        `json:"preflightCautionCount"`

	// PreflightAutomated/PreflightManual reuse preflight.CheckResult
	// verbatim (see docs/recording.md's design note) rather than
	// re-deriving or duplicating per-component readiness logic.
	PreflightAutomated []preflight.CheckResult `json:"preflightAutomated,omitempty"`
	PreflightManual    []preflight.CheckResult `json:"preflightManual,omitempty"`

	TrustedTimeAvailable bool `json:"trustedTimeAvailable"`
	GPSFixAvailable      bool `json:"gpsFixAvailable"`

	CalibrationProfileID        string `json:"calibrationProfileId,omitempty"`
	CalibrationProfileName      string `json:"calibrationProfileName,omitempty"`
	CalibrationProfileKind      string `json:"calibrationProfileKind,omitempty"`
	CalibrationValid            bool   `json:"calibrationValid"`
	CalibrationProfileAvailable bool   `json:"calibrationProfileAvailable"`
}

// SessionFinalization holds the fields that legitimately change after a
// recording starts. It is populated once, when the recording stops
// normally (Complete: true), and is otherwise left at its zero value
// (Complete: false) - which is also the honest, correct representation of
// a recording interrupted by a daemon restart or device reboot: nothing
// ever ran the normal stop path for it, so nothing ever set Complete true.
// No separate "crash recovery" pass is needed to produce that signal.
type SessionFinalization struct {
	Complete        bool       `json:"complete"`
	StoppedAtUTC    *time.Time `json:"stoppedAtUtc,omitempty"`
	DurationSeconds float64    `json:"durationSeconds,omitempty"`
	SampleCount     int64      `json:"sampleCount"`
}

// metadataPath returns the fixed sidecar path inside a recording directory
// already known to be valid (the caller is responsible for path-safety -
// see main/recordingapi.go's validRecordingDir, the same check every other
// recording-directory consumer already uses).
func metadataPath(dir string) string {
	return filepath.Join(dir, metadataFileName)
}

// atomicWriteMetadata marshals meta and writes it to metadataPath(dir)
// atomically: a temp file in the same directory, fsync'd, then renamed over
// the final name - the same pattern already established by
// calprofile.Store's atomicWriteJSON and readiness.WriteDiagnosticBundle
// (see docs/recording.md's design note for why no directory-level fsync is
// added). A reader never observes a partially-written file, and a crash
// between the temp-file write and the rename leaves, at worst, a harmless
// orphaned ".tmp" file next to either no metadata.json or the previous
// valid one.
func atomicWriteMetadata(dir string, meta SessionMetadata) error {
	data, err := json.MarshalIndent(&meta, "", "  ")
	if err != nil {
		return fmt.Errorf("could not marshal recording metadata: %w", err)
	}
	path := metadataPath(dir)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("could not create temp metadata file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not write temp metadata file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("could not sync temp metadata file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not close temp metadata file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("could not finalize metadata file: %w", err)
	}
	return nil
}

// WriteInitialMetadata writes a recording's SessionMetadata for the first
// time, with the given snapshot and a zero-value (incomplete)
// Finalization. Must be called at most once per recording, before any
// finalization call - see FinalizeMetadata.
//
// This performs file I/O and must never be called while recMu (or any
// other recording-lifecycle lock) is held - see docs/recording.md's
// locking-strategy note.
func WriteInitialMetadata(dir, recordingID string, snapshot SessionSnapshot) error {
	meta := SessionMetadata{
		SchemaVersion: MetadataSchemaVersion,
		RecordingID:   recordingID,
		Snapshot:      snapshot,
	}
	return atomicWriteMetadata(dir, meta)
}

// FinalizeMetadata updates only the Finalization field of a recording's
// already-written metadata, atomically, leaving Snapshot exactly as it was
// written by WriteInitialMetadata. If no metadata.json exists yet (the
// initial write itself failed, or this is somehow called for a legacy
// recording), FinalizeMetadata returns ErrNoMetadata rather than
// fabricating a Snapshot - a finalization record with no matching snapshot
// would misrepresent recording-start state that was never actually
// captured.
//
// Idempotent: calling this twice with the same finalization simply
// rewrites the same content - matching stopActiveRecording's own existing
// idempotent-stop behavior.
//
// Performs file I/O and must never be called while recMu is held.
func FinalizeMetadata(dir string, finalization SessionFinalization) error {
	existing := ReadMetadata(dir)
	if existing.Status != MetadataOK {
		return ErrNoMetadata
	}
	existing.Metadata.Finalization = finalization
	return atomicWriteMetadata(dir, existing.Metadata)
}

// MetadataStatus classifies what ReadMetadata found, distinguishing
// "legacy recording, no metadata was ever written" from "metadata exists
// but failed to parse" - these must never be conflated: the former is
// completely expected for anything recorded before this feature (or a
// recording whose initial write itself failed), the latter is a real,
// reportable data-integrity condition.
type MetadataStatus string

const (
	// MetadataOK: metadata.json exists and parsed successfully.
	MetadataOK MetadataStatus = "ok"
	// MetadataUnavailable: no metadata.json exists - a legacy recording,
	// or one whose initial write failed. Not an error.
	MetadataUnavailable MetadataStatus = "unavailable"
	// MetadataCorrupt: metadata.json exists but could not be parsed
	// (truncated, invalid JSON, or unreadable).
	MetadataCorrupt MetadataStatus = "corrupt"
)

// ErrNoMetadata is returned by FinalizeMetadata when no valid
// metadata.json exists yet to finalize.
var ErrNoMetadata = fmt.Errorf("no recording metadata to finalize")

// MetadataReadResult is ReadMetadata's return shape - Metadata is only
// meaningful when Status == MetadataOK.
type MetadataReadResult struct {
	Status MetadataStatus
	// Error is set only when Status == MetadataCorrupt, with the
	// underlying parse failure - safe to log, never a secret.
	Error    string
	Metadata SessionMetadata
}

// ReadMetadata reads and parses dir's metadata.json, if any. It never
// returns a Go error for the ordinary "legacy recording" case - see
// MetadataStatus. A ".tmp" file left behind by an interrupted write (e.g.
// a daemon killed mid-write) is never itself read; only the final,
// atomically-renamed name is ever considered, so residue like that is
// harmless and ignored here (see docs/recording.md's atomic-write note).
func ReadMetadata(dir string) MetadataReadResult {
	data, err := os.ReadFile(metadataPath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return MetadataReadResult{Status: MetadataUnavailable}
		}
		return MetadataReadResult{Status: MetadataCorrupt, Error: err.Error()}
	}
	var meta SessionMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return MetadataReadResult{Status: MetadataCorrupt, Error: err.Error()}
	}
	return MetadataReadResult{Status: MetadataOK, Metadata: meta}
}
