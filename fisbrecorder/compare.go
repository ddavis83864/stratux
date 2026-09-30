package fisbrecorder

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func readManifestAt(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func readGDL90At(dir string) ([]GDL90Record, error) {
	f, err := os.Open(filepath.Join(dir, "gdl90.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	var out []GDL90Record
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var rec GDL90Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

func readSnapshotsAt(dir string) ([]Snapshot, error) {
	f, err := os.Open(filepath.Join(dir, "snapshots.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	var out []Snapshot
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var rec Snapshot
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// CompareReport is the result of comparing two session bundles - normally
// a live field-recorded session (dir A) and a bench replay-derived
// session (dir B) produced by feeding dir A's frames.jsonl.gz back through
// the real parser/cache/GDL90 path (main wires ReplayFrames's handler to
// handleUatMessage; recording stays on during that replay, producing a
// second, independent bundle with its own gdl90.jsonl.gz/snapshots.jsonl.gz
// to diff against the original). See Phase 3 bench-proof #4 in
// docs/fisb-field-recorder-design.md.
//
// A caller is free to pass two live sessions, or two replays, or replay
// dir A against a DIFFERENT prior replay of itself (a regression check) -
// Compare itself has no opinion on which is "live" vs "replay", only the
// two paths it's given.
type CompareReport struct {
	DirA, DirB string

	FrameCountA, FrameCountB uint64

	GDL90 GDL90Comparison

	// SnapshotLabels lists every distinct Snapshot.Label found in either
	// bundle, each compared independently - see CompareSnapshots.
	SnapshotLabels []string
	Snapshots      map[string]SnapshotComparison
}

// GDL90Comparison compares two bundles' outbound GDL90 records.
//
// Comparison is done pairwise by (ConnectionKey, index within that
// connection's own record sequence) - i.e. the Nth message dir A sent to
// a given client is compared against the Nth message dir B sent to that
// SAME client, never across different destinations. This is deliberately
// exact-index rather than nearest-timestamp: GDL90 write order per
// connection is exactly what connectionWriter (main/network.go) produced,
// and preserving that order is the whole point of the comparison.
//
// Byte comparison is exact by default (Normalize is the identity
// function if the caller passes nil), matching this task's own
// requirement to prove byte-for-byte determinism where it genuinely
// holds. Any GDL90 field that is expected to legitimately differ between
// a live run and a replay run (e.g. a heartbeat message's own embedded
// wall-clock timestamp, if one exists in a given message type) must be
// masked out by a caller-supplied Normalize func - this package does not
// itself know GDL90's wire format and does not guess at which bytes are
// safe to ignore. An all-identical result with Normalize left at the
// default is the strongest claim this makes; any mismatch under the
// default should be treated as a real behavioral difference until a
// specific, justified Normalize rule explains it away.
type GDL90Comparison struct {
	ConnectionKeys []string // union of every key seen in either bundle, sorted

	// PerConnection maps each key to how many of that connection's
	// records matched / mismatched / were only on one side.
	PerConnection map[string]ConnectionComparison

	TotalMatched    int
	TotalMismatched int
	TotalOnlyInA    int
	TotalOnlyInB    int
}

// ConnectionComparison is one connection key's own tally within a
// GDL90Comparison.
type ConnectionComparison struct {
	Matched    int
	Mismatched int
	OnlyInA    int
	OnlyInB    int

	// MismatchIndexes lists the (per-connection) record indexes that
	// differed, capped at 20 entries so a badly-diverged comparison
	// doesn't produce an unusable wall of output - TotalMismatched (on
	// the parent GDL90Comparison) still reflects the true total.
	MismatchIndexes []int
}

// SnapshotComparison compares every recorded Snapshot under one label
// between two bundles. Because ElapsedNanos is relative to each bundle's
// OWN session start (a live session and a replay of it do not share a
// clock epoch - see the design doc's monotonic-vs-wall-clock discussion),
// snapshots are paired by RELATIVE position (the Nth "fisbCacheStatus"
// snapshot in A against the Nth in B), not by absolute or elapsed time.
// A cache/status field whose value is itself clock-dependent (an age in
// seconds, a freshness bucket near a threshold) is expected to drift
// between a live run and any replay that does not reproduce the exact
// original wall-clock schedule - CountA/CountB and the label's own
// presence/absence are what this proves reliably; per-field payload
// diffing is left to a caller that knows the label's own JSON shape and
// which fields are clock-dependent (see field-by-field examples in
// docs/fisb-field-recorder-design.md's comparison-report section).
type SnapshotComparison struct {
	Label       string
	CountA      int
	CountB      int
	OnlyInLabel string // "A", "B", or "" if present in both
}

// Compare reads dirA and dirB (each a session directory as Recorder.Stop
// produces) and reports how their GDL90 and snapshot records line up.
// Compare does not itself call Validate - callers doing field acceptance
// should Validate both bundles first (see the design doc's field
// procedure) so a comparison is never run against a bundle already known
// to be structurally unusable.
func Compare(dirA, dirB string, normalize func([]byte) []byte) (*CompareReport, error) {
	if normalize == nil {
		normalize = func(b []byte) []byte { return b }
	}

	mA, err := readManifestAt(dirA)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading manifest for dirA: %w", err)
	}
	mB, err := readManifestAt(dirB)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading manifest for dirB: %w", err)
	}

	gdl90A, err := readGDL90At(dirA)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading gdl90.jsonl.gz for dirA: %w", err)
	}
	gdl90B, err := readGDL90At(dirB)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading gdl90.jsonl.gz for dirB: %w", err)
	}

	rep := &CompareReport{
		DirA:        dirA,
		DirB:        dirB,
		FrameCountA: mA.FrameCount,
		FrameCountB: mB.FrameCount,
		GDL90:       compareGDL90(gdl90A, gdl90B, normalize),
	}

	snapA, err := readSnapshotsAt(dirA)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading snapshots.jsonl.gz for dirA: %w", err)
	}
	snapB, err := readSnapshotsAt(dirB)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading snapshots.jsonl.gz for dirB: %w", err)
	}
	rep.SnapshotLabels, rep.Snapshots = compareSnapshots(snapA, snapB)

	return rep, nil
}

func compareGDL90(a, b []GDL90Record, normalize func([]byte) []byte) GDL90Comparison {
	byKeyA := map[string][]GDL90Record{}
	for _, r := range a {
		byKeyA[r.ConnectionKey] = append(byKeyA[r.ConnectionKey], r)
	}
	byKeyB := map[string][]GDL90Record{}
	for _, r := range b {
		byKeyB[r.ConnectionKey] = append(byKeyB[r.ConnectionKey], r)
	}

	keySet := map[string]bool{}
	for k := range byKeyA {
		keySet[k] = true
	}
	for k := range byKeyB {
		keySet[k] = true
	}

	out := GDL90Comparison{PerConnection: map[string]ConnectionComparison{}}
	for k := range keySet {
		out.ConnectionKeys = append(out.ConnectionKeys, k)
		ra := byKeyA[k]
		rb := byKeyB[k]
		var cc ConnectionComparison
		n := len(ra)
		if len(rb) < n {
			n = len(rb)
		}
		for i := 0; i < n; i++ {
			if string(normalize(ra[i].Bytes)) == string(normalize(rb[i].Bytes)) {
				cc.Matched++
			} else {
				cc.Mismatched++
				if len(cc.MismatchIndexes) < 20 {
					cc.MismatchIndexes = append(cc.MismatchIndexes, i)
				}
			}
		}
		if len(ra) > n {
			cc.OnlyInA = len(ra) - n
		}
		if len(rb) > n {
			cc.OnlyInB = len(rb) - n
		}
		out.PerConnection[k] = cc
		out.TotalMatched += cc.Matched
		out.TotalMismatched += cc.Mismatched
		out.TotalOnlyInA += cc.OnlyInA
		out.TotalOnlyInB += cc.OnlyInB
	}
	return out
}

func compareSnapshots(a, b []Snapshot) ([]string, map[string]SnapshotComparison) {
	countA := map[string]int{}
	for _, s := range a {
		countA[s.Label]++
	}
	countB := map[string]int{}
	for _, s := range b {
		countB[s.Label]++
	}

	labelSet := map[string]bool{}
	for l := range countA {
		labelSet[l] = true
	}
	for l := range countB {
		labelSet[l] = true
	}

	var labels []string
	out := map[string]SnapshotComparison{}
	for l := range labelSet {
		labels = append(labels, l)
		ca, cb := countA[l], countB[l]
		only := ""
		if ca > 0 && cb == 0 {
			only = "A"
		} else if cb > 0 && ca == 0 {
			only = "B"
		}
		out[l] = SnapshotComparison{Label: l, CountA: ca, CountB: cb, OnlyInLabel: only}
	}
	return labels, out
}
