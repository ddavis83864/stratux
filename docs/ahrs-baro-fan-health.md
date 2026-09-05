# AHRS, Barometer, and Fan-Controller Health

This documents the live health model for the ICM-20948 AHRS, BMP280 barometer, and
dual-fan PWM controller now wired into `readiness`/`main`/`recording`, replacing the
placeholder `FutureHardwareHealth` (always `NOT_INSTALLED`) that previously stood in for
all three. See [readiness-and-time-trust.md](readiness-and-time-trust.md) for the
component-health model, `ComponentState` enum, and `Rollup` rules this builds on.

**All of this is supplemental, non-certified situational-awareness data** — exactly like
the existing AHRS browser page and GDL90 AHRS reports. Nothing here changes that framing;
it only reports whether the supplemental data can currently be trusted.

## AHRS health (`readiness.AHRSHealth`, `readiness.BuildAHRSHealth`)

Source signals (all gathered in `main/fancontrolstatus.go`'s `buildAHRSHealth`, under
`mySituation`'s existing `muAttitude` lock):

| Field | Source |
|---|---|
| `Enabled` | `globalSettings.IMU_Sensor_Enabled` (operator configuration) |
| `Connected` | `globalStatus.IMUConnected` |
| `RawStatus` | `mySituation.AHRSStatus` (see bit table below) |
| `PitchDeg`/`RollDeg`/`GLoad` | `mySituation.AHRSPitch`/`AHRSRoll`/`AHRSGLoad`, `nil` whenever the value equals the AHRS library's sentinel (`goflying/ahrs.Invalid`, ~`3276.7`) — see `main/sensors.go`'s `isAHRSInvalidValue`. The sentinel is converted to `nil` in `main/`, before it ever reaches `readiness` or the API, so it can never be read as a real (e.g. level `0.0`) measurement. |
| `LastMeasurementTime`/`LastMeasurementAgeSeconds` | `mySituation.AHRSLastAttitudeTime`, converted with the same monotonic-clock-domain age computation as `RadioHealth.LastFrameAgeSeconds` (immune to a wall-clock step) |
| `LevelCalibrated` | `globalSettings.SensorQuaternion` is non-zero (a level reference — see Set Level below) |
| `GyroCalibrated` | `globalSettings.D` (gyro zero bias) is non-zero |
| `IMUMapping` | `globalSettings.IMUMapping`, passed through |
| `HeadingSupported` | always `false` — see below |

### `AHRSStatus` bit meanings (unchanged, `main/sensors.go`'s `updateAHRSStatus`)

| Bit | Meaning |
|---|---|
| 0 (`0x01`) | GPS ground track valid |
| 1 (`0x02`) | IMU enabled and connected |
| 2 (`0x04`) | Barometer enabled/connected, or a recent valid temp/pressure reading exists |
| 3 (`0x08`) | IMU is currently running a calibration (`ahrsCalibrating`) |
| 4 (`0x10`) | IMU in use and CSV analysis logging is active |

### State rules

- **Disabled by configuration** (`IMU_Sensor_Enabled = false`) → `NOT_INSTALLED` (gray) —
  never a failure.
- **Enabled but not connected** → `NOT_READY` — a real problem for hardware the baseline
  requires.
- **Connected, no attitude solution produced yet** (including immediately after a
  reconnect — `main/sensors.go` resets `AHRSLastAttitudeTime` to the zero value on every
  IMU read failure) → `NOT_READY`.
- **Connected, last solution older than the staleness threshold** (2s, looser than the
  existing `isAHRSValid()` 1s check, to avoid flapping between `healthUpdateInterval`'s
  5-second health ticks on a momentary hiccup) → `DEGRADED`.
- **Connected and recent, but pitch/roll unavailable** (sentinel value) → `DEGRADED`.
- **Connected and recent, but no level reference set** → `DEGRADED` (use **Set Level** —
  see below).
- **Connected, recent, valid, calibrated** → `READY`.

### Heading is never part of readiness

`main/sensors.go`'s `sensorAttitudeSender` deliberately never computes a calibrated
magnetic heading (`mySituation.AHRSMagHeading = ahrs.Invalid`, unconditionally — no
magnetometer calibration flow exists). `BuildAHRSHealth` has **no heading parameter at
all**, and `HeadingSupported` is always `false`, so readiness can never be conditioned on
a value this build cannot honestly produce.

### Set Level vs. Zero Drift

- **Set Level** (`POST /cageAHRS` → `CageAHRS()` → `main/sensors.go`, action `"level"`)
  recalibrates the accelerometer zero bias and derives a new `SensorQuaternion` from the
  unit's *current* physical orientation, i.e. it declares "the receiver is level right
  now." **Use this whenever the receiver's mounting angle or airframe changes** — moving
  it between airplanes, or re-mounting it at a different angle in the same airplane —
  since the previous level reference no longer describes the new orientation.
- **Zero Drift** (`POST /calibrateAHRS` → `CalibrateAHRS()` → action `"cal"`) recalibrates
  the gyro zero bias only. **Use this only while stationary** (on the ground, engine off,
  not being handled) **and only when gyro drift actually warrants it** — a slow, expected
  attitude drift over time with no aircraft motion. Recalibrating while moving bakes real
  motion into the "zero" bias.

Both persist into `globalSettings` (`C`, `D`, `SensorQuaternion`) via the existing
`saveSettings()` and are unchanged by this work — see `readiness.AHRSHealth.LevelCalibrated`/
`GyroCalibrated`, which simply report whether either has ever been set.

Both actions are now additionally captured into the active named calibration profile,
if one exists — see [aircraft-calibration-profiles.md](aircraft-calibration-profiles.md)
for named, persistent, per-airframe calibration (letting the receiver move between
aircraft without losing each one's mounting calibration), which this section's
`CageAHRS`/`CalibrateAHRS` mechanism is unchanged by and still the actual calibration
engine underneath.

## Barometer health (`readiness.BaroHealth`, `readiness.BuildBaroHealth`)

Source signals (`main/fancontrolstatus.go`'s `buildBaroHealth`, under `mySituation`'s
existing `muBaro` lock):

| Field | Source |
|---|---|
| `Enabled` | `globalSettings.BMP_Sensor_Enabled` |
| `Connected` | `globalStatus.BMPConnected` |
| `TemperatureC`/`PressureAltitudeFt`/`VerticalSpeedFPM` | `mySituation.BaroTemperature`/`BaroPressureAltitude`/`BaroVerticalSpeed`, `nil` unless connected and at least one real measurement has been taken |
| `SourceType` | `readiness.BaroSourceTypeName(mySituation.BaroSourceType)` — mirrors `main/gps.go`'s `BARO_TYPE_*` constants (`NONE`/`BMP280`/`OGNTRACKER`/`NMEA`/`ADSBESTIMATE`) |
| `LastMeasurementTime`/`LastMeasurementAgeSeconds` | `mySituation.BaroLastMeasurementTime`, same monotonic-age convention as AHRS |

### State rules

- **Disabled** → `NOT_INSTALLED`. **Enabled but not connected** → `NOT_READY`. **Connected,
  no measurement yet** (including reconnection) → `NOT_READY`.
- **Non-finite** (NaN/Inf in any present value — always a failed read, never a plausible
  reading) → `NOT_READY`.
- **Stale** (>15s since last measurement, matching the existing `isTempPressValid()`
  threshold) → `DEGRADED`.
- **Structurally implausible but finite** → `DEGRADED`, never `NOT_READY` — see bounds
  below. Ordinary cabin-pressure/temperature variation in a light aircraft must never trip
  this; the bounds are deliberately generous (mirroring the existing >70000ft guard
  already in `main/sensors.go`'s `tempAndPressureSender`, which discards a wild pressure
  reading before it ever reaches `mySituation`):
  - Pressure altitude: −2000ft to 60000ft.
  - Vertical speed: magnitude ≤ 10000 fpm.
  - Temperature: −40°C to 85°C (the BMP280 datasheet's own operating range).
- **Connected, recent, plausible** → `READY`.

## Fan-controller health (`readiness.FanHealth`, `readiness.BuildFanHealth`)

`fancontrol_main/fancontrol.go` is a **separate daemon** (`stratux_fancontrol.service`),
not a library linked into `stratuxrun` — the two processes only ever communicate via a
`SIGUSR1` reconfigure signal (unchanged). **This hardware has no tachometer or other
rotation-feedback pin.** `FanHealth.TachometerSupported` is always `false`, and no
`State`/`Reason` text may claim the physical fan is confirmed spinning — only that the
controller process is running and what duty cycle it is commanding.

### The runtime status file

The daemon writes its own state once per second (the same cadence as its existing
Prometheus stats update, `updateStats()`) to a small atomic JSON file:
`common.FanControllerStatusPath` = `/run/stratux-fancontrol/status.json` — **RAM-backed
tmpfs, never the SD card or the persistent data partition**, cleared on reboot. The write
is atomic (temp file + `os.Rename`, mirroring `readiness.WriteDiagnosticBundle`'s
pattern), so `main/`'s reader never observes a partial file. The containing directory is
created by systemd's `RuntimeDirectory=stratux-fancontrol` (see
`debian/stratux_fancontrol.service`) and, redundantly, by the Go code itself
(`os.MkdirAll`), so running the binary manually (e.g. during development) behaves the
same way.

This design was chosen over consuming the daemon's existing `:9977` HTTP/Prometheus
endpoint: the status file needs no network round trip or fixed-port assumption, carries a
timestamp and an explicit error field the Prometheus gauges do not, and cannot leave a
stale value silently indistinguishable from "just slow to respond."

`common.FanControllerStatus` fields: `UpdatedAt`, `ControllerState` (`STARTING` during the
brief power-on fan test, `COMMANDING` while the PID output is above 5% and being applied,
`IDLE` while running at the configured minimum duty, `ERROR` on a GPIO/hardware problem),
`Error`, `CPUTempC`, `TempTargetC`, `PWMDutyMinPercent`, `RequestedDutyPercent`,
`PWMFrequencyHz`, `PWMPin`, `TachometerSupported` (always `false`).

### Reading it (`main/fancontrolstatus.go`'s `buildFanHealth`)

1. `readiness.UnitActiveState("stratux_fancontrol")` runs `systemctl is-active` and
   `readiness.UnitInstalled` classifies the result: `"unknown"`/`""` (systemd has never
   loaded the unit — e.g. a non-Pi dev build) → **not installed**; anything else (`active`,
   `inactive`, `failed`, ...) → **installed**, with `active` also setting `ServiceActive`.
2. `common.ReadFanControllerStatus` reads and parses the status file. A missing file
   (`os.IsNotExist`) is distinguished from a malformed one (any other parse error).

### State rules

- **Service not installed** (never loaded by systemd on this platform/build) →
  `NOT_INSTALLED` — a platform fact, not a failure, matching how a disabled AHRS/barometer
  reads gray rather than red.
- **Installed but not active** → `NOT_READY` — on the deployed baseline (Raspberry Pi 4B
  with the fan controller physically connected), this is a real problem.
- **Active, no status observed yet** → `DEGRADED` (the daemon may have just started).
- **Active, status file present but malformed** → `DEGRADED`.
- **Active, status reports a controller error** (e.g. GPIO open failure) → `NOT_READY`.
- **Active, status older than 10s** (a few missed 1Hz writes tolerated before flagging) →
  `DEGRADED`.
- **Active, status fresh, no error** → `READY` — `Reason` always includes the literal
  phrase **"rotation feedback unavailable"**, so no reading of this state can be mistaken
  for a confirmed-spinning fan.

## Recording fields and CSV columns

`recording.Sample` (unchanged fields preserved for backward compatibility with existing
recordings — every new field is additive) gained:

| Field | Meaning | Source |
|---|---|---|
| `BaroVerticalSpeedFPM` | barometric vertical speed, ft/min | `readiness.BaroHealth.VerticalSpeedFPM` at sample time |
| `GLoadMin`/`GLoadMax` | running min/max g-load since the AHRS last reset | `mySituation.AHRSGLoadMin`/`AHRSGLoadMax` |
| `AHRSStatus` | raw `AHRSStatus` bitfield (see table above) | `readiness.AHRSHealth.RawStatus`, only when connected |
| `AHRSCalibrationState` | the `readiness.AHRSHealth.State` string in effect at sample time (`"READY"`, `"DEGRADED"`, ...) | derived at sample time |
| `AHRSMeasurementAgeSeconds` | age of the attitude solution backing this sample's pitch/bank/g-load | `readiness.AHRSHealth.LastMeasurementAgeSeconds` |

`PitchDeg`/`BankDeg`/`GLoad`/`PressureAltitudeFt` (pre-existing reserved fields) are now
populated live from `readiness.AHRSHealth`/`BaroHealth` rather than left permanently
`nil`. `main/recordingapi.go`'s `appendRecordingSample` calls the same
`buildAHRSHealth`/`buildBaroHealth` glue `main/health.go` uses, fresh every sample (not
the 5-second-cached `globalHealth`), under the same `mySituation` locks already used for
GPS fields — the 1Hz nonblocking sampling architecture and lock discipline are unchanged.
`VerticalAccelG` remains `nil` unconditionally: nothing in this codebase computes it
(only `GLoad`, the total acceleration magnitude, is available from the AHRS library).

**CSV column order** (`recording.CSVExporter`, stable/documented — new columns were
inserted immediately after `GLoad` and before the message-rate columns, so every prior
column's position is unchanged):

```
UTC, TimeTrustState, Latitude, Longitude, GPSAltitudeFt, GPSAccuracyMeters,
GroundspeedKt, CourseDeg, PressureAltitudeFt, PitchDeg, BankDeg, VerticalAccelG, GLoad,
GLoadMin, GLoadMax, BaroVerticalSpeedFPM, AHRSStatus, AHRSCalibrationState,
AHRSMeasurementAgeSeconds, UAT978MessageRateLastMinute, ES1090MessageRateLastMinute,
FISBTowerCount, SystemHealthTransition, TimeSourceTransition
```

A `nil` value renders as an empty CSV cell, never `"0"` — unchanged behavior, now also
exercised by the new fields.

## Dashboard

The Readiness page's AHRS/Barometer/Fan Controller tiles (`web/plates/readiness.html`,
`web/plates/js/readiness.js`) follow the same tile pattern as every other component
(state label + reason + conditional detail lines), and remain a **summary only** — the
full artificial-horizon display stays on the dedicated AHRS page
(`web/plates/gps.html`), not duplicated here. New detail lines, each only shown once the
component is connected:

- **AHRS**: connection state, measurement age (`"no attitude solution yet"` when null —
  never an interpolated `"last s ago"`, matching the existing radio-tile convention),
  pitch/roll (or `"unavailable"` per-axis), and whether a level reference is set.
- **Barometer**: pressure altitude, vertical speed, temperature, measurement age.
- **Fan Controller**: CPU temperature, requested duty percent when available, and an
  **always-shown**, unconditional line: *"Rotation feedback unavailable - no tachometer
  is installed; PWM commands cannot confirm the fan is physically spinning."*

No new CSS/breakpoints were added — the tiles reuse the existing `col-sm-6 col-md-4
label_adj` grid every other tile already uses, which is what makes the page responsive
across iPhone portrait, iPad portrait/landscape, and desktop today.

## Diagnostics

No changes were needed to `readiness/diagnostics.go` or `main/diagnosticsapi.go`:
`DiagnosticBundle.Health` is the full `readiness.HealthReport`, so the new `AHRS`/`Baro`/
`Fan` fields (connection state, measurement ages, calibration state, controller state)
flow into every generated bundle automatically. `SanitizeSettings`'s credential-pattern
scan and `recentSanitizedLogLines`'s bounded, line-dropping log filter are unaffected —
neither AHRS/baro/fan settings nor status contain anything matching the sensitive-key
pattern.

## Hardware validation checklist

- [ ] `GET /getHealth`'s `AHRS`/`Baro` read `READY` on the live Pi with the AHRS 2.0 board
      installed and enabled, pitch/roll responding to physical movement.
- [ ] `Fan` reads `READY` with `ControllerState: "COMMANDING"` or `"IDLE"` and the
      dashboard shows the rotation-feedback-unavailable message — confirm this by
      listening/feeling for the fan, never by trusting the health tile alone (no
      tachometer exists to confirm this in software).
- [ ] Disconnect/disable the IMU (`IMU_Sensor_Enabled = false` via `/setSettings`) and
      confirm `AHRS` reads `NOT_INSTALLED` (gray), not `NOT_READY` (red).
- [ ] Stop `stratux_fancontrol` (`systemctl stop stratux_fancontrol`) and confirm `Fan`
      reads `NOT_READY`, then restart it and confirm it recovers to `READY` within
      `fanControllerStaleAfter` (10s) once the daemon's next status write lands.
- [ ] `Set Level` while the receiver is deliberately tilted, confirm pitch/roll re-zero
      correctly and `AHRSHealth.LevelCalibrated` stays `true`.
- [ ] `Zero Drift` while stationary, confirm no spurious attitude jump.
- [ ] Start a recording, export CSV, confirm the new columns are populated with live
      values while airborne/moving and are empty cells (not `0`) before the AHRS produces
      its first solution.
- [ ] Generate a diagnostic bundle and confirm AHRS/Baro/Fan health, including
      `ControllerState` and measurement ages, appears in the JSON.
- [ ] Existing regression checklist in [readiness-and-time-trust.md](readiness-and-time-trust.md)
      still passes unchanged (radio assignment, GPS, time trust, storage).

## Rollback procedure

This work only adds fields/endpoints/tiles — it does not change any existing `/getStatus`
field, GDL90 output, receiver assignment, GPS, or OTA behavior. Two rollback paths, from
least to most disruptive:

1. **Preferred — OTA rollback (already proven, see [ota.md](ota.md)):** if this is
   deployed as an OTA `.deb` update and the new build shows a problem, the existing
   deterministic install state machine's rollback path restores the prior package and
   package-database state automatically on a rejected/failed install, and can also be
   triggered manually per `ota.md`'s state machine documentation — no manual file surgery
   needed.
2. **Manual — reflash or redeploy the prior build:** the previously deployed commit
   (`38b33fb46d`, this branch's base) remains a valid, unmodified target — `git checkout`
   that commit (or the tagged `.deb` built from it) and reinstall through the same OTA
   mechanism. Since this branch never modified `feature/us-dual-band-baseline` or the
   `d3ac9396` rollback baseline, both remain available as known-good fallbacks
   independent of this work entirely.

Because `AHRS`/`Baro`/`Fan` are additive report fields (not settings, not stored
persistent state beyond the pre-existing `globalSettings.C`/`D`/`SensorQuaternion`/
`IMUMapping`, which this work does not change the meaning of), rolling back requires no
data migration and no settings cleanup.
