package fisbrecorder

import (
	"fmt"
	"strings"
)

// WeatherGDL90Report isolates FIS-B/weather-bearing GDL90 traffic (GDL90
// message ID 0x07, see GDL90MessageIDUplink) from the heartbeat/ownship/
// traffic-report noise that dominates a raw GDL90Comparison - see
// CompareWeatherGDL90's own doc comment for why an index-aligned
// comparison of EVERY packet (GDL90Comparison, compare.go) is misleading
// once sessions run different durations: the daemon keeps sending its
// own periodic non-weather output for as long as each session runs,
// independent of anything this fixture ever transmits.
type WeatherGDL90Report struct {
	DirA, DirB string

	// PerConnection is keyed by ConnectionKey (e.g. "192.168.10.22:4000")
	// - only connections that carried AT LEAST ONE weather-bearing frame
	// in either bundle appear here.
	PerConnection map[string]WeatherConnectionComparison

	// EFBConnectionKeys lists, of the keys in PerConnection, which ones
	// carry the NETWORK_GDL90_STANDARD capability bit (main/network.go) -
	// the actual GDL90-EFB-client port convention this fork uses (UDP
	// 4000 by default; see main/gen_gdl90.go's own default
	// NetworkOutputs). A weather-bearing frame sent to a connection NOT
	// in this list did not reach a GDL90-capable client - see
	// WeatherConnectionComparison.IsEFBCapable, computed from each
	// bundle's OWN recorded "settings" snapshot (this package has no
	// hardcoded assumption about which port number is used - it reads
	// each connection's real Capability bit from whichever
	// NetworkOutputs entry matches, per session).
	EFBConnectionKeys []string

	// ParseErrors records any GDL90Record whose Bytes could not be split
	// (SplitGDL90Frames returned an error) - a comparison that silently
	// skipped an unparseable record would understate loss; every one is
	// listed here by its bundle ("A"/"B"), ConnectionKey, and Seq.
	ParseErrors []string
}

// WeatherConnectionComparison is one connection's own weather-only tally.
type WeatherConnectionComparison struct {
	ConnectionKey string
	IsEFBCapable  bool // true if this connection's Capability includes NETWORK_GDL90_STANDARD in EITHER bundle's own settings snapshot

	CountA, CountB int

	// Matched/Mismatched are index-aligned comparisons within this
	// connection's OWN weather-only subsequence (never across
	// connections, never against non-weather traffic) - the Nth
	// weather-bearing UplinkPayload sent to this connection in dirA
	// against the Nth in dirB.
	Matched    int
	Mismatched []WeatherMismatch

	OnlyInA int // weather frames dirA sent to this connection beyond what dirB has
	OnlyInB int
}

// WeatherMismatch is one index-aligned pair whose UplinkPayload bytes
// differed - reported with enough detail to judge WHY (a truncated
// synthetic fixture, real corruption, or a genuine parser/relay
// regression) rather than just a count.
type WeatherMismatch struct {
	Index      int
	LenA, LenB int
	// FirstDiffOffset is the first byte offset at which the two payloads
	// differ, or -1 if one is a strict prefix of the other (lengths
	// differ but all shared bytes match).
	FirstDiffOffset int
}

// CompareWeatherGDL90 reads both bundles' gdl90.jsonl.gz, splits every
// record into its individual framed GDL90 messages (a single captured
// write commonly batches several - see SplitGDL90Frames), keeps only the
// weather-bearing ones (GDL90MessageIDUplink), and compares them per
// destination connection. This is the narrow, product-focused
// alternative to raw GDL90Comparison (compare.go) - heartbeats, ownship
// reports, and traffic reports never appear here at all, so a session-
// duration difference (which inflates non-weather traffic counts) cannot
// dilute this result the way it does the raw comparison.
//
// No normalization is applied anywhere in this function: UplinkPayload
// bytes are this fork's own verbatim relay of parseInput's decoded
// `frame` (main/gen_gdl90.go's relayMessage - see UplinkPayload's own
// doc comment), which is deterministic given identical input bytes and
// carries no per-send timestamp or counter of its own (unlike a
// heartbeat message) - a genuine byte-for-byte match is the expected,
// provable result for identical source frames, not something that needs
// masking to achieve.
func CompareWeatherGDL90(dirA, dirB string) (*WeatherGDL90Report, error) {
	gdl90A, err := readGDL90At(dirA)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading gdl90.jsonl.gz for dirA: %w", err)
	}
	gdl90B, err := readGDL90At(dirB)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: reading gdl90.jsonl.gz for dirB: %w", err)
	}
	efbKeysA, err := efbCapableConnectionKeys(dirA)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: determining EFB-capable connections for dirA: %w", err)
	}
	efbKeysB, err := efbCapableConnectionKeys(dirB)
	if err != nil {
		return nil, fmt.Errorf("fisbrecorder: determining EFB-capable connections for dirB: %w", err)
	}

	rep := &WeatherGDL90Report{DirA: dirA, DirB: dirB, PerConnection: map[string]WeatherConnectionComparison{}}

	byKeyA := map[string][][]byte{}
	for _, r := range gdl90A {
		frames, err := SplitGDL90Frames(r.Bytes)
		if err != nil {
			rep.ParseErrors = append(rep.ParseErrors, fmt.Sprintf("A %s seq=%d: %v", r.ConnectionKey, r.Seq, err))
			continue
		}
		for _, f := range frames {
			if p := f.UplinkPayload(); p != nil {
				byKeyA[r.ConnectionKey] = append(byKeyA[r.ConnectionKey], p)
			}
		}
	}
	byKeyB := map[string][][]byte{}
	for _, r := range gdl90B {
		frames, err := SplitGDL90Frames(r.Bytes)
		if err != nil {
			rep.ParseErrors = append(rep.ParseErrors, fmt.Sprintf("B %s seq=%d: %v", r.ConnectionKey, r.Seq, err))
			continue
		}
		for _, f := range frames {
			if p := f.UplinkPayload(); p != nil {
				byKeyB[r.ConnectionKey] = append(byKeyB[r.ConnectionKey], p)
			}
		}
	}

	// efbKeys are frequently port-only templates ("Ip":"" in an unbound
	// NetworkOutputs entry means "any client that connects here"), while
	// a GDL90Record's own ConnectionKey is always the real, connected
	// client address - matching by port suffix is the correct comparison
	// (see connectionKeyHasPortSuffix's own doc comment), not exact
	// string equality, which would silently never match anything for the
	// common unbound-template case.
	efbTemplates := append(append([]string{}, efbKeysA...), efbKeysB...)
	isEFBCapable := func(connKey string) bool {
		for _, tmpl := range efbTemplates {
			if strings.HasPrefix(tmpl, ":") {
				if connectionKeyHasPortSuffix(connKey, tmpl) {
					return true
				}
			} else if connKey == tmpl {
				return true
			}
		}
		return false
	}

	keySet := map[string]bool{}
	for k := range byKeyA {
		keySet[k] = true
	}
	for k := range byKeyB {
		keySet[k] = true
	}

	for key := range keySet {
		pa, pb := byKeyA[key], byKeyB[key]
		cc := WeatherConnectionComparison{
			ConnectionKey: key,
			IsEFBCapable:  isEFBCapable(key),
			CountA:        len(pa),
			CountB:        len(pb),
		}
		n := len(pa)
		if len(pb) < n {
			n = len(pb)
		}
		for i := 0; i < n; i++ {
			if string(pa[i]) == string(pb[i]) {
				cc.Matched++
			} else {
				cc.Mismatched = append(cc.Mismatched, WeatherMismatch{
					Index:           i,
					LenA:            len(pa[i]),
					LenB:            len(pb[i]),
					FirstDiffOffset: firstDiffOffset(pa[i], pb[i]),
				})
			}
		}
		if len(pa) > n {
			cc.OnlyInA = len(pa) - n
		}
		if len(pb) > n {
			cc.OnlyInB = len(pb) - n
		}
		rep.PerConnection[key] = cc
		if cc.IsEFBCapable {
			rep.EFBConnectionKeys = append(rep.EFBConnectionKeys, key)
		}
	}

	return rep, nil
}

func firstDiffOffset(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n // one is a strict prefix of the other
	}
	return -1
}

// efbCapableConnectionKeys reads a session's own "settings" snapshot
// (recordFISBRecorderSnapshots in main/fisbrecorderwiring.go captures
// globalSettings, which includes NetworkOutputs) and returns the
// ConnectionKey-shaped ("ip:port") strings for every configured UDP
// output whose Capability includes NETWORK_GDL90_STANDARD (bit 1) - the
// same bit main/network.go's sendMsg checks before ever queuing a GDL90
// message to a connection. Reading this from the bundle itself (not a
// hardcoded port number) means this stays correct if a future session
// configures a non-default port.
func efbCapableConnectionKeys(dir string) ([]string, error) {
	snaps, err := readSnapshotsAt(dir)
	if err != nil {
		return nil, err
	}
	const networkGDL90Standard = 1
	var keys []string
	for _, s := range snaps {
		if s.Label != "settings" {
			continue
		}
		data, ok := s.Data.(map[string]interface{})
		if !ok {
			continue
		}
		outputs, ok := data["NetworkOutputs"].([]interface{})
		if !ok {
			continue
		}
		for _, o := range outputs {
			m, ok := o.(map[string]interface{})
			if !ok {
				continue
			}
			cap, _ := m["Capability"].(float64)
			if int(cap)&networkGDL90Standard == 0 {
				continue
			}
			ip, _ := m["Ip"].(string)
			port, _ := m["Port"].(float64)
			if ip == "" {
				// This session's own default/unbound entries (Ip=="")
				// represent "any client that connects on this port" -
				// match by port suffix instead of a specific address.
				keys = append(keys, fmt.Sprintf(":%d", int(port)))
				continue
			}
			keys = append(keys, fmt.Sprintf("%s:%d", ip, int(port)))
		}
	}
	return keys, nil
}

// connectionKeyHasPortSuffix reports whether key (e.g.
// "192.168.10.22:4000") ends in the same ":<port>" as suffix (e.g.
// ":4000") - used because a live session's own recorded NetworkOutputs
// entries are often unbound (Ip=="") templates, while GDL90Record's own
// ConnectionKey values are the real, connected client address - matching
// by port is the correct comparison, not by full address equality.
func connectionKeyHasPortSuffix(key, suffix string) bool {
	return strings.HasSuffix(key, suffix)
}
