/*
fisbcachepayload.go: a single, read-only, best-effort endpoint that lets a
Web UI client recover the actual decoded content (raw METAR/TAF/PIREP/
text-report body, or a NEXRAD tile's raw intensity data) for one specific
entry the rolling FIS-B cache is currently tracking.

Why this exists: fisbcache.Store deliberately holds metadata only, never
the payload itself (see fisbcache/entry.go's and fisbcache/store.go's own
doc comments) - /getFISBCacheInventory therefore never returns raw
content, by design (see fisbcacheapi.go's doc comment on that handler).
That is the right choice for the cache's own bookkeeping, but it leaves
no way for a Weather-viewing page to show the actual text of a product
that arrived before that page connected to the live /weather websocket -
exactly the gap the Weather page (web/plates/js/weather.js) needs closed
to show cached, not just live, reports.

This endpoint closes that gap the narrowest way available: it reads back
the SAME on-disk persisted file fisbCachePersist already wrote (if
persistence is enabled and the file has not since been evicted/purged),
using the exact same deterministic filename derivation
(fisbCacheEntryFileName) and the exact same strict decoder
(fisbcache.DecodePersistedEntry) the startup-recovery path already uses -
no new storage format, no new in-memory retention, and no change to
fisbcache's own Store/eviction/persistence behavior.

Availability is inherently best-effort and is reported honestly, never
inferred as a failure of the underlying cache:
  - fisbcache disabled, or persistence disabled (the default): 404, a
    clear reason, no disk access attempted at all.
  - entry never persisted, already evicted/purged, or the identity does
    not name a currently-tracked entry: 404.
  - anything else read/decoded successfully: 200, with the payload text
    (or, for a NEXRAD tile, its base64 big-endian-uint16 intensity
    array, exactly as fisbEncodeNexradPayload produced it) plus the
    entry's own timestamps, so the client can show "as of" alongside the
    content it fetched.

GET /getFISBCachePayload?class=<productClass>&identity=<identity>
*/
package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"github.com/stratux/stratux/fisbcache"
)

// maxFISBCachePayloadIdentityLen bounds the query parameter, not any
// stored value - fisbcache.Key.Identity itself is already bounded far
// below this in practice (a station identifier or a NEXRAD tile's own
// encoded bounds string), so this is only defense against a pathological
// request, mirroring the spirit of maxFISBCacheRequestBytes above.
const maxFISBCachePayloadIdentityLen = 512

type fisbCachePayloadResponse struct {
	Success         bool       `json:"success"`
	ProductClass    string     `json:"productClass"`
	Identity        string     `json:"identity"`
	Payload         string     `json:"payload"`
	PayloadEncoding string     `json:"payloadEncoding"` // "text" or "base64-uint16be"
	SourceTimeUTC   *time.Time `json:"sourceTimeUtc,omitempty"`
	ReceivedAtUTC   *time.Time `json:"receivedAtUtc,omitempty"`
}

type fisbCachePayloadErrorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

func fisbCachePayloadError(w http.ResponseWriter, status int, reason string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(fisbCachePayloadErrorResponse{Success: false, Error: reason})
}

func handleGetFISBCachePayloadRequest(w http.ResponseWriter, r *http.Request) {
	setNoCache(w)
	setJSONHeaders(w)
	if r.Method != http.MethodGet {
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}

	class := r.URL.Query().Get("class")
	identity := r.URL.Query().Get("identity")
	if class == "" || identity == "" {
		fisbCachePayloadError(w, http.StatusBadRequest, "class and identity are both required")
		return
	}
	if len(identity) > maxFISBCachePayloadIdentityLen {
		fisbCachePayloadError(w, http.StatusBadRequest, "identity is too long")
		return
	}
	productClass := fisbcache.ProductClass(class)
	if productClass != fisbcache.ClassText && productClass != fisbcache.ClassNexradTile {
		// Not a hard requirement for reading a file back
		// (fisbcache.DecodePersistedEntry rejects an unrecognized class
		// on its own too, via PolicyFor), but rejecting here first gives
		// a precise, honest reason instead of a generic "not found" for
		// a request that could never have named a real cache entry.
		fisbCachePayloadError(w, http.StatusBadRequest, "unrecognized productClass")
		return
	}
	key := fisbcache.Key{Class: productClass, Identity: identity}

	fisbCacheMu.Lock()
	settings := fisbCacheSettingsCache
	fisbCacheMu.Unlock()
	if !settings.Enabled || !settings.PersistenceEnabled {
		fisbCachePayloadError(w, http.StatusNotFound, "persistence is disabled - cached entries retain no retrievable content")
		return
	}

	path := filepath.Join(fisbCacheDir, fisbCacheEntryFileName(key))
	raw, err := fisbReadFileBounded(path)
	if err != nil {
		fisbCachePayloadError(w, http.StatusNotFound, "no persisted content for this entry (never persisted, or since evicted/purged)")
		return
	}
	entry, payload, err := fisbcache.DecodePersistedEntry(raw, fisbCacheTrustedNowUTC())
	if err != nil {
		// The startup-recovery path treats this the same way (quarantine
		// and move on, never a hard error) - a corrupt/interrupted write
		// is exactly as "not available" to this endpoint as a missing
		// file, not a 500: nothing about THIS request went wrong.
		fisbCachePayloadError(w, http.StatusNotFound, "stored content for this entry could not be read back")
		return
	}

	encoding := "text"
	if key.Class == fisbcache.ClassNexradTile {
		encoding = "base64-uint16be"
	}
	resp := fisbCachePayloadResponse{
		Success:         true,
		ProductClass:    class,
		Identity:        identity,
		Payload:         payload,
		PayloadEncoding: encoding,
	}
	if entry.Source.Trusted {
		t := entry.Source.UTC
		resp.SourceTimeUTC = &t
	}
	if !entry.ReceivedAtUTC.IsZero() {
		t := entry.ReceivedAtUTC
		resp.ReceivedAtUTC = &t
	}
	json.NewEncoder(w).Encode(resp)
}
