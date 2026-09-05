package main

import "testing"

func TestRecordingIDPattern_RejectsPathTraversalShapes(t *testing.T) {
	for _, bad := range []string{
		"../../etc",
		"rec-123", // wrong digit count
		"rec-20260101T000000Z/../../etc",
		"",
		"RECORDING-20260101T000000Z",
	} {
		if recordingIDPattern.MatchString(bad) {
			t.Errorf("pattern incorrectly matched malformed/traversal id: %q", bad)
		}
	}
	if !recordingIDPattern.MatchString("rec-20260101T000000Z") {
		t.Error("pattern should match a well-formed session id")
	}
}

func TestExportNamePattern_OnlyAcceptsKnownFormats(t *testing.T) {
	if !exportNamePattern.MatchString("rec-20260101T000000Z.csv") {
		t.Error("pattern should match a well-formed CSV export name")
	}
	for _, bad := range []string{
		"rec-20260101T000000Z.gpx", // not yet implemented, must not be servable as a file
		"../../etc/passwd",
		"rec-20260101T000000Z.csv/../../etc/passwd",
		"",
	} {
		if exportNamePattern.MatchString(bad) {
			t.Errorf("pattern incorrectly matched: %q", bad)
		}
	}
}
