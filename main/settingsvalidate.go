/*
	settingsvalidate.go: Input validation for the /setSettings request body
	handled by handleSettingsSetRequest (see managementinterface.go).

	The real contract - confirmed against every web/plates/js/settings.js
	call site - is a partial patch containing one or more already-known,
	correctly-typed top-level keys. No dashboard code ever posts back a
	full /getSettings document, and the largest legitimate request (the
	WiFi configuration modal) sends 10 keys.

	Validating every key here, before handleSettingsSetRequest mutates
	globalSettings, is what lets that handler reject a bad request with no
	partial mutation: either every key in the request is valid and all of
	them are applied, or none are.
*/

package main

import "fmt"

// maxSettingsRequestKeys bounds how many top-level keys a single
// /setSettings request may contain. This is well above the largest
// legitimate request (10 keys, the WiFi configuration modal) and well
// below the ~48 keys a full /getSettings document would round-trip back,
// so a full-object repost is rejected here rather than by comparing
// against an exact, brittle field list.
const maxSettingsRequestKeys = 20

type settingsFieldKind int

const (
	settingsFieldBool settingsFieldKind = iota
	settingsFieldString
	settingsFieldNumber
	settingsFieldIMUMapping
	settingsFieldWiFiClientNetworks
)

// settingsFieldTypes lists every key handleSettingsSetRequest recognizes
// and the exact JSON value shape each one requires. Keep this in sync
// with the switch statement in handleSettingsSetRequest: a key missing
// here is rejected as unrecognized before that switch ever runs, and
// every key present here must have a matching case there.
var settingsFieldTypes = map[string]settingsFieldKind{
	"DarkMode":                       settingsFieldBool,
	"UAT_Enabled":                    settingsFieldBool,
	"ES_Enabled":                     settingsFieldBool,
	"OGN_Enabled":                    settingsFieldBool,
	"AIS_Enabled":                    settingsFieldBool,
	"APRS_Enabled":                   settingsFieldBool,
	"Ping_Enabled":                   settingsFieldBool,
	"Pong_Enabled":                   settingsFieldBool,
	"OGNI2CTXEnabled":                settingsFieldBool,
	"GPS_Enabled":                    settingsFieldBool,
	"IMU_Sensor_Enabled":             settingsFieldBool,
	"BMP_Sensor_Enabled":             settingsFieldBool,
	"DEBUG":                          settingsFieldBool,
	"DisplayTrafficSource":           settingsFieldBool,
	"ReplayLog":                      settingsFieldBool,
	"TraceLog":                       settingsFieldBool,
	"AHRSLog":                        settingsFieldBool,
	"PersistentLogging":              settingsFieldBool,
	"IMUMapping":                     settingsFieldIMUMapping,
	"Dump1090Gain":                   settingsFieldNumber,
	"PPM":                            settingsFieldNumber,
	"AltitudeOffset":                 settingsFieldNumber,
	"RadarLimits":                    settingsFieldNumber,
	"RadarRange":                     settingsFieldNumber,
	"Baud":                           settingsFieldNumber,
	"WatchList":                      settingsFieldString,
	"GLimits":                        settingsFieldString,
	"OwnshipModeS":                   settingsFieldString,
	"StaticIps":                      settingsFieldString,
	"WiFiCountry":                    settingsFieldString,
	"WiFiSSID":                       settingsFieldString,
	"WiFiChannel":                    settingsFieldNumber,
	"WiFiSecurityEnabled":            settingsFieldBool,
	"WiFiPassphrase":                 settingsFieldString,
	"WiFiIPAddress":                  settingsFieldString,
	"WiFiMode":                       settingsFieldNumber,
	"WiFiDirectPin":                  settingsFieldString,
	"WiFiClientNetworks":             settingsFieldWiFiClientNetworks,
	"WiFiInternetPassThroughEnabled": settingsFieldBool,
	"EstimateBearinglessDist":        settingsFieldBool,
	"OGNAddrType":                    settingsFieldNumber,
	"OGNAddr":                        settingsFieldString,
	"OGNAcftType":                    settingsFieldNumber,
	"OGNPilot":                       settingsFieldString,
	"OGNReg":                         settingsFieldString,
	"OGNTxPower":                     settingsFieldNumber,
	"PWMDutyMin":                     settingsFieldNumber,
}

// validateSettingsKeyCount rejects a request carrying more top-level keys
// than any legitimate dashboard request sends - in particular, a full
// /getSettings document posted back to /setSettings (roughly 48 keys) is
// rejected here by count rather than by an exact field-set comparison, so
// this stays correct as fields are added to the settings struct.
func validateSettingsKeyCount(msg map[string]interface{}) error {
	if len(msg) > maxSettingsRequestKeys {
		return fmt.Errorf("request contains %d settings, more than the %d a single update may change", len(msg), maxSettingsRequestKeys)
	}
	return nil
}

// validateSettingsValue reports whether val is the exact JSON shape key
// requires, without mutating any package state. It never includes val
// itself in the returned error: some recognized keys (WiFiPassphrase,
// WiFiClientNetworks) carry values that must not reach a log line or an
// HTTP error response.
func validateSettingsValue(key string, val interface{}) error {
	kind, known := settingsFieldTypes[key]
	if !known {
		return fmt.Errorf("unrecognized setting %q", key)
	}
	switch kind {
	case settingsFieldBool:
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("setting %q must be a boolean", key)
		}
	case settingsFieldString:
		if _, ok := val.(string); !ok {
			return fmt.Errorf("setting %q must be a string", key)
		}
	case settingsFieldNumber:
		if _, ok := val.(float64); !ok {
			return fmt.Errorf("setting %q must be a number", key)
		}
	case settingsFieldIMUMapping:
		if _, err := decodeIMUMapping(val); err != nil {
			return fmt.Errorf("setting %q: %s", key, err.Error())
		}
	case settingsFieldWiFiClientNetworks:
		if _, err := decodeWiFiClientNetworks(val); err != nil {
			return fmt.Errorf("setting %q: %s", key, err.Error())
		}
	}
	return nil
}

// validateSettingsMessage validates every key in msg, stopping at the
// first problem it finds, so the caller can reject the whole request
// without having applied any part of it. Map iteration order is
// unspecified, so which key is named first in the error is not a
// documented ordering contract - only a diagnostic aid.
func validateSettingsMessage(msg map[string]interface{}) error {
	if err := validateSettingsKeyCount(msg); err != nil {
		return err
	}
	for key, val := range msg {
		if err := validateSettingsValue(key, val); err != nil {
			return err
		}
	}
	return nil
}

// decodeIMUMapping converts a validated "IMUMapping" value - a JSON array
// of exactly two numbers - into the [2]int globalSettings.IMUMapping
// expects. encoding/json never decodes a JSON array into a Go array
// through an interface{} (only into []interface{}), so a direct
// val.([2]int) type assertion can never succeed; this is the fix for the
// unconditional panic that motivated this file.
func decodeIMUMapping(val interface{}) ([2]int, error) {
	var out [2]int
	arr, ok := val.([]interface{})
	if !ok || len(arr) != 2 {
		return out, fmt.Errorf("must be a 2-element array of integers")
	}
	for i, e := range arr {
		f, ok := e.(float64)
		if !ok {
			return out, fmt.Errorf("must be a 2-element array of integers")
		}
		out[i] = int(f)
	}
	return out, nil
}

// decodeWiFiClientNetworks converts a validated "WiFiClientNetworks"
// value - a JSON array of {"SSID": string, "Password": string} objects -
// into the []wifiClientNetwork setWifiClientNetworks expects. Password
// values are never included in a returned error.
func decodeWiFiClientNetworks(val interface{}) ([]wifiClientNetwork, error) {
	arr, ok := val.([]interface{})
	if !ok {
		return nil, fmt.Errorf("must be an array")
	}
	networks := make([]wifiClientNetwork, 0, len(arr))
	for _, rawNetwork := range arr {
		network, ok := rawNetwork.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("entries must be objects")
		}
		ssid, ok := network["SSID"].(string)
		if !ok {
			return nil, fmt.Errorf("entries must have a string SSID")
		}
		password, ok := network["Password"].(string)
		if !ok {
			return nil, fmt.Errorf("entries must have a string Password")
		}
		networks = append(networks, wifiClientNetwork{ssid, password})
	}
	return networks, nil
}
