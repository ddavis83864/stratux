# Named Aircraft Calibration Profiles

This documents the `calprofile` package and its wiring into `main/`: persistent,
named AHRS calibration profiles that let the same Stratux move between aircraft and
restore the right mounting calibration, instead of overwriting a single global one.
See [readiness-and-time-trust.md](readiness-and-time-trust.md) for the component-health
model this integrates with, and [ahrs-baro-fan-health.md](ahrs-baro-fan-health.md) for
the live AHRS health this extends.

**Purpose and limitations.** A calibration profile is mounting/aircraft *metadata*
attached to a level-reference quaternion and a gyro zero-drift bias - nothing more. A
profile's `VALID` calibration state, however it is named, **never makes the AHRS
certified**, is never used for flight-control commands, and never changes anything
about GDL90 output, traffic-alert logic, or ForeFlight interoperability. This remains
supplemental, non-certified situational-awareness equipment, exactly as documented
everywhere else AHRS data is surfaced.

## Why `globalSettings` still exists

`main/sensors.go`'s calibration engine (`sensorAttitudeSender`, `CageAHRS`/`Set Level`,
`CalibrateAHRS`/`Zero Drift`) is **completely unmodified** by this feature - it still
reads and writes `globalSettings.C`/`D`/`SensorQuaternion`/`IMUMapping` exactly as
before. Profiles are a layer *on top*: at startup (after `readSettings()`, before
`initI2CSensors()` - see `main()` in `main/gen_gdl90.go`), the active profile's stored
calibration is copied into `globalSettings`, so the engine consumes profile-sourced
values through the identical, unchanged code path. Every time a profile is activated,
or Set Level/Zero Drift completes, the same copy happens again, in the same direction:
**the profile store is the durable source of truth; `globalSettings.C/D/SensorQuaternion`
is a live, in-memory mirror the engine reads.** This mirror is deliberately not written
back to `stratux.conf` via `saveSettings()` on every profile-driven update (the profile
store already durably persists it) - `stratux.conf`'s own calibration fields simply
reflect whatever the active profile was at last `Set Level`/`Zero Drift` time through
the engine's own existing `saveSettings()` calls, and may lag the profile store slightly
between an activation and the next calibration action. This is a deliberate,
documented, harmless divergence - `globalSettings` in memory is always current, which is
all the engine and the API ever read.

## Profile schema (`calprofile.Profile`)

| Field | Type | Notes |
|---|---|---|
| `ID` | string | Server-generated, e.g. `profile-0123456789abcdef` - stable, opaque, independent of `Name`. |
| `Name` | string | Required, ≤64 unicode runes. |
| `Registration` | string | Optional, ≤20 runes. |
| `AircraftType` | string | Optional, ≤64 runes. |
| `MountingNote` | string | Optional, ≤200 runes. |
| `IMUMapping` | `[2]int` | Same as `globalSettings.IMUMapping`. |
| `SensorQuaternion` | `[4]float64` | Level reference - same as `globalSettings.SensorQuaternion`. |
| `C`, `D` | `[3]float64` | Accelerometer/gyro zero bias - same as `globalSettings.C`/`D`. |
| `LevelCalibrated`, `GyroCalibrated` | bool | Recomputed from the vectors' magnitude on every load/save - never trusted from a caller. |
| `Kind` | string | `migrated`, `user`, or `uncalibrated` - see below. |
| `SchemaVersion` | int | Currently `1`. |
| `CreatedAt`, `ModifiedAt` | time.Time | |
| `LastCalibratedAt` | `*time.Time` (nullable) | `null` until Set Level *and* Zero Drift have both succeeded at least once for this profile. |

`Kind`:
- **`migrated`** - the one profile `EnsureMigrated` creates automatically the first
  time a profile-aware build starts on a system with an existing, fully-calibrated
  legacy calibration. Named `"Current Installation"`.
- **`uncalibrated`** - a profile (migrated or user-created) whose level reference and/or
  gyro bias have never been successfully set. Never silently treated as calibrated.
- **`user`** - any profile the owner explicitly created, or an uncalibrated profile that
  has since received a real calibration (see "graduation" below).

Bounds: at most 20 profiles, unicode-rune-safe length limits above, unique
IDs independent of display names, duplicate names (case-insensitive) rejected.

## Persistence and atomicity

Profiles live under `/var/lib/stratux-data/calibration-profiles/` - a sibling of
`recordings/`, `diagnostics/`, and `exports/` (see `main/recordingapi.go`/
`main/diagnosticsapi.go`), never the temporary root overlay, so profiles survive both
reboot and OTA update. Each profile is one file, `<id>.json`; the active-profile
pointer is a small separate file, `active.json`.

Every write (`calprofile.Store.Save`/`SetActiveID`) is atomic: a temp file in the same
directory, `fsync`'d, then `os.Rename`'d over the final path - matching
`readiness.WriteDiagnosticBundle`/`common.WriteFanControllerStatus`'s established
pattern, with an added `fsync` before rename (profile data is rarer-written and more
precious than a diagnostic bundle or a 1Hz fan-status snapshot). A crash or power loss
mid-write can never leave a partially-written or corrupt profile file at its final name.

Path safety mirrors `main/safepath.go`'s principle (exact match against a fresh
directory listing, never string-scrubbing an input) reimplemented inside `calprofile`
itself so the package stays free of any dependency on `main` - profile IDs are validated
against a fixed shape (`^profile-[0-9a-f]{16}$`) generated from 8 bytes of
`crypto/rand`, never derived from or predictable by a display name.

A profile file that fails to parse (corruption) is skipped by `Store.List`, not fatal -
`Store.ListWithErrors` reports which files were unreadable, for diagnostics. A profile
with a `SchemaVersion` newer than this build understands is treated the same way
(skipped, reported), not silently misread.

## Legacy migration

On first startup, `EnsureMigrated` checks whether the profile store already has any
profiles:

- **Empty store** (first profile-aware boot): the current `globalSettings.C`/`D`/
  `SensorQuaternion`/`IMUMapping` are copied verbatim into a new profile named
  `"Current Installation"`, marked active. If that legacy calibration was genuinely
  complete (both level and gyro set), the profile is `Kind: migrated` and immediately
  usable. If it was never calibrated (the normal factory-fresh state), or contained a
  non-finite (NaN/Inf) value that could not be safely stored, the profile is honestly
  `Kind: uncalibrated` - the legacy values are still preserved wherever they were finite;
  only a component that could never legitimately occur from a real calibration read is
  zeroed, and only that axis is marked uncalibrated as a result. **A working legacy
  calibration is never reset to zero by migration.**
- **Store already has profiles**: a pure no-op - the existing active profile is returned
  unchanged, even if `globalSettings` has since drifted. Safe to run on every startup, not
  just the first.
- **Profiles exist but the active pointer is missing/corrupt**: `EnsureMigrated` does
  **not** guess or auto-repair by picking an arbitrary profile - it surfaces the
  underlying error, and `main/health.go`'s AHRS tile reports an honest `DEGRADED` (see
  below) rather than silently activating something nobody chose.

## Active-profile behavior

Exactly one profile is active at a time. Activating a profile:

1. Refuses (`409 Conflict`) while a recording is active - the mission's preferred,
   simpler-and-safer choice over an in-recording calibration-change event (see
   "Recording integration" below).
2. Validates the target profile exists.
3. Persists the new active-profile pointer.
4. Copies the complete calibration into `globalSettings` as one step, under
   `profilesMu` - the running system never observes a partial mix of two profiles'
   calibration.
5. On any failure, the currently active calibration is left completely unchanged.

Re-activating the already-active profile is a safe, idempotent no-op that still
returns success. Activating an uncalibrated profile is allowed - AHRS health becomes
`DEGRADED`, not falsely `READY`, and the dashboard clearly shows `INCOMPLETE` and
directs the owner to Set Level / Zero Drift.

## Calibration actions (Set Level / Zero Drift)

`main/sensors.go`'s existing calibration retry loop (unchanged) still writes
`globalSettings.C`/`D`/`SensorQuaternion` exactly as before. One new, additive line at
the exact point that loop finishes (`captureActiveProfileCalibration(action)`) snapshots
the result into the active profile and updates `LastCalibratedAt` once *both* Set Level
and Zero Drift have succeeded for it. An uncalibrated profile that receives its first
real calibration this way "graduates" from `Kind: uncalibrated` to `Kind: user` - it is
no longer just inherited data, it reflects an action the owner deliberately took.

If no active profile exists (a corrupt/uninitialized profile store), `POST /cageAHRS`
and `POST /calibrateAHRS` (Set Level/Zero Drift) now return `409 Conflict` with a clear
message, rather than silently running the calibration algorithm with nowhere durable
for its result to land beyond the legacy `stratux.conf` mirror.

`POST /captureCalibrationProfile[?id=...]` is a separate, explicit action: it snapshots
whatever calibration is *currently live* in `globalSettings` into a profile (the active
one by default), without requiring a fresh Set Level/Zero Drift run - useful for
assigning an already-good live calibration to a specific profile record. Capturing into
a profile that is not currently active never changes what the running system is using.

Automatic in-flight recalibration is not implemented. Magnetic heading is never a
calibration-validity gate - `main/sensors.go` does not calibrate one at all (see
`ahrs-baro-fan-health.md`), and `readiness.BuildAHRSHealth` has no heading parameter to
begin with.

## API

All endpoints follow the existing `main/recordingapi.go`/`main/diagnosticsapi.go`
conventions: JSON bodies, explicit HTTP status codes, no path parameters (query string
only), `success`/`error` response shape.

| Endpoint | Method | Purpose |
|---|---|---|
| `/getCalibrationProfiles` | GET | List every profile plus `activeProfileId`. |
| `/getActiveCalibrationProfile` | GET | The active profile. `404` if none is set. |
| `/getCalibrationProfileStatus` | GET | Active profile + subsystem availability - a single-request dashboard summary. |
| `/createCalibrationProfile` | POST | Body: `{name, registration, aircraftType, mountingNote}`. Creates an uncalibrated profile; does **not** activate it. |
| `/updateCalibrationProfile?id=...` | POST | Body: same metadata fields. Never touches calibration vectors. |
| `/activateCalibrationProfile?id=...` | POST | Makes a profile active; `409` while recording. |
| `/deleteCalibrationProfile?id=...` | POST | `409` if `id` is the active profile. |
| `/captureCalibrationProfile[?id=...]` | POST | Snapshots the current live calibration into a profile (active, if `id` omitted). |

Status codes: `400` invalid ID/JSON, `404` unknown profile/no active profile, `409`
duplicate name / delete-active / activate-while-recording, `507` (Insufficient Storage)
profile-count limit reached, `503` profile subsystem unavailable, `405` wrong method.
No filesystem path is ever exposed in any response - only the opaque profile ID.

## Dashboard

An "Aircraft Calibration Profiles" panel on the existing AHRS/GPS page
(`web/plates/gps.html`, alongside the existing Set Level/Zero Drift buttons) shows the
active profile's name/registration/aircraft/mounting note/calibration state, a table of
every profile with Activate/Delete actions (delete asks for confirmation, and is hidden
for the active profile), and a compact create-profile form. All actions use a shared
`CalProfiles.busy` flag to prevent duplicate in-flight requests, matching the existing
recording/diagnostics panels' pattern. No new CSS - it reuses the existing Bootstrap-like
grid and panel classes every other panel on this page already uses, so it renders
correctly on iPhone portrait, iPad portrait/landscape, and desktop the same way the rest
of the page does.

The Readiness page's AHRS tile stays concise, adding only two lines:

```text
Profile: Cherokee Six
Calibration: VALID
```

(or `INCOMPLETE`, or an explicit "calibration profile unavailable: ..." line if the
subsystem itself has a problem) - it is not turned into a profile-management interface;
full CRUD lives on the AHRS/GPS page.

## Readiness integration

`readiness.AHRSHealth` gained a `Profile AHRSProfileInfo` field
(`ID`/`Name`/`Kind`/`LastCalibratedAt`/`Available`/`Error`). Rollup rules, added to the
existing AHRS state machine (see `ahrs-baro-fan-health.md` for the pre-existing rules
this extends):

- Active, calibrated profile with fresh, valid AHRS data -> `READY` (unchanged).
- Active but incomplete calibration (level set without gyro, or vice versa, or neither)
  -> `DEGRADED` - this also closes a gap in the pre-profile logic, which only checked
  the level reference and never separately gated on the gyro bias.
- Hardware perfectly healthy but the profile subsystem itself unavailable
  (missing/corrupt store) -> honest `DEGRADED`, with a reason naming the problem. This
  **never masks a genuine hardware failure**: a disconnected IMU stays `NOT_READY`
  regardless of profile-subsystem state, and a profile problem never quietly resets to
  `READY`.
- Missing AHRS hardware -> unchanged existing `NOT_INSTALLED`/`NOT_READY` behavior.

A profile-subsystem failure never affects any other component's health - see
`main/calprofilesapi.go`'s doc comment for the full list of subsystems this is
guaranteed not to interrupt (978/1090/GPS/trusted time/FIS-B/GDL90/Wi-Fi/diagnostics/
existing recordings/fan control/barometer).

## Recording and export integration

Profile identity is **session-level** metadata (`recordingSession`'s
`CalibrationProfileID`/`Name`/`Kind`/`Registration`/`AircraftType`/`MountingNote`/
`Valid`/`LastCalibratedAt`/`Available` fields, exposed via `/getRecordingStatus` and
`/getRecordings`), captured once at `/startRecording` time and never changed for the
life of the session - deliberately *not* a per-sample field, since the profile cannot
change mid-recording (see below) and repeating an unchanging string into every 1Hz
sample would only duplicate it. `recording.Sample`'s own existing per-sample
`AHRSCalibrationState` field (already `READY`/`DEGRADED`/etc.) continues to convey the
moment-to-moment calibration quality; the session-level fields answer the separate
question of *which* profile that quality was measured against. CSV export's existing 23
documented columns are unchanged - profile identity is available via the recording
status/list JSON API rather than repeated into every row, exactly the anti-duplication
choice this document's mission scope calls for.

**Switching the active profile while a recording is active is rejected** (`409` from
`/activateCalibrationProfile`) - the simpler, safer of the two options the design
considered, chosen deliberately over introducing a mid-recording calibration-change
event.

## Diagnostics

`readiness.DiagnosticBundle` gained `CalibrationProfiles []CalibrationProfileSummary`
(every profile's ID/Name/AircraftType/Kind/CalibrationValid/LastCalibratedAt/
SchemaVersion) and `ActiveCalibrationProfileID`. No separate sanitization pass is
needed for these fields - a profile never contains real-world location data, passwords,
or any other credential-shaped value by this feature's own scope.

## Recovery from corrupt storage

- A corrupt individual profile JSON file is skipped by every read path (`List`,
  `Active` if it happens to be the active one - see `Store.Active`'s distinguished
  `ErrNotFound`/parse-error returns) - never crashes the daemon.
- A corrupt or missing `active.json` with profiles still present is surfaced as
  `ErrNoActiveProfile`/a parse error, not auto-repaired by guessing - `AHRSHealth`
  reports `DEGRADED` with an explanatory reason; **AHRS hardware itself keeps working**
  off whatever calibration was already loaded into `globalSettings` at last successful
  startup, and every other subsystem (978/1090/GPS/GDL90/etc.) is completely unaffected.
- Operator recovery: activate any known-good profile via the dashboard or
  `/activateCalibrationProfile`, which rewrites a fresh `active.json`; or, if the entire
  `calibration-profiles/` directory is unusable, remove it and restart - `EnsureMigrated`
  will re-migrate from whatever calibration is currently live in `globalSettings`/
  `stratux.conf`, exactly as it did on the original first boot.

## Rollback

This feature is purely additive: no existing `/getStatus`/`/getSettings`/`/getHealth`
field was renamed, removed, or reinterpreted, and `main/sensors.go`'s calibration engine
is unmodified. Rolling back to a pre-profile build requires no data migration -
`globalSettings.C`/`D`/`SensorQuaternion`/`IMUMapping` continue to hold whatever the
active profile last copied into them, exactly as if that calibration had been set the
old-fashioned way, so a rolled-back build keeps working with the same calibration.
The `calibration-profiles/` directory on the persistent partition is simply unused (not
deleted) by a build that predates this feature.

## Hardware-validation checklist

- [ ] First boot after this update creates exactly one profile, `"Current Installation"`,
      preserving the live device's existing (already hardware-validated) calibration
      verbatim - confirm `LevelCalibrated`/`GyroCalibrated` are both `true` and `Kind`
      is `migrated`.
- [ ] A second restart does not create a second profile (idempotent migration).
- [ ] Create a new profile, confirm it is `uncalibrated` and NOT auto-activated.
- [ ] Activate the new profile, confirm AHRS health goes `DEGRADED` with an
      "incomplete" reason, and the dashboard shows `INCOMPLETE`.
- [ ] Perform Set Level then Zero Drift on the new (now active) profile, confirm
      `LastCalibratedAt` becomes non-null, `Kind` becomes `user`, and AHRS health
      returns to `READY`.
- [ ] Activate the original `"Current Installation"` profile back, confirm its
      calibration (pitch/roll response) is exactly as it was before switching.
- [ ] Start a recording, confirm `/getRecordingStatus` reports the active profile's
      identity; attempt to activate a different profile while recording, confirm `409`.
- [ ] Delete the test profile (after deactivating it), confirm it is gone and the
      active profile is unaffected.
- [ ] Generate a diagnostic bundle, confirm it lists every profile and the active id.
- [ ] Reboot, confirm the active profile and its calibration survive.
