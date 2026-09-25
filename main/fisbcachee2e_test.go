package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/storagelifecycle"
	"github.com/stratux/stratux/uatparse"
)

// End-to-end, byte-level validation of the FIS-B path this project actually has:
//
//	raw UAT uplink bytes -> uatparse.New/DecodeUplink -> fisbCacheCaptureFrame
//	  -> reservation queue -> Store.Admit -> (optional) persisted file
//	  -> the same inventory/status handlers the dashboard uses
//
// Provenance of the fixtures: they are SYNTHETIC. No captured FIS-B frame is
// available in this repository (the bench receiver has never received a ground
// station - see docs/fisb-weather-cache.md), so each frame is built here, from
// the frame layout uatparse itself decodes (DO-282B uplink: 8-byte header,
// then information frames of 9-bit length + 4-bit type + product ID + time
// option + payload; text is DLAC-packed, NEXRAD is the RLE block form). The
// builders are ~60 lines and every byte is derived from the fields named in
// the code, not pasted as an opaque blob.

const fisbTestDLACAlphabet = "\x03ABCDEFGHIJKLMNOPQRSTUVWXYZ\x1A\t\x1E\n| !\"#$%&'()*+,-./0123456789:;<=>?"

// dlacEncode packs text into 6-bit DLAC characters, four characters per three bytes.
func dlacEncode(t *testing.T, s string) []byte {
	t.Helper()
	var bits []byte
	for i := 0; i < len(s); i++ {
		idx := strings.IndexByte(fisbTestDLACAlphabet, s[i])
		if idx < 0 {
			t.Fatalf("character %q is not in the DLAC alphabet", s[i])
		}
		bits = append(bits, byte(idx))
	}
	var out []byte
	for i := 0; i < len(bits); i += 4 {
		var c [4]byte
		copy(c[:], bits[i:fisbMinInt(i+4, len(bits))])
		out = append(out, c[0]<<2|c[1]>>4, c[1]<<4|c[2]>>2, c[2]<<6|c[3])
	}
	// trim the padding bytes that carry no real character
	need := (len(bits)*6 + 7) / 8
	return out[:need]
}

func fisbMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// fisbInfoFrame builds one FIS-B information frame with time option 0 (hours,
// minutes) - or option 2 (month, day, hours, minutes) when withDate - around
// the product-specific data.
func fisbInfoFrame(product uint32, when time.Time, withDate bool, data []byte) []byte {
	raw := []byte{byte(product >> 6 & 0x1f), byte(product & 0x3f << 2)}
	h, m := uint32(when.Hour()), uint32(when.Minute())
	if withDate {
		raw[1] |= 0x01 // t_opt = 2
		mo, d := uint32(when.Month()), uint32(when.Day())
		raw = append(raw, byte(mo<<3|d>>2), byte(d<<6|h<<1|m>>5), byte(m<<3))
	} else {
		raw = append(raw, byte(h<<2|m>>4), byte(m<<4))
	}
	raw = append(raw, data...)
	l := len(raw)
	return append([]byte{byte(l >> 1), byte(l&1) << 7}, raw...)
}

func fisbTextFrame(t *testing.T, product uint32, when time.Time, reports ...string) []byte {
	t.Helper()
	return fisbInfoFrame(product, when, false, dlacEncode(t, strings.Join(reports, "\x1E")+"\x1E"))
}

// fisbNexradRLEFrame builds a product 63/64 frame carrying one RLE block.
func fisbNexradRLEFrame(product uint32, when time.Time, scale, block int, intensities ...byte) []byte {
	d := []byte{0x80 | byte(scale&3)<<4 | byte(block>>16&0x0f), byte(block >> 8), byte(block)}
	d = append(d, intensities...)
	return fisbInfoFrame(product, when, false, d)
}

// fisbUplink wraps information frames in a valid 432-byte uplink and returns
// the hex text form the live receive path hands to uatparse.New.
func fisbUplink(frames ...[]byte) string {
	frame := make([]byte, uatparse.UPLINK_FRAME_DATA_BYTES)
	frame[6] = 0x20 // application data valid
	pos := 8
	for _, f := range frames {
		pos += copy(frame[pos:], f)
	}
	return fmt.Sprintf("+%s;rs=0;ss=100", hex.EncodeToString(frame))
}

// receiveUplink is the exact sequence the live receive path runs per uplink
// (main/gen_gdl90.go parseInput): decode, then hand every frame to the cache.
func receiveUplink(t *testing.T, hexFrame string) *uatparse.UATMsg {
	t.Helper()
	msg, err := uatparse.New(hexFrame)
	if err != nil {
		t.Fatalf("uatparse.New: %v", err)
	}
	if err := msg.DecodeUplink(); err != nil {
		t.Fatalf("DecodeUplink: %v", err)
	}
	for _, f := range msg.Frames {
		fisbCacheCaptureFrame(f)
	}
	drainFISBPendingSynchronously(t)
	return msg
}

func enableFISBCacheForTest(t *testing.T, persistence bool) {
	t.Helper()
	withTestStorageManagerReportingPressure(t, storagelifecycle.PressureNormal)
	fisbCacheMu.Lock()
	fisbCacheSettingsCache.Enabled = true
	fisbCacheSettingsCache.PersistenceEnabled = persistence
	fisbCacheMu.Unlock()
}

func fisbInventoryForTest(t *testing.T) []fisbCacheInventoryItem {
	t.Helper()
	rr := httptest.NewRecorder()
	handleGetFISBCacheInventoryRequest(rr, httptest.NewRequest(http.MethodGet, "/getFISBCacheInventory", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("inventory: HTTP %d: %s", rr.Code, rr.Body.String())
	}
	var entries []fisbCacheInventoryItem
	if err := json.Unmarshal(rr.Body.Bytes(), &entries); err != nil {
		t.Fatalf("inventory JSON: %v (%s)", err, rr.Body.String())
	}
	return entries
}

const (
	metarSEA = "METAR KSEA 091853Z AUTO 00000KT 10SM CLR 15/10 A3000"
	metarPDX = "METAR KPDX 091853Z AUTO 27005KT 10SM FEW040 14/09 A3001"
	tafSEA   = "TAF KSEA 091730Z 0918/1024 18008KT P6SM SCT050"
)

func TestFISBEndToEnd_RawUplinkBytesReachTheCacheAndInventory(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, false)
	now := time.Now().UTC()

	msg := receiveUplink(t, fisbUplink(
		fisbTextFrame(t, 413, now.Add(-10*time.Minute), metarSEA, metarPDX, tafSEA),
		fisbNexradRLEFrame(63, now.Add(-2*time.Minute), 0, 1234, 0x01, 0x0a, 0x13),
	))

	// The decoder really recovered what was put on the wire.
	if len(msg.Frames) != 2 || msg.Frames[0].Product_id != 413 || msg.Frames[1].Product_id != 63 {
		t.Fatalf("decoded frames = %+v", msg.Frames)
	}
	if got := msg.Frames[0].Text_data; len(got) < 3 || got[0] != metarSEA || got[1] != metarPDX || got[2] != tafSEA {
		t.Fatalf("decoded text = %q", got)
	}

	inv := fisbInventoryForTest(t)
	if len(inv) != 4 {
		t.Fatalf("inventory has %d entries, want 4 (METAR KSEA, METAR KPDX, TAF KSEA, one NEXRAD tile): %+v", len(inv), inv)
	}
	byID := map[string]fisbCacheInventoryItem{}
	for _, it := range inv {
		byID[it.Identity] = it
		if it.Freshness == string(fisbcache.FreshnessLive) {
			t.Errorf("%s: a cached entry must never be labelled LIVE", it.Identity)
		}
		if !it.SourceTrusted {
			t.Errorf("%s: trusted receive clock but source time untrusted", it.Identity)
		}
	}
	classes := map[string]int{}
	for _, it := range inv {
		classes[it.ProductClass]++
	}
	if classes[string(fisbcache.ClassText)] != 3 || classes[string(fisbcache.ClassNexradTile)] != 1 {
		t.Errorf("classes = %v", classes)
	}
}

func TestFISBEndToEnd_RetransmissionsDoNotAccumulateAndUnrelatedProductsSurvive(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, false)
	now := time.Now().UTC()

	frame := fisbUplink(fisbTextFrame(t, 413, now.Add(-10*time.Minute), metarSEA, metarPDX))
	receiveUplink(t, frame)
	for i := 0; i < 500; i++ { // a ground station repeats the same uplink for as long as you are in range
		receiveUplink(t, frame)
	}
	if n := fisbCacheStore.Len(); n != 2 {
		t.Fatalf("500 identical retransmissions changed the cache to %d entries, want 2", n)
	}

	// A newer report for one station replaces only that station.
	newer := "METAR KSEA 091953Z AUTO 18004KT 10SM BKN030 16/10 A3001"
	receiveUplink(t, fisbUplink(fisbTextFrame(t, 413, now.Add(-2*time.Minute), newer)))
	if n := fisbCacheStore.Len(); n != 2 {
		t.Fatalf("a newer KSEA report changed the cache to %d entries, want 2 (KSEA replaced in place, KPDX untouched)", n)
	}
	sea, _ := fisbCacheStore.Get(fisbcache.TextKey("METAR", "KSEA"))
	pdx, _ := fisbCacheStore.Get(fisbcache.TextKey("METAR", "KPDX"))
	if !sea.Source.UTC.After(pdx.Source.UTC) {
		t.Fatalf("KSEA source time %v should now be newer than KPDX %v", sea.Source.UTC, pdx.Source.UTC)
	}
	seaTime := sea.Source.UTC

	// A delayed, OLDER copy arriving afterwards must not regress it.
	receiveUplink(t, fisbUplink(fisbTextFrame(t, 413, now.Add(-30*time.Minute), "METAR KSEA 091753Z AUTO 25010KT 3SM BR OVC008 12/11 A2990")))
	after, _ := fisbCacheStore.Get(fisbcache.TextKey("METAR", "KSEA"))
	if !after.Source.UTC.Equal(seaTime) {
		t.Fatalf("an older retransmission overwrote the newer entry: source %v -> %v", seaTime, after.Source.UTC)
	}
	if n := fisbCacheStore.Len(); n != 2 {
		t.Fatalf("cache has %d entries after the out-of-order copy, want 2", n)
	}
}

func TestFISBEndToEnd_PersistedFileRoundTripsTheOriginalReport(t *testing.T) {
	dir := withTestFISBCacheStorage(t)
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, true)
	now := time.Now().UTC()
	fisbCacheMu.Lock()
	fisbCacheDir = dir
	fisbCacheNamespace.Root = dir
	fisbCacheMu.Unlock()

	receiveUplink(t, fisbUplink(fisbTextFrame(t, 413, now.Add(-5*time.Minute), metarSEA)))

	files, _ := filepath.Glob(filepath.Join(dir, "*"))
	if len(files) != 1 {
		t.Fatalf("expected exactly one persisted file, found %v", files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	entry, payload, err := fisbcache.DecodePersistedEntry(raw, time.Now().UTC())
	if err != nil {
		t.Fatalf("the persisted file does not decode: %v", err)
	}
	if payload != metarSEA {
		t.Fatalf("persisted payload = %q, want the report as received: %q", payload, metarSEA)
	}
	if entry.Key != fisbcache.TextKey("METAR", "KSEA") {
		t.Fatalf("persisted key = %+v", entry.Key)
	}
}

func TestFISBEndToEnd_UnsupportedAndMalformedInputNeverCorruptsTheCache(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, false)
	now := time.Now().UTC()

	receiveUplink(t, fisbUplink(fisbTextFrame(t, 413, now.Add(-5*time.Minute), metarSEA)))
	if fisbCacheStore.Len() != 1 {
		t.Fatal("setup: expected one entry")
	}
	before := fisbCacheStore.Snapshot()

	// Products this decoder does not structurally decode (AIRMET 8, NOTAM 11/13, an
	// arbitrary 2000) are never cached.
	receiveUplink(t, fisbUplink(
		fisbInfoFrame(8, now, false, []byte{1, 2, 3, 4, 5, 6}),
		fisbInfoFrame(11, now, false, []byte{1, 2, 3, 4, 5, 6}),
		fisbInfoFrame(2000, now, false, []byte{1, 2, 3, 4, 5, 6}),
	))
	// A report with too few fields, an empty report, and a NEXRAD frame too short to hold a block.
	receiveUplink(t, fisbUplink(
		fisbTextFrame(t, 413, now, "METAR KSEA", ""),
		fisbInfoFrame(63, now, false, []byte{0x80, 0x00}),
	))
	// Frame lengths that used to slice past the end of the buffer and panic the decoder.
	for _, l := range []int{424, 423, 400} {
		bad := make([]byte, uatparse.UPLINK_FRAME_DATA_BYTES)
		bad[6] = 0x20
		bad[8], bad[9] = byte(l>>1), byte(l&1)<<7
		receiveUplink(t, fmt.Sprintf("+%s;rs=0;ss=100", hex.EncodeToString(bad)))
	}

	after := fisbCacheStore.Snapshot()
	if len(after) != len(before) {
		t.Fatalf("junk changed the cache from %d to %d entries", len(before), len(after))
	}
	for k, e := range before {
		if got, ok := after[k]; !ok || got != e {
			t.Fatalf("entry %v was altered by junk input", k)
		}
	}
}

func TestFISBEndToEnd_DisabledCacheStoresNothingFromTheSameBytes(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	now := time.Now().UTC()
	receiveUplink(t, fisbUplink(fisbTextFrame(t, 413, now.Add(-5*time.Minute), metarSEA)))
	if n := fisbCacheStore.Len(); n != 0 {
		t.Fatalf("the cache is disabled by default but holds %d entries", n)
	}
}

// The live GDL90 uplink relay is a separate path that this feature must not
// change: relayMessage still forwards the payload as message ID 0x07 with a
// zero reception time, and the cache never re-injects anything (replay is
// rejected by settings validation).
func TestFISBReplayIntoGDL90RemainsRejected(t *testing.T) {
	s := DefaultFISBCacheSettings()
	s.ReplayEnabled = true
	if err := s.Validate(); err == nil {
		t.Fatal("replay into GDL90 must stay disabled until a reception-time signal exists")
	}
}
