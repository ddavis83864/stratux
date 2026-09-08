package configbackup

import (
	"strings"
	"testing"
)

// TestPreAutoRecordBackupIsRejectedNotSilentlyDefaulted documents a
// deliberately verified characteristic of this package's checksum-based
// validation, discovered while independently verifying PR #14
// (Automatic Flight Recording): restoring a backup document created
// before the autoRecordSettings section existed is REJECTED outright by
// Validate() - both by the whole-document ContentChecksum (any field
// ever added to Document changes it) and by the per-section checksum
// loop (a document missing a section's checksum entry fails that
// section explicitly) - rather than silently defaulting the missing
// section to its zero value and proceeding.
//
// This is not a defect introduced by adding autoRecordSettings: it is
// this package's own pre-existing, established checksum design (see
// BuildDocument/Validate's own doc comments) - ANY additive change to
// Document's shape has always had this same effect on a genuinely older
// export, because ContentChecksum covers the whole document, not just
// the section that changed. Confirmed here for the specific
// autoRecordSettings case because restoring an old backup is explicitly
// safety-relevant for this feature (a restore must never silently
// enable automatic recording, or silently do anything else unintended).
//
// The failure mode this produces - a clear, non-partial checksum-
// mismatch rejection - is the safe direction: an old backup simply does
// not restore at all, rather than restoring with the new section
// silently defaulted to some guessed value. It does mean an operator
// must re-export a fresh backup after upgrading before restore is
// available again - a real, known limitation of the existing design,
// not something this PR could fix within its own scope (it would mean
// redesigning the whole-document checksum model).
func TestPreAutoRecordBackupIsRejectedNotSilentlyDefaulted(t *testing.T) {
	doc, err := BuildDocument(testBuildInputs())
	if err != nil {
		t.Fatalf("BuildDocument: %v", err)
	}
	// Simulate a genuinely pre-autoRecordSettings export: strip both the
	// section's own checksum entry and reset the section field itself to
	// its zero value, exactly as code that never knew this field existed
	// would have produced.
	legacy := doc
	legacy.AutoRecordSettings = AutoRecordSettingsSection{}
	delete(legacy.SectionChecksums, "autoRecordSettings")

	res := Validate(legacy, mustMarshalLen(t, legacy))
	if res.OK() {
		t.Fatalf("expected a simulated pre-autoRecordSettings backup to be rejected by Validate(), got OK with no errors - a legacy backup must never silently pass through with the new section defaulted unexplained")
	}
	foundContentMismatch, foundMissingSection := false, false
	for _, e := range res.Errors {
		if strings.Contains(e, "whole-document checksum") {
			foundContentMismatch = true
		}
		if strings.Contains(e, "missing checksum for section") && strings.Contains(e, `"autoRecordSettings"`) {
			foundMissingSection = true
		}
	}
	if !foundContentMismatch {
		t.Errorf("expected a whole-document ContentChecksum mismatch error, got: %v", res.Errors)
	}
	if !foundMissingSection {
		t.Errorf("expected a missing-section-checksum error naming autoRecordSettings, got: %v", res.Errors)
	}
}
