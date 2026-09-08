/*
autorecordrecovery.go: honest recovery from a daemon restart or device
reboot that left an Automatic Flight Recording session running - see
docs/automatic-flight-recording.md's "Restart and crash recovery"
section.

This never inspects a recording's sample data (the *.jsonl files) - only
its small metadata.json sidecar - and never touches more than the single
most recently created recording directory: bounded, one-time work at
startup, not a scan that grows with a device's recording history. It
never overwrites an already-marked interrupted session, never fabricates
a stop time/duration/sample count it cannot know, and never resumes
writing to an old session's own files - a later continuation, if any, is
always a new, separate, explicitly linked recording (see
autoRecordPendingContinuationID).
*/
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/stratux/stratux/recording"
)

// autoRecordContinuationWindowSeconds bounds how long after startup a
// freshly-detected interrupted automatic recording may still be linked
// (via SessionSnapshot.AutoRecordContinuationOfRecordingID) to the next
// automatic recording that starts - "shortly after this boot," not "the
// next time this aircraft happens to fly," which could be days later and
// unrelated to whatever caused the earlier interruption.
const autoRecordContinuationWindowSeconds = 10 * 60

var (
	autoRecordPendingContinuationID string
	autoRecordBootMonotonic         float64
)

// autoRecordScanForInterrupted runs once, from initAutoRecord, before the
// detection loop starts. A no-op whenever there is nothing to recover:
// no recordings directory yet, the most recent recording finished
// normally, was never one Automatic Flight Recording started, or was
// already marked interrupted by an earlier boot's recovery pass.
func autoRecordScanForInterrupted() {
	autoRecordBootMonotonic = monotonicSeconds()

	id, dir, ok := mostRecentRecordingDir()
	if !ok {
		return
	}
	result := recording.ReadMetadata(dir)
	if result.Status != recording.MetadataOK {
		// Absent (legacy recording) or corrupt - neither is this
		// feature's concern; an existing, separate mechanism already
		// reports corrupt metadata honestly (see
		// handleRecordingMetadataRequest) without this feature's help.
		return
	}
	meta := result.Metadata
	if meta.Snapshot.AutoRecordInitiationMode != "automatic" {
		return // a manually-started recording - not this feature's concern
	}
	if meta.Finalization.Complete {
		return // stopped normally before this boot - nothing to recover
	}
	if meta.Finalization.AutoRecordInterrupted {
		return // already marked by an earlier boot - never re-patch
	}

	if err := recording.FinalizeMetadata(dir, recording.SessionFinalization{AutoRecordInterrupted: true}); err != nil {
		log.Printf("autorecord: could not mark interrupted recording %s: %s\n", id, err)
		return
	}
	log.Printf("autorecord: found automatic recording %s left active by a restart or crash - marked interrupted\n", id)

	autoRecordMu.Lock()
	autoRecordPendingContinuationID = id
	autoRecordMu.Unlock()
}

// mostRecentRecordingDir returns the most recently created recording
// directory's ID and path, if any - recording IDs are
// "rec-YYYYMMDDThhmmssZ" (recordingIDPattern), so lexicographic
// comparison is chronological order. A single, one-level, non-recursive
// directory listing - never descends into any recording's own files.
func mostRecentRecordingDir() (id, dir string, ok bool) {
	entries, err := os.ReadDir(recordingsDir)
	if err != nil {
		return "", "", false
	}
	var latest string
	for _, e := range entries {
		if !e.IsDir() || !recordingIDPattern.MatchString(e.Name()) {
			continue
		}
		if e.Name() > latest {
			latest = e.Name()
		}
	}
	if latest == "" {
		return "", "", false
	}
	return latest, filepath.Join(recordingsDir, latest), true
}

// autoRecordConsumeContinuation returns the pending interrupted
// recording's ID to link as this new session's
// AutoRecordContinuationOfRecordingID, if one is still pending and still
// within autoRecordContinuationWindowSeconds of boot - empty otherwise.
// Does NOT clear the pending id itself (see autoRecordClearContinuation);
// a failed start attempt must not burn the one-time link before a
// recording actually, successfully starts. Called with autoRecordMu
// already held (from within autoRecordPerformStart).
func autoRecordConsumeContinuation(nowMonotonic float64) string {
	if autoRecordPendingContinuationID == "" {
		return ""
	}
	if nowMonotonic-autoRecordBootMonotonic > autoRecordContinuationWindowSeconds {
		autoRecordPendingContinuationID = "" // window elapsed - drop it, never offered again
		return ""
	}
	return autoRecordPendingContinuationID
}

// autoRecordClearContinuation consumes (clears) the pending continuation
// id after it has actually been used on a successfully started
// recording - called with autoRecordMu already held.
func autoRecordClearContinuation() {
	autoRecordPendingContinuationID = ""
}
