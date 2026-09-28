/*
fisbcachepayload_test.go: tests for GET /getFISBCachePayload
(fisbcachepayload.go) - the Weather page's best-effort readback of one
cached entry's actual decoded content from its persisted file.
*/
package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stratux/stratux/fisbcache"
)

// fisbCacheDeleteEntryFile removes a key's persisted file directly,
// simulating eviction/purge without exercising the retention loop -
// isolates this test from that loop's own scheduling.
func fisbCacheDeleteEntryFile(t *testing.T, key fisbcache.Key) {
	t.Helper()
	path := filepath.Join(fisbCacheDir, fisbCacheEntryFileName(key))
	if err := os.Remove(path); err != nil {
		t.Fatalf("could not remove persisted file for test setup: %v", err)
	}
}

func fisbCachePayloadRequest(t *testing.T, class, identity string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	target := "/getFISBCachePayload"
	q := url.Values{}
	if class != "" {
		q.Set("class", class)
	}
	if identity != "" {
		q.Set("identity", identity)
	}
	if encoded := q.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	handleGetFISBCachePayloadRequest(rec, req)
	var body map[string]interface{}
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response was not valid JSON: %v (body: %s)", err, rec.Body.String())
		}
	}
	return rec, body
}

func TestHandleGetFISBCachePayload_WrongMethod(t *testing.T) {
	withFISBCacheTestEnv(t)
	req := httptest.NewRequest(http.MethodPost, "/getFISBCachePayload", nil)
	rec := httptest.NewRecorder()
	handleGetFISBCachePayloadRequest(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandleGetFISBCachePayload_MissingParams(t *testing.T) {
	withFISBCacheTestEnv(t)
	for _, tc := range []struct{ class, identity string }{
		{"", ""},
		{"text", ""},
		{"", "METAR KSEA"},
	} {
		rec, body := fisbCachePayloadRequest(t, tc.class, tc.identity)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("class=%q identity=%q: expected 400, got %d", tc.class, tc.identity, rec.Code)
		}
		if body["success"] != false {
			t.Errorf("class=%q identity=%q: expected success:false, got %v", tc.class, tc.identity, body)
		}
	}
}

func TestHandleGetFISBCachePayload_UnrecognizedClassRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	rec, body := fisbCachePayloadRequest(t, "not-a-real-class", "METAR KSEA")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %v)", rec.Code, body)
	}
}

func TestHandleGetFISBCachePayload_IdentityTooLongRejected(t *testing.T) {
	withFISBCacheTestEnv(t)
	huge := make([]byte, maxFISBCachePayloadIdentityLen+1)
	for i := range huge {
		huge[i] = 'x'
	}
	rec, body := fisbCachePayloadRequest(t, "text", string(huge))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %v)", rec.Code, body)
	}
}

func TestHandleGetFISBCachePayload_PersistenceDisabled(t *testing.T) {
	withFISBCacheTestEnv(t) // DefaultFISBCacheSettings(): disabled, persistence off
	rec, body := fisbCachePayloadRequest(t, "text", "METAR KSEA")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when persistence is disabled, got %d (body: %v)", rec.Code, body)
	}
	if body["success"] != false {
		t.Errorf("expected success:false, got %v", body)
	}
}

func TestHandleGetFISBCachePayload_NeverPersistedEntry(t *testing.T) {
	withFISBCacheTestEnv(t)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache.Enabled = true
	fisbCacheSettingsCache.PersistenceEnabled = true
	fisbCacheMu.Unlock()

	rec, body := fisbCachePayloadRequest(t, "text", "METAR KNEVERSEEN")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a never-persisted identity, got %d (body: %v)", rec.Code, body)
	}
}

// TestHandleGetFISBCachePayload_RealTextRoundTrip proves the endpoint
// returns exactly what was persisted, byte for byte, for a real text
// entry - the primary case the Weather page depends on to show a cached
// METAR's raw text after the page reconnects.
func TestHandleGetFISBCachePayload_RealTextRoundTrip(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache.Enabled = true
	fisbCacheSettingsCache.PersistenceEnabled = true
	fisbCacheMu.Unlock()

	key := makeFISBTestKey("KSEA")
	entry := fisbcache.Entry{
		Key:                 key,
		ReceivedAtMonotonic: monotonicSeconds(),
		ReceivedAtUTC:       fisbCacheTrustedNowUTC(),
		Source:              fisbcache.SourceTime{Trusted: true, UTC: fisbCacheTrustedNowUTC()},
	}
	const rawText = "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"
	if err := fisbCachePersist(entry, rawText); err != nil {
		t.Fatalf("fisbCachePersist: %v", err)
	}

	rec, body := fisbCachePayloadRequest(t, string(key.Class), key.Identity)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %v)", rec.Code, body)
	}
	if body["success"] != true {
		t.Fatalf("expected success:true, got %v", body)
	}
	if body["payload"] != rawText {
		t.Errorf("payload mismatch: got %q, want %q", body["payload"], rawText)
	}
	if body["payloadEncoding"] != "text" {
		t.Errorf("expected payloadEncoding \"text\", got %v", body["payloadEncoding"])
	}
	if body["receivedAtUtc"] == nil {
		t.Errorf("expected receivedAtUtc to be present for a trusted-receive entry")
	}
	if body["sourceTimeUtc"] == nil {
		t.Errorf("expected sourceTimeUtc to be present for a trusted-source entry")
	}
}

// TestHandleGetFISBCachePayload_RealNexradRoundTrip proves a NEXRAD
// tile's intensity data survives the same round trip and is correctly
// labeled so the client knows how to decode it - base64 of big-endian
// uint16 intensity values, exactly as fisbEncodeNexradPayload produces.
func TestHandleGetFISBCachePayload_RealNexradRoundTrip(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache.Enabled = true
	fisbCacheSettingsCache.PersistenceEnabled = true
	fisbCacheMu.Unlock()

	key := fisbcache.NexradKey(63, 0, 41.3333, -95.2000, 0.0667, 0.8000)
	entry := fisbcache.Entry{
		Key:                 key,
		ReceivedAtMonotonic: monotonicSeconds(),
		ReceivedAtUTC:       fisbCacheTrustedNowUTC(),
	}
	intensity := []uint16{0, 3, 7, 15, 2, 0}
	buf := make([]byte, len(intensity)*2)
	for i, v := range intensity {
		binary.BigEndian.PutUint16(buf[i*2:], v)
	}
	payload := base64.StdEncoding.EncodeToString(buf)
	if err := fisbCachePersist(entry, payload); err != nil {
		t.Fatalf("fisbCachePersist: %v", err)
	}

	rec, body := fisbCachePayloadRequest(t, string(key.Class), key.Identity)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %v)", rec.Code, body)
	}
	if body["payloadEncoding"] != "base64-uint16be" {
		t.Errorf("expected payloadEncoding \"base64-uint16be\", got %v", body["payloadEncoding"])
	}
	got, err := base64.StdEncoding.DecodeString(body["payload"].(string))
	if err != nil {
		t.Fatalf("returned payload did not decode as base64: %v", err)
	}
	if len(got) != len(buf) {
		t.Fatalf("decoded length mismatch: got %d bytes, want %d", len(got), len(buf))
	}
	for i, v := range intensity {
		if binary.BigEndian.Uint16(got[i*2:]) != v {
			t.Errorf("intensity[%d] mismatch: got %d, want %d", i, binary.BigEndian.Uint16(got[i*2:]), v)
		}
	}
}

// TestHandleGetFISBCachePayload_EvictedEntryIsHonestlyNotFound proves
// that once an entry's persisted file is gone (evicted/purged), this
// endpoint reports 404 like any other never-persisted entry - never a
// 500, and never fabricated content.
func TestHandleGetFISBCachePayload_EvictedEntryIsHonestlyNotFound(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache.Enabled = true
	fisbCacheSettingsCache.PersistenceEnabled = true
	fisbCacheMu.Unlock()

	key := makeFISBTestKey("KEVICT")
	entry := fisbcache.Entry{Key: key, ReceivedAtMonotonic: monotonicSeconds(), ReceivedAtUTC: fisbCacheTrustedNowUTC()}
	if err := fisbCachePersist(entry, "METAR KEVICT 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"); err != nil {
		t.Fatalf("fisbCachePersist: %v", err)
	}
	// Confirm the round trip works before removing it, so the eventual
	// 404 below is proven to be about eviction, not a setup mistake.
	if rec, _ := fisbCachePayloadRequest(t, string(key.Class), key.Identity); rec.Code != http.StatusOK {
		t.Fatalf("expected the entry to be readable before eviction, got %d", rec.Code)
	}

	fisbCacheDeleteEntryFile(t, key)

	rec, body := fisbCachePayloadRequest(t, string(key.Class), key.Identity)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 after eviction, got %d (body: %v)", rec.Code, body)
	}
}
