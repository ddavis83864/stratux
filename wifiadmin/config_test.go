package wifiadmin

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func validAPConfig() Config {
	return Config{
		SchemaVersion:   CurrentSchemaVersion,
		SSID:            "MyPlane N12345",
		SecurityEnabled: true,
		Passphrase:      "correcthorse",
		Channel:         6,
		Country:         "US",
		Mode:            ModeAP,
		IPAddress:       "192.168.10.1",
	}
}

func TestValidate_DefaultConfigIsValid(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("DefaultConfig() failed validation: %v", err)
	}
}

func TestValidate_ValidOpenAP(t *testing.T) {
	c := validAPConfig()
	c.SecurityEnabled = false
	c.Passphrase = ""
	if err := c.Validate(); err != nil {
		t.Errorf("valid open AP rejected: %v", err)
	}
}

func TestValidate_ValidProtectedAP(t *testing.T) {
	if err := validAPConfig().Validate(); err != nil {
		t.Errorf("valid protected AP rejected: %v", err)
	}
}

func TestValidate_SSIDLengthBounds(t *testing.T) {
	cases := []struct {
		name    string
		ssid    string
		wantErr bool
	}{
		{"empty", "", true},
		{"one char", "a", false},
		{"32 bytes exactly", strings.Repeat("a", 32), false},
		{"33 bytes - too long", strings.Repeat("a", 33), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validAPConfig()
			c.SSID = tc.ssid
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("SSID=%q: err=%v, wantErr=%v", tc.ssid, err, tc.wantErr)
			}
		})
	}
}

func TestValidate_SSIDUnicodeAndControlCharacters(t *testing.T) {
	cases := []string{
		"emoji\U0001F600ssid",
		"newline\nssid",
		"nul\x00ssid",
		"tab\tssid",
		"cr\rssid",
		"unicode-é",
	}
	for _, ssid := range cases {
		t.Run(ssid, func(t *testing.T) {
			c := validAPConfig()
			c.SSID = ssid
			if err := c.Validate(); err == nil {
				t.Errorf("SSID %q (non-ASCII-safe) should have been rejected", ssid)
			}
		})
	}
}

func TestValidate_PassphraseBoundsAndMismatch(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		passphrase string
		wantErr    bool
	}{
		{"disabled, empty - ok", false, "", false},
		{"disabled, nonempty - reject (mismatch)", false, "somepass", true},
		{"enabled, 7 chars - too short", true, "1234567", true},
		{"enabled, 8 chars - min ok", true, "12345678", false},
		{"enabled, 63 chars - max ok", true, strings.Repeat("a", 63), false},
		{"enabled, 64 chars - too long", true, strings.Repeat("a", 64), true},
		{"enabled, empty - missing passphrase", true, "", true},
		{"enabled, non-ASCII", true, "café1234", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validAPConfig()
			c.SecurityEnabled = tc.enabled
			c.Passphrase = tc.passphrase
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("enabled=%v passphrase=%q: err=%v, wantErr=%v", tc.enabled, tc.passphrase, err, tc.wantErr)
			}
		})
	}
}

func TestValidate_ChannelBoundary(t *testing.T) {
	for ch := 1; ch <= 11; ch++ {
		c := validAPConfig()
		c.Channel = ch
		if err := c.Validate(); err != nil {
			t.Errorf("channel %d should be valid: %v", ch, err)
		}
	}
	for _, ch := range []int{0, -1, 12, 13, 14, 36, 100} {
		c := validAPConfig()
		c.Channel = ch
		if err := c.Validate(); err == nil {
			t.Errorf("channel %d should be rejected (outside the template's supported 1-11 set)", ch)
		}
	}
}

func TestValidate_CountryNormalizationAndInvalid(t *testing.T) {
	cases := []struct {
		country string
		wantErr bool
	}{
		{"", false}, // unset allowed
		{"US", false},
		{"DE", false},
		{"us", true},  // must be uppercase
		{"USA", true}, // must be exactly 2 letters
		{"U1", true},
		{"1U", true},
		{" U", true},
	}
	for _, tc := range cases {
		t.Run(tc.country, func(t *testing.T) {
			c := validAPConfig()
			c.Country = tc.country
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("country=%q: err=%v, wantErr=%v", tc.country, err, tc.wantErr)
			}
		})
	}
}

func TestValidate_IPv4ParsingAndSpecialAddresses(t *testing.T) {
	cases := []struct {
		ip      string
		wantErr bool
	}{
		{"192.168.10.1", false},
		{"10.0.0.5", false},
		{"not-an-ip", true},
		{"::1", true},             // IPv6 loopback, not IPv4
		{"2001:db8::1", true},     // IPv6
		{"0.0.0.0", true},         // unspecified
		{"127.0.0.1", true},       // loopback
		{"224.0.0.1", true},       // multicast
		{"239.255.255.255", true}, // multicast
		{"192.168.10.0", true},    // network address in assumed /24
		{"192.168.10.255", true},  // broadcast address in assumed /24
		{"999.1.1.1", true},       // invalid octet
		{"192.168.10.1.1", true},  // malformed
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			c := validAPConfig()
			c.IPAddress = tc.ip
			err := c.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("ip=%q: err=%v, wantErr=%v", tc.ip, err, tc.wantErr)
			}
		})
	}
}

func TestValidate_ModeInvalid(t *testing.T) {
	c := validAPConfig()
	c.Mode = Mode(99)
	if err := c.Validate(); err == nil {
		t.Error("invalid mode should be rejected")
	}
}

func TestValidate_ClientNetworksDuplicateAndExcessive(t *testing.T) {
	c := validAPConfig()
	c.Mode = ModeAPClient
	c.ClientNetworks = []ClientNetwork{
		{SSID: "home", Password: "12345678"},
		{SSID: "home", Password: "12345678"},
	}
	if err := c.Validate(); err == nil {
		t.Error("duplicate client-network SSID should be rejected")
	}

	c2 := validAPConfig()
	c2.Mode = ModeAPClient
	for i := 0; i < MaxClientNetworks+1; i++ {
		c2.ClientNetworks = append(c2.ClientNetworks, ClientNetwork{SSID: "net" + itoa(i), Password: "12345678"})
	}
	if err := c2.Validate(); err == nil {
		t.Error("excessive client-network count should be rejected")
	}
}

func TestValidate_ClientNetworkMalformedEntry(t *testing.T) {
	c := validAPConfig()
	c.Mode = ModeAPClient
	c.ClientNetworks = []ClientNetwork{{SSID: "", Password: "12345678"}}
	if err := c.Validate(); err == nil {
		t.Error("empty client-network SSID should be rejected")
	}

	c2 := validAPConfig()
	c2.Mode = ModeAPClient
	c2.ClientNetworks = []ClientNetwork{{SSID: "open-net", Password: "short"}}
	if err := c2.Validate(); err == nil {
		t.Error("too-short client-network password should be rejected")
	}
}

func TestValidate_ClientNetworkOpenIsAllowed(t *testing.T) {
	c := validAPConfig()
	c.Mode = ModeAPClient
	c.ClientNetworks = []ClientNetwork{{SSID: "open-net", Password: ""}}
	if err := c.Validate(); err != nil {
		t.Errorf("an open (no-password) client network should be valid: %v", err)
	}
}

func TestValidate_DirectPinBounds(t *testing.T) {
	cases := []struct {
		pin     string
		wantErr bool
	}{
		{"", false}, // unset allowed
		{"1234", false},
		{"12345678", false},
		{"123", true},
		{"123456", true},
		{"12a4", true},
	}
	for _, tc := range cases {
		c := validAPConfig()
		c.DirectPin = tc.pin
		err := c.Validate()
		if (err != nil) != tc.wantErr {
			t.Errorf("directPin=%q: err=%v, wantErr=%v", tc.pin, err, tc.wantErr)
		}
	}
}

func TestValidate_InjectionStrings(t *testing.T) {
	// These strings target the actual risk this project has (unescaped
	// text/template holes in wpa_supplicant*.conf/interfaces - see
	// config.go's own package doc comment) - a `"` or newline in SSID/
	// passphrase could otherwise break out of a quoted config value or
	// inject an extra config line.
	injectors := []string{
		`already-closed"\nkey_mgmt=NONE`,
		"line1\nline2",
		"has\"quote",
		"has;semicolon",
		"has$(command)",
		"has`backtick`",
		"has|pipe",
		"has&ampersand",
	}
	for _, s := range injectors {
		t.Run(s, func(t *testing.T) {
			c := validAPConfig()
			c.SSID = s
			if err := c.Validate(); err == nil {
				t.Errorf("SSID %q containing an injection-risk character should be rejected", s)
			}
			c2 := validAPConfig()
			c2.Passphrase = s + "x" // pad length; still contains disallowed chars for some
			// Passphrase intentionally allows a broader printable-ASCII set
			// (a real WPA passphrase may contain punctuation) - the config-
			// injection risk for passphrase is closed by template escaping,
			// not by character exclusion; assert only that a NEWLINE
			// specifically (which cannot appear in a single-line
			// psk="..." value at all) is rejected.
			if strings.ContainsAny(s, "\n") {
				if err := c2.Validate(); err == nil {
					t.Errorf("passphrase containing a newline should be rejected")
				}
			}
		})
	}
}

// TestValidate_SSIDApostropheIsAllowed confirms this package's char set
// matches the existing project's own client-side SSID validator
// (web/plates/js/settings.js's isValidSSID: alphanumeric plus
// ()!  ._'-  and space) - an apostrophe (e.g. "Bob's Plane") is a
// legitimate, already-supported SSID character, not an injection risk:
// it is embedded inside a DOUBLE-quoted ssid="..." template value
// (debian/wpa_supplicant_ap.conf.template), so a single quote has no
// special meaning there. Only a literal double quote or a newline can
// actually break out of that value - both are covered by
// TestValidate_InjectionStrings.
func TestValidate_SSIDApostropheIsAllowed(t *testing.T) {
	c := validAPConfig()
	c.SSID = "Bob's Plane"
	if err := c.Validate(); err != nil {
		t.Errorf("an apostrophe in SSID should be allowed (matches existing client-side convention): %v", err)
	}
}

func TestValidate_NoPartialMutation(t *testing.T) {
	// Validate is a pure function on a value receiver - it cannot mutate
	// its argument. This test exists as an explicit, named regression
	// guard for that invariant rather than relying on Go's own value-
	// semantics to make it self-evident to a future reader.
	c := validAPConfig()
	before := c
	_ = c.Validate()
	if !reflect.DeepEqual(c, before) {
		t.Error("Validate must not mutate its receiver")
	}
}

func TestValidate_Determinism(t *testing.T) {
	c := validAPConfig()
	err1 := c.Validate()
	err2 := c.Validate()
	if (err1 == nil) != (err2 == nil) {
		t.Error("Validate must be deterministic")
	}
}

func TestRedact_NeverExposesSecrets(t *testing.T) {
	c := validAPConfig()
	c.DirectPin = "90210"
	c.ClientNetworks = []ClientNetwork{{SSID: "home", Password: "supersecret1"}}
	r := c.Redact()

	// Redacted has no field capable of holding a secret value at all -
	// this is a structural guarantee (Passphrase/DirectPin/Password are
	// simply not fields on Redacted/RedactedClientNetwork), verified
	// here by checking the two secret values do not appear ANYWHERE in
	// a full-struct dump, which would catch a future field added to
	// Redacted by mistake with the real value instead of a *Set flag.
	dump := fmt.Sprintf("%+v", r)
	if strings.Contains(dump, "correcthorse") {
		t.Errorf("Redact leaked the AP passphrase: %s", dump)
	}
	if strings.Contains(dump, "supersecret1") {
		t.Errorf("Redact leaked a client-network password: %s", dump)
	}
	if strings.Contains(dump, "90210") {
		t.Errorf("Redact leaked the WiFi-Direct PIN: %s", dump)
	}
	if !r.PassphraseSet || !r.DirectPinSet || !r.ClientNetworks[0].PasswordSet {
		t.Error("Redact should still report that secrets ARE set")
	}
}

func TestDerivedDHCPRange_NeverContainsAPAddress(t *testing.T) {
	for last := 1; last <= 254; last++ {
		ip := "192.168.10." + itoa(last)
		start, end, ok := DerivedDHCPRange(ip)
		if !ok {
			t.Fatalf("DerivedDHCPRange(%s): ok=false", ip)
		}
		startLast := lastOctet(t, start)
		endLast := lastOctet(t, end)
		if last >= startLast && last <= endLast {
			t.Errorf("AP address .%d falls inside its own derived DHCP range %s-%s", last, start, end)
		}
	}
}

func lastOctet(t *testing.T, ip string) int {
	t.Helper()
	parts := strings.Split(ip, ".")
	n := 0
	for _, r := range parts[3] {
		n = n*10 + int(r-'0')
	}
	return n
}
