package epaper

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Thresholds are the explicit freshness windows behind every state the
// operating dashboard can show. They are deliberately different for each
// kind of data - a GPS fix, a ground-station uplink and a weather product
// have different natural cadences - and are documented, with the reason
// for each value, in docs/epaper-operating-dashboard.md.
type Thresholds struct {
	// SourceStaleAfter: /getStatus (the essential source) has not been
	// read successfully for this long -> the whole screen is NO DATA.
	// Four missed 5 s polls.
	SourceStaleAfter time.Duration
	// HeartbeatStallAfter: /getStatus keeps answering but its Uptime
	// field (advanced once a second by the daemon's own status loop) has
	// not moved for this long -> the daemon's status loop is wedged and
	// the values are frozen; treated exactly like an unreachable source.
	HeartbeatStallAfter time.Duration
	// AuxStaleAfter: a secondary source (/getHealth, /getTowers,
	// /getClients, /getPowerHealth) has not been read successfully for
	// this long -> what depends on it becomes unknown and a DATA warning
	// is shown, without declaring the receiver failed.
	AuxStaleAfter time.Duration
	// StartupGrace: while the Stratux daemon's own uptime is below this
	// the banner says STARTING instead of FAULT/DEGRADED (radios and GPS
	// legitimately take that long to appear).
	StartupGrace time.Duration
	// ReceiveActiveWithin: a band with a valid message in this window is
	// ACTIVE. Matches the daemon's own 60 s "receiving" window
	// (main/sdr.go receivingFreshness). Silence after this is QUIET, not a
	// fault: quiet airspace is normal.
	ReceiveActiveWithin time.Duration
	// UplinkRecent: a ground-station uplink was seen within this window.
	// Stations transmit about once a second when in range; two minutes
	// rides through a brief fade without flapping.
	UplinkRecent time.Duration
	// WXCurrentWithin / WXAgingWithin: age of the newest received weather
	// product frame. FIS-B weather products repeat on intervals of about
	// 5 minutes (METAR, radar, SIGMET) to 10 minutes (TAF, winds, PIREP,
	// NOTAM); CURRENT is inside one 5 minute cycle, AGING up to three,
	// and beyond that STALE. This is reception age, not the age of the
	// weather itself - see the limitations in the doc.
	WXCurrentWithin time.Duration
	WXAgingWithin   time.Duration
	// ClientHold: a client counts as connected until it has not been seen
	// awake for this long. The daemon flaps a device that answers pings but
	// has no app listening (awake ~5 s in ~30 s); 45 s rides through that
	// without a panel refresh per flap, and a real departure still shows
	// within a minute.
	ClientHold time.Duration
	// TempWarnC: CPU temperature at or above which a HOT warning shows
	// (the Raspberry Pi starts to throttle at 80 C).
	TempWarnC float64
}

// DefaultThresholds returns the documented values.
func DefaultThresholds() Thresholds {
	return Thresholds{
		SourceStaleAfter:    20 * time.Second,
		HeartbeatStallAfter: 15 * time.Second,
		AuxStaleAfter:       30 * time.Second,
		StartupGrace:        90 * time.Second,
		ReceiveActiveWithin: 60 * time.Second,
		UplinkRecent:        120 * time.Second,
		WXCurrentWithin:     5 * time.Minute,
		WXAgingWithin:       15 * time.Minute,
		ClientHold:          45 * time.Second,
		TempWarnC:           80,
	}
}

// BandData is one receiver band as reported by /getStatus.
type BandData struct {
	Enabled, Detected, Assigned, DecoderRunning bool
	Ambiguous, Conflict                         bool
	// ExternalConnected is the live external low-power UAT radio flag
	// (UATRadio_connected); always false for 1090.
	ExternalConnected bool
	// Total counts every valid message the band decoded; LastMinute is
	// the daemon's rolling 60 s count of the same.
	Total      uint64
	LastMinute uint
}

// ProductTotals are the daemon's FIS-B product-frame counters. They count
// decoded information frames (not distinct products) since the daemon
// started; the dashboard only ever uses their increases.
type ProductTotals struct {
	METAR, TAF, NEXRAD, SIGMET, PIREP, NOTAM, Other uint32
}

func (p ProductTotals) weather() uint64 {
	return uint64(p.METAR) + uint64(p.TAF) + uint64(p.NEXRAD) + uint64(p.SIGMET) + uint64(p.PIREP)
}
func (p ProductTotals) all() uint64 {
	return p.weather() + uint64(p.NOTAM) + uint64(p.Other)
}

// StatusData is what one successful /getStatus read yields.
type StatusData struct {
	Version, Build string
	UptimeMs       int64

	GPSConnected               bool
	GPSSolution                string
	GPSSatsLocked, GPSSatsSeen uint16
	UAT, ES                    BandData
	Products                   ProductTotals
}

// HealthData is what one successful /getHealth read yields (only the
// parts the dashboard uses).
type HealthData struct {
	CPUTempC       float64
	TimeState      string // readiness.TimeState verbatim
	FailedServices []string
}

// TowerData is what one successful /getTowers read yields.
type TowerData struct {
	Active int // towers with an uplink in the daemon's last-60 s window
	Known  int // every tower heard since the daemon started
}

// ClientData is what one successful /getClients read yields: the addresses
// of the network clients the daemon currently considers awake. A device on
// the network with nothing listening on the GDL90 port answers pings but
// draws an ICMP port-unreachable, so the daemon flips it awake for a few
// seconds out of every ~30 (seen on the bench with a laptop); presence is
// therefore judged over a hold time (Thresholds.ClientHold), not from one
// reading.
type ClientData struct {
	AwakeIPs []string
}

// ClientsOf returns ClientData with n distinct synthetic addresses, for
// tests and fixtures.
func ClientsOf(n int) *ClientData {
	c := &ClientData{}
	for i := 1; i <= n; i++ {
		c.AwakeIPs = append(c.AwakeIPs, fmt.Sprintf("192.0.2.%d", i))
	}
	return c
}

// PowerData is what one successful /getPowerHealth read yields: the
// Raspberry Pi's *current* under-voltage/throttle bits, not the sticky
// "has occurred since boot" ones.
type PowerData struct {
	UndervoltageNow, ThrottledNow bool
}

// Sample is one poll cycle's result: a nil member means that endpoint's
// read failed this cycle.
type Sample struct {
	At      time.Time
	Status  *StatusData
	Health  *HealthData
	Towers  *TowerData
	Clients *ClientData
	Power   *PowerData
}

type endpoint int

const (
	epStatus endpoint = iota
	epHealth
	epTowers
	epClients
	epPower
	epCount
)

var endpointNames = [epCount]string{"STATUS", "HEALTH", "TOWERS", "CLIENTS", "POWER"}

// Tracker turns a stream of Samples into the freshness history the
// dashboard needs (when a band last received, when weather last arrived,
// when an uplink was last seen, whether the daemon is still alive). It is
// pure: every method takes the time explicitly, so the whole thing is
// driven by synthetic clocks in tests and never reads the wall clock.
type Tracker struct {
	th        Thresholds
	startedAt time.Time

	status  StatusData
	health  HealthData
	towers  TowerData
	clients ClientData
	// clientSeen is when each client address was last seen awake.
	clientSeen map[string]time.Time
	power      PowerData
	okAt       [epCount]time.Time
	haveEP     [epCount]bool

	upMs         int64
	upAdvancedAt time.Time

	// satsShown is the satellite count the GPS tile displays. The real count
	// wanders by one or two every minute or so; redrawing the panel for each
	// wobble would spend refreshes on noise, so the shown count only follows
	// the real one when it differs by three or more, or crosses zero or the
	// four-satellite line that separates a usable solution from a poor one.
	satsShown uint16
	// seenShown is the same for the "N SAT SEEN" detail while there is no
	// fix (the count of satellites merely visible wanders far more, and
	// costs a refresh per wobble); it has no four-satellite line.
	seenShown uint16

	uatLastAt, esLastAt time.Time
	wxLastAt            time.Time
	uplinkLastAt        time.Time
}

// NewTracker starts a tracker at now.
func NewTracker(th Thresholds, now time.Time) *Tracker {
	return &Tracker{th: th, startedAt: now}
}

// Observe folds one poll cycle into the tracker.
func (t *Tracker) Observe(s Sample) {
	at := s.At
	if s.Status != nil {
		st := *s.Status
		// A restart of the Stratux daemon shows up as its uptime or any
		// counter going backwards: forget everything derived from the old
		// run so nothing from it is presented as current.
		if t.haveEP[epStatus] && (st.UptimeMs+1000 < t.status.UptimeMs ||
			st.UAT.Total < t.status.UAT.Total || st.ES.Total < t.status.ES.Total ||
			st.Products.all() < t.status.Products.all()) {
			t.resetHistory()
		}
		// Any change of the daemon's uptime is a sign of life - including
		// a fall to near zero, which is a restart, not a freeze.
		if !t.haveEP[epStatus] || st.UptimeMs != t.upMs {
			t.upAdvancedAt = at
		}
		t.upMs = st.UptimeMs
		if t.haveEP[epStatus] {
			if st.UAT.Total > t.status.UAT.Total {
				t.uatLastAt = at
			}
			if st.ES.Total > t.status.ES.Total {
				t.esLastAt = at
			}
			if st.Products.weather() > t.status.Products.weather() {
				t.wxLastAt = at
			}
			if st.Products.all() > t.status.Products.all() {
				t.uplinkLastAt = at
			}
		}
		t.satsShown = shownSats(t.satsShown, st.GPSSatsLocked, !t.haveEP[epStatus], true)
		t.seenShown = shownSats(t.seenShown, st.GPSSatsSeen, !t.haveEP[epStatus], false)
		t.status = st
		t.okAt[epStatus] = at
		t.haveEP[epStatus] = true
	}
	if s.Health != nil {
		t.health = *s.Health
		t.okAt[epHealth], t.haveEP[epHealth] = at, true
	}
	if s.Towers != nil {
		t.towers = *s.Towers
		if s.Towers.Active > 0 {
			t.uplinkLastAt = at
		}
		t.okAt[epTowers], t.haveEP[epTowers] = at, true
	}
	if s.Clients != nil {
		t.clients = *s.Clients
		if t.clientSeen == nil {
			t.clientSeen = map[string]time.Time{}
		}
		for _, ip := range s.Clients.AwakeIPs {
			t.clientSeen[ip] = at
		}
		for ip, seen := range t.clientSeen { // forget long-gone addresses
			if at.Sub(seen) > 10*t.th.ClientHold {
				delete(t.clientSeen, ip)
			}
		}
		t.okAt[epClients], t.haveEP[epClients] = at, true
	}
	if s.Power != nil {
		t.power = *s.Power
		t.okAt[epPower], t.haveEP[epPower] = at, true
	}
}

// StatusAge is the age of the newest successful /getStatus reading.
func (t *Tracker) StatusAge(now time.Time) (time.Duration, bool) {
	if !t.haveEP[epStatus] {
		return 0, false
	}
	return now.Sub(t.okAt[epStatus]), true
}

// shownSats applies the satellite-count hysteresis described on satsShown.
func shownSats(shown, actual uint16, first, fourLine bool) uint16 {
	if first {
		return actual
	}
	diff := int(actual) - int(shown)
	if diff < 0 {
		diff = -diff
	}
	if diff >= 3 || (actual == 0) != (shown == 0) || (fourLine && (actual >= 4) != (shown >= 4)) {
		return actual
	}
	return shown
}

func (t *Tracker) resetHistory() {
	t.uatLastAt, t.esLastAt, t.wxLastAt, t.uplinkLastAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
}

func (t *Tracker) fresh(ep endpoint, now time.Time, limit time.Duration) bool {
	return t.haveEP[ep] && now.Sub(t.okAt[ep]) <= limit
}

// statusFrozen reports that readings keep arriving but the daemon's own
// uptime, advanced once a second by its status loop, has not moved for
// longer than HeartbeatStallAfter: the loop is wedged and every value in
// the readings is frozen.
func (t *Tracker) statusFrozen() bool {
	return t.haveEP[epStatus] && t.okAt[epStatus].Sub(t.upAdvancedAt) > t.th.HeartbeatStallAfter
}

// statusFresh reports whether the essential source is both reachable and
// still advancing.
func (t *Tracker) statusFresh(now time.Time) bool {
	return t.fresh(epStatus, now, t.th.SourceStaleAfter) && !t.statusFrozen()
}

// agoText renders an age as a coarse upper bound ("<5 MIN AGO"), never a
// ticking number: the panel must not need a refresh every minute just
// because an age moved on, and a bucket is what the freshness rules
// actually distinguish. The bounds line up with the documented windows.
func agoText(d time.Duration) string {
	switch {
	case d < 2*time.Minute:
		return "<2 MIN AGO"
	case d < 5*time.Minute:
		return "<5 MIN AGO"
	case d < 15*time.Minute:
		return "<15 MIN AGO"
	case d < 30*time.Minute:
		return "<30 MIN AGO"
	case d < time.Hour:
		return "<1 HR AGO"
	default:
		return "1 HR+ AGO"
	}
}

// durText is a coarse lower bound on a silence (">5 MIN"), bucketed for
// the same reason as agoText.
func durText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return ">20 S"
	case d < 5*time.Minute:
		return ">1 MIN"
	case d < 30*time.Minute:
		return ">5 MIN"
	case d < time.Hour:
		return ">30 MIN"
	default:
		return ">1 HR"
	}
}

func upText(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	m := int(ms / 60000)
	return fmt.Sprintf("%02d:%02d", m/60, m%60)
}

// Derive builds the Dashboard as of now. wall is the device's wall-clock
// reading (used only when the daemon reports the clock as trusted).
func (t *Tracker) Derive(now, wall time.Time) Dashboard {
	d := Dashboard{}

	// Footer time basis: only ever show a clock time when the daemon says
	// the device clock is trusted (GNSS or network synced).
	trusted := t.fresh(epHealth, now, t.th.AuxStaleAfter) &&
		(t.health.TimeState == "GNSS_SYNCED" || t.health.TimeState == "NETWORK_SYNCED")
	upNow := t.status.UptimeMs + int64(now.Sub(t.okAt[epStatus])/time.Millisecond)
	if trusted {
		d.Footer = Footer{Text: "UPDATED " + wall.UTC().Format("15:04") + "Z", Basis: BasisUTC}
	} else if t.haveEP[epStatus] {
		d.Footer = Footer{Text: "UPDATED UP " + upText(upNow), Basis: BasisUptime}
	} else {
		d.Footer = Footer{Text: "UPDATED CLOCK ?", Basis: BasisUptime}
	}

	// Version comes only from the running daemon.
	if t.haveEP[epStatus] && t.status.Version != "" {
		d.Version = t.status.Version
		if b := t.status.Build; b != "" {
			if len(b) > 7 {
				b = b[:7]
			}
			d.Version += " " + b
		}
	} else {
		d.Version = "VERSION ?"
	}

	if !t.statusFresh(now) {
		return t.deriveNoStatus(d, now)
	}

	st := t.status
	auxOK := func(ep endpoint) bool { return t.fresh(ep, now, t.th.AuxStaleAfter) }
	upDur := time.Duration(upNow) * time.Millisecond

	// ---- tiles ----
	gps, gpsTok := t.gpsTile(st)
	es, esTok := t.esTile(now, st)
	uat, uatTok := t.uatTile(now, st)
	// While the daemon is still inside its startup grace, a subsystem that
	// is not up yet is "starting", not failed: no fault glyphs for things
	// that simply have not come up.
	starting := upDur < t.th.StartupGrace
	uatDown := uat.Level == LevelFault || uat.Level == LevelOff
	if starting {
		// (A band reads "disabled" until the daemon has evaluated its
		// radios at startup - seen for ~90 s on the bench - so during the
		// grace period "off" is also "not known yet", not a setting.)
		startup := func(tile *Tile, tok *string, name string) {
			if tile.Level == LevelFault || tile.Level == LevelOff {
				tile.Headline, tile.Detail, tile.Level = "STARTING", "NOT READY YET", LevelIdle
				*tok = name + " STARTING"
			}
		}
		startup(&gps, &gpsTok, "GPS")
		startup(&es, &esTok, "1090")
		startup(&uat, &uatTok, "978")
	}
	fisb := t.fisbTile(now, st, uat, auxOK(epTowers))
	if starting && uatDown {
		fisb = Tile{Label: "FIS-B WX", Headline: "STARTING", Detail: "WAITING FOR 978", Level: LevelIdle}
	}
	d.GPS, d.ES, d.UAT, d.FISB = gps, es, uat, fisb

	// ---- warnings ----
	if auxOK(epPower) {
		if t.power.UndervoltageNow {
			d.Warnings = append(d.Warnings, Warning{"UNDERVOLT", "the Raspberry Pi reports under-voltage right now"})
		} else if t.power.ThrottledNow {
			d.Warnings = append(d.Warnings, Warning{"THROTTLED", "the Raspberry Pi reports CPU throttling right now"})
		}
	}
	if auxOK(epHealth) {
		if t.health.CPUTempC >= t.th.TempWarnC {
			d.Warnings = append(d.Warnings, Warning{fmt.Sprintf("HOT %.0fC", t.health.CPUTempC), "CPU temperature is at or above the warning threshold"})
		}
		if len(t.health.FailedServices) > 0 {
			names := append([]string(nil), t.health.FailedServices...)
			sort.Strings(names)
			d.Warnings = append(d.Warnings, Warning{"SVC FAIL", "failed systemd units: " + strings.Join(names, ", ")})
		}
	}
	var staleAux []string
	for _, ep := range []endpoint{epHealth, epTowers, epClients, epPower} {
		if !auxOK(ep) {
			staleAux = append(staleAux, endpointNames[ep])
		}
	}
	if len(staleAux) > 0 {
		d.Warnings = append(d.Warnings, Warning{"DATA", "status sources unavailable: " + strings.Join(staleAux, ", ")})
	}

	// ---- clients ----
	if auxOK(epClients) {
		n := 0
		for _, seen := range t.clientSeen {
			if now.Sub(seen) <= t.th.ClientHold {
				n++
			}
		}
		switch n {
		case 0:
			d.Clients = "NO CLIENTS CONNECTED"
		case 1:
			d.Clients = "1 CLIENT CONNECTED"
		default:
			d.Clients = fmt.Sprintf("%d CLIENTS CONNECTED", n)
		}
	} else {
		d.Clients = "CLIENTS UNKNOWN"
	}

	// ---- overall ----
	// The subtitle is built from the very tokens the tiles were built
	// from, so it cannot contradict them.
	d.Subtitle = gpsTok + " • " + esTok + " • " + uatTok
	switch {
	case starting:
		d.Overall, d.OverallTxt = OverallStarting, "STARTING"
	case anyLevel(LevelFault, gps, es, uat):
		d.Overall, d.OverallTxt = OverallFault, "RECEIVER FAULT"
	case anyLevel(LevelWarn, gps, es, uat) || anyLevel(LevelUnknown, gps, es, uat) || len(d.Warnings) > 0:
		d.Overall, d.OverallTxt = OverallDegraded, "RECEIVER DEGRADED"
	default:
		d.Overall, d.OverallTxt = OverallOnline, "RECEIVER ONLINE"
	}
	return d
}

func anyLevel(l Level, tiles ...Tile) bool {
	for _, t := range tiles {
		if t.Level == l {
			return true
		}
	}
	return false
}

// deriveNoStatus is the screen when the essential source cannot be
// confirmed current. Every tile is UNKNOWN; nothing from the last good
// sample is shown as if it were live.
func (t *Tracker) deriveNoStatus(d Dashboard, now time.Time) Dashboard {
	unk := func(label string) Tile {
		return Tile{Label: label, Headline: "UNKNOWN", Detail: "NO CURRENT DATA", Level: LevelUnknown}
	}
	d.GPS, d.ES, d.UAT, d.FISB = unk("GPS"), unk("1090 ADS-B"), unk("978 UAT"), unk("FIS-B WX")
	d.Clients = "CLIENTS UNKNOWN"
	if !t.haveEP[epStatus] && now.Sub(t.startedAt) <= t.th.SourceStaleAfter {
		// Still waiting for the very first reading after the display
		// service itself started.
		d.Overall, d.OverallTxt, d.Subtitle = OverallStarting, "STARTING", "WAITING FOR STRATUX STATUS"
		return d
	}
	d.Stale = true
	d.Overall, d.OverallTxt = OverallNoData, "NO STATUS DATA"
	switch {
	case !t.haveEP[epStatus]:
		d.Subtitle = "STRATUX STATUS UNAVAILABLE"
	case t.statusFrozen() && now.Sub(t.okAt[epStatus]) <= t.th.SourceStaleAfter:
		d.Subtitle = "STATUS STOPPED UPDATING • TILES NOT CURRENT"
	default:
		d.Subtitle = "NO DATA FOR " + durText(now.Sub(t.okAt[epStatus])) + " • TILES NOT CURRENT"
	}
	return d
}

// ---- tiles ----

func (t *Tracker) gpsTile(st StatusData) (Tile, string) {
	tile := Tile{Label: "GPS"}
	switch {
	case !st.GPSConnected || st.GPSSolution == "Disconnected":
		tile.Headline, tile.Detail, tile.Level = "NO GPS", "DISCONNECTED", LevelFault
		return tile, "GPS OFFLINE"
	case st.GPSSolution == "3D GPS" || st.GPSSolution == "3D GPS + SBAS":
		sbas := ""
		if st.GPSSolution == "3D GPS + SBAS" {
			sbas = " SBAS"
		}
		switch {
		case st.GPSSatsLocked >= 4:
			tile.Headline, tile.Detail, tile.Level = "3D FIX", fmt.Sprintf("%d SAT%s", t.satsShown, sbas), LevelOK
			return tile, "GPS FIX"
		case st.GPSSatsLocked > 0:
			// The daemon's fix flag cannot tell 2D from 3D; fewer than
			// four satellites cannot be a 3D solution, so do not say so.
			tile.Headline, tile.Detail, tile.Level = "FIX", fmt.Sprintf("%d SAT • LOW", st.GPSSatsLocked), LevelWarn
			return tile, "GPS FIX LOW"
		default:
			tile.Headline, tile.Detail, tile.Level = "FIX", "SAT COUNT ?", LevelWarn
			return tile, "GPS FIX ?"
		}
	case st.GPSSolution == "Dead Reckoning":
		tile.Headline, tile.Detail, tile.Level = "DEAD RECK", "NO SATELLITE FIX", LevelWarn
		return tile, "GPS DR ONLY"
	default: // "No Fix", "Unknown", ""
		detail := "SEARCHING"
		if st.GPSSatsSeen > 0 {
			detail = fmt.Sprintf("%d SAT SEEN", t.seenShown)
		}
		tile.Headline, tile.Detail, tile.Level = "NO FIX", detail, LevelWarn
		return tile, "NO GPS FIX"
	}
}

// bandConnected classifies the hardware side of a band, independent of
// whether anything has been received.
type bandHW int

const (
	hwOff bandHW = iota
	hwMissing
	hwNotRunning
	hwUp
)

func esHW(b BandData) bandHW {
	switch {
	case !b.Enabled:
		return hwOff
	case !b.Detected || !b.Assigned || b.Ambiguous || b.Conflict:
		return hwMissing
	case !b.DecoderRunning:
		return hwNotRunning
	default:
		return hwUp
	}
}

func uatHW(b BandData) bandHW {
	switch {
	case !b.Enabled:
		return hwOff
	case b.ExternalConnected:
		return hwUp // the live external-radio flag wins over the SDR fields
	case !b.Detected || !b.Assigned || b.Ambiguous || b.Conflict:
		return hwMissing
	case !b.DecoderRunning:
		return hwNotRunning
	default:
		return hwUp
	}
}

func (t *Tracker) bandTile(now time.Time, label, tok string, b BandData, hw bandHW, lastAt time.Time, activeText string) (Tile, string) {
	tile := Tile{Label: label}
	switch hw {
	case hwOff:
		tile.Headline, tile.Detail, tile.Level = "OFF", "DISABLED IN SETTINGS", LevelOff
		return tile, tok + " OFF"
	case hwMissing:
		tile.Headline, tile.Detail, tile.Level = "NO RADIO", "NOT DETECTED", LevelFault
		return tile, tok + " DOWN"
	case hwNotRunning:
		tile.Headline, tile.Detail, tile.Level = "NOT RUNNING", "DECODER STOPPED", LevelFault
		return tile, tok + " DOWN"
	}
	activeNow := b.LastMinute > 0 || (!lastAt.IsZero() && now.Sub(lastAt) <= t.th.ReceiveActiveWithin)
	switch {
	case activeNow:
		tile.Headline, tile.Detail, tile.Level = "ACTIVE", activeText, LevelOK
		return tile, tok + " ACTIVE"
	case !lastAt.IsZero():
		tile.Headline, tile.Detail, tile.Level = "QUIET", "LAST MSG "+agoText(now.Sub(lastAt)), LevelIdle
		return tile, tok + " QUIET"
	case b.Total > 0:
		tile.Headline, tile.Detail, tile.Level = "QUIET", "NONE IN LAST MIN", LevelIdle
		return tile, tok + " QUIET"
	default:
		tile.Headline, tile.Detail, tile.Level = "CONNECTED", "NO MESSAGES YET", LevelIdle
		return tile, tok + " NO MSGS"
	}
}

func (t *Tracker) esTile(now time.Time, st StatusData) (Tile, string) {
	return t.bandTile(now, "1090 ADS-B", "1090", st.ES, esHW(st.ES), t.esLastAt, "TRAFFIC RECEIVED")
}

func (t *Tracker) uatTile(now time.Time, st StatusData) (Tile, string) {
	return t.bandTile(now, "978 UAT", "978", st.UAT, uatHW(st.UAT), t.uatLastAt, "MESSAGES RECEIVED")
}

func (t *Tracker) fisbTile(now time.Time, st StatusData, uat Tile, towersFresh bool) Tile {
	tile := Tile{Label: "FIS-B WX"}
	switch uat.Level {
	case LevelOff:
		tile.Headline, tile.Detail, tile.Level = "OFF", "978 DISABLED", LevelOff
		return tile
	case LevelFault:
		tile.Headline, tile.Detail, tile.Level = "NO RECEIVER", "978 RADIO DOWN", LevelFault
		return tile
	}

	uplinkNow := (towersFresh && t.towers.Active > 0) ||
		(!t.uplinkLastAt.IsZero() && now.Sub(t.uplinkLastAt) <= t.th.UplinkRecent)
	wxSeen := !t.wxLastAt.IsZero()
	wxAge := now.Sub(t.wxLastAt)

	switch {
	case wxSeen && wxAge <= t.th.WXCurrentWithin:
		tile.Headline, tile.Detail, tile.Level = "WX CURRENT", "NEWEST "+agoText(wxAge), LevelOK
	case wxSeen && wxAge <= t.th.WXAgingWithin:
		tile.Headline, tile.Detail, tile.Level = "WX AGING", "NEWEST "+agoText(wxAge), LevelWarn
	case wxSeen:
		tile.Headline, tile.Detail, tile.Level = "WX STALE", "NEWEST "+agoText(wxAge), LevelWarn
	case st.Products.weather() > 0:
		// Products were decoded before this display service started
		// watching; their age is unknowable, so they are not shown as
		// current - and not as stale either.
		if uplinkNow {
			tile.Headline, tile.Detail, tile.Level = "UPLINK", "WX AGE UNKNOWN", LevelIdle
		} else {
			tile.Headline, tile.Detail, tile.Level = "NO UPLINK NOW", "WX AGE UNKNOWN", LevelIdle
		}
	case uplinkNow:
		tile.Headline, tile.Detail, tile.Level = "UPLINK", "NO WX PRODUCTS YET", LevelIdle
	case !towersFresh:
		tile.Headline, tile.Detail, tile.Level = "UNKNOWN", "UPLINK STATUS ?", LevelUnknown
	case !t.uplinkLastAt.IsZero():
		tile.Headline, tile.Detail, tile.Level = "UPLINK LOST", "LAST "+agoText(now.Sub(t.uplinkLastAt)), LevelWarn
	case t.towers.Known > 0:
		tile.Headline, tile.Detail, tile.Level = "NO UPLINK", "NONE IN LAST MIN", LevelIdle
	default:
		tile.Headline, tile.Detail, tile.Level = "NO UPLINK", "AWAITING GROUND STATION", LevelIdle
	}
	return tile
}
