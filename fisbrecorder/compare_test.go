package fisbrecorder

import (
	"path/filepath"
	"testing"
)

func recordFullSession(t *testing.T, frames []string, gdl90 []struct {
	key string
	msg []byte
}, snapshots []struct {
	label string
	data  interface{}
}) string {
	t.Helper()
	dir := t.TempDir()
	r := New(dir, "build", DefaultOptions())
	sid, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, f := range frames {
		r.RecordFrame(f)
	}
	for _, g := range gdl90 {
		r.RecordGDL90(g.key, g.msg)
	}
	for _, s := range snapshots {
		r.RecordSnapshot(s.label, s.data)
	}
	if _, err := r.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	return filepath.Join(dir, sid)
}

func TestCompare_IdenticalBundlesMatchExactly(t *testing.T) {
	gdl90 := []struct {
		key string
		msg []byte
	}{
		{"192.168.10.22:12345", []byte{1, 2, 3}},
		{"192.168.10.22:12345", []byte{4, 5, 6}},
	}
	snaps := []struct {
		label string
		data  interface{}
	}{
		{"status", map[string]string{"a": "b"}},
	}

	dirA := recordFullSession(t, []string{"f1"}, gdl90, snaps)
	dirB := recordFullSession(t, []string{"f1"}, gdl90, snaps)

	rep, err := Compare(dirA, dirB, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if rep.GDL90.TotalMismatched != 0 {
		t.Errorf("TotalMismatched = %d, want 0", rep.GDL90.TotalMismatched)
	}
	if rep.GDL90.TotalMatched != 2 {
		t.Errorf("TotalMatched = %d, want 2", rep.GDL90.TotalMatched)
	}
	if rep.GDL90.TotalOnlyInA != 0 || rep.GDL90.TotalOnlyInB != 0 {
		t.Errorf("unexpected only-in-one-side records: onlyA=%d onlyB=%d", rep.GDL90.TotalOnlyInA, rep.GDL90.TotalOnlyInB)
	}
	sc, ok := rep.Snapshots["status"]
	if !ok {
		t.Fatal("no comparison entry for label \"status\"")
	}
	if sc.CountA != 1 || sc.CountB != 1 || sc.OnlyInLabel != "" {
		t.Errorf("status snapshot comparison = %+v, want CountA=1 CountB=1 OnlyInLabel=\"\"", sc)
	}
}

func TestCompare_DetectsGDL90ByteMismatch(t *testing.T) {
	gdl90A := []struct {
		key string
		msg []byte
	}{{"1.2.3.4:1", []byte{1, 2, 3}}}
	gdl90B := []struct {
		key string
		msg []byte
	}{{"1.2.3.4:1", []byte{9, 9, 9}}}

	dirA := recordFullSession(t, nil, gdl90A, nil)
	dirB := recordFullSession(t, nil, gdl90B, nil)

	rep, err := Compare(dirA, dirB, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if rep.GDL90.TotalMismatched != 1 {
		t.Errorf("TotalMismatched = %d, want 1", rep.GDL90.TotalMismatched)
	}
	cc := rep.GDL90.PerConnection["1.2.3.4:1"]
	if len(cc.MismatchIndexes) != 1 || cc.MismatchIndexes[0] != 0 {
		t.Errorf("MismatchIndexes = %v, want [0]", cc.MismatchIndexes)
	}
}

func TestCompare_NormalizeMasksExpectedDifferences(t *testing.T) {
	gdl90A := []struct {
		key string
		msg []byte
	}{{"1.2.3.4:1", []byte{0xAA, 1, 2, 3}}} // first byte = a fake "timestamp" nibble
	gdl90B := []struct {
		key string
		msg []byte
	}{{"1.2.3.4:1", []byte{0xBB, 1, 2, 3}}} // differs only in that same byte

	dirA := recordFullSession(t, nil, gdl90A, nil)
	dirB := recordFullSession(t, nil, gdl90B, nil)

	// Without normalization, this must be reported as a mismatch.
	repRaw, err := Compare(dirA, dirB, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if repRaw.GDL90.TotalMismatched != 1 {
		t.Fatalf("without normalize: TotalMismatched = %d, want 1", repRaw.GDL90.TotalMismatched)
	}

	// A caller-supplied Normalize masking that byte must make them match.
	maskFirstByte := func(b []byte) []byte {
		out := make([]byte, len(b))
		copy(out, b)
		if len(out) > 0 {
			out[0] = 0
		}
		return out
	}
	repMasked, err := Compare(dirA, dirB, maskFirstByte)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if repMasked.GDL90.TotalMismatched != 0 || repMasked.GDL90.TotalMatched != 1 {
		t.Errorf("with normalize: matched=%d mismatched=%d, want matched=1 mismatched=0", repMasked.GDL90.TotalMatched, repMasked.GDL90.TotalMismatched)
	}
}

func TestCompare_OnlyInOneConnectionIsCounted(t *testing.T) {
	gdl90A := []struct {
		key string
		msg []byte
	}{
		{"1.1.1.1:1", []byte{1}},
		{"1.1.1.1:1", []byte{2}},
		{"1.1.1.1:1", []byte{3}},
	}
	gdl90B := []struct {
		key string
		msg []byte
	}{
		{"1.1.1.1:1", []byte{1}},
	}

	dirA := recordFullSession(t, nil, gdl90A, nil)
	dirB := recordFullSession(t, nil, gdl90B, nil)

	rep, err := Compare(dirA, dirB, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if rep.GDL90.TotalMatched != 1 {
		t.Errorf("TotalMatched = %d, want 1", rep.GDL90.TotalMatched)
	}
	if rep.GDL90.TotalOnlyInA != 2 {
		t.Errorf("TotalOnlyInA = %d, want 2", rep.GDL90.TotalOnlyInA)
	}
	if rep.GDL90.TotalOnlyInB != 0 {
		t.Errorf("TotalOnlyInB = %d, want 0", rep.GDL90.TotalOnlyInB)
	}
}

func TestCompare_SnapshotLabelOnlyOnOneSide(t *testing.T) {
	snapsA := []struct {
		label string
		data  interface{}
	}{{"towers", map[string]string{}}}
	var snapsB []struct {
		label string
		data  interface{}
	}

	dirA := recordFullSession(t, nil, nil, snapsA)
	dirB := recordFullSession(t, nil, nil, snapsB)

	rep, err := Compare(dirA, dirB, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	sc, ok := rep.Snapshots["towers"]
	if !ok {
		t.Fatal("expected a comparison entry for \"towers\" even though it's absent from dirB")
	}
	if sc.OnlyInLabel != "A" {
		t.Errorf("OnlyInLabel = %q, want %q", sc.OnlyInLabel, "A")
	}
}

func TestCompare_MissingManifestReturnsError(t *testing.T) {
	dirA := t.TempDir() // never recorded - no manifest.json
	dirB := recordFullSession(t, nil, nil, nil)
	if _, err := Compare(dirA, dirB, nil); err == nil {
		t.Fatal("Compare with a missing manifest in dirA returned no error")
	}
}
