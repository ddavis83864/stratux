/*
fisbcacherun.go: main/'s glue that actually drives the fisbcache.Store -
the bounded capture queue and its worker goroutine, startup recovery,
periodic retention, and the read-only status/inventory snapshot the HTTP
API and dashboard consume. See docs/fisb-weather-cache.md.

Failure isolation (mirrors autorecordrun.go's own established doc
comment): live UAT/978 reception and GDL90 forwarding must continue even
if this file's own code panics, blocks, or fails - see
fisbCaptureText/fisbCaptureNexrad's own doc comments for exactly how a
capture-path caller in gen_gdl90.go is protected from ever blocking on
this feature.
*/
package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/readiness"
	"github.com/stratux/stratux/storagelifecycle"
)

// fisbCacheStartupRecoveryTimeout bounds how long startup recovery may
// run before this feature simply reports itself degraded and moves on -
// never delays Stratux startup itself, since recovery runs in its own
// goroutine from the very first line of initFISBCache. A var, not a
// const, solely so a test can shrink it to prove the bound is actually
// honored without a real 30-second wait - mirrors
// autoRecordMountWaitTimeout's identical pattern. Never reassigned in
// production.
var fisbCacheStartupRecoveryTimeout = 30 * time.Second

// fisbCacheRetentionInterval is deliberately similar to
// storageLifecycleScanInterval - retention does not need to run more
// often than the underlying storage-pressure signal it partly reacts to.
const fisbCacheRetentionInterval = 60 * time.Second

type fisbCaptureItem struct {
	entry   fisbcache.Entry
	payload string
}

var (
	fisbCacheMu                sync.Mutex
	fisbCacheStore             *fisbcache.Store
	fisbCacheSettingsCache     FISBCacheSettings
	fisbCachePending           *fisbPendingQueue
	fisbCacheDroppedWrites     uint64 // atomic
	fisbCachePressureRejected  uint64 // atomic
	fisbCacheOversizedRejected uint64 // atomic
	fisbCacheCapacityRejected  uint64 // atomic
	fisbCacheShutdownRejected  uint64 // atomic
	fisbCacheStartupRecovered  bool
	fisbCacheRecoveryError     bool
	fisbCacheNonFatalErrors    bool
	fisbCacheLastCleanupUTC    time.Time
	fisbCacheLastCleanupCount  int
	fisbCacheShuttingDown      bool
)

// fisbCacheDiskMu guards every actual mutation of this feature's own
// persisted files (fisbCacheDir) - a write (fisbCachePersist) and a
// planned eviction's check-then-delete (fisbCacheEvictKeyIfUnchanged) can
// never run concurrently for this whole namespace while it is held.
//
// This exists to close a genuine TOCTOU: budget/retention eviction is
// always PLANNED against a Snapshot taken slightly before it executes
// (see fisbCacheRunRetention) - without this lock, a live capture could
// admit and persist a fresher copy of a key in the gap between that
// snapshot and eviction's own decision to remove it, and eviction would
// then delete the FRESH file a moment after it was written, because
// Store.Delete (unlike DeleteIfUnchanged) has no way to notice the entry
// changed underneath it. Serializing "one write" against "one planned
// delete's own check-then-remove" removes that gap entirely: whichever
// side wins the race for fisbCacheDiskMu completes first, so eviction's
// re-check of the Store (fisbCacheEvictKeyIfUnchanged's own Get call,
// taken while already holding this lock) can never observe a state that
// a concurrent write is still in the middle of changing. See
// docs/fisb-weather-cache.md's "Synchronous admission bounds" section for
// the full model this makes possible to prove.
//
// Scope: only the raw file mutation (fisbCachePersist's ReplaceProduct
// call, and fisbCacheEvictKeyIfUnchanged's Remove call) is ever done
// while holding this lock - never the surrounding decision logic
// (PlanEviction, Snapshot, JSON encoding), which stays outside it so this
// lock is held only as briefly as the actual I/O requires. Independent
// of fisbCacheMu (settings/state) and Store's own internal mutex - never
// acquired while already holding either of those, so there is no
// possibility of a lock-order cycle between them.
var fisbCacheDiskMu sync.Mutex

// --- Asynchronous cleanup worker ---------------------------------------
//
// Every disk-mutating capacity/retention decision this feature ever
// makes runs on ONE dedicated goroutine (fisbCacheCleanupWorker), never
// on the live UAT/978 decode path and never inline inside an HTTP
// handler. fisbCacheEnqueue/reserveAndEnqueue (fisbcachereserve.go) -
// the one function called directly from main/gen_gdl90.go's decode loop
// - never evicts; when capacity is not immediately available it rejects
// the offer and calls fisbCacheRequestCleanup, a non-blocking,
// coalescing signal, then returns immediately. See
// docs/fisb-weather-cache.md's "Synchronous admission bounds" section
// for the full model this establishes.

// fisbCacheCleanupSignal is a buffered(1), coalescing, non-blocking
// signal channel: "capacity or settings changed - please re-evaluate
// whether anything needs evicting." At most one pending signal can ever
// sit in this channel - a second request while one is already pending
// is a harmless, intentional no-op (see fisbCacheRequestCleanup): the
// eventual pass re-reads whatever the CURRENT state is when it actually
// runs, never a stale snapshot from when either request was made, so
// coalescing never loses information a caller needed acted on.
var fisbCacheCleanupSignal chan struct{}

// fisbCacheCleanupRunning is true for the duration of one cleanup pass -
// exposed via the status API so an operator can distinguish "idle,
// nothing to do" from "actively evicting right now."
var fisbCacheCleanupRunning atomic.Bool

// fisbCacheCleanupRequested/fisbCacheCleanupRuns together give an honest,
// directly observable measure of coalescing: every fisbCacheRequestCleanup
// call increments Requested, regardless of whether it actually triggers
// a new pass or coalesces into one already pending; Runs only increments
// once a pass actually executes. Requested > Runs is expected and fine
// under sustained pressure - it is coalescing working as designed, not
// lost work.
var (
	fisbCacheCleanupRequested uint64 // atomic
	fisbCacheCleanupRuns      uint64 // atomic
)

// fisbCacheRequestCleanup asks the asynchronous cleanup worker to run a
// pass soon - a non-blocking, coalescing send, NEVER a wait. Safe to
// call from any goroutine, including the live UAT/978 decode path itself
// (fisbCacheEnqueue calls this when a capture is rejected for lack of
// capacity) and an HTTP handler goroutine (handleSetFISBCacheSettingsRequest
// calls this after a settings change, rather than running the
// enforcement pass inline - see that handler's own doc comment for why).
func fisbCacheRequestCleanup() {
	atomic.AddUint64(&fisbCacheCleanupRequested, 1)
	select {
	case fisbCacheCleanupSignal <- struct{}{}:
	default:
	}
}

// fisbCacheCleanupWorker is this feature's ONE dedicated goroutine for
// every disk-mutating capacity/retention decision - the sole caller of
// fisbCacheRunRetention (see that function's own doc comment). Runs a
// pass whenever signaled (fisbCacheRequestCleanup - a capacity
// rejection, a settings change, startup recovery finishing, or the
// capture worker's own post-admission backstop - see each call site's
// own doc comment) OR on the periodic fisbCacheRetentionInterval tick (a
// backstop for pure time-based expiry, which needs no new admission or
// request to become due). At most one pass ever executes at a time, by
// construction: this is the only goroutine that ever calls
// fisbCacheRunRetention, so two passes can never run concurrently with
// each other.
func fisbCacheCleanupWorker() {
	ticker := time.NewTicker(fisbCacheRetentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-fisbCacheCleanupSignal:
		case <-ticker.C:
		}
		fisbCacheCleanupRunning.Store(true)
		fisbCacheRunRetention()
		atomic.AddUint64(&fisbCacheCleanupRuns, 1)
		fisbCacheCleanupRunning.Store(false)
	}
}

// fisbCacheHandleShutdown stops this feature from admitting any further
// capture into the queue - called once from gracefulShutdown (see
// main/gen_gdl90.go). Never closes fisbCacheQueue itself (an in-flight
// send from a concurrent capture call could otherwise panic on a closed
// channel) and never awaits the worker goroutine draining what is
// already queued - per this feature's own "do not delay shutdown
// indefinitely" requirement, any already-queued item is simply abandoned
// if the process exits before the worker gets to it.
func fisbCacheHandleShutdown() {
	fisbCacheMu.Lock()
	fisbCacheShuttingDown = true
	fisbCacheMu.Unlock()
}

// initFISBCache loads settings, constructs the in-memory Store, and
// starts this feature's own capture-worker and retention-loop goroutines
// - must run after initStorageLifecycle (fisbCacheNamespace is already
// registered - see main/storagelifecycleapi.go) and after
// PersistentDataPath is final. Never blocks: startup recovery runs in
// its own goroutine, bounded by fisbCacheStartupRecoveryTimeout, and this
// function returns immediately regardless of recovery's own progress.
func initFISBCache() {
	fisbCacheMu.Lock()
	fisbCacheSettingsCache = loadFISBCacheSettings()
	fisbCacheStore = fisbcache.NewStore()
	fisbCachePending = newFISBPendingQueue(fisbCachePendingCapacity)
	fisbCacheCleanupSignal = make(chan struct{}, 1)
	fisbCacheFS = storagelifecycle.NewOSFS()
	fisbCacheAtomicWriter = storagelifecycle.NewAtomicWriter(fisbCacheFS)
	settingsSnapshot := fisbCacheSettingsCache
	fisbCacheMu.Unlock()

	if err := os.MkdirAll(fisbCacheDir, 0o755); err != nil {
		log.Printf("fisbcache: could not create cache directory: %s\n", err)
		fisbCacheMu.Lock()
		fisbCacheRecoveryError = true
		fisbCacheMu.Unlock()
	}

	go fisbCacheCaptureWorker()
	go fisbCacheStartupRecovery()
	go fisbCacheCleanupWorker()

	log.Printf("fisbcache: initialized (enabled=%v persistenceEnabled=%v)\n", settingsSnapshot.Enabled, settingsSnapshot.PersistenceEnabled)
}

// fisbCacheTrustedTimeState mirrors autoRecordTrustedTime's own,
// identical policy: DEGRADED/INVALID/UNSYNCHRONIZED all count as
// untrusted.
func fisbCacheTrustedTimeState() bool {
	switch timeTrust.State() {
	case readiness.TimeGNSSSynced, readiness.TimeNetworkSynced:
		return true
	default:
		return false
	}
}

// fisbCacheTrustedNowUTC returns the current wall-clock time only when
// it is currently trusted - the zero value otherwise. Every caller in
// this feature that would otherwise use time.Now() for anything other
// than pure display uses this instead (see docs/fisb-weather-cache.md's
// time-model section: "never trust an uncorrected startup clock").
func fisbCacheTrustedNowUTC() time.Time {
	if !fisbCacheTrustedTimeState() {
		return time.Time{}
	}
	return time.Now().UTC()
}

// fisbReadFileBounded reads path, refusing anything larger than a
// persisted entry could ever legitimately be - defense in depth beyond
// DecodePersistedEntry's own size check, so a pathological file can never
// even be fully read into memory first.
func fisbReadFileBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, fisbcacheMaxReadBytes))
}

// fisbcacheMaxReadBytes mirrors fisbcache's own maxPersistedEntryBytes
// bound (kept as a small, explicit redundant constant here rather than
// exporting that unexported constant, since this is the one place
// outside the pure package that needs it, and a slightly-generous local
// bound costs nothing).
const fisbcacheMaxReadBytes = 128 * 1024

// --- Capture path ----------------------------------------------------

// fisbCaptureText is called from main.registerADSBTextMessageReceived
// (main/gen_gdl90.go) for every decoded FIS-B text line - see that
// function's own doc comment for the (productType, location, timeToken,
// data) tuple it already extracts. Never blocks the caller: if this
// feature is disabled, or the bounded queue is full, this returns
// immediately having done nothing but increment a counter.
func fisbCaptureText(productType, location, data string, ft fisbcache.FISBTime) {
	fisbCacheMu.Lock()
	enabled := fisbCacheSettingsCache.Enabled
	fisbCacheMu.Unlock()
	if !enabled || location == "" || productType == "" {
		return
	}
	key := fisbcache.TextKey(productType, location)
	fisbCacheEnqueue(key, ft, data)
}

// fisbCaptureNexrad is called for every decoded NEXRAD tile (product ID
// 63/64) - see main/gen_gdl90.go's own weatherRawUpdate.SendJSON(f) call
// site. payload is a caller-supplied, already-bounded encoding of the
// tile's intensity data (see fisbCacheEncodeNexradPayload) - this
// function never touches uatparse types directly, keeping this file's
// only dependency on uatparse's shape at its one call site in
// gen_gdl90.go.
func fisbCaptureNexrad(radarType uint32, scale int, latNorth, lonWest, height, width float64, payload string, ft fisbcache.FISBTime) {
	fisbCacheMu.Lock()
	enabled := fisbCacheSettingsCache.Enabled
	fisbCacheMu.Unlock()
	if !enabled {
		return
	}
	key := fisbcache.NexradKey(radarType, scale, latNorth, lonWest, height, width)
	fisbCacheEnqueue(key, ft, payload)
}

// fisbCacheEnqueue is this feature's one synchronous capacity-reservation
// gate. It never merely queues a capture and hopes there is room later -
// every check here, including the projected-capacity reservation itself,
// happens before this function returns, so a caller (fisbCaptureText/
// fisbCaptureNexrad) never learns "accepted" for something that was not
// actually, synchronously, reserved room for at that exact moment. See
// docs/fisb-weather-cache.md's "Synchronous admission bounds" section for
// the full model and its proof.
func fisbCacheEnqueue(key fisbcache.Key, ft fisbcache.FISBTime, payload string) {
	fisbCacheMu.Lock()
	shuttingDown := fisbCacheShuttingDown
	settings := fisbCacheSettingsCache
	fisbCacheMu.Unlock()
	if shuttingDown {
		atomic.AddUint64(&fisbCacheShutdownRejected, 1)
		return
	}
	// A single entry whose own payload alone already exceeds the
	// currently configured byte budget could never be admitted without
	// either violating that budget outright or evicting every other
	// entry - including ones far fresher than this one - just to make
	// room for something that still would not fit twice. Rejected here,
	// before this function ever computes a full reservation: this is
	// what keeps the strict disk-overhead bound this feature promises
	// independent of what any single incoming product happens to be
	// sized like (see fisbCacheOversizedRejected).
	// settings.MaxCacheBytes<=0 cannot happen for a validated settings
	// value (see FISBCacheSettings.Validate) but is treated as "no
	// limit" here too, consistent with fisbcache.PlanEviction's own
	// convention, rather than this check inventing a stricter one.
	if settings.MaxCacheBytes > 0 && int64(len(payload)) > settings.MaxCacheBytes {
		atomic.AddUint64(&fisbCacheOversizedRejected, 1)
		return
	}
	// Global storage pressure (HIGH/CRITICAL/UNKNOWN) prohibits admitting
	// any new entry - never a second, this-cache-only pressure
	// computation; see fisbCacheStoragePressureProhibited's own doc
	// comment. Already-cached entries are still served; this only stops
	// growing the cache further while storage is genuinely tight or its
	// state cannot currently be confirmed. Counted separately from
	// fisbCacheDroppedWrites/fisbCacheCapacityRejected so an operator can
	// tell "backlogged," "budget exhausted," and "storage is under
	// pressure" apart at a glance.
	if _, prohibited := fisbCacheStoragePressureProhibited(); prohibited {
		atomic.AddUint64(&fisbCachePressureRejected, 1)
		return
	}
	receiveUTC := fisbCacheTrustedNowUTC()
	source := fisbcache.ReconstructSourceTime(ft, receiveUTC, !receiveUTC.IsZero())
	entry := fisbcache.Entry{
		Key:                 key,
		Source:              source,
		ReceivedAtMonotonic: monotonicSeconds(),
		ReceivedAtUTC:       receiveUTC,
		SizeBytes:           int64(len(payload)),
	}
	item := fisbCaptureItem{entry: entry, payload: payload}

	accepted, reason := fisbCachePending.reserveAndEnqueue(key, item, settings)
	if !accepted {
		switch reason {
		case fisbReserveReasonStructuralFull:
			atomic.AddUint64(&fisbCacheDroppedWrites, 1)
		case fisbReserveReasonCapacity:
			atomic.AddUint64(&fisbCacheCapacityRejected, 1)
		}
	}
}

// fisbCacheCaptureWorker is this feature's one dedicated goroutine for
// admitting queued captures into the Store and, when persistence is
// enabled, writing them to disk - mirrors autorecordrun.go's own
// dedicated-goroutine-never-blocks-the-decoder pattern. A single slow or
// failing write here only ever delays this goroutine's own next queue
// read, never the capture-path caller (fisbCaptureText/fisbCaptureNexrad
// already returned by the time this runs).
//
// Synchronous admission bounds: by the time an item reaches this worker
// at all, fisbCacheEnqueue's own reservation has already, synchronously
// and purely in-memory, made room for it - see fisbcachereserve.go. This
// worker itself still requests a cleanup pass (fisbCacheRequestCleanup,
// never a direct fisbCacheRunRetention call - this goroutine, like the
// live decode goroutine, never performs disk-mutating eviction itself)
// immediately after each Admit, before persisting - usually a no-op
// given the reservation already made room, but this is also what
// self-corrects an already-reserved item against a settings tightening
// that happened after the reservation was made but before it was
// committed - see docs/fisb-weather-cache.md's "Synchronous admission
// bounds" section.
func fisbCacheCaptureWorker() {
	for range fisbCachePending.wake {
		for {
			key, item, ok := fisbCachePending.pop()
			if !ok {
				break
			}
			fisbCacheProcessOneCaptureItem(key, item)
		}
	}
}

// fisbCacheProcessOneCaptureItem is fisbCacheCaptureWorker's own inner,
// per-item logic - factored out as a plain function (not itself a
// goroutine) so it can be driven directly, deterministically, without
// starting a real background worker: production always reaches it via
// fisbCacheCaptureWorker's pop loop above; tests that need to prove the
// wake/pop goroutine plumbing itself still can (and one does), while
// tests that only need to prove admission/eviction/persistence behavior
// can call this directly after their own pop(), with no leaked
// goroutine risk across test boundaries.
func fisbCacheProcessOneCaptureItem(key fisbcache.Key, item fisbCaptureItem) {
	result := fisbCacheStore.Admit(item.entry)
	// The reservation's job ends the instant Admit decides this key's
	// fate - see fisbPendingQueue's own doc comment.
	fisbCachePending.releaseInFlight(key)
	if result != fisbcache.AdmitAccepted && result != fisbcache.AdmitSuperseded {
		return
	}
	fisbCacheRequestCleanup()

	fisbCacheMu.Lock()
	persistenceEnabled := fisbCacheSettingsCache.Enabled && fisbCacheSettingsCache.PersistenceEnabled
	fisbCacheMu.Unlock()
	if !persistenceEnabled {
		return
	}
	// Defensive re-check, not expected to ever actually trigger (see
	// fisbCacheCaptureWorker's own doc comment): only spend a write on
	// this entry if enforcement, just above, did not remove it.
	if _, stillPresent := fisbCacheStore.Get(item.entry.Key); !stillPresent {
		return
	}
	if err := fisbCachePersist(item.entry, item.payload); err != nil {
		log.Printf("fisbcache: could not persist entry: %s\n", err)
	}
}

// fisbCachePersist writes e/payload to disk, replacing any prior file for
// the same Key. The actual file mutation (ReplaceProduct's temp-write-
// then-rename) is done while holding fisbCacheDiskMu - see that lock's
// own doc comment for exactly which concurrent operation this excludes
// and why.
func fisbCachePersist(e fisbcache.Entry, payload string) error {
	persisted, err := fisbcache.EncodePersistedEntry(e, payload)
	if err != nil {
		return err
	}
	meta := storagelifecycle.CacheEntryMetadata{
		Key:                  storagelifecycle.CacheProductKey{ProductType: e.Key.StorageProductType(), ProductID: e.Key.StorageProductID()},
		ReceivedAtMonotonic:  e.ReceivedAtMonotonic,
		GeneratedAtWallClock: e.ReceivedAtUTC,
		SizeBytes:            e.SizeBytes,
	}
	fisbCacheDiskMu.Lock()
	defer fisbCacheDiskMu.Unlock()
	return (fisbCacheLifecycle{}).ReplaceProduct(meta, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(persisted)
	})
}

// --- Startup recovery --------------------------------------------------

// fisbCacheStartupRecovery runs once, asynchronously, from initFISBCache.
// It never delays Stratux startup (it is always started with `go`, from
// the very first thing initFISBCache does) and it never trusts a
// not-yet-corrected wall clock: it waits (bounded by
// fisbCacheStartupRecoveryTimeout) for trusted time before recovering any
// interrupted temp file or re-admitting any persisted entry into the
// in-memory Store, per docs/fisb-weather-cache.md's explicit "wait for
// trusted time before determining cross-reboot freshness" requirement.
// If trusted time never arrives within the bound, recovery gives up
// (logged, RecoveryError left false - this is not a fatal condition, it
// simply means no persisted entry is re-admitted this boot) rather than
// serving anything it cannot trust.
//
// Synchronous admission bounds: recovery finishes by requesting the same
// cleanup pass (fisbCacheRequestCleanup) every live admission does, once,
// after every recoverable entry has been re-admitted - so a persisted
// set larger than the CURRENTLY configured budget (e.g. settings were
// tightened since these files were written) is trimmed back to budget
// promptly, asynchronously, rather than left to whatever the next
// periodic tick happens to be.
//
// This function used to document a known, narrow race here: its own
// quarantine deletes were individually serialized against a concurrent
// live write via fisbCacheDiskMu, but the DECISION to quarantine a given
// file was made from content read moments earlier, outside that lock -
// so a live capture that persisted a fresh replacement for the exact
// same product in the narrow window between this loop reading a stale/
// corrupt file and deciding to quarantine it could have that fresh file
// wrongly removed. This is now fully closed:
// fisbCacheQuarantineIfStillWarranted re-reads and re-classifies each
// file's CURRENT content, against its OWN freshly-fetched now/nowMono
// (never this scan's own, by-then-stale nowUTC/nowMono above - see that
// function's own doc comment for why reusing a stale reference time
// would silently reintroduce this exact race under a different guise),
// immediately before removing it, while holding fisbCacheDiskMu the
// entire time - the same lock a concurrent fisbCachePersist for that
// exact path also requires, so whichever side wins the race for the lock
// completes entirely before the other starts, and this function's own
// re-check can never observe a state a concurrent write is still in the
// middle of changing. See
// TestFISBCacheStartupRecovery_QuarantineNeverDestroysAConcurrentReplacement.
func fisbCacheStartupRecovery() {
	deadline := time.Now().Add(fisbCacheStartupRecoveryTimeout)
	for !fisbCacheTrustedTimeState() {
		if time.Now().After(deadline) {
			log.Printf("fisbcache: trusted time not available within %s of startup - skipping cross-reboot recovery this boot\n", fisbCacheStartupRecoveryTimeout)
			fisbCacheMu.Lock()
			fisbCacheStartupRecovered = true
			fisbCacheMu.Unlock()
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	entries, err := fisbCacheFS.ReadDir(fisbCacheDir)
	if err != nil {
		log.Printf("fisbcache: could not read cache directory for recovery: %s\n", err)
		fisbCacheMu.Lock()
		fisbCacheRecoveryError = true
		fisbCacheStartupRecovered = true
		fisbCacheMu.Unlock()
		return
	}

	plan := storagelifecycle.PlanRecovery(fisbCacheNamespace, entries, storagelifecycle.RecoveryOptions{
		NowMonotonic:   monotonicSeconds(),
		AllowPromotion: false, // this feature never trusts an unvalidated interrupted write - see PlanRecovery's own doc comment
	})
	results := storagelifecycle.ExecuteRecovery(fisbCacheFS, plan)
	for _, r := range results {
		if r.Error != nil {
			log.Printf("fisbcache: recovery of %s: %s\n", r.Item.TempName, r.Error)
		}
	}

	nowUTC := fisbCacheTrustedNowUTC()
	nowMono := monotonicSeconds()
	nonFatal := false
	recovered := 0
	for _, e := range entries {
		if !e.IsRegular || e.IsSymlink {
			continue
		}
		path := fisbCacheNamespace.Root + "/" + e.Name
		raw, err := fisbReadFileBounded(path)
		if err != nil {
			nonFatal = true
			continue
		}
		decision := fisbCacheClassifyRecoveryFile(raw, nowUTC, nowMono)
		switch decision.reason {
		case fisbCacheRecoveryOK:
			if fisbCacheStore.Admit(decision.entry) == fisbcache.AdmitAccepted {
				recovered++
			}
		case fisbCacheRecoveryCorrupt:
			// Invalid per DecodePersistedEntry's own strict validation -
			// quarantine by removing only this exact file (never
			// anything else in this namespace).
			fisbCacheQuarantineIfStillWarranted(path)
			nonFatal = true
		case fisbCacheRecoveryUnknownAge:
			// Age can never be proven for an entry that was never
			// recorded with a trusted receive time - expire
			// conservatively rather than guess (see
			// docs/fisb-weather-cache.md).
			fisbCacheQuarantineIfStillWarranted(path)
		case fisbCacheRecoveryFutureDated:
			// A recorded receive time in the future relative to the
			// current trusted clock is never trustworthy either.
			fisbCacheQuarantineIfStillWarranted(path)
			nonFatal = true
		case fisbCacheRecoveryExpired:
			fisbCacheQuarantineIfStillWarranted(path)
		}
	}

	// Bound recovery's own contribution to the configured budget
	// promptly - see this function's own doc comment.
	fisbCacheRequestCleanup()

	log.Printf("fisbcache: startup recovery complete (%d entries re-admitted)\n", recovered)
	fisbCacheMu.Lock()
	fisbCacheStartupRecovered = true
	fisbCacheNonFatalErrors = nonFatal
	fisbCacheMu.Unlock()
}

// fisbCacheRecoveryReason names why fisbCacheClassifyRecoveryFile did (or
// did not) recommend quarantining a persisted file - kept distinct from
// a plain bool so callers can preserve the exact nonFatal-tracking
// distinctions fisbCacheStartupRecovery's own state machine always has
// (a corrupt or future-dated file counts as a non-fatal recovery error;
// an honestly-unknown-age or genuinely-expired one does not - it is
// simply, unremarkably, not kept).
type fisbCacheRecoveryReason int

const (
	fisbCacheRecoveryOK fisbCacheRecoveryReason = iota
	fisbCacheRecoveryCorrupt
	fisbCacheRecoveryUnknownAge
	fisbCacheRecoveryFutureDated
	fisbCacheRecoveryExpired
)

// fisbCacheRecoveryDecision is one file's recovery classification, as
// computed by fisbCacheClassifyRecoveryFile - identical logic whether
// this is the FIRST pass (fisbCacheStartupRecovery's own loop, from
// content read once, outside fisbCacheDiskMu) or the RE-CHECK
// immediately before a quarantine delete
// (fisbCacheQuarantineIfStillWarranted, from a FRESH read, inside
// fisbCacheDiskMu) - using the exact same function for both is what
// guarantees the re-check can never disagree with the original decision
// for a reason THIS function's own logic could introduce, only because
// the file's real content genuinely changed in between.
type fisbCacheRecoveryDecision struct {
	reason fisbCacheRecoveryReason
	entry  fisbcache.Entry // meaningful only when reason == fisbCacheRecoveryOK
}

// fisbCacheClassifyRecoveryFile is fisbCacheStartupRecovery's entire
// per-file decision, pure and side-effect-free: decode, then age/
// freshness checks, in the exact order and with the exact bridged-
// monotonic-time computation fisbCacheStartupRecovery's own loop always
// used. Factored out so it can be run TWICE against the SAME file - once
// cheaply while scanning (outside any lock), and once more, right before
// an actual delete, while holding fisbCacheDiskMu - see
// fisbCacheQuarantineIfStillWarranted.
func fisbCacheClassifyRecoveryFile(raw []byte, nowUTC time.Time, nowMono float64) fisbCacheRecoveryDecision {
	entry, _, err := fisbcache.DecodePersistedEntry(raw, nowUTC)
	if err != nil {
		return fisbCacheRecoveryDecision{reason: fisbCacheRecoveryCorrupt}
	}
	if entry.ReceivedAtUTC.IsZero() {
		return fisbCacheRecoveryDecision{reason: fisbCacheRecoveryUnknownAge}
	}
	age := nowUTC.Sub(entry.ReceivedAtUTC)
	if age < 0 {
		return fisbCacheRecoveryDecision{reason: fisbCacheRecoveryFutureDated}
	}
	entry.ReceivedAtMonotonic = nowMono - age.Seconds()
	if fisbcache.Freshness(entry, fisbcache.PolicyFor(entry.Key), nowMono) == fisbcache.FreshnessExpired {
		return fisbCacheRecoveryDecision{reason: fisbCacheRecoveryExpired}
	}
	return fisbCacheRecoveryDecision{reason: fisbCacheRecoveryOK, entry: entry}
}

// fisbCacheQuarantineIfStillWarranted deletes path ONLY if a FRESH read
// of its CURRENT content, re-classified from scratch
// (fisbCacheClassifyRecoveryFile) while holding fisbCacheDiskMu, is
// STILL quarantine-worthy - this is what fully closes the recovery race
// fisbCacheStartupRecovery's own doc comment used to only document as a
// known, narrow limitation: a live capture that persists a fresh,
// legitimate replacement for the exact same product in the window
// between the original (stale, outside-the-lock) read and this call
// must never have that fresh file destroyed just because the STALE read
// that triggered this call happened to look corrupt/expired/
// future-dated. A read failure (the file is already gone, or newly
// unreadable for some other reason) is treated as "nothing left here to
// wrongly destroy," not as a reason to error - matching this call site's
// original, always-best-effort behavior.
//
// Deliberately fetches its OWN fresh nowUTC/nowMono here, rather than
// reusing the caller's (necessarily older, possibly much older across a
// slow directory scan) values: a genuinely fresh concurrent write's own
// ReceivedAtUTC is set from real wall-clock time at the moment it was
// captured, which can only ever be AT OR AFTER whatever nowUTC recovery's
// scan captured before that write happened - reusing the scan's frozen,
// now-stale nowUTC here would make the classifier see that fresh entry's
// receive time as being in ITS OWN future and misclassify it as
// future-dated, defeating this entire re-check's purpose. If trusted
// time is not currently available (a pathological, narrow case: time
// trust degraded in the brief window since recovery's own earlier wait
// already confirmed it), this conservatively skips the delete entirely
// rather than risk classifying against an untrustworthy nowUTC - a later
// pass (the next periodic cleanup tick, or a future boot's own recovery)
// will reconsider this file once trusted time is available again.
func fisbCacheQuarantineIfStillWarranted(path string) {
	fisbCacheDiskMu.Lock()
	defer fisbCacheDiskMu.Unlock()

	raw, err := fisbReadFileBounded(path)
	if err != nil {
		return
	}
	nowUTC := fisbCacheTrustedNowUTC()
	if nowUTC.IsZero() {
		return
	}
	nowMono := monotonicSeconds()
	if fisbCacheClassifyRecoveryFile(raw, nowUTC, nowMono).reason != fisbCacheRecoveryOK {
		_ = fisbCacheFS.Remove(path)
	}
	// else: a concurrent write replaced this file with legitimate
	// content since the earlier, stale read that triggered this call -
	// leave it alone entirely, exactly as if this call had never
	// happened.
}

// fisbCacheEvictKeyIfUnchanged deletes k's persisted file and its Store
// entry, but ONLY if the Store still holds exactly expected for k right
// before the file is removed - see fisbCacheDiskMu's own doc comment for
// why this check-then-delete must run as one atomic unit against a
// concurrent fisbCachePersist for the same key. Reports whether it
// actually deleted anything (false, nil error, means the plan that named
// k is stale - a concurrent admit changed or removed k since it was
// planned; a later pass will reconsider with fresh data, exactly as
// intended - see fisbCacheRunRetention).
func fisbCacheEvictKeyIfUnchanged(k fisbcache.Key, expected fisbcache.Entry) (deleted bool, err error) {
	fisbCacheDiskMu.Lock()
	defer fisbCacheDiskMu.Unlock()

	cur, ok := fisbCacheStore.Get(k)
	if !ok || cur != expected {
		return false, nil
	}
	n, errs := fisbCacheExecuteEviction([]fisbcache.Key{k})
	if len(errs) > 0 {
		// A missing file is not a failure to remove k from committed
		// state - there is nothing left on disk to protect, and the
		// Store index must still be corrected, or eviction of this key
		// would be permanently impossible whenever no file exists for
		// it: persistence disabled (in-memory-only mode never writes a
		// file at all), or - the case that surfaces this specifically -
		// this exact entry was just Admitted this same worker iteration
		// and this call is running (as fisbCacheRunRetention's own
		// post-Admit backstop always does) BEFORE that admission's own
		// fisbCachePersist has had a chance to run. Any OTHER error
		// (permission denied, I/O failure, a SafeJoin rejection, ...)
		// still refuses to touch the Store, rather than risk an
		// inconsistent claim about what actually happened.
		for _, e := range errs {
			if !os.IsNotExist(e) {
				return false, e
			}
		}
	} else if n != 1 {
		return false, nil
	}
	// Do NOT assume this succeeds just because the Get above matched:
	// fisbCacheDiskMu (held for this whole function) excludes a
	// concurrent fisbCachePersist WRITE for k, but Store.Admit is
	// deliberately NOT gated by fisbCacheDiskMu (see that lock's own
	// doc comment) - a live capture that supersedes k with a genuinely
	// fresher copy of the SAME real-world product can still run its
	// Admit() in the gap between the Get above and here. When that
	// happens, DeleteIfUnchanged correctly refuses (the Store no longer
	// holds `expected`) and this function must report that refusal
	// faithfully, not claim success - the file is gone (a stale copy,
	// harmless to have removed) but the Store's fresher in-memory entry
	// for k must survive untouched.
	if !fisbCacheStore.DeleteIfUnchanged(k, expected) {
		return false, nil
	}
	return true, nil
}

// fisbCacheRunRetention is this feature's one budget/retention
// enforcement pass - plan (fisbcache.PlanEviction, pure, against a single
// Snapshot) then execute (fisbCacheEvictKeyIfUnchanged, per key, safe
// against anything that changed since that snapshot was taken). This is
// the ONLY function in this feature that performs disk deletion for
// capacity/retention reasons, and fisbCacheCleanupWorker is its ONLY
// caller - every other site that used to call this directly (the
// capture path's own post-admission check, fisbCacheStartupRecovery,
// handleSetFISBCacheSettingsRequest) now calls fisbCacheRequestCleanup
// instead, which only ever signals this same worker asynchronously; see
// fisbCacheCleanupWorker's own doc comment for why this guarantees at
// most one pass ever runs at a time, and never on the live capture
// goroutine.
func fisbCacheRunRetention() {
	fisbCacheMu.Lock()
	settings := fisbCacheSettingsCache
	fisbCacheMu.Unlock()

	snap := fisbCacheStore.Snapshot()
	now := monotonicSeconds()
	keys := fisbcache.PlanEviction(snap, settings.MaxCacheBytes, settings.MaxEntries, now)
	if len(keys) == 0 {
		return
	}
	deleted := 0
	for _, k := range keys {
		ok, err := fisbCacheEvictKeyIfUnchanged(k, snap[k])
		if err != nil {
			log.Printf("fisbcache: retention: %s\n", err)
			continue
		}
		if ok {
			deleted++
		}
	}
	if deleted == 0 {
		return
	}
	fisbCacheMu.Lock()
	fisbCacheLastCleanupUTC = fisbCacheTrustedNowUTC()
	fisbCacheLastCleanupCount = deleted
	fisbCacheMu.Unlock()
}
