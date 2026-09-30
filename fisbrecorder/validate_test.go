package fisbrecorder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// newValidSession records a small, clean session and returns its dir -
// the common starting point for tests that then corrupt one specific
// thing and check Validate catches exactly that.
func newValidSession(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	r := New(base, "testbuild", DefaultOptions())
	sid, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.RecordFrame("frame-a")
	r.RecordFrame("frame-b")
	r.RecordGDL90("1.2.3.4:1", []byte{1, 2, 3})
	r.RecordSnapshot("status", map[string]string{"k": "v"})
	if _, err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return filepath.Join(base, sid)
}

func TestValidate_CleanSessionIsValid(t *testing.T) {
	dir := newValidSession(t)
	res := Validate(dir)
	if res.Classification != ClassificationValid {
		t.Fatalf("Classification = %q, want %q; errors=%v warnings=%v", res.Classification, ClassificationValid, res.Errors, res.Warnings)
	}
	if len(res.Errors) != 0 {
		t.Errorf("Valid result has errors: %v", res.Errors)
	}
	if res.CountedFrames != 2 || res.CountedGDL90 != 1 || res.CountedSnapshots != 1 {
		t.Errorf("counted mismatch: frames=%d gdl90=%d snapshots=%d", res.CountedFrames, res.CountedGDL90, res.CountedSnapshots)
	}
	if res.FrameSeqGaps != 0 || !res.FrameTimeOrderOK {
		t.Errorf("expected no seq gaps and ok time order on a clean session, got gaps=%d timeOK=%v", res.FrameSeqGaps, res.FrameTimeOrderOK)
	}
}

func TestValidate_MissingManifestIsUnusable(t *testing.T) {
	dir := newValidSession(t)
	if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatalf("remove manifest: %v", err)
	}
	res := Validate(dir)
	if res.Classification != ClassificationUnusable {
		t.Fatalf("Classification = %q, want %q", res.Classification, ClassificationUnusable)
	}
	if len(res.Errors) == 0 {
		t.Error("expected at least one error explaining the missing manifest")
	}
}

func TestValidate_CorruptManifestJSONIsUnusable(t *testing.T) {
	dir := newValidSession(t)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{not json"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	res := Validate(dir)
	if res.Classification != ClassificationUnusable {
		t.Fatalf("Classification = %q, want %q", res.Classification, ClassificationUnusable)
	}
}

func TestValidate_WrongFormatVersionIsUnusable(t *testing.T) {
	dir := newValidSession(t)
	m := readManifestFile(t, dir)
	m.FormatVersion = FormatVersion + 1
	writeManifestFile(t, dir, m)
	res := Validate(dir)
	if res.Classification != ClassificationUnusable {
		t.Fatalf("Classification = %q, want %q for an unrecognized format version", res.Classification, ClassificationUnusable)
	}
}

func TestValidate_TruncatedFrameFileHashMismatchIsUnusable(t *testing.T) {
	dir := newValidSession(t)
	// Simulate an interrupted copy / disk-full mid-write: truncate the
	// file after the manifest's hash was already computed over the full
	// original content, so the manifest's claimed hash no longer matches
	// what's actually on disk - this is the exact "clean hash alone must
	// never establish completeness, and a hash MISMATCH must be caught"
	// case.
	path := filepath.Join(dir, "frames.jsonl.gz")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	res := Validate(dir)
	if res.Classification != ClassificationUnusable {
		t.Fatalf("Classification = %q, want %q after truncating frames.jsonl.gz", res.Classification, ClassificationUnusable)
	}
	if len(res.Errors) == 0 {
		t.Error("expected an error identifying the hash/size mismatch")
	}
}

func TestValidate_MissingDataFileIsUnusable(t *testing.T) {
	dir := newValidSession(t)
	if err := os.Remove(filepath.Join(dir, "gdl90.jsonl.gz")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	res := Validate(dir)
	if res.Classification != ClassificationUnusable {
		t.Fatalf("Classification = %q, want %q when a manifest-listed file is missing entirely", res.Classification, ClassificationUnusable)
	}
}

func TestValidate_ManifestCountMismatchIsPartial(t *testing.T) {
	dir := newValidSession(t)
	m := readManifestFile(t, dir)
	// Claim more frames than the file (now hash-consistent, since we
	// re-hash after editing) actually contains - a "the recorder's own
	// bookkeeping disagrees with the file" defect, distinct from
	// corruption, and must be Partial (usable, but flagged) not Unusable.
	m.FrameCount = 999
	writeManifestFile(t, dir, m)
	rehashManifestFiles(t, dir, m)

	res := Validate(dir)
	if res.Classification != ClassificationPartial {
		t.Fatalf("Classification = %q, want %q; errors=%v warnings=%v", res.Classification, ClassificationPartial, res.Errors, res.Warnings)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a warning describing the count mismatch")
	}
}

func TestValidate_NonZeroDroppedRecordsIsPartial(t *testing.T) {
	dir := newValidSession(t)
	m := readManifestFile(t, dir)
	m.DroppedFrames = 5
	writeManifestFile(t, dir, m)
	rehashManifestFiles(t, dir, m)

	res := Validate(dir)
	if res.Classification != ClassificationPartial {
		t.Fatalf("Classification = %q, want %q when the manifest itself reports dropped frames", res.Classification, ClassificationPartial)
	}
}

func TestValidate_NonRequestedStopReasonIsPartial(t *testing.T) {
	dir := newValidSession(t)
	m := readManifestFile(t, dir)
	m.StopReason = StopDiskFull
	writeManifestFile(t, dir, m)
	rehashManifestFiles(t, dir, m)

	res := Validate(dir)
	if res.Classification != ClassificationPartial {
		t.Fatalf("Classification = %q, want %q for a disk_full stop reason", res.Classification, ClassificationPartial)
	}
}

func TestValidate_ZeroFramesIsPartial(t *testing.T) {
	base := t.TempDir()
	r := New(base, "build", DefaultOptions())
	sid, _ := r.Start()
	// No RecordFrame calls at all - e.g. a session run entirely out of
	// 978 MHz reception range. Still a structurally sound bundle, but the
	// field-acceptance gate (>=1 real tower/frame) cannot be met from it.
	r.RecordSnapshot("sessionStart", nil)
	if _, err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	res := Validate(filepath.Join(base, sid))
	if res.Classification != ClassificationPartial {
		t.Fatalf("Classification = %q, want %q for a zero-frame session", res.Classification, ClassificationPartial)
	}
}

func TestValidate_SeqGapInFrameFileIsDetected(t *testing.T) {
	dir := newValidSession(t)
	frames := readFrames(t, dir)
	if len(frames) != 2 {
		t.Fatalf("setup: got %d frames, want 2", len(frames))
	}
	// Introduce a gap by bumping the second record's Seq, then rewrite
	// frames.jsonl.gz and refresh the manifest's hash/size for it so the
	// hash check itself passes and we're isolating the seq-gap detector.
	frames[1].Seq = 5
	rewriteFramesFile(t, dir, frames)

	m := readManifestFile(t, dir)
	rehashManifestFiles(t, dir, m)

	res := Validate(dir)
	if res.FrameSeqGaps == 0 {
		t.Error("expected FrameSeqGaps > 0 after introducing a gap")
	}
	if res.Classification != ClassificationPartial {
		t.Errorf("Classification = %q, want %q", res.Classification, ClassificationPartial)
	}
}

func TestValidate_OutOfTimeOrderFramesIsDetected(t *testing.T) {
	dir := newValidSession(t)
	frames := readFrames(t, dir)
	if len(frames) != 2 {
		t.Fatalf("setup: got %d frames, want 2", len(frames))
	}
	frames[1].ElapsedNanos = frames[0].ElapsedNanos - 1000 // going backwards
	rewriteFramesFile(t, dir, frames)

	m := readManifestFile(t, dir)
	rehashManifestFiles(t, dir, m)

	res := Validate(dir)
	if res.FrameTimeOrderOK {
		t.Error("expected FrameTimeOrderOK=false after making elapsed time go backwards")
	}
	if res.Classification != ClassificationPartial {
		t.Errorf("Classification = %q, want %q", res.Classification, ClassificationPartial)
	}
}

// --- shared test-only manifest helpers ---

func readManifestFile(t *testing.T, dir string) *Manifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	return &m
}

func writeManifestFile(t *testing.T, dir string, m *Manifest) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o640); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// rehashManifestFiles recomputes Files[].SHA256/Bytes from what's
// currently on disk and rewrites the manifest - used when a test edits
// manifest fields other than the file hashes themselves, so the hash
// check passes and the test isolates the specific field it's checking.
func rehashManifestFiles(t *testing.T, dir string, m *Manifest) {
	t.Helper()
	for i, fi := range m.Files {
		got, err := hashFile(filepath.Join(dir, fi.Name))
		if err != nil {
			t.Fatalf("hash %s: %v", fi.Name, err)
		}
		got.Name = fi.Name
		m.Files[i] = *got
	}
	writeManifestFile(t, dir, m)
}

// rewriteFramesFile replaces frames.jsonl.gz's content with the given
// records (marshaled the same way the recorder itself does) and updates
// the manifest's hash/size for that one file so hash checks still pass.
func rewriteFramesFile(t *testing.T, dir string, frames []FrameRecord) {
	t.Helper()
	path := filepath.Join(dir, "frames.jsonl.gz")
	gz, gzw, f, err := newJSONLWriter(path)
	if err != nil {
		t.Fatalf("newJSONLWriter: %v", err)
	}
	for _, rec := range frames {
		if err := writeJSONL(gz, rec); err != nil {
			t.Fatalf("writeJSONL: %v", err)
		}
	}
	gzw.Flush()
	gzw.Close()
	f.Close()

	m := readManifestFile(t, dir)
	rehashManifestFiles(t, dir, m)
}
