package fisbrecorder

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Classification is Validate's top-level verdict. A clean SHA-256 alone
// never establishes Valid on its own - see Validate's own doc comment.
type Classification string

const (
	// ClassificationValid: manifest present and consistent, every file's
	// hash matches, every record parses, frame sequence is gap-free, and
	// no records were dropped. Full confidence.
	ClassificationValid Classification = "valid"

	// ClassificationPartial: the bundle is genuinely usable (files
	// readable, hashes match what is actually on disk) but something
	// about the capture itself was incomplete - dropped records, a
	// non-"requested" stop reason, or sequence gaps. Replay/comparison
	// results from a partial bundle must be read with that caveat, not
	// discarded.
	ClassificationPartial Classification = "partial"

	// ClassificationUnusable: the bundle cannot be trusted at all - a
	// missing/unreadable file, a hash mismatch (silent corruption or
	// truncation), or a manifest that does not parse.
	ClassificationUnusable Classification = "unusable"
)

// Result is Validate's full report.
type Result struct {
	Classification Classification `json:"classification"`
	Errors         []string       `json:"errors,omitempty"`   // present only for Unusable
	Warnings       []string       `json:"warnings,omitempty"` // present for Partial (and may accompany Valid as informational)
	Manifest       *Manifest      `json:"manifest,omitempty"`

	// Counted* are independently recomputed by reading each file, then
	// compared against the manifest's own claimed counts - the check
	// that catches "the manifest says N but the file only has M", which
	// a hash match alone would not (a truncated-but-then-correctly-
	// re-flushed file could still hash-match a manifest written before
	// the truncation was noticed, in a hypothetical future bug; this
	// check exists so that class of defect is caught here, in the
	// validator, rather than assumed away).
	CountedFrames    uint64 `json:"countedFrames"`
	CountedGDL90     uint64 `json:"countedGdl90"`
	CountedSnapshots uint64 `json:"countedSnapshots"`
	FrameSeqGaps     int    `json:"frameSeqGaps"`
	FrameTimeOrderOK bool   `json:"frameTimeOrderOk"`
}

// Validate inspects the bundle at dir and reports whether it is
// structurally sound: hashes match, records are readable and time
// ordered, counters reconcile, and any gaps/drops the recording itself
// noticed. It distinguishes Valid, Partial, and Unusable - never upgrades
// a bundle with any detected problem to Valid.
func Validate(dir string) *Result {
	res := &Result{}

	mb, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		res.Classification = ClassificationUnusable
		res.Errors = append(res.Errors, fmt.Sprintf("cannot read manifest.json: %v", err))
		return res
	}
	var m Manifest
	if err := json.Unmarshal(mb, &m); err != nil {
		res.Classification = ClassificationUnusable
		res.Errors = append(res.Errors, fmt.Sprintf("manifest.json does not parse: %v", err))
		return res
	}
	res.Manifest = &m

	if m.FormatVersion != FormatVersion {
		res.Classification = ClassificationUnusable
		res.Errors = append(res.Errors, fmt.Sprintf("manifest format version %d, this validator understands only %d", m.FormatVersion, FormatVersion))
		return res
	}

	// Hash every file the manifest claims, in both directions: every
	// manifest-listed file must exist and hash-match, and (a corruption/
	// tampering check) recompute independently rather than trusting the
	// manifest's own recorded size/hash pair blindly.
	for _, want := range m.Files {
		got, err := hashFile(filepath.Join(dir, want.Name))
		if err != nil {
			res.Classification = ClassificationUnusable
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", want.Name, err))
			continue
		}
		if got.SHA256 != want.SHA256 {
			res.Classification = ClassificationUnusable
			res.Errors = append(res.Errors, fmt.Sprintf("%s: SHA-256 mismatch (manifest %s, actual %s)", want.Name, want.SHA256, got.SHA256))
		}
		if got.Bytes != want.Bytes {
			res.Classification = ClassificationUnusable
			res.Errors = append(res.Errors, fmt.Sprintf("%s: size mismatch (manifest %d bytes, actual %d bytes)", want.Name, want.Bytes, got.Bytes))
		}
	}
	if res.Classification == ClassificationUnusable {
		return res
	}

	// Read frames.jsonl.gz: every line parses, Seq is non-decreasing
	// (gap-free ideally), ElapsedNanos is non-decreasing (time order).
	frameCount, seqGaps, timeOK, err := scanFrames(filepath.Join(dir, "frames.jsonl.gz"))
	if err != nil {
		res.Classification = ClassificationUnusable
		res.Errors = append(res.Errors, fmt.Sprintf("frames.jsonl.gz: %v", err))
		return res
	}
	res.CountedFrames = frameCount
	res.FrameSeqGaps = seqGaps
	res.FrameTimeOrderOK = timeOK

	gdl90Count, err := countLines(filepath.Join(dir, "gdl90.jsonl.gz"))
	if err != nil {
		res.Classification = ClassificationUnusable
		res.Errors = append(res.Errors, fmt.Sprintf("gdl90.jsonl.gz: %v", err))
		return res
	}
	res.CountedGDL90 = gdl90Count

	snapCount, err := countLines(filepath.Join(dir, "snapshots.jsonl.gz"))
	if err != nil {
		res.Classification = ClassificationUnusable
		res.Errors = append(res.Errors, fmt.Sprintf("snapshots.jsonl.gz: %v", err))
		return res
	}
	res.CountedSnapshots = snapCount

	partial := false

	if frameCount != m.FrameCount {
		partial = true
		res.Warnings = append(res.Warnings, fmt.Sprintf("manifest claims %d frames, file contains %d", m.FrameCount, frameCount))
	}
	if gdl90Count != m.GDL90Count {
		partial = true
		res.Warnings = append(res.Warnings, fmt.Sprintf("manifest claims %d gdl90 records, file contains %d", m.GDL90Count, gdl90Count))
	}
	if snapCount != m.SnapshotCount {
		partial = true
		res.Warnings = append(res.Warnings, fmt.Sprintf("manifest claims %d snapshots, file contains %d", m.SnapshotCount, snapCount))
	}
	if seqGaps > 0 {
		partial = true
		res.Warnings = append(res.Warnings, fmt.Sprintf("%d gap(s) in the frame sequence", seqGaps))
	}
	if !timeOK {
		partial = true
		res.Warnings = append(res.Warnings, "frame ElapsedNanos is not monotonically non-decreasing")
	}
	if m.DroppedFrames > 0 || m.DroppedGDL90 > 0 || m.DroppedSnapshots > 0 {
		partial = true
		res.Warnings = append(res.Warnings, fmt.Sprintf("recorder itself dropped records: %d frames, %d gdl90, %d snapshots", m.DroppedFrames, m.DroppedGDL90, m.DroppedSnapshots))
	}
	if m.StopReason != StopRequested {
		partial = true
		res.Warnings = append(res.Warnings, fmt.Sprintf("session did not end with a normal requested stop (stopReason=%q)", m.StopReason))
	}
	if len(m.Gaps) > 0 {
		partial = true
		res.Warnings = append(res.Warnings, m.Gaps...)
	}
	if frameCount == 0 {
		partial = true
		res.Warnings = append(res.Warnings, "zero frames captured - no 978 MHz reception occurred during this session, or recording never actually started")
	}

	if partial {
		res.Classification = ClassificationPartial
	} else {
		res.Classification = ClassificationValid
	}
	return res
}

func scanFrames(path string) (count uint64, seqGaps int, timeOrderOK bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, false, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, 0, false, err
	}
	defer gz.Close()

	timeOrderOK = true
	var lastSeq int64 = -1
	var lastElapsed int64 = -1

	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		var rec FrameRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return count, seqGaps, timeOrderOK, fmt.Errorf("malformed record at line %d: %w", count+1, err)
		}
		if lastSeq >= 0 {
			if rec.Seq != uint64(lastSeq)+1 {
				seqGaps++
			}
			if rec.ElapsedNanos < lastElapsed {
				timeOrderOK = false
			}
		}
		lastSeq = int64(rec.Seq)
		lastElapsed = rec.ElapsedNanos
		count++
	}
	if err := scanner.Err(); err != nil {
		return count, seqGaps, timeOrderOK, err
	}
	return count, seqGaps, timeOrderOK, nil
}

func countLines(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer gz.Close()
	var n uint64
	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		n++
	}
	return n, scanner.Err()
}
