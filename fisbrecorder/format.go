// Package fisbrecorder implements a bounded, opt-in field-recording and
// offline-replay facility for the 978 MHz UAT/FIS-B pipeline, built for
// PR #51's field acceptance (docs/pr51-field-acceptance-kit.md) and issue
// tracking. See docs/fisb-field-recorder-design.md for the full design
// record, including why this exists alongside the pre-existing
// main/trace.go TraceLog facility rather than replacing it, exactly which
// two capture points were chosen and why, and the explicit limitation that
// this records decoded UAT frames (dump978's own output), never raw 978 MHz
// I/Q - replay proves the parser/cache/GDL90 pipeline is reproducible; it
// cannot validate RF demodulation or receiver/antenna performance.
//
// This package has no dependency on the main package (the reverse is
// true - main calls into here) and does not itself touch cgo, hardware,
// or global state - every hook point is wired from main by passing values
// in, keeping this package independently unit-testable on any platform.
package fisbrecorder

import "time"

// FormatVersion is bumped whenever the on-disk bundle shape changes in a
// way a reader should notice - the same convention this project already
// uses for fisbcache.SchemaVersion and configbackup.SchemaVersion. There
// is no earlier version: this is the first release of this recorder.
const FormatVersion = 1

// FrameRecord is one decoded UAT uplink frame, exactly as received from
// godump978.OutChan and handed to handleUatMessage - see
// main/sdr.go's uatReader(), the capture point this package hooks
// alongside (not instead of) main/trace.go's own, separate
// TraceLog.Record(CONTEXT_GODUMP978, ...) call.
type FrameRecord struct {
	// Seq is a per-session, gap-free, 0-based sequence number assigned at
	// capture time - independent of ElapsedNanos, so a reader can detect a
	// dropped record (a hole in Seq) even if clock behavior is ever in
	// question.
	Seq uint64 `json:"seq"`

	// ElapsedNanos is nanoseconds since the recording session started,
	// computed via time.Since (which uses Go's monotonic clock reading,
	// not wall-clock arithmetic) - the authoritative ordering/pacing value
	// for replay. Never derived from WallClock.
	ElapsedNanos int64 `json:"elapsedNanos"`

	// WallClock is this device's own wall-clock time at capture, for
	// human/GPS-log correlation only - explicitly untrusted on its own:
	// this project's Pi has no RTC and no GPS fix indoors (documented
	// project-wide). Never used for ordering or pacing.
	WallClock time.Time `json:"wallClock"`

	// Frame is the exact string godump978.OutChan produced - the same
	// bytes handleUatMessage/parseInput would receive live. Preserved
	// verbatim, not re-encoded or reformatted.
	Frame string `json:"frame"`
}

// GDL90Record is one outbound GDL90 write to one client connection -
// captured at main/network.go's connectionWriter, the point
// connection.Writer().Write(msg) actually happens, chosen specifically
// because it is the one place in this codebase that sees both the exact
// bytes and the exact per-client destination for every real send,
// regardless of network topology (see the design doc's explicit
// discussion of why sniffing UDP on a capture laptop cannot be trusted to
// see unicast traffic addressed to a different client).
type GDL90Record struct {
	Seq          uint64 `json:"seq"`
	ElapsedNanos int64  `json:"elapsedNanos"`

	// ConnectionKey is connection.GetConnectionKey()'s own value - e.g.
	// "192.168.10.22:12345" for a UDP client - the intended destination.
	ConnectionKey string `json:"connectionKey"`

	// Bytes is the exact message written (encoding/json base64-encodes a
	// []byte field automatically).
	Bytes []byte `json:"bytes"`
}

// Snapshot is one periodic (or start/stop) capture of some piece of live,
// in-process state - status, settings, FIS-B cache status/inventory,
// towers, GPS/clock, service health. Deliberately generic: main supplies
// whatever it already has in memory (the same values its own /getStatus
// etc. handlers serve) as Data, so this package never needs its own
// duplicate understanding of those types or an HTTP round trip.
type Snapshot struct {
	Seq          uint64    `json:"seq"`
	ElapsedNanos int64     `json:"elapsedNanos"`
	WallClock    time.Time `json:"wallClock"`

	// Label identifies what this snapshot is of - "status", "settings",
	// "fisbCacheStatus", "fisbCacheInventory", "towers", "gpsClock",
	// "health", captured together at each periodic tick under the same
	// Seq/ElapsedNanos, or "sessionStart"/"sessionStop" for the two bundle
	// boundary snapshots.
	Label string `json:"label"`

	// Data is the caller-supplied payload, already JSON-marshalable (the
	// same struct/map main's own handlers would serve) - stored as raw
	// JSON so this package never needs to import main's own types.
	Data interface{} `json:"data"`
}

// StopReason records why a session ended, for the manifest - "requested"
// is the normal, owner-initiated stop; the others are this package's own
// bounded-resource enforcement, never silent.
const (
	StopRequested   = "requested"
	StopDiskFull    = "disk_full"
	StopMaxDuration = "max_duration"
	StopMaxBytes    = "max_bytes"
	StopInterrupted = "interrupted" // process exited without an orderly Stop()
)

// FileInfo records one bundle file's identity for the manifest.
type FileInfo struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Manifest is written once, at Stop(), after every data file is flushed
// and hashed - the single source of truth for what a bundle actually
// contains, per this task's own requirement that a clean hash alone must
// not be read as proof of complete capture (see Validate in validate.go,
// which checks the manifest's own claims against the files on disk
// rather than trusting either alone).
type Manifest struct {
	FormatVersion int    `json:"formatVersion"`
	SessionID     string `json:"sessionId"`

	// EmbeddedBuild is this build's own stratuxBuild string (the same
	// value getStatus.Build reports) - so a bundle unambiguously
	// identifies which exact binary produced it.
	EmbeddedBuild string `json:"embeddedBuild"`

	StartWallClock time.Time `json:"startWallClock"`
	StopWallClock  time.Time `json:"stopWallClock"`
	DurationNanos  int64     `json:"durationNanos"`

	FrameCount    uint64 `json:"frameCount"`
	GDL90Count    uint64 `json:"gdl90Count"`
	SnapshotCount uint64 `json:"snapshotCount"`

	// Dropped* counts every record this session could not write - a
	// full queue (the hot path never blocks), a write error, or a
	// bounded-resource stop mid-batch. Non-zero here means the bundle is
	// incomplete even if every file's own hash matches what was actually
	// written - see Validate's "partial" classification.
	DroppedFrames    uint64 `json:"droppedFrames"`
	DroppedGDL90     uint64 `json:"droppedGdl90"`
	DroppedSnapshots uint64 `json:"droppedSnapshots"`

	StopReason string `json:"stopReason"`

	Files []FileInfo `json:"files"`

	// Gaps is a free-text list of anything else worth a human's attention
	// - e.g. "disk free space fell below the reserve during recording".
	Gaps []string `json:"gaps,omitempty"`
}
