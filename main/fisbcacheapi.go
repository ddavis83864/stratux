/*
fisbcacheapi.go: HTTP endpoints for the Rolling FIS-B Weather Cache -
status, settings, and a two-step confirmed purge. See
docs/fisb-weather-cache.md's "APIs" section.

Endpoints:

	GET  /getFISBCacheStatus    - operational state, counters, freshness
	                              breakdown.
	GET  /getFISBCacheInventory - per-entry summary (identity/freshness/
	                              age only - never raw payload content).
	POST /setFISBCacheSettings  - validate, persist, and apply new
	                              settings.
	POST /prepareFISBCachePurge - step one of the two-step confirmed
	                              purge flow.
	POST /confirmFISBCachePurge - step two: actually deletes every
	                              currently-cached entry (this
	                              namespace only).
	POST /cancelFISBCachePurge  - explicitly discards a pending purge
	                              token before it expires.
*/
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/readiness"
)

// newFISBCachePurgeToken mirrors newShutdownToken's own crypto/rand-based
// pattern (main/powerapi.go), with this feature's own distinct prefix -
// not reused directly, since that function's own "shutdown-" prefix is
// specific to the controlled-shutdown flow and panics (rather than
// returning an error) on a crypto/rand failure, which this endpoint
// prefers to report as an ordinary HTTP error instead.
func newFISBCachePurgeToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "fisbpurge-" + hex.EncodeToString(b[:]), nil
}

// maxFISBCacheRequestBytes mirrors maxAutoRecordRequestBytes - Settings
// is a small, flat struct; there is no legitimate reason for a request
// body anywhere near this large.
const maxFISBCacheRequestBytes = 4096

// --- status ------------------------------------------------------------

type fisbCacheStatusResponse struct {
	State              string                           `json:"state"`
	Enabled            bool                             `json:"enabled"`
	PersistenceEnabled bool                             `json:"persistenceEnabled"`
	TrustedTime        bool                             `json:"trustedTime"`
	TotalEntries       int                              `json:"totalEntries"`
	TotalBytes         int64                            `json:"totalBytes"`
	MaxCacheBytes      int64                            `json:"maxCacheBytes"`
	MaxEntries         int                              `json:"maxEntries"`
	ByFreshness        map[fisbcache.FreshnessState]int `json:"byFreshness"`
	ByProductClass     map[fisbcache.ProductClass]int   `json:"byProductClass"`
	QueueDepth         int                              `json:"queueDepth"`
	QueueCapacity      int                              `json:"queueCapacity"`
	InFlightEntries    int                              `json:"inFlightEntries"`
	ReservedBytes      int64                            `json:"reservedBytes"`
	ReservedEntries    int                              `json:"reservedEntries"`
	ProjectedBytes     int64                            `json:"projectedBytes"`
	ProjectedEntries   int                              `json:"projectedEntries"`
	DroppedWrites      uint64                           `json:"droppedWrites"`
	PressureRejected   uint64                           `json:"pressureRejected"`
	OversizedRejected  uint64                           `json:"oversizedRejected"`
	CapacityRejected   uint64                           `json:"capacityRejected"`
	LastCleanupUTC     time.Time                        `json:"lastCleanupUtc,omitempty"`
	LastCleanupCount   int                              `json:"lastCleanupCount"`
	StoragePressure    string                           `json:"storagePressure"`
	Notes              []string                         `json:"notes"`
}

func fisbCacheNotes() []string {
	return []string{
		"Weather is supplemental and may be incomplete or unavailable.",
		"Cached weather can be older than currently broadcast data - always check the product's own timestamp and age before use.",
		"The absence of a cached product does not indicate the absence of a hazard.",
		"This is never a substitute for an official preflight briefing or current airborne weather sources.",
	}
}

func handleGetFISBCacheStatusRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(fisbCacheStatusSnapshot())
}

func fisbCacheStatusSnapshot() fisbCacheStatusResponse {
	fisbCacheMu.Lock()
	settings := fisbCacheSettingsCache
	recovered := fisbCacheStartupRecovered
	recoveryErr := fisbCacheRecoveryError
	nonFatal := fisbCacheNonFatalErrors
	lastCleanupUTC := fisbCacheLastCleanupUTC
	lastCleanupCount := fisbCacheLastCleanupCount
	shuttingDown := fisbCacheShuttingDown
	fisbCacheMu.Unlock()

	pressure, pressureProhibited := fisbCacheStoragePressureProhibited()

	state := fisbcache.DetermineState(fisbcache.StateInputs{
		Enabled:                   settings.Enabled,
		StartupRecoveryComplete:   recovered,
		TrustedTime:               fisbCacheTrustedTimeState(),
		RecoveryError:             recoveryErr,
		StoragePressureProhibited: pressureProhibited,
		ReadOnly:                  shuttingDown,
		HasNonFatalErrors:         nonFatal,
	})

	var snap map[fisbcache.Key]fisbcache.Entry
	if fisbCacheStore != nil {
		snap = fisbCacheStore.Snapshot()
	}
	stats := fisbcache.ComputeStats(snap, monotonicSeconds())

	queueDepth, inFlightEntries := 0, 0
	var reservedBytes int64
	reservedEntries := 0
	projectedBytes, projectedEntries := stats.TotalBytes, stats.TotalEntries
	if fisbCachePending != nil {
		var queuedBytes, inFlightBytes int64
		queueDepth, inFlightEntries, queuedBytes, inFlightBytes = fisbCachePending.stats()
		reservedBytes = queuedBytes + inFlightBytes
		reservedEntries = queueDepth + inFlightEntries
		// A jointly-consistent read (not the separate `snap` above,
		// which is a plain committed-only snapshot) - see
		// fisbCacheProjectedSnapshotAtomic's own doc comment for why
		// this matters specifically for these two fields.
		projectedBytes, projectedEntries = fisbCacheProjectedSnapshotAtomic(fisbCachePending)
	}

	return fisbCacheStatusResponse{
		State:              string(state),
		Enabled:            settings.Enabled,
		PersistenceEnabled: settings.PersistenceEnabled,
		TrustedTime:        fisbCacheTrustedTimeState(),
		TotalEntries:       stats.TotalEntries,
		TotalBytes:         stats.TotalBytes,
		MaxCacheBytes:      settings.MaxCacheBytes,
		MaxEntries:         settings.MaxEntries,
		ByFreshness:        stats.ByFreshness,
		ByProductClass:     stats.ByProductClass,
		QueueDepth:         queueDepth,
		QueueCapacity:      fisbCachePendingCapacity,
		InFlightEntries:    inFlightEntries,
		ReservedBytes:      reservedBytes,
		ReservedEntries:    reservedEntries,
		ProjectedBytes:     projectedBytes,
		ProjectedEntries:   projectedEntries,
		DroppedWrites:      fisbCacheDroppedWrites,
		PressureRejected:   fisbCachePressureRejected,
		OversizedRejected:  fisbCacheOversizedRejected,
		CapacityRejected:   fisbCacheCapacityRejected,
		LastCleanupUTC:     lastCleanupUTC,
		LastCleanupCount:   lastCleanupCount,
		StoragePressure:    pressure,
		Notes:              fisbCacheNotes(),
	}
}

// --- inventory -----------------------------------------------------

type fisbCacheInventoryItem struct {
	ProductClass  string  `json:"productClass"`
	Identity      string  `json:"identity"`
	Freshness     string  `json:"freshness"`
	AgeSeconds    float64 `json:"ageSeconds"`
	SizeBytes     int64   `json:"sizeBytes"`
	SourceTrusted bool    `json:"sourceTrusted"`
}

// handleGetFISBCacheInventoryRequest never returns raw payload content -
// identity/freshness/age/size only, matching this feature's own "no raw
// payload download unless justified" requirement (nothing in this
// mission justifies it: no replay path exists to need byte-for-byte
// verification via the API, and a station identifier plus a decoded
// METAR/TAF's own text is exactly the kind of content this project's
// existing /weather websocket already displays live - see
// docs/fisb-weather-cache.md's privacy-considerations section for why
// this endpoint intentionally does not duplicate that).
func handleGetFISBCacheInventoryRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	if fisbCacheStore == nil {
		json.NewEncoder(w).Encode([]fisbCacheInventoryItem{})
		return
	}
	now := monotonicSeconds()
	snap := fisbCacheStore.Snapshot()
	out := make([]fisbCacheInventoryItem, 0, len(snap))
	for k, e := range snap {
		out = append(out, fisbCacheInventoryItem{
			ProductClass:  string(k.Class),
			Identity:      k.Identity,
			Freshness:     string(fisbcache.Freshness(e, fisbcache.PolicyFor(k), now)),
			AgeSeconds:    e.Age(now).Seconds(),
			SizeBytes:     e.SizeBytes,
			SourceTrusted: e.Source.Trusted,
		})
	}
	json.NewEncoder(w).Encode(out)
}

// --- settings --------------------------------------------------------

func handleGetFISBCacheSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	json.NewEncoder(w).Encode(loadFISBCacheSettings())
}

// fisbCacheDecodeStrictJSON decodes exactly one JSON value from body into
// v (unknown fields already rejected by the caller's own json.Decoder
// options) and additionally rejects trailing content after that value -
// a bare Decode call only consumes the first JSON value in the stream
// and silently ignores anything after it (a second concatenated object,
// or trailing garbage), which would otherwise let a malformed or
// malicious multi-value body appear to succeed. Returns the first
// decode error verbatim, or a distinct "trailing data" error.
func fisbCacheDecodeStrictJSON(dec *json.Decoder, v interface{}) error {
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("request body must contain exactly one JSON value")
	}
	return nil
}

func handleSetFISBCacheSettingsRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFISBCacheRequestBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var s FISBCacheSettings
	if err := fisbCacheDecodeStrictJSON(dec, &s); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
		return
	}
	s.SchemaVersion = FISBCacheSettingsSchemaVersion
	if err := s.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if err := saveFISBCacheSettings(s); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	fisbCacheMu.Lock()
	fisbCacheSettingsCache = s
	fisbCacheMu.Unlock()
	// A tightened budget (lower maxCacheBytes/maxEntries) must take effect
	// immediately, not up to fisbCacheRetentionInterval later - run the
	// same enforcement pass every admission already runs synchronously.
	// Harmless, cheap no-op when the new settings are not actually
	// tighter than what is currently cached (fisbCacheRunRetention's own
	// PlanEviction is a pure function of the current Snapshot - see its
	// own doc comment).
	if fisbCacheStore != nil {
		fisbCacheRunRetention()
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "settings": s})
}

// --- purge (two-step confirmed) ---------------------------------------

// fisbCachePurgeTokenTTL bounds how long a prepared purge token remains
// valid - short enough that a stale, forgotten token can never be
// accidentally confirmed long after the operator moved on, generous
// enough for a real confirm click.
const fisbCachePurgeTokenTTL = 5 * time.Minute

var (
	fisbPurgeMu    sync.Mutex
	fisbPurgeToken string
	fisbPurgeUntil time.Time
	fisbPurgeUsed  bool
)

func handlePrepareFISBCachePurgeRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	tok, err := newFISBCachePurgeToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "error": "could not generate a confirmation token"})
		return
	}
	fisbPurgeMu.Lock()
	fisbPurgeToken = tok
	fisbPurgeUntil = time.Now().Add(fisbCachePurgeTokenTTL)
	fisbPurgeUsed = false
	token := fisbPurgeToken
	fisbPurgeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "token": token, "expiresInSeconds": int(fisbCachePurgeTokenTTL.Seconds())})
}

type fisbCachePurgeConfirmRequest struct {
	Token string `json:"token"`
}

// fisbCachePurgeUsingSnapshot deletes every key named in snap - this is
// the confirmed-purge handler's own plan-then-execute step, factored out
// so a test can supply a DELIBERATELY stale snap (one captured before a
// concurrent replacement) and prove the same guarantee
// fisbCacheEvictKeyIfUnchanged already proves for a single key: a purge
// planned against an earlier Snapshot must never destroy a key that was
// concurrently superseded with fresher content before this function's
// own per-key deletes run - see docs/fisb-weather-cache.md's "purge and
// retention concurrency" coverage. Per-key check-then-delete
// (fisbCacheEvictKeyIfUnchanged, the same primitive retention/
// reservation eviction uses), never a blind batch delete-then-
// Store.Delete.
func fisbCachePurgeUsingSnapshot(snap map[fisbcache.Key]fisbcache.Entry) (deleted, errCount int) {
	for k, e := range snap {
		ok, err := fisbCacheEvictKeyIfUnchanged(k, e)
		if err != nil {
			errCount++
			continue
		}
		if ok {
			deleted++
		}
	}
	return deleted, errCount
}

// handleConfirmFISBCachePurgeRequest deletes every currently-cached entry
// - this feature's cache-owned namespace only, never anything else on
// the persistent partition. Single-use: a repeated confirm with the same
// token is rejected (HTTP 410), matching this project's established
// confirmation-token contract (see configbackup.VerifyToken/
// power.ShutdownManager.Confirm's identical single-use semantics).
func handleConfirmFISBCachePurgeRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFISBCacheRequestBytes)
	var req fisbCachePurgeConfirmRequest
	if err := fisbCacheDecodeStrictJSON(json.NewDecoder(r.Body), &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "invalid JSON body: " + err.Error()})
		return
	}
	if req.Token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "error": "missing required field: token"})
		return
	}

	fisbPurgeMu.Lock()
	valid := req.Token == fisbPurgeToken && !fisbPurgeUsed && time.Now().Before(fisbPurgeUntil)
	if valid {
		fisbPurgeUsed = true
	}
	fisbPurgeMu.Unlock()
	if !valid {
		writeJSON(w, http.StatusGone, map[string]interface{}{"success": false, "error": "confirmation token not found, already used, or expired"})
		return
	}

	if fisbCacheStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"success": false, "error": "cache not initialized"})
		return
	}
	// Discard anything still queued (not yet dequeued by the capture
	// worker) FIRST, so a purge is never immediately, silently undone by
	// whatever was waiting behind it - see fisbPendingQueue.clear's own
	// doc comment for the narrow, documented exception (an item already
	// in-flight when this runs is left alone).
	if fisbCachePending != nil {
		fisbCachePending.clear()
	}
	deleted, errCount := fisbCachePurgeUsingSnapshot(fisbCacheStore.Snapshot())
	resp := map[string]interface{}{"success": true, "deletedCount": deleted}
	if errCount > 0 {
		resp["partial"] = true
		resp["errorCount"] = errCount
	}
	writeJSON(w, http.StatusOK, resp)
}

func handleCancelFISBCachePurgeRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	fisbPurgeMu.Lock()
	fisbPurgeToken = ""
	fisbPurgeUsed = true
	fisbPurgeMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})
}

// buildFISBCacheHealth adapts this feature's own status snapshot into
// readiness.FISBCacheHealth - see that type's doc comment for why this
// translation exists instead of readiness importing fisbcache directly.
func buildFISBCacheHealth() readiness.FISBCacheHealth {
	s := fisbCacheStatusSnapshot()
	return readiness.BuildFISBCacheHealth(s.Enabled, s.State, s.TotalEntries)
}

// fisbCacheDiagnosticsSummary returns a bounded, sanitized summary for
// readiness.DiagnosticBundle - counts/state/settings only, never a
// station identifier, raw payload, or exact coordinate.
func fisbCacheDiagnosticsSummary() interface{} {
	s := fisbCacheStatusSnapshot()
	return map[string]interface{}{
		"state":              s.State,
		"enabled":            s.Enabled,
		"persistenceEnabled": s.PersistenceEnabled,
		"trustedTime":        s.TrustedTime,
		"totalEntries":       s.TotalEntries,
		"totalBytes":         s.TotalBytes,
		"byFreshness":        s.ByFreshness,
		"byProductClass":     s.ByProductClass,
		"queueDepth":         s.QueueDepth,
		"inFlightEntries":    s.InFlightEntries,
		"reservedBytes":      s.ReservedBytes,
		"reservedEntries":    s.ReservedEntries,
		"projectedBytes":     s.ProjectedBytes,
		"projectedEntries":   s.ProjectedEntries,
		"droppedWrites":      s.DroppedWrites,
		"pressureRejected":   s.PressureRejected,
		"oversizedRejected":  s.OversizedRejected,
		"capacityRejected":   s.CapacityRejected,
		"lastCleanupCount":   s.LastCleanupCount,
		"schemaVersion":      fisbcache.SchemaVersion,
	}
}
