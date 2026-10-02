package main

// Regression coverage for the October 1, 2026 field failure: the production
// receiver is the external low-power UAT radio (main/lowpower_uat.go), whose
// processRadioMessage used to call parseInput/relayMessage directly. The FIS-B
// field recorder's raw-frame hook lives in handleUatMessage, so a 52-minute
// live session recorded 262,266 GDL90 records and ZERO raw UAT frames.
//
// These tests drive the real path, radio bytes -> real Reed-Solomon FEC (the
// linked dump978 C code) -> handleUatMessage -> recorder AND normal decode/
// cache, and pin the shared-ingestion contract so a new receiver path cannot
// silently bypass recording again.
//
// The Reed-Solomon ENCODER below exists only in this test file: the repository
// has a decoder (dump978/fec.c) but no encoder, and a valid 552-byte radio
// frame is needed as input. Every frame is round-tripped through the real C
// decoder before any assertion relies on it, so an incorrect encoder fails
// loudly instead of silently weakening a test.

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

	"github.com/stratux/stratux/fisbrecorder"
	"github.com/stratux/stratux/uatparse"
)

// --- test-only UAT uplink Reed-Solomon encoder ------------------------------

// Parameters mirror dump978/fec.c init_fec(): GF(2^8), poly 0x187, fcr 120,
// prim 1, 20 parity bytes per 72-data-byte block, 6 interleaved blocks.
var (
	gfExp [512]byte
	gfLog [256]int
	rsGen []byte // generator polynomial, coefficient of x^k at index k
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = i
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x187
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
	rsGen = []byte{1}
	for i := 0; i < 20; i++ {
		root := gfExp[(120+i)%255]
		next := make([]byte, len(rsGen)+1)
		for k := range rsGen {
			next[k+1] ^= rsGen[k]
			next[k] ^= gfMul(rsGen[k], root)
		}
		rsGen = next
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[gfLog[a]+gfLog[b]]
}

// rsEncodeBlock returns data followed by 20 parity bytes.
func rsEncodeBlock(data []byte) []byte {
	par := make([]byte, 20)
	for _, d := range data {
		fb := d ^ par[0]
		copy(par, par[1:])
		par[19] = 0
		if fb != 0 {
			for j := 0; j < 20; j++ {
				par[j] ^= gfMul(fb, rsGen[19-j])
			}
		}
	}
	return append(append([]byte{}, data...), par...)
}

// encodeUplinkFrame builds the 552-byte interleaved on-the-wire frame the
// radio hands to processRadioMessage from 432 bytes of uplink data.
func encodeUplinkFrame(t *testing.T, data []byte) []byte {
	t.Helper()
	if len(data) != uatparse.UPLINK_FRAME_DATA_BYTES {
		t.Fatalf("uplink data is %d bytes, want %d", len(data), uatparse.UPLINK_FRAME_DATA_BYTES)
	}
	out := make([]byte, uatparse.UPLINK_FRAME_BLOCKS*uatparse.UPLINK_BLOCK_BYTES)
	for b := 0; b < uatparse.UPLINK_FRAME_BLOCKS; b++ {
		cw := rsEncodeBlock(data[b*uatparse.UPLINK_BLOCK_DATA_BYTES : (b+1)*uatparse.UPLINK_BLOCK_DATA_BYTES])
		for i, v := range cw {
			out[i*uatparse.UPLINK_FRAME_BLOCKS+b] = v
		}
	}
	return out
}

// radioMessage is what radioSerialPortReader passes to processRadioMessage:
// 1 RSSI byte + 4 timestamp bytes + the frame.
func radioMessage(rssi int8, frame []byte) []byte {
	return append([]byte{byte(rssi), 0, 0, 0, 0}, frame...)
}

// --- fixtures ----------------------------------------------------------------

// lowPowerUplinkData is a real-shaped uplink carrying a METAR, built with the
// same helpers the existing end-to-end FIS-B tests use.
func lowPowerUplinkData(t *testing.T, report string) []byte {
	t.Helper()
	s := fisbUplink(fisbTextFrame(t, 413, time.Now().UTC().Add(-10*time.Minute), report))
	h := s[1:strings.Index(s, ";")]
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != uatparse.UPLINK_FRAME_DATA_BYTES {
		t.Fatalf("fixture: %v, %d bytes", err, len(b))
	}
	return b
}

func wantRelayString(data []byte, rssi int8) string {
	return fmt.Sprintf("+%s;ss=%d;", hex.EncodeToString(data), rssi)
}

// recordedFrames runs the recorder in a temp dir for the duration of fn and
// returns the manifest plus every frame string, in recorded order, read back
// through the production replay reader.
func recordedFrames(t *testing.T, fn func()) (*fisbrecorder.Manifest, []string) {
	t.Helper()
	if stratuxClock == nil { // normally created by main(); parseInput stamps messages with it
		stratuxClock = NewMonotonic()
	}
	base := t.TempDir()
	rec := fisbrecorder.New(base, "test-build", fisbrecorder.DefaultOptions())
	prev := fisbRecorder
	fisbRecorder = rec
	defer func() { fisbRecorder = prev }()

	sid, err := rec.Start()
	if err != nil {
		t.Fatalf("recorder Start: %v", err)
	}
	fn()
	m, err := rec.Stop()
	if err != nil {
		t.Fatalf("recorder Stop: %v", err)
	}
	var frames []string
	if _, err := fisbrecorder.ReplayFrames(filepath.Join(base, sid), func(r fisbrecorder.FrameRecord) {
		frames = append(frames, r.Frame)
	}, fisbrecorder.ReplayOptions{SpeedMultiplier: -1}); err != nil {
		t.Fatalf("ReplayFrames: %v", err)
	}
	return m, frames
}

func cacheEntries(t *testing.T) int {
	t.Helper()
	drainFISBPendingSynchronously(t)
	return len(fisbInventoryForTest(t))
}

// --- self-check of the test encoder against the REAL C decoder ----------------

func TestLowPowerUAT_TestEncoderRoundTripsThroughTheRealFEC(t *testing.T) {
	data := lowPowerUplinkData(t, metarSEA)
	m, frames := recordedFrames(t, func() {
		processRadioMessage(radioMessage(-50, encodeUplinkFrame(t, data)))
	})
	if m.FrameCount != 1 || len(frames) != 1 || frames[0] != wantRelayString(data, -50) {
		t.Fatalf("encoder/decoder disagree: manifest frames=%d, got %d frame(s); first=%q",
			m.FrameCount, len(frames), firstOrEmpty(frames))
	}
}

func firstOrEmpty(s []string) string {
	if len(s) == 0 {
		return ""
	}
	if len(s[0]) > 80 {
		return s[0][:80] + "..."
	}
	return s[0]
}

// --- Test A/B/E: recorded, downstream intact, FEC applied before recording ------

func TestLowPowerUAT_ValidFrameIsRecordedAndStillDecodedAndCached(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, false)
	data := lowPowerUplinkData(t, metarSEA)
	before := globalStatus.UAT_messages_total

	m, frames := recordedFrames(t, func() {
		processRadioMessage(radioMessage(-50, encodeUplinkFrame(t, data)))
	})

	// A: exactly the post-FEC frame, once, nothing dropped.
	if m.FrameCount != 1 || m.DroppedFrames != 0 || len(frames) != 1 {
		t.Fatalf("frames: manifest=%d dropped=%d replayed=%d, want 1/0/1", m.FrameCount, m.DroppedFrames, len(frames))
	}
	if want := wantRelayString(data, -50); frames[0] != want {
		t.Fatalf("recorded frame differs from the post-FEC relay string\n got %.100s...\nwant %.100s...", frames[0], want)
	}
	// B: recorder support must not become an alternate processing path -
	// the normal decode counter AND cache admission still happen, once.
	if got := globalStatus.UAT_messages_total - before; got != 1 {
		t.Errorf("UAT_messages_total advanced by %d, want 1", got)
	}
	if got := cacheEntries(t); got != 1 {
		t.Errorf("cache has %d entries after one METAR uplink, want 1", got)
	}
}

func TestLowPowerUAT_RecordedFrameCarriesTheRSSIAndIsPostCorrection(t *testing.T) {
	data := lowPowerUplinkData(t, metarSEA)
	frame := encodeUplinkFrame(t, data)
	// Three correctable symbol errors in three different blocks.
	frame[0*6+0] ^= 0xFF
	frame[7*6+2] ^= 0x5A
	frame[40*6+5] ^= 0x01

	_, frames := recordedFrames(t, func() {
		processRadioMessage(radioMessage(-64, frame)) // negative int8 RSSI
	})
	if len(frames) != 1 {
		t.Fatalf("recorded %d frames, want 1", len(frames))
	}
	// E: ss= is the signed radio RSSI; the data is the CORRECTED data, so a
	// replay (which never re-runs FEC) is byte-identical to what live
	// processing consumed.
	if want := wantRelayString(data, -64); frames[0] != want {
		t.Fatalf("recorded frame is not the corrected relay string\n got %.100s...\nwant %.100s...", frames[0], want)
	}
}

// --- Test C: recording disabled == previous production behaviour ----------------

func TestLowPowerUAT_RecordingDisabledProcessesExactlyAsBefore(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, false)
	if fisbRecorder.IsActive() {
		t.Fatal("precondition: recorder must be inactive")
	}
	data := lowPowerUplinkData(t, metarSEA)
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}
	before := globalStatus.UAT_messages_total

	processRadioMessage(radioMessage(-50, encodeUplinkFrame(t, data)))

	if fisbRecorder.IsActive() {
		t.Error("processing a frame must never start a recording")
	}
	if got := globalStatus.UAT_messages_total - before; got != 1 {
		t.Errorf("UAT_messages_total advanced by %d, want 1", got)
	}
	if got := cacheEntries(t); got != 1 {
		t.Errorf("cache has %d entries, want 1", got)
	}
}

// --- Test D: invalid FEC keeps its rejection semantics ---------------------------

func TestLowPowerUAT_UncorrectableFrameIsNeitherProcessedNorRecorded(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, false)
	data := lowPowerUplinkData(t, metarSEA)
	frame := encodeUplinkFrame(t, data)
	for k := 0; k < 40; k++ { // far beyond the 10-symbol correction limit in block 0
		frame[k*6+0] ^= 0xFF
	}
	before := globalStatus.UAT_messages_total

	m, frames := recordedFrames(t, func() {
		processRadioMessage(radioMessage(-50, frame))
		processRadioMessage(radioMessage(-50, make([]byte, 100))) // unhandled size
	})
	if m.FrameCount != 0 || len(frames) != 0 {
		t.Errorf("rejected frames were recorded: manifest=%d replayed=%d", m.FrameCount, len(frames))
	}
	if got := globalStatus.UAT_messages_total - before; got != 0 {
		t.Errorf("UAT_messages_total advanced by %d for rejected frames, want 0", got)
	}
	if got := cacheEntries(t); got != 0 {
		t.Errorf("rejected frame reached the cache (%d entries)", got)
	}
}

// --- Section 9: record -> serialize -> replay -> normal processing ----------------

func TestLowPowerUAT_RecordedFrameReplaysThroughNormalIngestionUnchanged(t *testing.T) {
	withFISBCacheTestEnv(t)
	withTrustedTimeForTest(t)
	enableFISBCacheForTest(t, false)
	data := lowPowerUplinkData(t, metarSEA)

	_, frames := recordedFrames(t, func() {
		processRadioMessage(radioMessage(-50, encodeUplinkFrame(t, data)))
	})
	if len(frames) != 1 {
		t.Fatalf("recorded %d frames, want 1", len(frames))
	}
	// Reset to an empty cache, then replay the way -fisbReplay does: the
	// recorded string straight into handleUatMessage (no FEC is re-applied).
	withFISBCacheTestEnv(t)
	enableFISBCacheForTest(t, false)
	before := globalStatus.UAT_messages_total
	handleUatMessage(frames[0])
	if got := globalStatus.UAT_messages_total - before; got != 1 {
		t.Errorf("replay advanced UAT_messages_total by %d, want 1 (double counting?)", got)
	}
	if got := cacheEntries(t); got != 1 {
		t.Errorf("replayed frame produced %d cache entries, want the original 1", got)
	}
}

// --- Test F: the shared ingestion contract, for every supported receiver path -----

func TestUATIngestion_EveryReceiverPathReachesTheRecorder(t *testing.T) {
	data := lowPowerUplinkData(t, metarSEA)
	want := wantRelayString(data, -50)
	paths := []struct {
		name   string
		ingest func()
	}{
		// uatReader, -uatin, -replay, -fisbReplay and CONTEXT_GODUMP978 trace
		// replay all call handleUatMessage (asserted from source below).
		{"RTL-SDR/godump978 and replay paths", func() { handleUatMessage(want) }},
		// radioSerialPortReader and CONTEXT_LOWPOWERUAT trace replay both
		// call processRadioMessage.
		{"external low-power UAT radio", func() { processRadioMessage(radioMessage(-50, encodeUplinkFrame(t, data))) }},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			m, frames := recordedFrames(t, p.ingest)
			if m.FrameCount != 1 || len(frames) != 1 || frames[0] != want {
				t.Fatalf("path %q: manifest frames=%d, replayed=%d; the receiver bypasses the recorder or alters the frame", p.name, m.FrameCount, len(frames))
			}
		})
	}
}

// TestUATIngestion_NoNewDirectParseInputCallers is a deliberate tripwire. The
// recorder hook lives in handleUatMessage, so any receiver that calls
// parseInput itself silently escapes recording - exactly the October 1 failure.
// Adding a caller here must be a conscious decision, not an accident.
func TestUATIngestion_NoNewDirectParseInputCallers(t *testing.T) {
	// File -> number of permitted parseInput( call sites.
	allowed := map[string]int{
		"sdr.go":  1, // handleUatMessage itself: the canonical post-FEC ingestion point
		"ping.go": 1, // KNOWN GAP: uAvionix Ping UAT frames are not recorded (not the installed receiver; documented follow-up)
		"pong.go": 1, // KNOWN GAP: uAvionix Pong UAT frames are not recorded (documented follow-up)
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func parseInput(") {
				continue
			}
			n += strings.Count(line, "parseInput(")
		}
		if f == "gen_gdl90.go" {
			continue // definition only; its comments/doc are not call sites
		}
		if n != allowed[f] {
			t.Errorf("%s has %d direct parseInput( call site(s), expected %d: a UAT receiver path that calls parseInput directly bypasses fisbRecorder.RecordFrame (see handleUatMessage)", f, n, allowed[f])
		}
	}

	// The two entry points that feed the recorder must still funnel into it.
	for file, mustContain := range map[string]string{
		"lowpower_uat.go": "handleUatMessage(toRelay)",
		"sdr.go":          "handleUatMessage(uat)",
		"trace.go":        "processRadioMessage(data)",
	} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(src), mustContain) {
			t.Errorf("%s no longer contains %q", file, mustContain)
		}
	}
	sdr, _ := os.ReadFile("sdr.go")
	if i := strings.Index(string(sdr), "func handleUatMessage("); i < 0 ||
		!strings.Contains(string(sdr)[i:i+120], "fisbRecorder.RecordFrame(") {
		t.Error("handleUatMessage must call fisbRecorder.RecordFrame first")
	}
}

// The field kit's early check polls GET /getFISBRecorderStatus. It must show
// raw frames from the production low-power radio while the session is still
// running (the gzip files can be buffered at 0 bytes) - the 2026-10-01 failure
// would have shown framesAccepted == 0 here within the first minutes.
func TestFISBRecorderStatusEndpoint_ShowsLowPowerFramesWhileRecording(t *testing.T) {
	if stratuxClock == nil {
		stratuxClock = NewMonotonic()
	}
	rec := fisbrecorder.New(t.TempDir(), "test-build", fisbrecorder.DefaultOptions())
	prev := fisbRecorder
	fisbRecorder = rec
	defer func() { fisbRecorder = prev }()

	get := func() fisbrecorder.Status {
		t.Helper()
		rr := httptest.NewRecorder()
		handleGetFISBRecorderStatus(rr, httptest.NewRequest(http.MethodGet, "/getFISBRecorderStatus", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", rr.Code, rr.Body.String())
		}
		var st fisbrecorder.Status
		if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
			t.Fatalf("status JSON: %v (%s)", err, rr.Body.String())
		}
		return st
	}
	if st := get(); st.Active {
		t.Fatalf("idle status = %+v", st)
	}
	if _, err := rec.Start(); err != nil {
		t.Fatal(err)
	}
	defer rec.Stop()
	if st := get(); !st.Active || st.FramesAccepted != 0 {
		t.Fatalf("fresh session status = %+v", st)
	}
	data := lowPowerUplinkData(t, metarSEA)
	processRadioMessage(radioMessage(-50, encodeUplinkFrame(t, data)))
	if st := get(); st.FramesAccepted != 1 || st.DroppedFrames != 0 {
		t.Fatalf("after one low-power frame: %+v, want framesAccepted=1", st)
	}
	rr := httptest.NewRecorder()
	handleGetFISBRecorderStatus(rr, httptest.NewRequest(http.MethodPost, "/getFISBRecorderStatus", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405 (read-only endpoint)", rr.Code)
	}
}
