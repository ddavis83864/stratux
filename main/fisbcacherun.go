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

// fisbCaptureQueueDepth bounds the capture path's own queue - generous
// for this project's own observed 978 uplink rate, small enough that a
// stalled/slow disk can never accumulate unbounded memory. Once full,
// new captures are dropped (see fisbCacheDroppedWrites), never blocking
// the caller.
const fisbCaptureQueueDepth = 256

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
	fisbCacheMu               sync.Mutex
	fisbCacheStore            *fisbcache.Store
	fisbCacheSettingsCache    FISBCacheSettings
	fisbCacheQueue            chan fisbCaptureItem
	fisbCacheDroppedWrites    uint64 // atomic
	fisbCachePressureRejected uint64 // atomic
	fisbCacheStartupRecovered bool
	fisbCacheRecoveryError    bool
	fisbCacheNonFatalErrors   bool
	fisbCacheLastCleanupUTC   time.Time
	fisbCacheLastCleanupCount int
	fisbCacheShuttingDown     bool
)

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
	fisbCacheQueue = make(chan fisbCaptureItem, fisbCaptureQueueDepth)
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
	go fisbCacheRetentionLoop()

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

func fisbCacheEnqueue(key fisbcache.Key, ft fisbcache.FISBTime, payload string) {
	fisbCacheMu.Lock()
	shuttingDown := fisbCacheShuttingDown
	fisbCacheMu.Unlock()
	if shuttingDown {
		return
	}
	// Global storage pressure (HIGH/CRITICAL/UNKNOWN) prohibits admitting
	// any new entry - never a second, this-cache-only pressure
	// computation; see fisbCacheStoragePressureProhibited's own doc
	// comment. Already-cached entries are still served; this only stops
	// growing the cache further while storage is genuinely tight or its
	// state cannot currently be confirmed. Counted separately from
	// fisbCacheDroppedWrites (queue overflow) so an operator can tell
	// "backlogged" apart from "storage is under pressure" at a glance.
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
	select {
	case fisbCacheQueue <- item:
	default:
		atomic.AddUint64(&fisbCacheDroppedWrites, 1)
	}
}

// fisbCacheCaptureWorker is this feature's one dedicated goroutine for
// admitting queued captures into the Store and, when persistence is
// enabled, writing them to disk - mirrors autorecordrun.go's own
// dedicated-goroutine-never-blocks-the-decoder pattern. A single slow or
// failing write here only ever delays this goroutine's own next queue
// read, never the capture-path caller (fisbCaptureText/fisbCaptureNexrad
// already returned by the time this runs).
func fisbCacheCaptureWorker() {
	for item := range fisbCacheQueue {
		result := fisbCacheStore.Admit(item.entry)
		if result != fisbcache.AdmitAccepted && result != fisbcache.AdmitSuperseded {
			continue
		}
		fisbCacheMu.Lock()
		persistenceEnabled := fisbCacheSettingsCache.Enabled && fisbCacheSettingsCache.PersistenceEnabled
		fisbCacheMu.Unlock()
		if !persistenceEnabled {
			continue
		}
		if err := fisbCachePersist(item.entry, item.payload); err != nil {
			log.Printf("fisbcache: could not persist entry: %s\n", err)
		}
	}
}

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
		raw, err := fisbReadFileBounded(fisbCacheNamespace.Root + "/" + e.Name)
		if err != nil {
			nonFatal = true
			continue
		}
		entry, _, err := fisbcache.DecodePersistedEntry(raw, nowUTC)
		if err != nil {
			// Invalid/corrupt/future-dated per DecodePersistedEntry's own
			// strict validation - quarantine by removing only this exact
			// file (never anything else in this namespace).
			_ = fisbCacheFS.Remove(fisbCacheNamespace.Root + "/" + e.Name)
			nonFatal = true
			continue
		}
		if entry.ReceivedAtUTC.IsZero() {
			// Age can never be proven for an entry that was never recorded
			// with a trusted receive time - expire conservatively rather
			// than guess (see docs/fisb-weather-cache.md).
			_ = fisbCacheFS.Remove(fisbCacheNamespace.Root + "/" + e.Name)
			continue
		}
		age := nowUTC.Sub(entry.ReceivedAtUTC)
		if age < 0 {
			// A recorded receive time in the future relative to the
			// current trusted clock is never trustworthy either.
			_ = fisbCacheFS.Remove(fisbCacheNamespace.Root + "/" + e.Name)
			nonFatal = true
			continue
		}
		entry.ReceivedAtMonotonic = nowMono - age.Seconds()
		if fisbcache.Freshness(entry, fisbcache.PolicyFor(entry.Key), nowMono) == fisbcache.FreshnessExpired {
			_ = fisbCacheFS.Remove(fisbCacheNamespace.Root + "/" + e.Name)
			continue
		}
		if fisbCacheStore.Admit(entry) == fisbcache.AdmitAccepted {
			recovered++
		}
	}

	log.Printf("fisbcache: startup recovery complete (%d entries re-admitted)\n", recovered)
	fisbCacheMu.Lock()
	fisbCacheStartupRecovered = true
	fisbCacheNonFatalErrors = nonFatal
	fisbCacheMu.Unlock()
}

// --- Retention ---------------------------------------------------------

// fisbCacheRetentionLoop periodically evicts expired and, if over budget,
// oldest-received entries - see fisbcache.PlanEviction. A scan/execution
// failure never panics or stops this loop, matching this project's other
// periodic-loop failure-isolation convention (e.g.
// storageLifecycleUpdateLoop).
func fisbCacheRetentionLoop() {
	ticker := time.NewTicker(fisbCacheRetentionInterval)
	defer ticker.Stop()
	for range ticker.C {
		fisbCacheRunRetention()
	}
}

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
	deleted, errs := fisbCacheExecuteEviction(keys)
	for _, err := range errs {
		log.Printf("fisbcache: retention: %s\n", err)
	}
	for _, k := range keys {
		fisbCacheStore.Delete(k)
	}
	fisbCacheMu.Lock()
	fisbCacheLastCleanupUTC = fisbCacheTrustedNowUTC()
	fisbCacheLastCleanupCount = deleted
	fisbCacheMu.Unlock()
}
