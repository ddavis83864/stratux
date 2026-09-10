// Package wifiadmin adds strict validation, safe two-step preview/apply/
// confirm/rollback, and crash recovery on top of this project's existing
// Wi-Fi configuration surface (main/networksettings.go's WiFi* fields on
// globalSettings). It is a pure, deterministic package: no networking,
// filesystem, hardware, global state, or wall-clock access - every
// input, including the current time, is passed in explicitly by the
// caller (main/wifiadminapi.go), and every dependency that touches the
// outside world (persistence, service execution, health observation, a
// clock, a token generator) is an injected interface. See
// docs/wifi-administration-hardening.md for the full design.
//
// # Why this package exists
//
// Today's Wi-Fi settings API (POST /setSettings) is a direct, single-
// step, unconfirmed write: it mutates globalSettings, saves it, and
// kicks off an async ifdown/rewrite-configs/ifup cycle whose result is
// never reported back to the client, with no backup of the prior
// working configuration and no way to know a bad value locked out the
// very device configuring it. This package adds preview, a short-lived
// confirmation token bound to the exact previewed configuration,
// mandatory reconnection confirmation after apply, and automatic
// rollback to the last confirmed-good configuration if that
// confirmation never arrives - without changing the existing /setSettings
// endpoint or any deployed device's current behavior until an owner
// explicitly starts a new transaction through this package's own API.
//
// # Non-goals
//
// This is not a general network manager, not a captive portal, not a
// cloud/remote administration surface, and it does not add a new
// security capability (WPA3, protected management frames, client
// isolation, firewall enforcement) beyond what main/networksettings.go
// and the underlying wpa_supplicant/dnsmasq stack already provide -
// those depend on the actual hardware, driver, and client compatibility
// and are never claimed here.
package wifiadmin

import "net"

// Mode mirrors main/networksettings.go's WifiMode* constants
// (WifiModeAp=0/WifiModeDirect=1/WifiModeApClient=2) - redefined here,
// not imported, to keep this package's dependency graph limited to the
// Go standard library. main/wifiadminapi.go is responsible for keeping
// these numerically identical to the existing constants; a test in that
// package asserts it.
type Mode int

const (
	ModeAP       Mode = 0
	ModeDirect   Mode = 1
	ModeAPClient Mode = 2
)

// MaxClientNetworks bounds how many saved client-mode networks a single
// configuration may carry - existing behavior has no such bound (an
// unbounded []wifiClientNetwork slice); this is a new, conservative cap
// so a malicious or malformed request cannot grow the persisted
// configuration without limit.
const MaxClientNetworks = 10

// ClientNetwork is one saved client-mode (AP+Client / station) network -
// the same {SSID, Password} shape as main/networksettings.go's
// wifiClientNetwork, redefined here for the same dependency-isolation
// reason as Mode.
type ClientNetwork struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
}

// Config is the complete, validated Wi-Fi configuration this package
// manages - the fields of globalSettings that are actually
// connectivity-affecting. SchemaVersion follows this project's own
// checksum/schema-version convention (see configbackup) for future
// evolution.
type Config struct {
	SchemaVersion int `json:"schemaVersion"`

	SSID            string `json:"ssid"`
	SecurityEnabled bool   `json:"securityEnabled"`
	// Passphrase must be empty when SecurityEnabled is false, and 8-63
	// printable-ASCII characters when true - see Validate. Never
	// serialized back to a client in any status/diagnostics response;
	// only Config itself (held server-side) and a Redacted copy exist.
	Passphrase string `json:"passphrase,omitempty"`
	Channel    int    `json:"channel"`
	// Country is a 2-letter, uppercase ISO-3166-1 alpha-2 code, format-
	// checked only - see Validate's own doc comment on why this package
	// deliberately does not claim to validate channel/country regulatory
	// legality.
	Country string `json:"country"`
	Mode    Mode   `json:"mode"`
	// IPAddress is the AP interface's own static address (existing
	// default 192.168.10.1) - the DHCP range is always derived from it,
	// exactly matching main/networksettings.go's existing
	// applyNetworkSettings convention, never separately configurable.
	IPAddress                  string          `json:"ipAddress"`
	ClientNetworks             []ClientNetwork `json:"clientNetworks,omitempty"`
	InternetPassThroughEnabled bool            `json:"internetPassThroughEnabled"`
	DirectPin                  string          `json:"directPin,omitempty"`
}

// CurrentSchemaVersion is this package's own settings-shape version.
const CurrentSchemaVersion = 1

// Redacted returns a copy of c with Passphrase and every ClientNetwork's
// Password replaced by a presence marker - "set"/"" - never the actual
// secret. Used for every status/preview/diagnostics response; Config
// itself (with real secrets) must never cross an HTTP boundary except as
// the write-only body of a preview request.
type Redacted struct {
	SchemaVersion              int                     `json:"schemaVersion"`
	SSID                       string                  `json:"ssid"`
	SecurityEnabled            bool                    `json:"securityEnabled"`
	PassphraseSet              bool                    `json:"passphraseSet"`
	Channel                    int                     `json:"channel"`
	Country                    string                  `json:"country"`
	Mode                       Mode                    `json:"mode"`
	IPAddress                  string                  `json:"ipAddress"`
	ClientNetworks             []RedactedClientNetwork `json:"clientNetworks,omitempty"`
	InternetPassThroughEnabled bool                    `json:"internetPassThroughEnabled"`
	DirectPinSet               bool                    `json:"directPinSet"`
}

// RedactedClientNetwork is one saved client network with its password
// replaced by a presence flag.
type RedactedClientNetwork struct {
	SSID        string `json:"ssid"`
	PasswordSet bool   `json:"passwordSet"`
}

// Redact never returns an error - a Config that failed Validate can
// still be redacted for display (e.g. in a rejected-preview error
// response that echoes back what was rejected).
func (c Config) Redact() Redacted {
	r := Redacted{
		SchemaVersion:              c.SchemaVersion,
		SSID:                       c.SSID,
		SecurityEnabled:            c.SecurityEnabled,
		PassphraseSet:              c.Passphrase != "",
		Channel:                    c.Channel,
		Country:                    c.Country,
		Mode:                       c.Mode,
		IPAddress:                  c.IPAddress,
		InternetPassThroughEnabled: c.InternetPassThroughEnabled,
		DirectPinSet:               c.DirectPin != "",
	}
	if len(c.ClientNetworks) > 0 {
		r.ClientNetworks = make([]RedactedClientNetwork, len(c.ClientNetworks))
		for i, n := range c.ClientNetworks {
			r.ClientNetworks[i] = RedactedClientNetwork{SSID: n.SSID, PasswordSet: n.Password != ""}
		}
	}
	return r
}

// DefaultConfig returns this project's existing, already-deployed
// default Wi-Fi configuration (main/gen_gdl90.go's defaultSettings) -
// used only as a fallback when no configuration has ever been persisted
// through this package yet (a fresh install, or an install that has
// never used this feature). It must never differ from the values a
// pre-existing device is already running, so that adopting this package
// on an existing installation changes nothing until an owner explicitly
// starts a transaction.
func DefaultConfig() Config {
	return Config{
		SchemaVersion:   CurrentSchemaVersion,
		SSID:            "stratux",
		SecurityEnabled: false,
		Passphrase:      "",
		Channel:         1,
		Country:         "",
		Mode:            ModeAP,
		IPAddress:       "192.168.10.1",
		ClientNetworks:  nil,
	}
}

// ValidationError names exactly which field failed and why - never a
// generic "invalid configuration," matching this project's own
// established alerting.Config/trafficcpa.Config error-reporting
// convention.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return "wifiadmin: invalid " + e.Field + ": " + e.Reason
}

// validChannels is the exhaustive set of channels
// debian/wpa_supplicant_ap.conf.template actually maps to a frequency
// (1-11, 2.4GHz). A channel outside this set is REJECTED here rather
// than silently falling back to channel 1 the way the existing template
// does today (its {{else}} branch) - an explicit, honest failure instead
// of a silent default is the whole point of this package.
var validChannels = map[int]bool{
	1: true, 2: true, 3: true, 4: true, 5: true, 6: true,
	7: true, 8: true, 9: true, 10: true, 11: true,
}

// ssidAllowedChars matches the character set main's own client-side
// validator already uses (web/plates/js/settings.js's isValidSSID:
// alphanumeric plus ()!  ._'-  and space) - reproduced here so the same
// rule is finally enforced server-side too, not just in a browser that a
// direct API call can trivially bypass.
func ssidAllowed(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	}
	switch r {
	case '(', ')', '!', ' ', '.', '_', '\'', '-':
		return true
	}
	return false
}

// Validate checks every field of c in isolation and reports the FIRST
// failure found - see docs/wifi-administration-hardening.md's
// "Validation rules" section for the complete, exhaustive list this
// implements and the rationale for each bound. Nothing here reaches a
// shell command or an unescaped template hole directly; Validate exists
// specifically so no value that could corrupt
// wpa_supplicant_ap.conf/wpa_supplicant.conf/stratux-dnsmasq.conf/
// interfaces (via the existing text/template, which does not
// HTML/shell-escape) is ever accepted in the first place.
//
// # Country/channel regulatory legality - an explicit, disclosed limit
//
// This project has no authoritative regulatory database mapping country
// codes to legal channels/power levels. Validate therefore only checks
// Country's FORMAT (two uppercase ASCII letters) and Channel's
// membership in the fixed set the existing template already supports
// (1-11) - it does NOT verify that a given channel is legal to operate
// on on Wi-Fi hardware regulatory-configured for the given country. This
// is a real, disclosed limitation, not an oversight - see "Known
// limitations" in the design doc.
func (c Config) Validate() error {
	if err := validateSSID(c.SSID); err != nil {
		return err
	}
	if err := validatePassphrase(c.SecurityEnabled, c.Passphrase); err != nil {
		return err
	}
	if !validChannels[c.Channel] {
		return &ValidationError{Field: "channel", Reason: "must be one of 1-11 (the only channels this project's own AP template maps to a frequency)"}
	}
	if err := validateCountry(c.Country); err != nil {
		return err
	}
	if c.Mode != ModeAP && c.Mode != ModeDirect && c.Mode != ModeAPClient {
		return &ValidationError{Field: "mode", Reason: "must be 0 (AP), 1 (Wi-Fi Direct), or 2 (AP+Client)"}
	}
	if err := validateAPAddress(c.IPAddress); err != nil {
		return err
	}
	if len(c.ClientNetworks) > MaxClientNetworks {
		return &ValidationError{Field: "clientNetworks", Reason: "too many saved networks (max " + itoa(MaxClientNetworks) + ")"}
	}
	seen := make(map[string]bool, len(c.ClientNetworks))
	for i, n := range c.ClientNetworks {
		if err := validateSSID(n.SSID); err != nil {
			return &ValidationError{Field: "clientNetworks[" + itoa(i) + "].ssid", Reason: err.(*ValidationError).Reason}
		}
		if seen[n.SSID] {
			return &ValidationError{Field: "clientNetworks[" + itoa(i) + "].ssid", Reason: "duplicate SSID among saved client networks"}
		}
		seen[n.SSID] = true
		// A client network's password is either empty (open network) or
		// a valid WPA passphrase - the same bounds as the AP's own,
		// reusing validatePassphrase with securityEnabled inferred from
		// non-empty.
		if err := validatePassphrase(n.Password != "", n.Password); err != nil {
			return &ValidationError{Field: "clientNetworks[" + itoa(i) + "].password", Reason: err.(*ValidationError).Reason}
		}
	}
	if c.DirectPin != "" {
		if err := validateDirectPin(c.DirectPin); err != nil {
			return err
		}
	}
	return nil
}

func validateSSID(ssid string) error {
	if ssid == "" {
		return &ValidationError{Field: "ssid", Reason: "must not be empty"}
	}
	// IEEE 802.11 SSIDs are at most 32 OCTETS - checked in bytes, not
	// runes, since a multi-byte UTF-8 SSID could pass a rune-count check
	// while still overflowing the actual wire field. This package
	// additionally restricts SSID to a known-safe ASCII subset below, so
	// this byte/rune distinction is mostly defense in depth.
	if len(ssid) > 32 {
		return &ValidationError{Field: "ssid", Reason: "must be at most 32 bytes"}
	}
	for _, r := range ssid {
		if !ssidAllowed(r) {
			return &ValidationError{Field: "ssid", Reason: "contains an unsupported character - only letters, digits, spaces, and ()!._'- are allowed"}
		}
	}
	return nil
}

func validatePassphrase(securityEnabled bool, passphrase string) error {
	if !securityEnabled {
		if passphrase != "" {
			return &ValidationError{Field: "passphrase", Reason: "must be empty when security is disabled"}
		}
		return nil
	}
	if len(passphrase) < 8 || len(passphrase) > 63 {
		return &ValidationError{Field: "passphrase", Reason: "must be 8-63 characters when security is enabled"}
	}
	for _, r := range passphrase {
		if r < 0x20 || r > 0x7e {
			return &ValidationError{Field: "passphrase", Reason: "must contain only printable ASCII characters"}
		}
	}
	return nil
}

func validateCountry(country string) error {
	if country == "" {
		return nil // unset is allowed - matches existing optional {{if .WiFiCountry}} template behavior
	}
	if len(country) != 2 {
		return &ValidationError{Field: "country", Reason: "must be a 2-letter ISO-3166-1 alpha-2 code (format only - not regulatory-validated, see Validate's own doc comment)"}
	}
	for _, r := range country {
		if r < 'A' || r > 'Z' {
			return &ValidationError{Field: "country", Reason: "must be two uppercase ASCII letters"}
		}
	}
	return nil
}

// validateAPAddress checks IPAddress is a usable IPv4 host address for
// the AP interface, in the exact fixed-/24 scheme
// main/networksettings.go's applyNetworkSettings already assumes (no
// separate, user-settable netmask exists today - the DHCP range is
// always derived from the first three octets, see DerivedDHCPRange).
// Rejects: unparseable input, IPv6, multicast, loopback, unspecified
// (0.0.0.0), and a last octet of 0 or 255 (the /24 network/broadcast
// addresses, which the existing code does not itself reject - see
// DerivedDHCPRange's own doc comment for why every other host value is
// already guaranteed collision-free against the derived DHCP range by
// construction).
func validateAPAddress(s string) error {
	ip := net.ParseIP(s)
	if ip == nil {
		return &ValidationError{Field: "ipAddress", Reason: "must be a valid IPv4 address"}
	}
	v4 := ip.To4()
	if v4 == nil {
		return &ValidationError{Field: "ipAddress", Reason: "must be IPv4"}
	}
	if v4.IsMulticast() || v4.IsLoopback() || v4.IsUnspecified() {
		return &ValidationError{Field: "ipAddress", Reason: "must not be multicast, loopback, or unspecified"}
	}
	last := v4[3]
	if last == 0 || last == 255 {
		return &ValidationError{Field: "ipAddress", Reason: "must not be a network or broadcast address (last octet 0 or 255) in its /24"}
	}
	return nil
}

// DerivedDHCPRange returns the exact DHCP pool
// main/networksettings.go's applyNetworkSettings already derives for a
// given AP address - reproduced here (not imported, to keep this
// package dependency-free) so preview/apply can show the real range an
// owner will get, and so this package's own tests can prove it never
// overlaps the AP address itself. apAddress must already have passed
// validateAPAddress.
//
// The algorithm (unchanged from existing behavior): default pool is
// .10-.50 in the AP's own /24; if the AP's own last octet falls inside
// that default pool, the pool shifts to .60-.110 instead. For any last
// octet in [1,254] (which validateAPAddress guarantees), exactly one of
// these two fixed windows always excludes it - there is no host value
// for which both windows could collide with the AP address.
func DerivedDHCPRange(apAddress string) (start, end string, ok bool) {
	ip := net.ParseIP(apAddress).To4()
	if ip == nil {
		return "", "", false
	}
	prefix := itoa(int(ip[0])) + "." + itoa(int(ip[1])) + "." + itoa(int(ip[2]))
	last := int(ip[3])
	if last >= 10 && last <= 50 {
		return prefix + ".60", prefix + ".110", true
	}
	return prefix + ".10", prefix + ".50", true
}

func validateDirectPin(pin string) error {
	if len(pin) != 4 && len(pin) != 8 {
		return &ValidationError{Field: "directPin", Reason: "must be exactly 4 or 8 digits"}
	}
	for _, r := range pin {
		if r < '0' || r > '9' {
			return &ValidationError{Field: "directPin", Reason: "must contain only digits"}
		}
	}
	return nil
}

// itoa avoids importing strconv solely for these error-message field
// paths; kept trivially simple since it only ever needs to format a
// small non-negative int.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
