package fisbrecorder

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FrameHandler is called once per replayed frame, in recorded order, with
// its original ElapsedNanos-derived pacing already applied (see
// ReplayOptions.SpeedMultiplier). main wires this directly to
// handleUatMessage(rec.Frame) - the real production parser entry point,
// unmodified - so replay exercises tower derivation, cache admission, and
// GDL90 relay through the actual code, not a reimplementation. See the
// design doc's own "Tower identification: derived, not recorded
// separately" section for why this is correct and sufficient.
type FrameHandler func(rec FrameRecord)

// ReplayOptions controls pacing. Source timestamps (ElapsedNanos) are
// always retained and always what pacing is computed from - never
// discarded, even in accelerated mode.
type ReplayOptions struct {
	// SpeedMultiplier: 1.0 = original pacing (wait the real inter-frame
	// gaps). Values > 1 replay faster (a gap of 10s at 10x becomes a 1s
	// wait). 0 or negative means "as fast as possible" - no waiting at
	// all, still calling FrameHandler strictly in recorded order.
	SpeedMultiplier float64

	// StopAfter, if non-zero, halts replay after this many frames
	// (primarily for tests/bounded rehearsals).
	StopAfter uint64
}

// ReplayStats summarizes what a replay run actually did.
type ReplayStats struct {
	FramesReplayed uint64
	FirstSeq       uint64
	LastSeq        uint64
	SeqGaps        []uint64 // seq numbers where a gap in the sequence was detected
	Duration       time.Duration
}

// ReplayFrames reads frames.jsonl.gz from dir in order and invokes handler
// for each, pacing according to opts. It never touches gdl90.jsonl.gz or
// snapshots.jsonl.gz (comparison against those is a separate, explicit
// step - see the comparison report in cmd/fisb-recording-tool) and never
// opens any network connection or hardware handle itself - replay driving
// the live device or a production network is entirely main's own
// responsibility to prevent (see docs/fisb-field-recorder-design.md's
// "Replay cannot accidentally drive the live device" discussion), not
// something this package can do on its own since it has no network code
// at all.
func ReplayFrames(dir string, handler FrameHandler, opts ReplayOptions) (*ReplayStats, error) {
	f, err := os.Open(filepath.Join(dir, "frames.jsonl.gz"))
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: could not open frames.jsonl.gz: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: could not read gzip stream: %w", err)
	}
	defer gz.Close()

	speed := opts.SpeedMultiplier
	asFastAsPossible := speed <= 0

	stats := &ReplayStats{}
	replayStart := time.Now()
	var lastSeq int64 = -1
	var haveFirst bool

	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // frames can be a few KB; generous ceiling
	for scanner.Scan() {
		var rec FrameRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return stats, fmt.Errorf("fisbrecorder: malformed frame record at replayed index %d: %w", stats.FramesReplayed, err)
		}

		if lastSeq >= 0 && rec.Seq != uint64(lastSeq)+1 {
			for s := uint64(lastSeq) + 1; s < rec.Seq; s++ {
				stats.SeqGaps = append(stats.SeqGaps, s)
			}
		}
		lastSeq = int64(rec.Seq)
		if !haveFirst {
			stats.FirstSeq = rec.Seq
			haveFirst = true
		}
		stats.LastSeq = rec.Seq

		if !asFastAsPossible {
			targetElapsed := time.Duration(float64(rec.ElapsedNanos) / speed)
			actualElapsed := time.Since(replayStart)
			if wait := targetElapsed - actualElapsed; wait > 0 {
				time.Sleep(wait)
			}
		}

		handler(rec)
		stats.FramesReplayed++
		if opts.StopAfter > 0 && stats.FramesReplayed >= opts.StopAfter {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return stats, fmt.Errorf("fisbrecorder: error reading frames.jsonl.gz: %w", err)
	}
	stats.Duration = time.Since(replayStart)
	return stats, nil
}
