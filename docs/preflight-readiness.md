# Simplified Preflight Readiness Workflow

This documents the `preflight` package and its wiring into `main/`: a simplified,
supplemental preflight checklist built entirely on top of the existing
[readiness](readiness-and-time-trust.md) health model. It does not measure hardware or
duplicate any component-health logic - it consumes an already-built
`readiness.HealthReport` and applies an explicit, documented decision policy to reduce
it to a one-screen summary, plus a small fixed set of manual (human-observed) checks
software cannot prove on its own.

## Purpose and the non-certified limitation

Every preflight report and every page this feature renders carries the same exact
statement:

> Supplemental, non-certified preflight aid. The pilot remains responsible for
> verifying the aircraft, equipment, weather and flight conditions.

This is not a certification or an airworthiness determination. The workflow never
controls the aircraft and never modifies ADS-B/GDL90 operation - it is read-only with
respect to every other subsystem. The following words are deliberately never used
anywhere in this feature's text: *airworthy*, *certified*, *safe to fly*, *approved for
flight*, *collision avoidance*, *guaranteed*.

## Architecture

`preflight` is a new, independent package (not folded into `readiness`), matching the
project's existing pattern of `readiness`/`recording`/`ota`/`calprofile` each owning one
concern:

- **`readiness`** measures component health (`ComponentState`: `READY`/`DEGRADED`/
  `NOT_READY`/`NOT_INSTALLED`/`UNKNOWN`) - "is this component working right now."
- **`preflight`** answers a different question - "is this ready for the pilot to rely
  on before flight" - which needs a distinct, simpler top-level vocabulary
  (`READY`/`CAUTION`/`NOT_READY`) plus two ideas `readiness` has no use for: `VERIFY`
  (a human must look at this) and a fixed set of manual, session-bound
  acknowledgements. Folding this into `readiness` would have mixed a measurement
  package with a presentation/decision-policy layer; keeping it separate keeps both
  packages smaller and independently testable.

`preflight` is pure and hardware/cgo-free, exactly like `readiness`: `preflight.BuildReport`
takes an already-gathered `preflight.Input` (built from `globalHealth`, the active
calibration profile, the recording subsystem, and a manual-acknowledgement snapshot)
and returns a deterministic `preflight.Report`. It never touches global state or
performs I/O, so every decision-policy rule is exercised directly by `go test` without
a Raspberry Pi, an SDR, or a GPS receiver attached - see `preflight/report_test.go`.

The thin glue that gathers real signals and exposes the HTTP API lives in
`main/preflightapi.go`, following the same conventions `main/calprofilesapi.go` and
`main/recordingapi.go` already established.

## State definitions

Top-level report state (`Report.Overall` - always exactly one of these three):

| State | Meaning |
|---|---|
| `READY` | All required automated checks pass and all required manual checks are acknowledged. |
| `CAUTION` | Flight-support functions remain available, but an optional/degraded item, an environmental condition, or an incomplete manual check needs attention. |
| `NOT_READY` | A required technical function is unavailable or unsafe for the intended operation. |

Individual check state (any of the above, plus):

| State | Meaning |
|---|---|
| `VERIFY` | This item requires direct human observation - software cannot prove it. |
| `NOT_APPLICABLE` | The underlying feature is intentionally disabled or not installed - never a failure, and never affects the rollup. |
| `UNKNOWN` | Insufficient trustworthy information exists yet (e.g. still within a startup grace period). Never silently treated as `READY`. |

`UNKNOWN` as a top-level `Report.Overall` value is reserved for exactly one case: the
preflight *calculation itself* failing (see "Failure isolation" below) - `BuildReport`'s
own decision policy, given any input, always produces `READY`/`CAUTION`/`NOT_READY`.

## Blocking vs. caution: the decision policy

Every check carries a `Severity` (`BLOCKING`/`CAUTION`/`INFO`) alongside its `State`.
The overall rollup (`preflight.rollup`, in `report.go`) is:

1. If any `BLOCKING`-severity check is not `READY`/`NOT_APPLICABLE` (and not itself
   `INFO`-severity, which never counts): overall is `NOT_READY`.
2. Otherwise, if any non-`INFO` check is not `READY`/`NOT_APPLICABLE`: overall is
   `CAUTION`.
3. Otherwise: `READY`.

`INFO`-severity checks (e.g. the fixed "fan rotation is never electronically
confirmed" notice, or the GDL90 client-identification disclosure) never affect the
rollup regardless of their `State` - they exist to inform, not to gate.

**`NOT_READY` (blocking) conditions**, each documented at its call site in
`preflight/checks.go`:

- Persistent data expected but unavailable or read-only
- Protected-root overlay structurally invalid
- Active undervoltage, or a confirmed enabled receiver band with no receiver assigned
- GPS hardware entirely missing
- GDL90 output itself unavailable (not merely "no client connected")
- A failed systemd unit (`readiness.SystemHealth.FailedServices`)
- A corrupt/unavailable calibration-profile subsystem (not merely "uncalibrated")

**`CAUTION` conditions:**

- GPS present but no satellite solution, after the acquisition grace period
- Trusted time not yet established, after the GNSS-time grace period
- A correctly-detected receiver (978 or 1090) with zero *current* traffic - **this is
  never promoted to `NOT_READY`**; reception depends on nearby aircraft/ground
  stations, not receiver health
- No FIS-B tower currently receivable - an environmental/service-availability
  condition, never a receiver failure
- An uncalibrated (but otherwise connected/enabled) AHRS - **this never affects any
  other component's check**; GPS, 978/1090, GDL90, and FIS-B are computed
  independently and are unaffected by AHRS calibration state
- No recent GDL90 client
- An incomplete manual check
- Storage approaching (but not past) its warning threshold

A disabled optional sensor (AHRS or barometer turned off in settings) reports
`NOT_APPLICABLE` at the item level and is excluded from the rollup entirely, matching
`readiness.Rollup`'s own treatment of `NOT_INSTALLED` - it is not itself a `CAUTION`
trigger, so turning off an optional sensor you don't have installed never manufactures
a false caution.

## Startup grace periods

Every grace period is measured against monotonic time (`stratuxClock`, via
`preflight.Input.UptimeSeconds`) - **never wall-clock time**, because GNSS/NTP can step
the system clock forward or backward immediately after boot, before time is trusted
(see [readiness-and-time-trust.md](readiness-and-time-trust.md)). A wall-clock-based
grace period could be shortened, lengthened, or made negative by that step; a
monotonic one cannot.

| Grace period | Duration | Covers |
|---|---|---|
| SDR discovery | 120s | 978/1090 receiver assignment |
| GPS acquisition | 90s | GPS satellite solution |
| GNSS time establishment | 90s | Trusted time synchronization |
| Network client connection | 60s | First GDL90 client |
| Fan-controller status file | 30s | `/run/stratux-fancontrol/status.json` appearing |

Within its grace period with no evidence yet, an item reports `UNKNOWN` (not `READY`
and not `CAUTION`) - "initializing," not "fine" and not yet "a problem." After the
grace period expires with still no evidence, the item reports its documented `CAUTION`
or `NOT_READY` state as appropriate.

**SDR discovery is 120s, not a shorter guess, because it mirrors a real, pre-existing
constraint**: `main/sdr.go`'s `sdrWatcher()` deliberately delays configuring any SDR
device until GPS acquires a fix or 120s elapses, whichever comes first (to reduce RF
noise during GPS acquisition). Confirmed on live hardware with no GPS fix:
`readiness.RadioHealth.Band.Enabled` read `false` for the full ~120s window before
becoming `true`. During that window, `978_config`/`1090_config`/`fisb_tower` report
`UNKNOWN`, not `NOT_APPLICABLE` - a `Band.Enabled` reading of `false` is trusted as a
genuine, settled "this band is off" only once the grace period has actually elapsed
(see `preflight/checks.go`'s `radioChecks()`/`fisbChecks()`). An earlier, shorter
value here caused exactly the wrong reading (a band still initializing was
misreported as deliberately disabled) - caught during live-hardware validation and
corrected before merge.

## Automated checks

Built entirely from data `readiness.HealthReport` (via `globalHealth`) and the active
calibration profile already expose - see `preflight/checks.go` for the exact,
per-check derivation:

- **Core system**: daemon health, failed-unit state, power/thermal (undervoltage/
  throttling), persistent storage, protected overlay, recording capacity.
- **GPS and time**: hardware presence, satellite solution, position accuracy, trusted
  time, time freshness.
- **ADS-B**: 978/1090 configuration and receiver availability (including honest
  external-receiver classification), current-traffic freshness (with the "zero
  current traffic is not a failure" rule above).
- **GDL90 and clients**: output state, recent client count, and an unconditional
  disclosure that no specific EFB application can ever be identified from network
  activity alone.
- **AHRS and calibration**: hardware/fresh-data state, active profile availability and
  calibration validity.
- **Barometer**: hardware/fresh-data state, plausibility.
- **Fan controller**: controller status, freshness, and an unconditional `VERIFY`
  notice that physical rotation is never electronically confirmed (no tachometer on
  this hardware).
- **Recording**: storage availability, whether recording is currently permitted
  (which itself depends on trusted time - see `readiness.TimeHealth.RecordingAllowed`),
  current session state.
- **FIS-B**: current tower count, last UAT activity, weather-product counts - absence
  of a tower is explicitly never `NOT_READY`.

## Manual checks

Nine fixed items (`preflight.ManualCheckDefinitions`) software cannot verify:

1. Antennas attached
2. Stratux securely mounted
3. Case vents unobstructed
4. Cooling fans physically spinning
5. Power source sufficiently charged
6. EFB device connected to Stratux Wi-Fi
7. EFB application connection visually confirmed
8. Correct aircraft calibration profile selected
9. AHRS level reference appropriate for this mounting

### Acknowledgement rules

- Acknowledgements are held **only in memory** (`preflight.ManualAckStore`) - there is
  deliberately no persistence layer, unlike `calprofile.Store`. The whole point of a
  manual check is that it must be re-confirmed after every reboot.
- Every acknowledgement is stamped with the current daemon boot/session identity (a
  random token generated once at process start - see `initPreflight`). A session
  mismatch (which can only mean a different process lifetime, i.e. any restart
  including a reboot) is treated exactly like an expired acknowledgement - it never
  reads as valid.
- Acknowledgements expire after `preflight.DefaultAckExpiration` (4 hours), whichever
  comes first relative to the session boundary above.
- Expiration is computed on **monotonic** time; a trusted-UTC timestamp is attached
  when available (for human-readable display) but is never authoritative for whether
  an acknowledgement is still valid.
- Re-acknowledging an already-acknowledged check simply refreshes it (idempotent).
  Clearing an already-clear check is not an error.
- Acknowledging or clearing never touches calibration-profile data - a manual check is
  never written into permanent profile state, and fan rotation is never claimed to be
  electronically detected regardless of acknowledgement state.

## API

All new endpoints, none replacing or renaming an existing one - see
[http-api.md](http-api.md) for the project's general API conventions.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/getPreflightReport` | The current preflight report |
| `POST` | `/acknowledgePreflightCheck?id=...` (or JSON body `{"id": "..."}`) | Acknowledge one manual check (idempotent) |
| `POST` | `/clearPreflightCheck?id=...` | Clear one manual acknowledgement |
| `POST` | `/resetPreflightChecks` | Clear every manual acknowledgement |

Requirements enforced by every handler in `main/preflightapi.go`:

- Strict method checking (`405` on the wrong verb).
- A JSON body, when used, is bounded to 4096 bytes (`http.MaxBytesReader`) and
  strictly decoded - malformed JSON or a missing `id` field returns `400`.
- An unrecognized check id returns `404`.
- The manual-acknowledgement store is safe for concurrent use; every handler is
  thread-safe.
- No filesystem path is ever exposed or accepted - check ids are validated against
  the fixed, compiled-in `ManualCheckDefinitions` list, never used to build a path.
- No destructive side effect beyond clearing in-memory acknowledgement state (nothing
  here can affect ADS-B/GDL90 operation or persistent data).
- No new authentication model - these endpoints are only as reachable as every other
  existing management endpoint on this local, already-established interface.

## Report schema

See `preflight.Report`/`preflight.CheckResult` (`preflight/report.go`) for the
authoritative Go types; in summary:

- `schemaVersion` - bumped whenever the schema changes meaning, not just shape.
- `generatedAt` - nullable (`null` until trusted wall-clock time exists), plus an
  always-present `generatedAtMonoSeconds`.
- `bootSessionId` - the opaque session identity manual acknowledgements are bound to.
- `overall`/`summary`/`requiredActionCount`/`cautionCount`.
- `automated`/`manual` - arrays of `CheckResult` (`component`, `checkId`, `label`,
  `state`, `severity`, `blocking`, `reason`, `recommendedAction`, `source`
  [`"automated"` or `"manual"`], `observedAt`, `expiresAt`).
- `profile` - a small, already-sanitized calibration-profile summary.
- `recording` - storage/permission/current-state summary.
- `disclaimer` - the exact required safety statement, verbatim, on every report.

## Dashboard

A dedicated **Preflight** page (`web/plates/preflight.html` / `web/plates/js/preflight.js`),
in the fixed order: overall state, required actions, aircraft profile/AHRS, GPS/time,
ADS-B receivers, GDL90/EFB connection, power/system/storage, manual checks, a link to
the existing detailed Readiness page. Polls `/getPreflightReport` every 5 seconds with
an overlap guard (a request already in flight suppresses the next tick rather than
queuing a second one). The existing Readiness page is untouched - this is a separate,
simpler summary, not a replacement.

## Aircraft-profile relationship

The preflight report's `profile` field is derived from the same
`readiness.AHRSProfileInfo` `main/calprofilesapi.go`'s `activeProfileHealthInfo`
already exposes (see [aircraft-calibration-profiles.md](aircraft-calibration-profiles.md))
- `preflight` never re-derives calibration-profile logic, and `CalibrationValid` is
derived from the profile's `Kind` (`uncalibrated` vs. anything else) rather than
duplicating `calprofile`'s own validity computation.

## Recording integration

`populateSessionPreflightSummary` (`main/preflightapi.go`) captures a small,
session-level snapshot - overall state, required/caution counts, whether every manual
check was complete - at `/startRecording`, mirroring the existing
`populateSessionCalibrationProfile`. This is deliberately a summary, not the full
report, and is never repeated into every one-hertz sample.

That in-memory summary is a live-status convenience; the full point-in-time
snapshot (every automated/manual check result, not just counts) is additionally
persisted once per recording as a durable, versioned sidecar record - see
[recording.md](recording.md) for the complete design, retrieval API, and
dashboard behavior.

## Diagnostics integration

`readiness.DiagnosticBundle` gains an opaque `PreflightReport` field
(`interface{}`, so `readiness` never has to import `preflight` - the dependency only
ever goes the other direction) that `main/diagnosticsapi.go` populates from the current
report. Already free of sensitive data by `preflight.Report`'s own design - it is built
entirely from data the same `HealthReport` already carries, so it never contains GPS
coordinates, Wi-Fi secrets, client MAC addresses, credentials, or SSH material, and
needs no separate sanitization pass.

## Failure isolation

A panic anywhere inside `preflight.BuildReport`, or in `main/preflightapi.go`'s own
glue, is recovered in `buildPreflightReport` and reported as an honest `StateUnknown`
result with an explanation in the server log, rather than propagating. This is the one
place `Report.Overall` can legitimately hold `UNKNOWN` - `BuildReport`'s own decision
policy never produces it. A failure in this subsystem can never interrupt 978/1090
reception, GPS, GNSS time, FIS-B, GDL90, Wi-Fi, AHRS, barometer, fan control,
diagnostics, or recording - none of those are called from, or depend on, anything in
this package.

## Hardware-validation checklist (for a future deployment mission)

This feature has not been deployed. Before any future deployment:

- [ ] `/getPreflightReport` reflects live hardware state correctly on first boot
      (including the startup grace periods actually resolving to real states, not
      staying `UNKNOWN` forever)
- [ ] Manual acknowledgements survive a page reload but not a reboot
- [ ] The 4-hour expiration is observed on real hardware (or verified via a shortened
      test build)
- [ ] The Preflight page renders correctly at phone and tablet widths, no horizontal
      scroll, no controls outside the viewport
- [ ] A recording started with the checklist incomplete correctly captures that in
      session metadata
- [ ] A generated diagnostic bundle includes the preflight report and passes a
      secret/credential scan
- [ ] Confirm the startup-race regression (see the `hotfix/network-health-startup-race`
      branch) remains fixed with this feature's additional `main()` startup work
      present - repeat the five-reboot acceptance test

## Rollback

Purely additive - no existing `/getStatus`/`/getSettings`/`/getHealth` field is
renamed, removed, or reinterpreted, and no other subsystem is called from this one
(see "Failure isolation" above). Rolling back requires no data migration: manual
acknowledgements are in-memory only and are simply gone; nothing on the persistent
data partition is touched by this feature.
