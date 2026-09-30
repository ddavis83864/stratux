package fisbrecorder

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/uatparse"
)

// TestSyntheticFixture_SelfDecodesAsExpected proves the fixture itself is
// well-formed by running it through the REAL production parser package
// (uatparse), the same one main's handleUatMessage/parseInput calls live.
// If a future uatparse change ever alters these bit layouts, this test -
// not a field trip - is what catches that the fixture silently broke.
func TestSyntheticFixture_SelfDecodesAsExpected(t *testing.T) {
	cases := []struct {
		name             string
		buf              string
		wantLat          float64
		wantLon          float64
		wantText         string // "" if this frame carries no text product
		wantNexradBlocks int
	}{
		{"towerA_text", SyntheticTowerAFrame1, SyntheticTowerALat, SyntheticTowerALon, SyntheticTowerAText, 0},
		{"towerA_nexrad", SyntheticTowerANexradFrame2, SyntheticTowerALat, SyntheticTowerALon, "", 1},
		{"towerB_text", SyntheticTowerBFrame3, SyntheticTowerBLat, SyntheticTowerBLon, SyntheticTowerBText, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, err := uatparse.New(c.buf)
			if err != nil {
				t.Fatalf("uatparse.New: %v", err)
			}
			if err := msg.DecodeUplink(); err != nil {
				t.Fatalf("DecodeUplink: %v", err)
			}
			const tol = 0.001
			if d := msg.Lat - c.wantLat; d > tol || d < -tol {
				t.Errorf("Lat = %v, want ~%v", msg.Lat, c.wantLat)
			}
			if d := msg.Lon - c.wantLon; d > tol || d < -tol {
				t.Errorf("Lon = %v, want ~%v", msg.Lon, c.wantLon)
			}
			texts, err := msg.GetTextReports()
			if err != nil {
				t.Fatalf("GetTextReports: %v", err)
			}
			if c.wantText == "" {
				if len(texts) != 0 {
					t.Errorf("got text reports %q, want none", texts)
				}
			} else {
				if len(texts) != 1 || texts[0] != c.wantText {
					t.Errorf("text reports = %q, want [%q]", texts, c.wantText)
				}
			}
			nexradCount := 0
			for _, f := range msg.Frames {
				nexradCount += len(f.NEXRAD)
			}
			if nexradCount != c.wantNexradBlocks {
				t.Errorf("NEXRAD blocks = %d, want %d", nexradCount, c.wantNexradBlocks)
			}
		})
	}
}

// TestSyntheticFixture_TwoTowersAreDistinct is the harness-level version of
// this project's core "tower ID is derived, not hardcoded" claim: two
// fixture messages with different raw lat/lon bytes decode to two
// different tower identities, and two messages sharing raw lat/lon bytes
// decode to the SAME tower identity - proving the identity is a pure
// function of the frame bytes, not a counter or a fixture label.
func TestSyntheticFixture_TwoTowersAreDistinct(t *testing.T) {
	towerID := func(buf string) string {
		msg, err := uatparse.New(buf)
		if err != nil {
			t.Fatalf("uatparse.New: %v", err)
		}
		if err := msg.DecodeUplink(); err != nil {
			t.Fatalf("DecodeUplink: %v", err)
		}
		// Mirrors main/gen_gdl90.go's own ADSBTowerID derivation
		// (fmt.Sprintf("(%f,%f)", Lat, Lon)) without importing package
		// main (which would pull in cgo) - same inputs, same shape.
		return fmt.Sprintf("(%f,%f)", msg.Lat, msg.Lon)
	}

	idA1 := towerID(SyntheticTowerAFrame1)
	idA2 := towerID(SyntheticTowerANexradFrame2)
	idB := towerID(SyntheticTowerBFrame3)

	if idA1 != idA2 {
		t.Errorf("two messages from the same synthetic tower A produced different IDs: %q vs %q", idA1, idA2)
	}
	if idA1 == idB {
		t.Errorf("two messages from DIFFERENT synthetic towers produced the same ID: %q", idA1)
	}
}

// TestSyntheticFixture_RecordReplayRoundTrip demonstrates Phase 3's
// bench-proof #1 end to end using ONLY the synthetic fixture (never real
// RF): record the three fixture frames, validate the bundle, replay it,
// and confirm the replayed frame bytes/order are byte-for-byte identical
// to the fixture's own SyntheticFrames slice.
func TestSyntheticFixture_RecordReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, "synthetic-bench-test", DefaultOptions())
	sid, err := r.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, f := range SyntheticFrames {
		r.RecordFrame(f)
	}
	m, err := r.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if m.FrameCount != uint64(len(SyntheticFrames)) {
		t.Fatalf("FrameCount = %d, want %d", m.FrameCount, len(SyntheticFrames))
	}

	sessionDir := filepath.Join(dir, sid)
	res := Validate(sessionDir)
	if res.Classification != ClassificationValid {
		t.Fatalf("Validate() = %q, want %q; errors=%v warnings=%v", res.Classification, ClassificationValid, res.Errors, res.Warnings)
	}

	var replayed []string
	stats, err := ReplayFrames(sessionDir, func(rec FrameRecord) {
		replayed = append(replayed, rec.Frame)
	}, ReplayOptions{SpeedMultiplier: -1})
	if err != nil {
		t.Fatalf("ReplayFrames: %v", err)
	}
	if stats.FramesReplayed != uint64(len(SyntheticFrames)) {
		t.Fatalf("FramesReplayed = %d, want %d", stats.FramesReplayed, len(SyntheticFrames))
	}
	if len(replayed) != len(SyntheticFrames) {
		t.Fatalf("replayed %d frames, want %d", len(replayed), len(SyntheticFrames))
	}
	for i, want := range SyntheticFrames {
		if replayed[i] != want {
			t.Errorf("replayed frame %d does not match the original fixture byte-for-byte", i)
		}
	}

	// And the replayed bytes must decode through the real parser exactly
	// like the originals did - replay doesn't just move strings around,
	// it reproduces parseable UAT frames.
	for i, f := range replayed {
		msg, err := uatparse.New(f)
		if err != nil {
			t.Fatalf("replayed frame %d: uatparse.New: %v", i, err)
		}
		if err := msg.DecodeUplink(); err != nil {
			t.Fatalf("replayed frame %d: DecodeUplink: %v", i, err)
		}
	}
}
