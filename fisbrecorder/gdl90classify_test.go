package fisbrecorder

import "testing"

// buildGDL90Frame mirrors main/gen_gdl90.go's prepareMessage: appends a
// CRC16/CCITT, byte-stuffs 0x7E/0x7D, and wraps in 0x7E flags.
func buildGDL90Frame(body []byte) []byte {
	crc := gdl90CRC16(body)
	data := append(append([]byte{}, body...), byte(crc&0xFF), byte(crc>>8))
	out := []byte{0x7E}
	for _, b := range data {
		if b == 0x7E || b == 0x7D {
			out = append(out, 0x7D, b^0x20)
		} else {
			out = append(out, b)
		}
	}
	out = append(out, 0x7E)
	return out
}

func TestSplitGDL90Frames_SingleFrame(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x03} // heartbeat-shaped, message ID 0x00
	blob := buildGDL90Frame(body)

	frames, err := SplitGDL90Frames(blob)
	if err != nil {
		t.Fatalf("SplitGDL90Frames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.MessageID != 0x00 {
		t.Errorf("MessageID = 0x%02X, want 0x00", f.MessageID)
	}
	if string(f.Payload) != string(body[1:]) {
		t.Errorf("Payload = %v, want %v", f.Payload, body[1:])
	}
	if !f.CRCValid {
		t.Error("CRCValid = false, want true for a correctly-framed message")
	}
	if f.IsWeatherBearing() {
		t.Error("message ID 0x00 reported as weather-bearing")
	}
}

func TestSplitGDL90Frames_BatchedMessages(t *testing.T) {
	// Mirrors main/network.go's collectMessages: several already-framed
	// messages concatenated into one write.
	heartbeat := buildGDL90Frame([]byte{0x00, 0xAA})
	uplinkBody := append([]byte{GDL90MessageIDUplink, 0, 0, 0}, []byte("weather-payload-bytes")...)
	uplink := buildGDL90Frame(uplinkBody)
	traffic := buildGDL90Frame([]byte{0x14, 0xBB})

	blob := append(append(append([]byte{}, heartbeat...), uplink...), traffic...)

	frames, err := SplitGDL90Frames(blob)
	if err != nil {
		t.Fatalf("SplitGDL90Frames: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(frames))
	}
	if frames[0].IsWeatherBearing() || frames[2].IsWeatherBearing() {
		t.Error("non-uplink frames misclassified as weather-bearing")
	}
	if !frames[1].IsWeatherBearing() {
		t.Fatal("uplink frame not classified as weather-bearing")
	}
	want := "weather-payload-bytes"
	if string(frames[1].UplinkPayload()) != want {
		t.Errorf("UplinkPayload = %q, want %q", frames[1].UplinkPayload(), want)
	}
}

func TestSplitGDL90Frames_EscapedBytesRoundTrip(t *testing.T) {
	// A payload that itself contains 0x7E and 0x7D bytes, requiring
	// stuffing - proves un-escaping is correct, not just pass-through.
	body := []byte{0x00, 0x7E, 0x7D, 0x01, 0x7E}
	blob := buildGDL90Frame(body)

	frames, err := SplitGDL90Frames(blob)
	if err != nil {
		t.Fatalf("SplitGDL90Frames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if string(frames[0].Payload) != string(body[1:]) {
		t.Errorf("Payload = %v, want %v (escaped bytes not round-tripped correctly)", frames[0].Payload, body[1:])
	}
	if !frames[0].CRCValid {
		t.Error("CRCValid = false for a correctly round-tripped escaped frame")
	}
}

func TestSplitGDL90Frames_CorruptedCRCDetected(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02}
	blob := buildGDL90Frame(body)
	// Flip a bit in the payload region (after the leading 0x7E) without
	// touching the CRC bytes, so CRCValid must catch it.
	blob[2] ^= 0xFF

	frames, err := SplitGDL90Frames(blob)
	if err != nil {
		t.Fatalf("SplitGDL90Frames: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if frames[0].CRCValid {
		t.Error("CRCValid = true for a corrupted frame, want false")
	}
}

func TestSplitGDL90Frames_TruncatedBlobReturnsError(t *testing.T) {
	blob := buildGDL90Frame([]byte{0x00, 0x01, 0x02})
	truncated := blob[:len(blob)-3] // drop the closing flag and a byte

	_, err := SplitGDL90Frames(truncated)
	if err == nil {
		t.Fatal("SplitGDL90Frames on a truncated blob returned no error")
	}
}

func TestSplitGDL90Frames_EmptyBlobIsZeroFrames(t *testing.T) {
	frames, err := SplitGDL90Frames(nil)
	if err != nil {
		t.Fatalf("SplitGDL90Frames(nil): %v", err)
	}
	if len(frames) != 0 {
		t.Errorf("got %d frames, want 0", len(frames))
	}
}

func TestGDL90Frame_UplinkPayload_NonUplinkReturnsNil(t *testing.T) {
	f := GDL90Frame{MessageID: 0x00, Payload: []byte{1, 2, 3, 4, 5}}
	if f.UplinkPayload() != nil {
		t.Error("UplinkPayload() on a non-uplink frame returned non-nil")
	}
}

func TestGDL90Frame_UplinkPayload_TooShortReturnsNil(t *testing.T) {
	f := GDL90Frame{MessageID: GDL90MessageIDUplink, Payload: []byte{0, 0}} // < 3 bytes
	if f.UplinkPayload() != nil {
		t.Error("UplinkPayload() on a too-short uplink frame returned non-nil")
	}
}
