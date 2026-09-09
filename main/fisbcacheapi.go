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
	DroppedWrites      uint64                           `json:"droppedWrites"`
	PressureRejected   uint64                           `json:"pressureRejected"`
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

	queueDepth := 0
	if fisbCacheQueue != nil {
		queueDepth = len(fisbCacheQueue)
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
		QueueCapacity:      fisbCaptureQueueDepth,
		DroppedWrites:      fisbCacheDroppedWrites,
		PressureRejected:   fisbCachePressureRejected,
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
	if err := dec.Decode(&s); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	snap := fisbCacheStore.Snapshot()
	keys := make([]fisbcache.Key, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	deleted, errs := fisbCacheExecuteEviction(keys)
	for _, k := range keys {
		fisbCacheStore.Delete(k)
	}
	resp := map[string]interface{}{"success": true, "deletedCount": deleted}
	if len(errs) > 0 {
		resp["partial"] = true
		resp["errorCount"] = len(errs)
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
		"droppedWrites":      s.DroppedWrites,
		"pressureRejected":   s.PressureRejected,
		"lastCleanupCount":   s.LastCleanupCount,
		"schemaVersion":      fisbcache.SchemaVersion,
	}
}
