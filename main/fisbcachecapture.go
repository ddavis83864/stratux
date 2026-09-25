/*
fisbcachecapture.go: the one place this feature depends on uatparse's
concrete types - kept separate from fisbcacherun.go so that file's own
capture-path functions (fisbCaptureText/fisbCaptureNexrad) stay provably
independent of uatparse's shape, and so a future uatparse change only
ever needs a review of this one small file.
*/
package main

import (
	"encoding/base64"
	"encoding/binary"

	"github.com/stratux/stratux/fisbcache"
	"github.com/stratux/stratux/uatparse"
)

// fisbFISBTimeFromFrame adapts one uatparse.UATFrame's own partial
// broadcast-time fields into fisbcache.FISBTime - see that type's own
// doc comment for why HasMonthDay must be tracked explicitly rather than
// inferred from Month/Day being zero (a genuine "hours/minutes only"
// frame has Month/Day == 0, which must never be confused with "January
// 0th").
//
// uatparse.UATFrame does not itself record which of its four time-format
// options (t_opt 0-3) produced its FISB_month/day/hours/minutes/seconds
// values (see uatparse.go's decodeTimeFormat), so this function's own
// heuristic - month and day both nonzero means a month/day-bearing
// format was used - is the best signal available without a uatparse
// change; a genuine "hours/minutes only" broadcast happening to also
// carry nonzero leftover Month/Day bytes is not possible given
// decodeTimeFormat's own field-clearing behavior (an unset field is
// always its Go zero value), so this is exact, not a guess.
func fisbFISBTimeFromFrame(f *uatparse.UATFrame) fisbcache.FISBTime {
	return fisbcache.FISBTime{
		HasMonthDay: f.FISB_month != 0 && f.FISB_day != 0,
		Month:       f.FISB_month,
		Day:         f.FISB_day,
		Hour:        f.FISB_hours,
		Minute:      f.FISB_minutes,
		Second:      f.FISB_seconds,
	}
}

// fisbCacheCaptureFrame is called once per decoded uatparse.UATFrame,
// from the exact same loop in main/gen_gdl90.go that already calls
// weatherRawUpdate.SendJSON(f) for every frame - see that call site's own
// comment. This function only ever does work for the two product classes
// this project's own uatparse actually decodes into structured content
// (see fisbcache's own doc comment); every other frame is a fast no-op.
func fisbCacheCaptureFrame(f *uatparse.UATFrame) {
	switch fisbcache.ClassifyProductID(f.Product_id) {
	case fisbcache.ClassText:
		fisbCacheCaptureTextFrame(f)
	case fisbcache.ClassNexradTile:
		fisbCacheCaptureNexradFrame(f)
	}
}

// fisbCacheCaptureTextFrame handles product 413 - the SAME frame
// main.registerADSBTextMessageReceived already parses per decoded text
// line (f.Text_data, via uatMsg.GetTextReports() one layer up) - this
// function performs the identical whitespace-split classification so
// this cache's own notion of (type, location) always agrees with what
// the dashboard's existing /weather text view already shows, never a
// second, differently-tuned parser.
func fisbCacheCaptureTextFrame(f *uatparse.UATFrame) {
	ft := fisbFISBTimeFromFrame(f)
	for _, line := range f.Text_data {
		productType, location, ok := fisbParseTextReportHeader(line)
		if !ok {
			continue
		}
		fisbCaptureText(productType, location, line, ft)
	}
}

// fisbParseTextReportHeader mirrors registerADSBTextMessageReceived's own
// x := strings.Split(msg, " ") / len(x) >= 5 / x[0]=Type, x[1]=Location
// classification exactly (main/gen_gdl90.go) - duplicated rather than
// factored out of that existing function, so this feature's own capture
// path can never accidentally change that function's established
// counting/broadcast behavior by refactoring it.
func fisbParseTextReportHeader(line string) (productType, location string, ok bool) {
	fields := splitOnSpace(line)
	if len(fields) < 5 {
		return "", "", false
	}
	return fields[0], fields[1], true
}

func splitOnSpace(s string) []string {
	var out []string
	start := -1
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// fisbCacheCaptureNexradFrame handles product 63/64 - one uatparse.UATFrame
// may carry several uatparse.NEXRADBlock tiles (f.NEXRAD); each is cached
// independently under its own tile identity.
func fisbCacheCaptureNexradFrame(f *uatparse.UATFrame) {
	ft := fisbFISBTimeFromFrame(f)
	for _, block := range f.NEXRAD {
		payload := fisbEncodeNexradPayload(block)
		fisbCaptureNexrad(block.Radar_Type, block.Scale, block.LatNorth, block.LonWest, block.Height, block.Width, payload, ft)
	}
}

// fisbEncodeNexradPayload bounds-encodes one NEXRADBlock's intensity data
// as base64 text, so fisbcache's own schema (a plain string Payload
// field - see schema.go) never needs a second, binary-aware code path.
// uatparse.NEXRADBlock.Intensity is documented as "really only 4-bit
// values" widened to uint16 for JSON - encoded here as raw big-endian
// uint16s, not re-derived or reinterpreted.
func fisbEncodeNexradPayload(b uatparse.NEXRADBlock) string {
	raw := make([]byte, len(b.Intensity)*2)
	for i, v := range b.Intensity {
		binary.BigEndian.PutUint16(raw[i*2:], v)
	}
	return base64.StdEncoding.EncodeToString(raw)
}
