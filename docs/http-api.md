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
Runs the AHRS calibration routine (Zero Drift - gyro zero bias). Returns `409` if no
named calibration profile is active - see below.

#### `POST /cageAHRS`
Cages the AHRS to the current attitude (sets current orientation as level reference).
The resulting quaternion is saved to `SensorQuaternion` in settings (Set Level). Returns
`409` if no named calibration profile is active - see below.

#### `POST /resetGMeter`
Resets the G-meter min/max values.

---

### Aircraft Calibration Profiles

Named, persistent AHRS calibration profiles - see
[aircraft-calibration-profiles.md](aircraft-calibration-profiles.md) for the full
schema, persistence design, and dashboard workflow. Every endpoint below is additive;
none changes `/calibrateAHRS`/`/cageAHRS`'s underlying calibration algorithm.

| Endpoint | Method | Purpose |
|---|---|---|
| `/getCalibrationProfiles` | GET | List every profile plus the active profile ID. |
| `/getActiveCalibrationProfile` | GET | The active profile (`404` if none is set). |
| `/getCalibrationProfileStatus` | GET | Active profile + subsystem availability, for a single-request dashboard summary. |
| `/createCalibrationProfile` | POST | Body: `{name, registration, aircraftType, mountingNote}`. Creates an uncalibrated profile; does not activate it. |
| `/updateCalibrationProfile?id=...` | POST | Metadata only - never touches calibration vectors. |
| `/activateCalibrationProfile?id=...` | POST | Makes a profile active. `409` while a recording is active. |
| `/deleteCalibrationProfile?id=...` | POST | `409` if `id` is the active profile. |
| `/captureCalibrationProfile[?id=...]` | POST | Snapshots the currently live calibration into a profile (active, if `id` omitted). |

---

### Preflight Readiness

A simplified, supplemental preflight checklist built on top of `/getHealth` - see
[preflight-readiness.md](preflight-readiness.md) for the full state model, decision
policy, and manual-acknowledgement rules. Every endpoint below is additive and
read-only with respect to every other subsystem.

| Endpoint | Method | Purpose |
|---|---|---|
| `/getPreflightReport` | GET | The current preflight report. |
| `/acknowledgePreflightCheck?id=...` (or JSON body `{"id":"..."}`) | POST | Acknowledge one manual check. Idempotent. `404` for an unrecognized id. |
| `/clearPreflightCheck?id=...` | POST | Clear one manual acknowledgement. |
| `/resetPreflightChecks` | POST | Clear every manual acknowledgement. |

---

### Operational Alerting

Supplemental, non-certified traffic-proximity and system-health notices - see
[alerting.md](alerting.md) for the full design, threshold policy, and safety
boundaries. **Not collision avoidance, not TCAS/ACAS, issues no maneuver
guidance**, and never modifies the GDL90/FLARM stream ForeFlight and other EFBs
receive.

| Endpoint | Method | Purpose |
|---|---|---|
| `/getAlerts` | GET | Current active alerts, bounded recent event history, and counters. |
| `/getAlertSettings` | GET | The current persisted alert settings. |
| `/setAlertSettings` | POST | Replace the persisted settings (JSON body, fully validated, atomic). `400` for any NaN/Inf/negative/out-of-range field. |
| `/acknowledgeAlert?id=...` | POST | Acknowledge one traffic target (by its sanitized id) or system-health component (by name). Idempotent; `found:false` (still `200`) for an unknown/already-expired id. |
| `/muteAlerts` | POST | Mute audio. JSON body `{"durationSeconds": N}` - omitted or `0` mutes indefinitely; a documented maximum (12h) bounds any timed mute. Visual alerts remain unaffected. |
| `/unmuteAlerts` | POST | Clear mute immediately. |
| `/testAlertSound` | POST | Confirms the subsystem is reachable; the tone itself is generated entirely client-side and never creates a real alert event. |

---

### Configuration Backup and Restore

Backs up and restores supported Stratux **application** configuration only - never
Wi-Fi credentials, SSH material, OS configuration, recordings, or diagnostics. See
[configuration-backup-restore.md](configuration-backup-restore.md) for the full schema,
validation, confirmation-token, and rollback design.

| Endpoint | Method | Purpose |
|---|---|---|
| `/downloadConfigurationBackup` | GET | The current configuration as a bounded, checksummed JSON document. |
| `/validateConfigurationBackup` | POST | Validate an uploaded document (JSON body) and return a preview of what it would change. No writes. `200` with `{preview, confirmationToken, expiresInSeconds}` on success; `400` malformed/invalid, `413` oversized. |
| `/applyConfigurationBackup` | POST | `{"confirmationToken": "...", "backup": {...}}` - transactionally applies a previously validated document. `200` only after complete success; `400` invalid input, `409` stale preview/active recording/active OTA/concurrent restore, `410` expired or already-used token, `413` oversized. A failure includes a structured rollback result and never claims partial success. |
| `/getConfigurationRestoreStatus` | GET | Current restore-operation state (`idle`/`validating`/`preview-ready`/`applying`/`verifying`/`rolling-back`/`complete`/`failed`) and the last result, if any. |

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
Lists sessions: `[{id, sizeBytes, fileCount, startedAt, metadataAvailable,
metadataCorrupt, metadataSchemaVersion, preflightStateAtStart, profileNameAtStart,
complete}, …]`, newest first. The `metadata*`/`preflightStateAtStart`/
`profileNameAtStart`/`complete` fields are additive - a client that ignores unknown
fields keeps working. See [recording.md](recording.md) for what they mean.

#### `GET /getRecordingMetadata?id=<session-id>`
The durable, versioned, point-in-time Preflight/calibration snapshot captured once
when the session started, plus its finalization state - see
[recording.md](recording.md) for the full schema. `{available: true, metadata: {...}}`
for a recording that has one; `{available: false, legacy: true}` for a recording
created before this feature (or whose initial capture failed); `{available: false,
corrupt: true, error}` if the sidecar file exists but could not be parsed. `404` for
an unknown id, `400` for a malformed one.

#### `GET /downloadRecordingMetadata?id=<session-id>`
Downloads the same metadata as a named `<id>.metadata.json` file
(`application/json`, `Content-Disposition: attachment`). `404` if the recording has
no valid metadata (legacy or corrupt).

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
