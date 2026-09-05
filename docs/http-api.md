# Stratux HTTP & WebSocket API Reference

Stratux exposes a set of HTTP JSON endpoints and WebSocket streams accessible on the Stratux IP address (default `192.168.10.1`). These are used by the web UI and can also be used by external tools, EFBs, or custom integrations.

---

## HTTP JSON Endpoints

### Status & Telemetry

#### `GET /getStatus`
Returns the current Stratux system status as JSON, including software version, connected devices, message counts, GPS status, CPU temperature, and error list.

Example fields: `Version`, `GPS_connected`, `GPS_satellites_locked`, `UAT_messages_last_minute`, `ES_messages_last_minute`, `CPUTemp`, `Errors`

Primary 978/1090 receiver status is exposed as `UAT_*` / `ES_*` fields (`Enabled`, `Detected`,
`Assigned`, `DeviceSerial`, `DeviceIndex`, `AssignmentSource`, `Ambiguous`, `Conflict`,
`ExternallySatisfied`, `IdentityUnstable`, `DecoderRunning`, `Receiving`, `Degraded`,
`DiagnosticReason`) - see
[hardware/sdr-and-bands.md](hardware/sdr-and-bands.md#verifying-assignment-in-the-status-page).
A frontend that hasn't received these fields yet (older cached page, or a backend predating
this API) should treat the band as unknown, not as disabled.

#### `GET /getHealth`
Returns a unified component-readiness report as JSON: one `ComponentState`
(`READY`/`DEGRADED`/`NOT_READY`/`NOT_INSTALLED`/`UNKNOWN`) per monitored subsystem (978, 1090,
GPS, GDL90, System, persistent Storage, temporary Overlay, trusted Time, AHRS, Barometer, and Fan
controller), plus an overall rollup. Recomputed on its own 5-second interval, independent of
`/getStatus`. This is purely additive — no existing `/getStatus` field changed. See
[readiness-and-time-trust.md](readiness-and-time-trust.md) for the full model and the color rules
the dashboard applies to each state, and
[ahrs-baro-fan-health.md](ahrs-baro-fan-health.md) for the AHRS/Barometer/Fan-controller field
definitions specifically.

#### `GET /getSituation`
Returns the current GPS/AHRS situation: position, altitude, track, speed, vertical speed, and attitude (pitch/roll/slip-skid) if AHRS is connected.

#### `GET /getTowers`
Returns all ADS-B ground towers that have been received, as a JSON object keyed by `"(lat,lng)"`. Each entry includes:
- `Lat`, `Lng` — tower coordinates
- `Signal_strength_last_minute`, `Signal_strength_max`
- `Messages_last_minute`, `Messages_total`

Useful for coverage mapping and signal analysis.

#### `GET /getSatellites`
Returns all GNSS satellites currently being tracked, with signal levels and fix status per satellite.

#### `GET /getClients`
Returns all currently connected GDL90 clients (EFBs, apps) with their IP addresses and connection metadata.

#### `GET /getRegion`
Returns the currently selected region as JSON: `{"IsSet": true, "Region": "US"}` or `{"IsSet": false}`. Region values: `US`, `EU`.

---

### Settings

#### `GET /getSettings`
Returns the full `stratux.conf` settings as JSON. All fields are returned, including those not exposed in the web UI (see [Advanced Settings](#advanced-settings) below).

#### `POST /setSettings`
Accepts a JSON body with one or more settings fields to update. Changes are applied immediately and persisted to `stratux.conf`.

Example:
```json
{ "UAT_Enabled": true, "ES_Enabled": true, "PPM": -5 }
```

#### `POST /setRegion`
Sets the region. Accepts JSON: `{"Region": "US"}` or `{"Region": "EU"}`. This applies the region change at runtime (UAT band selection, OGN behavior); note that it does **not** itself write `stratux.conf` — persistence is handled by the region-change path, not an explicit settings save in the handler.

---

### Logs & Data

#### `GET /logs/`
Browse and download log files via HTTP. Useful for remote diagnostics without SSH access.

#### `GET /downloadlog`
Downloads the current debug log file.

#### `GET /downloadahrslogs`
Downloads AHRS log files as a zip archive.

#### `GET /downloaddb`
Downloads the traffic/message database.

#### `POST /deletelogfile`
Deletes the current debug log file.

#### `POST /deleteahrslogfiles`
Deletes all AHRS log files.

---

### System Control

#### `POST /reboot`
Reboots the Raspberry Pi.

#### `POST /shutdown`
Shuts down the Raspberry Pi.

#### `POST /restart`
Restarts the Stratux software without rebooting the Pi.

#### `POST /develmodetoggle`
**Enables** Developer Mode (equivalent to tapping the version number in the web UI Settings page). Despite the name this is one-way — it only ever sets `DeveloperMode` to `true`; it does not toggle it back off. To disable Developer Mode, clear it via `POST /setSettings` (`{"DeveloperMode": false}`).

#### `POST /roPartitionRebuild`
Rebuilds the read-only filesystem partition. Use with caution — intended for recovery scenarios.

---

### AHRS Calibration

#### `POST /orientAHRS`
Triggers AHRS orientation detection.

#### `POST /calibrateAHRS`
Runs the AHRS calibration routine.

#### `POST /cageAHRS`
Cages the AHRS to the current attitude (sets current orientation as level reference). The resulting quaternion is saved to `SensorQuaternion` in settings.

#### `POST /resetGMeter`
Resets the G-meter min/max values.

---

### OTA Update

#### `POST /updateUpload`
Upload a `.deb` OTA update package directly via HTTP POST (multipart form).

#### `POST /updatePong`
Upload firmware for the Pong ADS-B receiver.

---

### Diagnostics

Generates and serves sanitized troubleshooting bundles - never enabled automatically, only on
request. See `docs/readiness-and-time-trust.md` for what a bundle contains and excludes.

#### `POST /generateDiagnostics`
Builds and writes one new sanitized diagnostic bundle under `/var/lib/stratux-data/diagnostics`.
Returns `{success, name, sizeBytes, generatedAt}` on success, or `{success:false, error}`. A
retention-pruning failure after a successful write still reports `success:true` with
`partial:true` and a `warning` - the bundle itself was written.

#### `GET /getDiagnostics`
Lists available bundles: `[{name, sizeBytes, generatedAt}, …]`, newest first.

#### `GET /downloadDiagnostics?name=<bundle-name>`
Downloads one bundle. `name` must exactly match an entry from `/getDiagnostics` - any other value
(including path-traversal attempts) returns 404, never a filesystem error.

---

### Recording

An on-demand, explicitly-controlled recording for troubleshooting/analysis. Automatic flight
recording remains disabled regardless of this API's existence - nothing here runs unless
requested. See `docs/readiness-and-time-trust.md` for the sample schema and known limitations
(GPX/KML still return "not implemented") and `docs/ahrs-baro-fan-health.md` for the live
AHRS/barometer sample fields and CSV columns.

#### `POST /startRecording`
Starts a new session (`/var/lib/stratux-data/recordings/<id>/`, `id` server-generated as
`rec-<UTC timestamp>`). Returns `{success, status}`. `409 Conflict` if a session is already
active. `503`/`507` if persistent storage is unavailable or below the minimum free-space
threshold.

#### `POST /stopRecording`
Stops the active session, if any; a safe no-op if nothing is active. Returns `{success, status}`.

#### `GET /getRecordingStatus`
Current (or last) session status: `{id, state, startedAt, stoppedAt, sampleCount, lastError}`.
`state` is one of `idle`, `active`, `error`.

#### `GET /getRecordings`
Lists sessions: `[{id, sizeBytes, fileCount, startedAt}, …]`, newest first.

#### `POST /exportRecording?id=<session-id>&format=csv|gpx|kml`
Exports a session to a persisted file under `/var/lib/stratux-data/exports`. `gpx`/`kml` honestly
return `501 Not Implemented` (see `recording.ErrExportNotImplemented`). Returns
`{success, name, sizeBytes, sampleCount}`.

#### `GET /downloadRecording?id=<session-id>`
Downloads a session's raw JSONL file(s) as a zip.

#### `GET /downloadExport?name=<export-name>`
Downloads a previously-created export. `name` must exactly match an entry produced by
`/exportRecording` - same traversal-safety rule as `/downloadDiagnostics`.

---

### Map Data

#### `GET /tiles/tilesets`
Returns available offline map tilesets.

#### `GET /tiles/{tileset}/{z}/{x}/{y}`
Serves individual map tiles from offline tilesets.

#### `GET /mapdata/styles/…`
Serves vector-tile map style JSON files (referenced by the `stratux_style_url` field in the `/tiles/tilesets` response). Backed by a static file server rooted at `$STRATUX_HOME/mapdata/styles`.

---

### Web UI (static)

#### `GET /`
The root path is a catch-all static file server for the web configuration UI (served from `$STRATUX_WWW_DIR`, default `/opt/stratux/www`). Responses carry a short `Cache-Control: max-age` header. Not a JSON API, but listed here for completeness since it shares the same HTTP server.

---

## WebSocket Streams

All WebSocket endpoints are at `ws://192.168.10.1/<endpoint>`.

#### `ws://…/gdl90`
Live GDL90 binary message stream. This is the same data sent over UDP port 4000 but delivered via WebSocket. Used by the web UI map and traffic display.

#### `ws://…/status`
Live status updates pushed as JSON whenever system status changes.

#### `ws://…/situation`
Live GPS/AHRS situation updates pushed as JSON.

#### `ws://…/weather`
Live FIS-B weather messages. On connect, sends the current weather buffer, then pushes updates as they arrive.

#### `ws://…/traffic`
Live traffic updates pushed as JSON.

#### `ws://…/radar`
Radar/NEXRAD data stream.

#### `ws://…/jsonio`
Live traffic as JSON objects, one per message. On connect, sends all currently tracked traffic with valid positions, then pushes updates. Alternative to GDL90 for integrations that prefer JSON.

---

## Advanced Settings

The following settings are available via `/getSettings` and `/setSettings` but are not exposed in the web UI. They are persisted to `stratux.conf`.

### Manual GPS Configuration

Useful when GPS autodetection fails or for non-standard hardware:

| Field | Type | Description |
|-------|------|-------------|
| `GpsManualConfig` | bool | Enable manual GPS config (disables autodetect) |
| `GpsManualDevice` | string | Serial device path, e.g. `/dev/ttyAMA0` |
| `GpsManualChip` | string | Chip type: `ublox6`, `ublox7`, `ublox8`, `ublox9`, `ublox10`, or `ublox` (generic). Any other/empty value leaves the chip unconfigured |
| `GpsManualTargetBaud` | int | Target baud rate, e.g. `115200` |

Example:
```json
{
  "GpsManualConfig": true,
  "GpsManualDevice": "/dev/ttyAMA0",
  "GpsManualChip": "ublox9",
  "GpsManualTargetBaud": 115200
}
```

### Other Advanced Settings

| Field | Type | Description |
|-------|------|-------------|
| `ClearLogOnStart` | bool | Wipe the debug log file on each boot |
| `NoSleep` | bool | Disable sleep mode detection for GDL90 clients. Useful for always-on panel-mount EFIS installations where the display never sleeps |
| `SensorQuaternion` | [4]float64 | AHRS calibration quaternion. Set by the calibration wizard; can be set manually for aircraft-specific alignment |
| `RegionSelected` | int | `0`=none, `1`=US, `2`=EU. Drives UAT band selection and some OGN behavior. Prefer using `/setRegion` |
| `DeveloperMode` | bool | Enables additional SDR diagnostics. Enable via `/develmodetoggle` (one-way) or by tapping the version number in Settings; disable by posting `{"DeveloperMode": false}` to `/setSettings` |

---

## Notes

- All endpoints are HTTP (not HTTPS). Stratux operates on a local Wi-Fi network.
- No authentication is required.
- The default Stratux IP is `192.168.10.1` in AP mode, or the DHCP-assigned address in client mode.
- `Content-Type: application/json` is returned on all JSON endpoints.
- WebSocket connections remain open until the client disconnects.
