package fisbrecorder

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Options bounds a Recorder's resource use. Every field has a safe,
// generous-but-finite default (see DefaultOptions) - recording must never
// be able to grow without limit just because a session ran long or a
// field trip lasted longer than planned.
type Options struct {
	// QueueSize is the buffered-channel depth for each of the three
	// record kinds. A full queue means the writer goroutine cannot keep
	// up (or is blocked); new records are dropped and counted, never
	// blocking the caller - see Record{Frame,GDL90,Snapshot}'s own doc
	// comments.
	QueueSize int

	// MaxDuration stops the session automatically once reached (0 = no
	// limit).
	MaxDuration time.Duration

	// MaxBytes stops the session once the sum of all written file sizes
	// would exceed this (0 = no limit, still bounded by MinFreeBytes).
	MaxBytes int64

	// MinFreeBytes is the free-space reserve on the recording
	// filesystem below which the session stops itself rather than risk
	// filling the persistent partition - see the design doc's own
	// discussion of why main/trace.go's TraceLog checking free space on
	// the *overlay* while writing to /var/log is the mistake this value
	// exists to avoid repeating.
	MinFreeBytes int64

	// SnapshotInterval is how often RecordSnapshot's caller is expected
	// to be invoked - this package does not itself run a timer; main
	// owns that goroutine and calls in on its own schedule. Recorded here
	// only so it can be included in the manifest for a reader's context.
	SnapshotInterval time.Duration
}

// DefaultOptions is deliberately conservative for a field session on a
// Raspberry Pi with limited RAM and a data partition also shared by the
// FIS-B cache and recordings: 4 hours max, 500 MB max bundle size, 200 MB
// free-space reserve, 10s snapshot cadence, 4096-deep queues (generous for
// UAT's own real-world message rate - dozens/sec at the very busiest,
// never sustained thousands).
func DefaultOptions() Options {
	return Options{
		QueueSize:        4096,
		MaxDuration:      4 * time.Hour,
		MaxBytes:         500 * 1024 * 1024,
		MinFreeBytes:     200 * 1024 * 1024,
		SnapshotInterval: 10 * time.Second,
	}
}

// diskFreeBytes is overridable by tests; real implementation in
// recorder_unix.go (statfs-based, no new dependency).
var diskFreeBytes = realDiskFreeBytes

// Recorder is one field-recording session. Not safe for concurrent Start
// calls; RecordFrame/RecordGDL90/RecordSnapshot are safe to call from any
// number of goroutines at any time (including when inactive - they are
// then simply no-ops, so call sites never need their own IsActive check
// on the hot path).
type Recorder struct {
	baseDir       string
	embeddedBuild string
	opts          Options

	mu        sync.Mutex
	active    bool
	sessionID string
	dir       string
	start     time.Time // for ElapsedNanos via time.Since (monotonic)
	startWall time.Time

	// *Seq assigns every attempt (accepted or dropped) its own gap-free
	// number, so a hole in a written file's Seq values always means "this
	// one was dropped" - the validator's seq-gap check depends on that.
	// *Accepted counts only attempts actually handed to the writer
	// goroutine; the manifest's FrameCount/GDL90Count/SnapshotCount report
	// *Accepted (never the raw attempt count), so
	// FrameCount+DroppedFrames always equals the number of RecordFrame
	// calls with no double-booking between the two.
	frameSeq    atomic.Uint64
	gdl90Seq    atomic.Uint64
	snapshotSeq atomic.Uint64

	framesAccepted    atomic.Uint64
	gdl90Accepted     atomic.Uint64
	snapshotsAccepted atomic.Uint64

	droppedFrames    atomic.Uint64
	droppedGDL90     atomic.Uint64
	droppedSnapshots atomic.Uint64

	frameCh    chan FrameRecord
	gdl90Ch    chan GDL90Record
	snapshotCh chan Snapshot

	stopCh     chan string // stop reason, closed by watchdog or Stop()
	doneCh     chan struct{}
	stopOnce   sync.Once
	stopReason string
}

// New creates a Recorder that will write under baseDir (the caller is
// responsible for passing the persistent partition, e.g.
// filepath.Join(main.PersistentDataPath, "fisb-recordings") - this
// package hardcodes no path of its own, so it stays testable with a
// t.TempDir()). embeddedBuild is recorded in the manifest verbatim.
func New(baseDir, embeddedBuild string, opts Options) *Recorder {
	return &Recorder{baseDir: baseDir, embeddedBuild: embeddedBuild, opts: opts}
}

// IsActive reports whether a session is currently recording.
func (r *Recorder) IsActive() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// Start begins a new session, named by its own start time
// (YYYYMMDD-HHMMSS, filesystem- and shell-safe). Returns an error without
// starting anything if baseDir cannot be created/written, or if a session
// is already active.
func (r *Recorder) Start() (sessionID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active {
		return "", fmt.Errorf("fisbrecorder: a session is already active (%s)", r.sessionID)
	}

	now := time.Now()
	sessionID = now.UTC().Format("20060102-150405")
	dir := filepath.Join(r.baseDir, sessionID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("fisbrecorder: could not create session directory %s: %w", dir, err)
	}

	r.sessionID = sessionID
	r.dir = dir
	r.start = now
	r.startWall = now
	r.frameSeq.Store(0)
	r.gdl90Seq.Store(0)
	r.snapshotSeq.Store(0)
	r.framesAccepted.Store(0)
	r.gdl90Accepted.Store(0)
	r.snapshotsAccepted.Store(0)
	r.droppedFrames.Store(0)
	r.droppedGDL90.Store(0)
	r.droppedSnapshots.Store(0)

	r.frameCh = make(chan FrameRecord, r.opts.QueueSize)
	r.gdl90Ch = make(chan GDL90Record, r.opts.QueueSize)
	r.snapshotCh = make(chan Snapshot, r.opts.QueueSize)
	r.stopCh = make(chan string, 1)
	r.doneCh = make(chan struct{})
	r.stopReason = ""

	r.active = true

	go r.run()
	return sessionID, nil
}

// RecordFrame queues one decoded UAT frame. Never blocks: if the queue is
// full (the writer cannot keep up), the record is dropped and counted in
// DroppedFrames - visible in the manifest, never silent. A no-op if no
// session is active.
func (r *Recorder) RecordFrame(frame string) {
	r.mu.Lock()
	active := r.active
	ch := r.frameCh
	start := r.start
	r.mu.Unlock()
	if !active {
		return
	}
	rec := FrameRecord{
		Seq:          r.frameSeq.Add(1) - 1,
		ElapsedNanos: time.Since(start).Nanoseconds(),
		WallClock:    time.Now(),
		Frame:        frame,
	}
	select {
	case ch <- rec:
		r.framesAccepted.Add(1)
	default:
		r.droppedFrames.Add(1)
	}
}

// RecordGDL90 queues one outbound GDL90 write. Never blocks - see
// RecordFrame's own doc comment for the same contract, critical here
// specifically because this is called from connectionWriter's own hot
// send path and must never add latency to real GDL90 delivery.
func (r *Recorder) RecordGDL90(connectionKey string, msg []byte) {
	r.mu.Lock()
	active := r.active
	ch := r.gdl90Ch
	start := r.start
	r.mu.Unlock()
	if !active {
		return
	}
	cp := make([]byte, len(msg))
	copy(cp, msg)
	rec := GDL90Record{
		Seq:           r.gdl90Seq.Add(1) - 1,
		ElapsedNanos:  time.Since(start).Nanoseconds(),
		ConnectionKey: connectionKey,
		Bytes:         cp,
	}
	select {
	case ch <- rec:
		r.gdl90Accepted.Add(1)
	default:
		r.droppedGDL90.Add(1)
	}
}

// RecordSnapshot queues one periodic (or start/stop) state snapshot.
// data must be JSON-marshalable; marshaling happens on the writer
// goroutine, not the caller's, so a slow/large marshal never blocks
// main's own periodic-snapshot goroutine either.
func (r *Recorder) RecordSnapshot(label string, data interface{}) {
	r.mu.Lock()
	active := r.active
	ch := r.snapshotCh
	start := r.start
	r.mu.Unlock()
	if !active {
		return
	}
	rec := Snapshot{
		Seq:          r.snapshotSeq.Add(1) - 1,
		ElapsedNanos: time.Since(start).Nanoseconds(),
		WallClock:    time.Now(),
		Label:        label,
		Data:         data,
	}
	select {
	case ch <- rec:
		r.snapshotsAccepted.Add(1)
	default:
		r.droppedSnapshots.Add(1)
	}
}

// Stop ends the session: signals the writer goroutine to flush and close
// every file, computes SHA-256 for each, and writes manifest.json. Safe
// to call once; a second call returns the same result without error. If
// the session already stopped itself (disk full, max duration/bytes),
// Stop still returns the completed manifest with that StopReason.
func (r *Recorder) Stop() (*Manifest, error) {
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return nil, fmt.Errorf("fisbrecorder: no active session")
	}
	r.mu.Unlock()

	r.stopOnce.Do(func() {
		select {
		case r.stopCh <- StopRequested:
		default:
		}
	})
	<-r.doneCh

	r.mu.Lock()
	r.active = false
	r.mu.Unlock()

	return r.readManifest()
}

func (r *Recorder) readManifest() (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(r.dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// run is the writer goroutine: owns all three output files for the whole
// session, drains the three channels, and enforces the bounded-resource
// stop conditions. Runs until stopCh fires (Stop() or self-triggered).
func (r *Recorder) run() {
	defer close(r.doneCh)

	frameW, frameGz, frameFile, err := newJSONLWriter(filepath.Join(r.dir, "frames.jsonl.gz"))
	if err != nil {
		r.finishWithError(fmt.Sprintf("could not open frames file: %v", err))
		return
	}
	gdl90W, gdl90Gz, gdl90File, err := newJSONLWriter(filepath.Join(r.dir, "gdl90.jsonl.gz"))
	if err != nil {
		r.finishWithError(fmt.Sprintf("could not open gdl90 file: %v", err))
		return
	}
	snapW, snapGz, snapFile, err := newJSONLWriter(filepath.Join(r.dir, "snapshots.jsonl.gz"))
	if err != nil {
		r.finishWithError(fmt.Sprintf("could not open snapshots file: %v", err))
		return
	}

	checkTicker := time.NewTicker(2 * time.Second)
	defer checkTicker.Stop()

	stopReason := ""
	var gaps []string

drainLoop:
	for {
		select {
		case rec := <-r.frameCh:
			if err := writeJSONL(frameW, rec); err != nil {
				gaps = append(gaps, fmt.Sprintf("frame write error at seq %d: %v", rec.Seq, err))
			}
		case rec := <-r.gdl90Ch:
			if err := writeJSONL(gdl90W, rec); err != nil {
				gaps = append(gaps, fmt.Sprintf("gdl90 write error at seq %d: %v", rec.Seq, err))
			}
		case rec := <-r.snapshotCh:
			if err := writeJSONL(snapW, rec); err != nil {
				gaps = append(gaps, fmt.Sprintf("snapshot write error at seq %d: %v", rec.Seq, err))
			}
		case reason := <-r.stopCh:
			stopReason = reason
			break drainLoop
		case <-checkTicker.C:
			if reason, ok := r.checkBounds(); ok {
				stopReason = reason
				break drainLoop
			}
		}
	}

	// Final drain: pick up anything queued at the moment of stop, bounded
	// so a stop can never hang - at most one pass over whatever is
	// already buffered, matching this package's own no-block contract.
drainRemaining:
	for {
		select {
		case rec := <-r.frameCh:
			writeJSONL(frameW, rec)
		case rec := <-r.gdl90Ch:
			writeJSONL(gdl90W, rec)
		case rec := <-r.snapshotCh:
			writeJSONL(snapW, rec)
		default:
			break drainRemaining
		}
	}

	frameW.Flush()
	frameGz.Close()
	frameFile.Close()
	gdl90W.Flush()
	gdl90Gz.Close()
	gdl90File.Close()
	snapW.Flush()
	snapGz.Close()
	snapFile.Close()

	if stopReason == "" {
		stopReason = StopInterrupted
	}

	stopWall := time.Now()
	m := Manifest{
		FormatVersion:    FormatVersion,
		SessionID:        r.sessionID,
		EmbeddedBuild:    r.embeddedBuild,
		StartWallClock:   r.startWall,
		StopWallClock:    stopWall,
		DurationNanos:    time.Since(r.start).Nanoseconds(),
		FrameCount:       r.framesAccepted.Load(),
		GDL90Count:       r.gdl90Accepted.Load(),
		SnapshotCount:    r.snapshotsAccepted.Load(),
		DroppedFrames:    r.droppedFrames.Load(),
		DroppedGDL90:     r.droppedGDL90.Load(),
		DroppedSnapshots: r.droppedSnapshots.Load(),
		StopReason:       stopReason,
		Gaps:             gaps,
	}

	for _, name := range []string{"frames.jsonl.gz", "gdl90.jsonl.gz", "snapshots.jsonl.gz"} {
		fi, err := hashFile(filepath.Join(r.dir, name))
		if err != nil {
			m.Gaps = append(m.Gaps, fmt.Sprintf("could not hash %s: %v", name, err))
			continue
		}
		fi.Name = name
		m.Files = append(m.Files, *fi)
	}

	mb, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(filepath.Join(r.dir, "manifest.json"), mb, 0o640)
}

func (r *Recorder) finishWithError(msg string) {
	m := Manifest{
		FormatVersion:  FormatVersion,
		SessionID:      r.sessionID,
		EmbeddedBuild:  r.embeddedBuild,
		StartWallClock: r.startWall,
		StopWallClock:  time.Now(),
		StopReason:     StopInterrupted,
		Gaps:           []string{msg},
	}
	mb, _ := json.MarshalIndent(m, "", "  ")
	os.WriteFile(filepath.Join(r.dir, "manifest.json"), mb, 0o640)
}

// checkBounds enforces MaxDuration/MaxBytes/MinFreeBytes. Called every 2s
// from the writer goroutine (not the hot record path) - a small, bounded
// latency to notice a limit was hit is an explicit, acceptable tradeoff
// for never touching the hot path with filesystem syscalls.
func (r *Recorder) checkBounds() (reason string, stop bool) {
	if r.opts.MaxDuration > 0 && time.Since(r.start) > r.opts.MaxDuration {
		return StopMaxDuration, true
	}
	if r.opts.MaxBytes > 0 {
		var total int64
		for _, name := range []string{"frames.jsonl.gz", "gdl90.jsonl.gz", "snapshots.jsonl.gz"} {
			if fi, err := os.Stat(filepath.Join(r.dir, name)); err == nil {
				total += fi.Size()
			}
		}
		if total > r.opts.MaxBytes {
			return StopMaxBytes, true
		}
	}
	if r.opts.MinFreeBytes > 0 {
		free, err := diskFreeBytes(r.dir)
		if err == nil && free < uint64(r.opts.MinFreeBytes) {
			return StopDiskFull, true
		}
	}
	return "", false
}

func newJSONLWriter(path string) (*gzip.Writer, *gzip.Writer, *os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return nil, nil, nil, err
	}
	gz := gzip.NewWriter(f)
	// gz is both the writer used for JSONL lines and the thing we flush/
	// close - keep a second reference name for readability at call sites.
	return gz, gz, f, nil
}

func writeJSONL(w io.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err = w.Write([]byte{'\n'})
	return err
}

func hashFile(path string) (*FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, err
	}
	return &FileInfo{Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}
