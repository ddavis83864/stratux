/*
fisbcachestorage.go: this feature's implementation of
storagelifecycle.CacheLifecycle - see storagelifecycle/contracts.go's own
doc comment for the contract this implements, and
docs/fisb-weather-cache.md's "Storage Lifecycle integration" and
"Filesystem ownership" sections for the full design.

This file introduces no second whole-disk-pressure engine: admission
still gates on storageManager's own already-computed Pressure (see
fisbcacherun.go), and interrupted-write recovery reuses
storagelifecycle.PlanRecovery/ExecuteRecovery unmodified. Per-entry
retention (which of THIS cache's own tracked entries to remove when its
own configured byte/entry budget is exceeded) is pure logic in the
fisbcache package itself (fisbcache.PlanEviction) rather than re-derived
from a storagelifecycle.RetentionPlan - see that function's own doc
comment for why: this cache's in-memory Store snapshot already IS the
authoritative index of what is persisted, so no separate filesystem scan
is needed to decide what to evict.
*/
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
)

// fisbCacheNamespaceID identifies this feature's own dedicated,
// cache-owned namespace under the persistent-storage root - registered
// into storageManager's Registry directly inside initStorageLifecycle
// (main/storagelifecycleapi.go), exactly like calibration-profiles/
// diagnostics/recordings/exports already are, and confirmed not to
// collide with any of them or with the documented unmanaged entries
// (alert-settings.json, power-session.json, updates/, health/ - see
// docs/storage-lifecycle.md's ownership matrix).
const fisbCacheNamespaceID = "fisb-weather-cache"

var fisbCacheDir = PersistentDataPath + "/fisb-weather-cache"

// fisbCacheNamespace is a plain package-level var (not built inside a
// function) so main/storagelifecycleapi.go's initStorageLifecycle can
// reference it while constructing the Registry, before this feature's
// own initFISBCache has any reason to run.
var fisbCacheNamespace = storagelifecycle.Namespace{
	ID: fisbCacheNamespaceID, Root: fisbCacheDir,
	Criticality: storagelifecycle.CriticalityCache, ItemKind: storagelifecycle.ItemKindFile,
	AllowedExtensions: []string{".json"},
}

// fisbCacheAtomicWriter/fisbCacheFS are package-level so both the
// capture path and the retention/recovery path share one AtomicWriter
// instance (and its own cryptographically random temp-suffix source),
// matching storageManager's own single-instance pattern. Constructed by
// initFISBCache (fisbcacherun.go), nil until then.
var (
	fisbCacheFS           storagelifecycle.FS
	fisbCacheAtomicWriter *storagelifecycle.AtomicWriter
)

// fisbCacheStoragePressureProhibited reports the whole-system storage
// pressure Manager.Status() currently observes, and whether that
// pressure is severe enough to prohibit this cache from admitting any
// new entry - HIGH/CRITICAL (genuinely low on room) and UNKNOWN (the
// scan itself failed or has never completed) are all treated
// conservatively alike, mirroring storagelifecycle.RecordingSpaceDenied's
// own "treat unknown as denied, never as fine" precedent
// (autorecordstorage.go). A nil storageManager (not yet initialized)
// is reported as UNKNOWN/prohibited for the same reason - this cache
// must never admit before it can actually confirm there is room.
//
// This is the single source of truth for that classification - both
// the capture-path admission gate (fisbCacheEnqueue, fisbcacherun.go)
// and the status/dashboard snapshot (fisbCacheStatusSnapshot,
// fisbcacheapi.go) call this rather than each re-deriving their own
// copy of the HIGH/CRITICAL/UNKNOWN judgment, so the reported
// "PRESSURE_INHIBITED" state can never drift from what actually
// inhibited admission.
func fisbCacheStoragePressureProhibited() (pressure string, prohibited bool) {
	if storageManager == nil {
		return string(storagelifecycle.PressureUnknown), true
	}
	st := storageManager.Status()
	switch st.Pressure {
	case storagelifecycle.PressureHigh, storagelifecycle.PressureCritical, storagelifecycle.PressureUnknown:
		return string(st.Pressure), true
	default:
		return string(st.Pressure), false
	}
}

// fisbCacheEntryFileName derives a safe, single-segment, deterministic
// filename for k - never the raw Identity string (which can contain
// spaces and, for a station identifier or a NEXRAD tile's own encoded
// bounds, characters this package has no reason to trust as path-safe):
// SafeJoin already rejects a separator/traversal attempt outright, but
// this hash-based naming means a resulting file's name IS the entry's
// content-addressed identity, avoiding any encoding question entirely.
func fisbCacheEntryFileName(k fisbcache.Key) string {
	sum := sha256.Sum256([]byte(k.StorageProductType() + "\x00" + k.StorageProductID()))
	return hex.EncodeToString(sum[:]) + ".json"
}

// fisbCacheLifecycle is the one production implementation of
// storagelifecycle.CacheLifecycle.
type fisbCacheLifecycle struct{}

var _ storagelifecycle.CacheLifecycle = fisbCacheLifecycle{}

func (fisbCacheLifecycle) ReplaceProduct(meta storagelifecycle.CacheEntryMetadata, write storagelifecycle.WriteFunc) error {
	if fisbCacheAtomicWriter == nil {
		return fmt.Errorf("fisbcache: storage not initialized")
	}
	name := fisbCacheEntryFileNameFromMeta(meta)
	_, err := fisbCacheAtomicWriter.Write(context.Background(), storagelifecycle.AtomicWriteOptions{
		Namespace: fisbCacheNamespace,
		Name:      name,
		Mode:      0o644,
		Write:     write,
		Validate:  fisbCacheValidateEntryFile,
	})
	return err
}

func (fisbCacheLifecycle) IsExpired(meta storagelifecycle.CacheEntryMetadata, nowMonotonic float64) bool {
	return storagelifecycle.IsCacheEntryExpired(meta, nowMonotonic)
}

func (fisbCacheLifecycle) ReclaimPriority() storagelifecycle.Criticality {
	return storagelifecycle.CriticalityCache
}

// fisbCacheEntryFileNameFromMeta adapts a
// storagelifecycle.CacheEntryMetadata's opaque Key back into this
// feature's own fisbcache.Key just long enough to derive its filename -
// CacheProductKey and fisbcache.Key carry exactly the same two strings
// by construction (see fisbcache.Key.StorageProductType/StorageProductID),
// so this is a lossless adaptation, not a guess.
func fisbCacheEntryFileNameFromMeta(meta storagelifecycle.CacheEntryMetadata) string {
	return fisbCacheEntryFileName(fisbcache.Key{
		Class:    fisbcache.ProductClass(meta.Key.ProductType),
		Identity: meta.Key.ProductID,
	})
}

// fisbCacheValidateEntryFile is AtomicWriter's Validate step - it must
// parse as this feature's own schema before ever being committed, so a
// programming error in the capture path can never leave a corrupt file
// where a reader would find it.
func fisbCacheValidateEntryFile(tempPath string) error {
	raw, err := fisbReadFileBounded(tempPath)
	if err != nil {
		return err
	}
	_, _, err = fisbcache.DecodePersistedEntry(raw, fisbCacheTrustedNowUTC())
	return err
}

// fisbCacheExecuteEviction deletes exactly the files named by keys -
// derived from fisbcache.PlanEviction's own pure decision (see
// fisbcacherun.go's fisbCacheRunRetention) - never anything else in this
// or any other namespace.
func fisbCacheExecuteEviction(keys []fisbcache.Key) (deleted int, errs []error) {
	if fisbCacheFS == nil {
		return 0, []error{fmt.Errorf("fisbcache: storage not initialized")}
	}
	for _, k := range keys {
		path, err := storagelifecycle.SafeJoin(fisbCacheNamespace.Root, fisbCacheEntryFileName(k))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := fisbCacheFS.Remove(path); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
	}
	return deleted, errs
}
