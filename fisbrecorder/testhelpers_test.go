package fisbrecorder

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"testing"
)

// readFrames reads every FrameRecord from sessionDir/frames.jsonl.gz, in
// file order. Test-only helper shared across this package's test files.
func readFrames(t *testing.T, sessionDir string) []FrameRecord {
	t.Helper()
	f, err := os.Open(sessionDir + "/frames.jsonl.gz")
	if err != nil {
		t.Fatalf("open frames.jsonl.gz: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	var out []FrameRecord
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var rec FrameRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan frames.jsonl.gz: %v", err)
	}
	return out
}

// readGDL90 reads every GDL90Record from sessionDir/gdl90.jsonl.gz.
func readGDL90(t *testing.T, sessionDir string) []GDL90Record {
	t.Helper()
	f, err := os.Open(sessionDir + "/gdl90.jsonl.gz")
	if err != nil {
		t.Fatalf("open gdl90.jsonl.gz: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()
	var out []GDL90Record
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var rec GDL90Record
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("unmarshal gdl90 record: %v", err)
		}
		out = append(out, rec)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan gdl90.jsonl.gz: %v", err)
	}
	return out
}
