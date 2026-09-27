package epaper

// This file defines the structured, display-ready status model behind the
// operating dashboard (docs/epaper-operating-dashboard.md). It is pure: no
// hardware, no clock reads, no I/O. Derive (dashboard_derive.go) builds a
// Dashboard from a Snapshot, and epaper_main renders it. Nothing in the
// model is a fixed sample string: every headline, detail line and overall
// subtitle is chosen from live subsystem state, and the subtitle is built
// from the same tile states it summarizes so the two can never disagree.

// Level is the coarse severity of one tile or of the whole screen. It only
// selects a pictogram treatment (for example a marker on a degraded tile);
// the words on the screen carry the meaning, so a one-bit panel never has
// to rely on shading to tell states apart.
type Level int

const (
	// LevelOK: the subsystem is working and its data is current.
	LevelOK Level = iota
	// LevelIdle: connected and healthy, but nothing has been received
	// (quiet airspace, no ground station in range). Never a fault.
	LevelIdle
	// LevelWarn: attention needed (no fix, stale, degraded).
	LevelWarn
	// LevelFault: the subsystem is missing or failing.
	LevelFault
	// LevelUnknown: current data for this subsystem cannot be confirmed.
	LevelUnknown
	// LevelOff: disabled by configuration; not a problem.
	LevelOff
)

// OverallState is the top banner's state. "ONLINE" means only that the
// Stratux service is running and answering with fresh data; it never
// asserts that GPS, either radio, FIS-B, or any client is working - the
// tiles and the subtitle say that.
type OverallState string

const (
	OverallStarting OverallState = "STARTING"
	OverallOnline   OverallState = "ONLINE"
	OverallDegraded OverallState = "DEGRADED"
	OverallFault    OverallState = "FAULT"
	// OverallNoData: the status source cannot be reached (or has stopped
	// updating), so nothing on the screen can be confirmed as current.
	OverallNoData OverallState = "NO DATA"
)

// Tile is one of the four subsystem tiles. Label is the fixed tile name
// (GPS, 1090 ADS-B, 978 UAT, FIS-B WX); Headline is the large state word;
// Detail is the small supporting line.
type Tile struct {
	Label    string
	Headline string
	Detail   string
	Level    Level
}

// Warning is one active device-health condition. Text is a short, fixed
// token that fits the header; Detail is longer, for the API/doc/tests.
type Warning struct {
	Text   string
	Detail string
}

// Dashboard is everything the operating screen shows.
type Dashboard struct {
	// Version is the running version and short build, e.g.
	// "2.0.0~rc2 b26686c" - taken from the running daemon, never fixed.
	Version string

	Overall    OverallState
	OverallTxt string // large banner text, e.g. "RECEIVER ONLINE"
	Subtitle   string // derived from the tile states below

	GPS, ES, UAT, FISB Tile

	Warnings []Warning

	// Clients is the footer's left text. UpdatedBasis says how the footer
	// time should be read; the time itself is stamped by the renderer at
	// the moment the frame is drawn (see Footer).
	Clients string
	Footer  Footer

	// Stale is set when current data cannot be confirmed; the panel shows
	// it prominently rather than leaving the last frame looking live.
	Stale bool
}

// TimeBasis says what the "UPDATED" footer time means.
type TimeBasis int

const (
	// BasisUTC: the device clock is trusted (GNSS or network synced); the
	// footer shows UTC ("12:42Z").
	BasisUTC TimeBasis = iota
	// BasisUptime: the device clock is not trusted; the footer shows time
	// since Stratux started ("UP 01:04"), never an untrusted clock time.
	BasisUptime
)

// Footer is the right-hand footer content. Text is the fully-formatted
// string ("UPDATED 12:42Z" or "UPDATED UP 01:04"), Basis records which
// basis was used so it can be compared and tested.
type Footer struct {
	Text  string
	Basis TimeBasis
}

// MaterialKey returns the string that decides whether the screen must be
// redrawn: everything visible except the footer's time. Two dashboards
// with the same key look identical apart from "UPDATED hh:mm", which must
// never by itself trigger a panel refresh.
func (d Dashboard) MaterialKey() string {
	k := string(d.Overall) + "|" + d.OverallTxt + "|" + d.Subtitle + "|" + d.Version + "|" + d.Clients + "|"
	for _, t := range []Tile{d.GPS, d.ES, d.UAT, d.FISB} {
		k += t.Label + "/" + t.Headline + "/" + t.Detail + "/" + string(rune('0'+int(t.Level))) + "|"
	}
	for _, w := range d.Warnings {
		k += "!" + w.Text
	}
	if d.Stale {
		k += "|STALE"
	}
	return k
}
