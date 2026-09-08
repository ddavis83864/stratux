# Automatic Flight Recording

## Why this exists

The existing recording feature (`docs/recording.md`) is entirely manual: an
operator presses Start, then Stop, through the dashboard or the HTTP API.
Automatic Flight Recording is an **opt-in, disabled-by-default** addition
that can start and stop that same recording subsystem on the operator's
behalf, based on conservative, GNSS-derived movement detection - so a
recording is not lost to forgetting to press Start before taxiing.

It introduces no parallel recording implementation. It never deletes an
existing recording. It never claims to know an aircraft's actual taxi,
takeoff, landing, block, or Hobbs time - only that a configurable,
hedged movement threshold was observed for a configurable, hedged dwell
period.

> **Automatic Flight Recording uses configurable GNSS movement thresholds
> and dwell periods to start and stop supplemental recordings. Its event
> times are approximate and are not authoritative taxi, takeoff, landing,
> block, Hobbs, maintenance, or pilot-logbook times. The feature does not
> replace pilot recordkeeping.**

> **Automatic Flight Recording is disabled by default and never deletes
> an existing recording to create space.**

## Architecture: pure detection core, impure glue

Following this project's established convention (`alerting`, `readiness`,
`power`, `storagelifecycle`), the feature is split into two halves:

- **`autorecord`** (new Go package): a pure, hardware-independent state
  machine. No filesystem, GPS, HTTP, or clock calls of its own - every
  input is caller-supplied (`autorecord.MachineInput`), every output is a
  read-only `Snapshot` plus an `Action` the caller must actually carry
  out. Fully unit-tested without any device, GPS receiver, or timer.
- **`main/autorecord*.go`**: the glue that reads real GPS/settings/
  conflict state, drives the `Machine` on a 1 Hz ticker, and carries out
  its `ActionRequestStart`/`ActionRequestStop` through the *existing*
  recording subsystem (`startRecordingLocked`/`stopActiveRecording` -
  never a second, parallel start/stop implementation).

## The state machine

```
DISABLED -> ARMED_WAITING <-> START_CANDIDATE -> STARTING -> RECORDING
                  ^                                              |
                  |                                       STOP_CANDIDATE <-> RECORDING
              INHIBITED                                          |
                                                             FINALIZING -> (ARMED_WAITING | DISABLED)
                                                                          ERROR (any failed attempt)
```

- **DISABLED**: the feature is off. No detection runs.
- **ARMED_WAITING**: enabled, no qualifying motion yet.
- **START_CANDIDATE**: groundspeed at/above the start threshold; a
  continuous dwell timer is running. Any disqualifying sample - too slow,
  invalid/stale GPS, or a conflict (OTA, configuration restore, shutdown,
  a manual recording already active, denied/unknown storage, untrusted
  time, a restart cooldown still in effect) - **resets** the candidate,
  it does not pause it. A start candidate never itself starts a
  recording.
- **STARTING**: the dwell completed; a start request is in flight through
  the real recording subsystem.
- **RECORDING**: an automatic recording is active.
- **STOP_CANDIDATE**: groundspeed at/below the (strictly lower) stop
  threshold; its own continuous dwell timer is running. Speed rising back
  above the stop threshold cancels the candidate, returning to RECORDING.
- **FINALIZING**: the stop dwell completed - or the feature was disabled,
  or a confirmed controlled shutdown began - while recording; a stop
  request is in flight.
- **INHIBITED**: enabled, but a precondition currently blocks a *new*
  start. Never a failure; returns to ARMED_WAITING the moment the
  blocking condition clears.
- **ERROR**: the real recording subsystem reported a start/stop failure
  this feature cannot resolve on its own. Requires either
  `Settings.Enabled` to become false, or an explicit
  `POST /clearAutoRecordError`.

Every reason the machine is in its current state is exposed as a stable,
machine-readable `ReasonCode` (e.g. `waiting_for_motion`,
`start_dwell_in_progress`, `storage_denied`, `gps_stale`,
`restart_cooldown`, `manual_stop`), not only a human-readable string - see
`autorecord/state.go`.

## Detection policy: trustworthy GNSS only, conservative by design

Motion is judged **only** by GPS ground speed and fix validity/freshness -
never AHRS pitch/roll, magnetic heading, barometric altitude alone, ADS-B
traffic, FIS-B availability, ForeFlight connection, or internet access.
This mirrors `power.Health`'s own capability-honesty stance: no signal
this project cannot actually trust is treated as authoritative.

- **Start**: groundspeed &ge; `StartGroundspeedKnots`, held continuously
  for `StartDwellSeconds` (default 8 kt / 30 s).
- **Stop**: groundspeed &le; `StopGroundspeedKnots`, held continuously
  for `StopDwellSeconds` (default 4 kt / 120 s). `StopGroundspeedKnots`
  is **strictly** lower than `StartGroundspeedKnots` (enforced by
  `Settings.Validate`) - the hysteresis gap this project relies on to
  never flap at one boundary value.
- **GPS loss while recording never itself stops a recording.** A grace
  period (`GPSLossGraceSeconds`, default 30 s) is purely informational
  (`ReasonGPSLossGrace` vs. `ReasonGPSUnavailableExtended`); only a later
  *valid* sample at/below the stop threshold, held for the full stop
  dwell, can trigger a stop. A GPS dropout of any length, by itself,
  leaves an active recording running.
- **Restart cooldown** (`RestartCooldownSeconds`, default 300 s): after
  any stop of an automatic recording, a new automatic start is inhibited
  until the cooldown elapses, even if the start condition is already
  satisfied - preventing many short, fragmented recordings from
  repeatedly crossing the start threshold right after a stop.
- **Minimum recording duration** (`MinimumRecordingDurationSeconds`,
  default 0/disabled): if set, a satisfied stop dwell is deferred (not
  discarded) until the recording has been active at least this long.

### Clock sources

- **Monotonic** time (`NowMonotonic`, seconds since an arbitrary epoch -
  this project's `stratuxClock`/`monotonicSeconds()`) drives every dwell,
  grace period, cooldown, and staleness check. A wall-clock correction
  (GPS time jump, NTP step) never perturbs an in-progress dwell.
- **Trusted UTC** wall-clock time is used only for persisted timestamps
  and human-readable metadata (recording IDs, `StartedAt`/`StoppedAt`) -
  never for state-machine decisions.
- A new automatic recording is **inhibited** (`ReasonTrustedTimeUnavailable`)
  until the system clock is backed by a trusted source
  (`readiness.TimeGNSSSynced` or `TimeNetworkSynced` - see
  `main/autorecordinput.go`'s `autoRecordTrustedTime`). `DEGRADED`/
  `INVALID`/`UNSYNCHRONIZED` all count as untrusted.

## Storage integration: the validated Storage Lifecycle Foundation, unchanged

Automatic Flight Recording introduces **no new quota or pressure
computation**. Every storage decision is
`storagelifecycle.EvaluateRecordingSpace`, fed the existing
`storageManager`'s own already-computed `Status()` - see
`main/autorecordstorage.go`'s `autoRecordLifecycleAdapter`, the first
production implementation of the `storagelifecycle.RecordingLifecycle`
contract (`docs/storage-lifecycle.md`'s "Future automatic-recording
contract" section, written during the Storage Lifecycle Foundation
mission specifically for this feature).

- `RecordingSpaceAllowed`/`RecordingSpaceCaution` both permit a start
  (caution is reported, not blocking - the whole point of `caution` in
  that contract is "technically fine, elevated pressure").
- `RecordingSpaceDenied` and an inability to determine pressure at all
  (`StorageUnknown`) both **inhibit** a new automatic start
  (`ReasonStorageDenied`/`ReasonStorageUnknown`) - treated conservatively
  alike, but reported with distinct reason codes so a diagnostic
  consumer can tell "no room" apart from "could not tell."
- `RegisterActive`/`Complete` are accepted for full contract compliance
  but are effectively no-ops: `storageLifecycleActiveChecker`
  (`main/storagelifecycleapi.go`) already reports *any* `recCurrent`
  (automatic or manual) as active purely from its own `ID`/`State` -
  verified unchanged during this mission's implementation, no code there
  needed to move.
- **Automatic Flight Recording never triggers or waits on a fresh
  storage scan.** It only ever reads `Manager.Status()`'s cached
  snapshot (see `docs/storage-lifecycle.md`'s "Scan cancellation"
  section) - detection never blocks on a filesystem walk.
- **No automatic eviction is introduced.** Denied storage means "do not
  start," never "delete something to make room."

## Recording-subsystem integration: no parallel implementation

`main/autorecordrun.go`'s `autoRecordPerformStart`/`autoRecordPerformStop`
call **exactly** the same functions the manual `/startRecording`/
`/stopRecording` endpoints use:

1. `buildPreflightReport()` - always called *before* any lock is taken
   (it transitively locks `recMu` itself; the project's own established
   deadlock precedent, see `main/recordingapi.go`).
2. `startRecordingLocked(preflightSnapshot)` / `stopActiveRecording(mode)`.
3. The same initial-metadata write (`recording.WriteInitialMetadata`) and
   the same finalization write (`recording.FinalizeMetadata`), both
   outside `recMu`.

The one behavioral difference from a manual start: an automatic start
also records *why* it started (see "Recording metadata," below) via an
additional, optional `autoRecordSessionContext` parameter that
`buildSessionSnapshot` accepts - `nil` for every manual start, so nothing
about the manual workflow changed.

### Coexistence with a manual recording

- If a recording (manual or automatic) is already active, automatic
  starting is inhibited (`ReasonManualRecordingActive`) - `recCurrent`'s
  own state is the single source of truth; this feature never tracks a
  second, parallel notion of "is something recording."
- If the owner manually stops what turns out to be an automatic
  recording (`POST /stopRecording` while automatic), the manual stop
  runs entirely unchanged, then a small, additive hook
  (`autoRecordNotifyManualStopIfOwned`) tells the `Machine` its
  recording just ended for a reason it did not itself request
  (`Machine.OnExternalStop`) - the finalization records
  `AutoRecordStopMode: "manual"`, and the machine re-arms (with the
  usual restart cooldown) instead of believing it is still recording.
- Enabling/disabling the feature never starts or stops a manual
  recording, and a manual recording's own start/stop is never
  reinterpreted as automatic (initiation mode is set once, at start, and
  never changed).

### Shutdown interaction

A **confirmed** controlled shutdown (`power.Manager` stage
`ShutdownRequested`/`Flushing`/`ReadyToPowerOff`/`CommandIssued` - not
merely `ConfirmationRequired`, which is still fully revocable) forces an
immediate finalize of an active automatic recording, bypassing any stop
dwell. `gracefulShutdown()` calls `autoRecordHandleShutdown()` - one
synchronous detection tick with the shutdown conflict already visible -
strictly *before* the existing, unconditional `stopRecordingForShutdown()`
call, which remains the correct, unchanged path for a manual recording
and is a harmless idempotent no-op if the automatic path already stopped
the only active recording.

### OTA / configuration-restore interaction

An OTA update in progress or a configuration restore in progress each
inhibit a *new* automatic start (`ReasonOTAInProgress`/
`ReasonConfigRestoreInProgress`), reusing the exact same precondition
checks `power.Manager`'s own shutdown flow already uses
(`otaNotBusyPrecondition`/`configBackupNotBusyPrecondition` -
`main/powerapi.go`). Neither forces a stop of an *already-active*
recording by itself (an OTA/restore does not imply an imminent power
loss the way a confirmed shutdown does); restoring settings still applies
atomically (see "Configuration Backup/Restore integration," below).

## Recording metadata (schema version 5, purely additive)

`recording.SessionSnapshot` gained (all `omitempty`, all zero-valued for
a legacy or manually-started recording):

| Field | Meaning |
|---|---|
| `AutoRecordInitiationMode` | `"manual"` or `"automatic"` |
| `AutoRecordTriggerReasonCode` | the `autorecord.ReasonCode` at the moment of the start request |
| `AutoRecordPolicySchemaVersion` | `autorecord.SettingsSchemaVersion` in effect at start |
| `AutoRecordStart/StopGroundspeedKnots`, `AutoRecordStart/StopDwellSeconds` | the configured thresholds in effect at start |
| `AutoRecordStorageDecision` | `allowed`/`caution` as observed at start |
| `AutoRecordContinuationOfRecordingID` | set only for a recovered continuation segment (see below) |

`recording.SessionFinalization` gained:

| Field | Meaning |
|---|---|
| `AutoRecordStopMode` | `"automatic"`, `"manual"`, `"shutdown"`, or `"feature_disabled"` |
| `AutoRecordInterrupted` | true only when a later boot's recovery pass found this session left running by a crash/reboot |
| `AutoRecordContinuedByRecordingID` | forward link to a recovered continuation segment, if one was started |

None of these fields are ever taxi, takeoff, landing, block, Hobbs, or
logbook time.

## Restart and crash recovery

`main/autorecordrecovery.go`'s `autoRecordScanForInterrupted` runs once,
at startup, before the detection loop starts:

1. Lists `recordingsDir` **one level deep** (bounded - never recurses
   into a recording's own sample files, never grows with a device's
   recording history) and takes only the single most recently created
   entry (recording IDs are lexicographically sortable timestamps).
2. Reads only that recording's small `metadata.json` sidecar
   (`recording.ReadMetadata` - never the `*.jsonl` sample files).
3. If it was automatically started, is not already `Complete`, and was
   not already marked interrupted by an earlier boot: writes
   `SessionFinalization{AutoRecordInterrupted: true}` - **never**
   fabricating a stop time, duration, or sample count it cannot know,
   and **never** re-patching an already-marked session (idempotent
   across repeated reboots).
4. Records the interrupted recording's ID as a one-time pending
   continuation link, valid only for `autoRecordContinuationWindowSeconds`
   (10 minutes) after this boot.

If normal detection then starts a new automatic recording within that
window (using **current**, not pre-restart, GPS samples - the `Machine`
itself is never "resumed," only re-armed from `Settings.Enabled` like any
other boot), that new session is linked via
`AutoRecordContinuationOfRecordingID`/`AutoRecordContinuedByRecordingID`
- a new, separate recording, never a blind append to the old session's
files. The link is consumed at most once, and only on a *successful*
start (a failed attempt does not burn it).

A manually-started recording interrupted by a crash is **not** touched by
this recovery pass - that is the pre-existing, unchanged behavior of the
manual recording feature.

## Configuration and persistence

`main/autorecordsettings.go` mirrors `alertsettings.go`'s exact pattern:
schema-versioned JSON, a mutex-protected temp-file/fsync/rename atomic
write, and a missing/corrupt/schema-mismatched/failed-validation file all
degrade safely to `autorecord.DefaultSettings()` (`Enabled: false`) -
never fatal, never silently enabling the feature.

## HTTP API

| Endpoint | Method | Purpose |
|---|---|---|
| `/getAutoRecordStatus` | GET | the `Machine`'s current `Snapshot` plus settings in effect |
| `/getAutoRecordSettings` | GET | persisted settings only |
| `/setAutoRecordSettings` | POST | validate, persist, and apply new settings |
| `/clearAutoRecordError` | POST | explicit ERROR-state recovery |

A settings change is visible starting with the very next 1 Hz detection
tick, never mid-tick (`autoRecordSettingsCache` is updated under the same
`autoRecordMu` the tick itself uses).

## Concurrency

Automatic Flight Recording runs on **its own dedicated goroutine**
(`autoRecordLoop`, 1 Hz) - it never blocks ADS-B/GPS/GDL90/AHRS/alerting/
the main daemon loop, and nothing else blocks on it beyond the brief,
uncontended `autoRecordMu` critical sections around one detection tick,
one settings update, or one manual-stop reconciliation.

**Lock order** (documented in `main/autorecordrun.go`, never inverted):
`autoRecordMu` may be held while acquiring/releasing `recMu`; `recMu`
must never be held while acquiring `autoRecordMu`.

GPS state is read directly from `mySituation` under its own `muGPS`
mutex, deliberately **without** calling the existing `isGPSValid()` -
that function resets several `mySituation` fields to sentinel values as
a side effect when it finds no valid fix, which is established, correct
behavior for *its own* callers but must not be triggered merely as a
side effect of this feature's independent, read-only GPS observation.
The validity criteria themselves (`fix quality > 0`, GPS connected, fix
age < 3 s) are an independent re-derivation of the *same* policy, not a
different one.

## Readiness, Preflight, and diagnostics integration

- **Readiness**: `NOT_INSTALLED`/`DISABLED` (feature off) never degrades
  overall system readiness. `READY` while armed/recording normally;
  `DEGRADED` while `INHIBITED` on a recoverable condition (storage
  caution, cooldown); `NOT_READY` only for `ERROR`; `UNKNOWN` if the
  feature has not yet initialized.
- **Preflight**: an informational-only check - a disabled feature is
  never `CAUTION`/`NOT_READY`, only a concise "automatic recording is
  off" note.
- **Diagnostics**: a sanitized summary (state, reason code, counters) -
  never exact GPS coordinates, never a full sample trace.

## Configuration Backup/Restore integration

Mirrors `AlertSettingsSection`'s established pattern
(`configbackup/document.go`, `main/configbackupapi.go`'s
`alertSettingsSectionFromCurrent`/`applyAlertSettingsSection`): automatic
recording's settings are one additional, independently versioned section
of the configuration backup document, applied atomically as part of the
existing restore transaction - a restore never changes any existing
recording's own recorded origin (manual/automatic), only the *going-
forward* configuration.

## Dashboard

A settings panel (enable/disable, the six numeric thresholds, a live
status readout) with the two disclaimer statements from the top of this
document shown verbatim, and bounded input ranges matching
`autorecord.Settings.Validate`.

## Non-goals (explicitly out of scope for this feature)

- No FIS-B weather cache (a separate, unrelated future contract in
  `storagelifecycle`).
- No closure-rate/CPA alerting.
- No automatic storage eviction of any kind.
- No pilot guidance, maneuver commands, or flight-phase labeling.
- No change to Wi-Fi, receiver assignments, AHRS calibration, calibration
  profiles, alert thresholds, or controlled-shutdown behavior.

## Test strategy

- `autorecord/machine_test.go` (28 tests): default-disabled, no start
  from a stale pre-enable sample, sustained qualifying start, spike
  rejection, hysteresis, stop dwell (including cancellation), no
  duplicate start/stop while a request is in flight, restart cooldown,
  monotonic-only dwell accounting (wall-clock independence), ERROR
  recovery, zero-value safety, every start-blocking precondition
  individually, shutdown-forces-immediate-finalize,
  feature-disabled-while-recording, GPS-loss grace (short and long),
  minimum-recording-duration deferral, repeated enable/disable cycling.
- `autorecord/settings_test.go`: default settings are disabled and
  self-valid; a table of `Validate()` bound/hysteresis checks.
- `main/autorecordsettings_test.go`: missing/corrupt/schema-mismatched/
  invalid persisted files all degrade to defaults; save rejects invalid
  settings without partially writing; save-then-load round-trips; save
  is atomic (no leftover temp file).

## Known limitations

- GPS ground speed jitter on a stationary receiver is the basis for the
  default 8 kt start threshold, not a formally derived bound for every
  receiver this project supports - an operator with a noisier receiver
  should raise it.
- Recovery only ever inspects the single most recent recording directory
  - an interrupted recording that is not the most recent one (e.g. a
  crash during a *later*, manually-started recording, itself later
  followed by another automatic one before the next reboot) is not
  retroactively found. This is an intentional, bounded scope, not a
  planned future expansion within this feature.
