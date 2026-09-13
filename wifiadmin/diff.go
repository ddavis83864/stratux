package wifiadmin

// FieldChange is one field's before/after values in a Preview - values
// are already redacted (never a real passphrase) by the time they reach
// here; see Preview's own doc comment.
type FieldChange struct {
	Field    string `json:"field"`
	Current  string `json:"current"`
	Proposed string `json:"proposed"`
}

// Preview is the complete, honest description of what applying proposed
// would change, computed against current - never a live mutation.
type Preview struct {
	Changes []FieldChange `json:"changes"`
	// WillDisconnect is true when at least one changed field requires
	// the existing ifdown/ifup wlan0 cycle (main/networksettings.go's
	// own applyNetworkSettings trigger) - which always drops the AP
	// association, including the connection of whatever device is
	// making this very request.
	WillDisconnect bool `json:"willDisconnect"`
	// ServiceRestartRequired is always true whenever WillDisconnect is
	// (this project has no way to apply a WiFi-affecting change without
	// restarting the interface) - kept as a distinct field for API
	// clarity and because a future, finer-grained apply path could in
	// principle decouple them.
	ServiceRestartRequired bool   `json:"serviceRestartRequired"`
	ProposedSSID           string `json:"proposedSSID"`
	CurrentSSID            string `json:"currentSSID"`
	// ProposedManagementAddress is only set (non-empty) when IPAddress
	// is changing - the new address an owner must browse to after
	// reconnecting.
	ProposedManagementAddress string `json:"proposedManagementAddress,omitempty"`
	HasChanges                bool   `json:"hasChanges"`
}

// connectivityAffectingFields is every Config field whose change requires
// the existing ifdown/ifup wlan0 cycle - i.e., every field this project's
// own applyNetworkSettings already writes into a
// wpa_supplicant/wpa_supplicant_ap/dnsmasq/interfaces template. This is
// every field except nothing today: there is no WiFi field in this
// package's Config that can be changed without a restart, matching
// existing behavior exactly (see docs/wifi-administration-hardening.md's
// ownership matrix). Kept as an explicit function, not a hardcoded bool,
// so a future field with a genuinely restart-free apply path has one
// obvious place to change this.
func connectivityAffecting(diffs []FieldChange) bool {
	return len(diffs) > 0
}

// ComputePreview diffs proposed against current field by field. Secrets
// (Passphrase, each ClientNetwork's Password, DirectPin) are NEVER
// included as plaintext current/proposed values - only a "changed"/
// "unchanged" marker, matching this project's own Configuration-Backup
// preview convention (docs/configuration-backup-restore.md) of never
// echoing a secret back, even one just submitted in the same request.
func ComputePreview(current, proposed Config) Preview {
	var changes []FieldChange
	add := func(field, cur, prop string) {
		if cur != prop {
			changes = append(changes, FieldChange{Field: field, Current: cur, Proposed: prop})
		}
	}
	add("ssid", current.SSID, proposed.SSID)
	add("securityEnabled", boolStr(current.SecurityEnabled), boolStr(proposed.SecurityEnabled))
	add("passphrase", secretMarker(current.Passphrase), secretMarker(proposed.Passphrase))
	add("channel", itoa(current.Channel), itoa(proposed.Channel))
	add("country", current.Country, proposed.Country)
	add("mode", itoa(int(current.Mode)), itoa(int(proposed.Mode)))
	add("ipAddress", current.IPAddress, proposed.IPAddress)
	add("internetPassThroughEnabled", boolStr(current.InternetPassThroughEnabled), boolStr(proposed.InternetPassThroughEnabled))
	add("directPin", secretMarker(current.DirectPin), secretMarker(proposed.DirectPin))
	if clientNetworksDiffer(current.ClientNetworks, proposed.ClientNetworks) {
		changes = append(changes, FieldChange{Field: "clientNetworks", Current: "(unchanged marker omitted - see clientNetworks count)", Proposed: itoa(len(proposed.ClientNetworks)) + " saved network(s)"})
	}

	p := Preview{
		Changes:        changes,
		HasChanges:     len(changes) > 0,
		WillDisconnect: connectivityAffecting(changes),
		CurrentSSID:    current.SSID,
		ProposedSSID:   proposed.SSID,
	}
	p.ServiceRestartRequired = p.WillDisconnect
	if current.IPAddress != proposed.IPAddress {
		p.ProposedManagementAddress = proposed.IPAddress
	}
	return p
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func secretMarker(s string) string {
	if s == "" {
		return "(empty)"
	}
	return "(set)"
}

func clientNetworksDiffer(a, b []ClientNetwork) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if a[i].SSID != b[i].SSID || a[i].Password != b[i].Password {
			return true
		}
	}
	return false
}
