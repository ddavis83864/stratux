package fisbrecorder

// gdl90classify.go splits and classifies the raw GDL90 bytes this
// package's own GDL90Record.Bytes captures - see recorder.go's doc
// comment on main/network.go's connectionWriter hook. A single captured
// write frequently contains MORE THAN ONE framed GDL90 message:
// main/network.go's collectMessages batches several already-framed
// (0x7E ... 0x7E) messages together into one write to reduce IOPS, so a
// byte-for-byte comparison of two whole GDL90Record.Bytes values can be
// misleading - what actually needs to be compared is the individual
// messages within, classified by type, not the batch boundaries (which
// depend on queue timing, not content).
//
// No dependency on package main: the framing (0x7E flag, 0x7D escape,
// trailing CRC16/CCITT) and the message-ID-based classification below
// are reimplemented independently from main/gen_gdl90.go's own
// prepareMessage/crcCompute/relayMessage - the same well-documented
// GDL90 wire format (FAA GDL90 ICD, and this fork's own MSGTYPE_UPLINK
// convention), not imported, so this package keeps building without
// cgo/libdump978.so.

import "fmt"

// GDL90MessageIDUplink is the message ID this fork's relayMessage
// (main/gen_gdl90.go) uses exclusively to relay a decoded FIS-B/UAT
// uplink frame to a client - the sole GDL90 message type that ever
// carries a weather/FIS-B product. Every other message ID this fork
// sends (heartbeat, ownship report, traffic reports, Stratux/ForeFlight
// identification messages) never carries weather.
const GDL90MessageIDUplink = 0x07

// GDL90Frame is one individually-framed GDL90 message extracted from a
// raw captured write.
type GDL90Frame struct {
	// MessageID is the frame's own first payload byte (after the 0x7E
	// flag and any byte-unstuffing) - see IsWeatherBearing.
	MessageID byte

	// Payload is everything between MessageID and the trailing CRC16
	// (already un-stuffed; the CRC itself and the 0x7E flags are not
	// included here - see CRCValid for whether it matched).
	Payload []byte

	// CRCValid reports whether this frame's own trailing CRC16/CCITT
	// (poly 0x1021, matching main/gen_gdl90.go's Crc16Table/crcCompute)
	// matches its MessageID+Payload bytes. false means either genuine
	// corruption or a parsing misalignment - never silently ignored by
	// CompareWeatherFrames (see gdl90compare.go).
	CRCValid bool
}

// IsWeatherBearing reports whether f is a FIS-B/UAT uplink relay message
// - see GDL90MessageIDUplink's own doc comment.
func (f GDL90Frame) IsWeatherBearing() bool { return f.MessageID == GDL90MessageIDUplink }

// UplinkPayload, for a weather-bearing frame, strips this fork's own
// 4-byte relay header (main/gen_gdl90.go's relayMessage: MessageID
// followed by 3 always-zero reserved/TODO time bytes) and returns the
// raw UAT uplink payload - directly comparable, byte-for-byte, against a
// FrameRecord.Frame's own hex-decoded payload (parseInput relays exactly
// the same `frame` bytes it derived tower identity and cache admission
// from - see main/gen_gdl90.go:1211's `return frame, msgtype`). Returns
// nil if f is not weather-bearing or is too short to contain the header.
func (f GDL90Frame) UplinkPayload() []byte {
	if !f.IsWeatherBearing() || len(f.Payload) < 3 {
		return nil
	}
	return f.Payload[3:]
}

var gdl90CRC16Table [256]uint16

func init() {
	for i := 0; i < 256; i++ {
		crc := uint16(i) << 8
		for b := 0; b < 8; b++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc = crc << 1
			}
		}
		gdl90CRC16Table[i] = crc
	}
}

func gdl90CRC16(data []byte) uint16 {
	var crc uint16
	for _, b := range data {
		crc = gdl90CRC16Table[crc>>8] ^ (crc << 8) ^ uint16(b)
	}
	return crc
}

// SplitGDL90Frames un-escapes and splits a raw captured write (one
// GDL90Record.Bytes value) into its individual framed messages, in
// order. A malformed frame (missing closing flag, too short to hold a
// message ID + CRC) is reported via err rather than silently dropped or
// guessed at - a caller comparing frame counts needs to know a blob
// didn't fully parse, not see an artificially short list.
func SplitGDL90Frames(blob []byte) ([]GDL90Frame, error) {
	var frames []GDL90Frame
	i := 0
	for i < len(blob) {
		if blob[i] != 0x7E {
			return frames, fmt.Errorf("fisbrecorder: expected 0x7E flag at offset %d, got 0x%02X", i, blob[i])
		}
		i++ // consume start flag
		var raw []byte
		closed := false
		for i < len(blob) {
			b := blob[i]
			if b == 0x7E {
				i++ // consume end flag
				closed = true
				break
			}
			if b == 0x7D {
				i++
				if i >= len(blob) {
					return frames, fmt.Errorf("fisbrecorder: truncated escape sequence at end of blob")
				}
				raw = append(raw, blob[i]^0x20)
				i++
				continue
			}
			raw = append(raw, b)
			i++
		}
		if !closed {
			return frames, fmt.Errorf("fisbrecorder: frame starting near offset %d never saw a closing 0x7E", i)
		}
		if len(raw) < 3 { // message ID + 2-byte CRC, minimum
			return frames, fmt.Errorf("fisbrecorder: frame too short (%d bytes) to hold a message ID and CRC16", len(raw))
		}
		body := raw[:len(raw)-2]
		declaredCRC := uint16(raw[len(raw)-2]) | uint16(raw[len(raw)-1])<<8
		frames = append(frames, GDL90Frame{
			MessageID: body[0],
			Payload:   body[1:],
			CRCValid:  gdl90CRC16(body) == declaredCRC,
		})
	}
	return frames, nil
}
