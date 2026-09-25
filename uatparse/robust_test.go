package uatparse

import (
	"encoding/hex"
	"fmt"
	"testing"
)

// buildUplink assembles a syntactically valid 432-byte UAT uplink frame (8-byte
// header with a valid position and "application data valid", then the
// application data area) around the given raw application-data bytes, and
// returns it in the "+<hex>;rs=0;ss=100" text form uatparse.New accepts.
func buildUplink(appData []byte) string {
	frame := make([]byte, UPLINK_FRAME_DATA_BYTES)
	frame[6] = 0x20 // app_data_valid
	copy(frame[8:], appData)
	return fmt.Sprintf("+%s;rs=0;ss=100", hex.EncodeToString(frame))
}

// infoFrame builds one information frame: 9-bit length, 4-bit type in the low
// nibble of the second byte, then payload (length counts the payload only).
func infoFrame(frameType byte, payload []byte) []byte {
	l := len(payload)
	b := []byte{byte(l >> 1), byte(l&1)<<7 | frameType&0x0f}
	return append(b, payload...)
}

func decodeNoPanic(t *testing.T, name, s string) (msg *UATMsg, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: DecodeUplink panicked: %v", name, r)
		}
	}()
	msg, err = New(s)
	if err != nil {
		return msg, err
	}
	err = msg.DecodeUplink()
	return msg, err
}

// A frame length that fits the remaining application data by itself but not
// once the 2-byte frame header is added used to slice past the end of the
// buffer and panic the live decode path.
func TestDecodeUplink_FrameLengthOverrunningByItsOwnHeaderDoesNotPanic(t *testing.T) {
	const total = UPLINK_FRAME_DATA_BYTES - 8 // application data area
	for _, l := range []int{total, total - 1, total - 2, total - 3, 511, 500} {
		hdr := []byte{byte(l >> 1), byte(l&1)<<7 | 0x00}
		app := append(hdr, make([]byte, 8)...) // far fewer bytes than the header claims
		decodeNoPanic(t, fmt.Sprintf("frame_length=%d", l), buildUplink(app))
	}
}

// The same overrun one frame later in the message (non-zero position).
func TestDecodeUplink_OverrunAtNonZeroPositionDoesNotPanic(t *testing.T) {
	const total = UPLINK_FRAME_DATA_BYTES - 8
	first := infoFrame(0, make([]byte, 20))
	pos := len(first)
	for _, l := range []int{total - pos, total - pos - 1, total - pos - 2} {
		hdr := []byte{byte(l >> 1), byte(l&1)<<7 | 0x00}
		decodeNoPanic(t, fmt.Sprintf("second frame_length=%d", l), buildUplink(append(first, hdr...)))
	}
}

func TestDecodeUplink_WellFormedFramesStillDecode(t *testing.T) {
	app := append(infoFrame(0, append([]byte{0x06, 0x74}, make([]byte, 10)...)), infoFrame(0, append([]byte{0x00, 0x00}, make([]byte, 6)...))...)
	msg, err := decodeNoPanic(t, "wellformed", buildUplink(app))
	if err != nil || len(msg.Frames) != 2 {
		t.Fatalf("expected 2 frames, got err=%v frames=%d", err, len(msg.Frames))
	}
	if msg.Frames[0].Product_id != 413 {
		t.Fatalf("first frame product = %d, want 413", msg.Frames[0].Product_id)
	}
}

// FuzzDecodeUplink throws arbitrary application-data bytes (framed as a
// valid uplink, so the fuzzer reaches the information-frame, FIS-B time,
// text and NEXRAD decoders instead of stopping at the header) at the whole
// decode path Stratux runs on every received uplink. A crash here would take
// down the whole daemon: the live caller does not recover.
//
// Run: go test -vet=off -fuzz=FuzzDecodeUplink -fuzztime=60s ./uatparse/
func FuzzDecodeUplink(f *testing.F) {
	f.Add([]byte{})
	f.Add(infoFrame(0, append([]byte{0x06, 0x74}, make([]byte, 10)...)))
	f.Add(infoFrame(0, append([]byte{0x0f, 0xc0}, make([]byte, 40)...)))
	f.Add(infoFrame(0, append([]byte{0x10, 0x00}, make([]byte, 40)...)))
	f.Add(append(infoFrame(0, make([]byte, 20)), infoFrame(15, make([]byte, 9))...))
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, app []byte) {
		if len(app) > UPLINK_FRAME_DATA_BYTES-8 {
			app = app[:UPLINK_FRAME_DATA_BYTES-8]
		}
		msg, err := New(buildUplink(app))
		if err != nil {
			t.Fatalf("New rejected a well-formed frame: %v", err)
		}
		if err := msg.DecodeUplink(); err != nil {
			t.Fatalf("DecodeUplink: %v", err)
		}
		msg.GetTextReports()
		for _, fr := range msg.Frames {
			_ = fr.Product_id
			_ = len(fr.Text_data)
			_ = fr.Points
		}
	})
}
