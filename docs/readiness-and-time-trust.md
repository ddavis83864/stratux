# Readiness, Trusted Time, and Persistent Storage

This is the Sentry-class readiness foundation: a unified component health model, trusted GNSS
time synchronization, and certified persistent storage, plus the diagnostic-bundle and
flight-recording groundwork built on top of them. It is supplemental to, and does not replace,
the existing `/getStatus` status model — see [architecture.md](architecture.md) for how the two
relate.

> Stratux remains supplemental, non-certified situational-awareness equipment. Nothing in this
> document changes that; "readiness" here means "this component is working," not flight
> certification.

## Why this exists

A hardware revision (dedicated third storage partition, no real-time clock relying on GNSS
time discipline) surfaced two problems the existing status model did not address:

1. **The system clock has no battery-backed real-time clock.** On boot it starts from
   whatever `fake-hwclock` last saved (often stale by days), and the only existing correction
   path (`main/gps.go`'s RMC handler) stepped the clock unconditionally whenever it disagreed
   with GPS time by more than 300ms — with no history, no plausibility check beyond "after
   2016," and no protection against ever stepping backward. That is unsafe once recording
   timestamps need to be trustworthy and monotonic.
2. **`DiskBytesFree` (in `/getStatus`) measures `/`**, which under the read-only-root overlay
   architecture reports the ~250 MiB writable tmpfs overlay, not the dedicated
   `/var/lib/stratux-data` application-data partition. A dashboard built on that field would
   report the overlay's capacity as the recorder's available space.

## Component health model (`readiness` package)

`readiness.ComponentState` is a five-value enum used for every monitored subsystem:

| State | Dashboard color | Meaning |
|---|---|---|
| `READY` | green | Affirmative evidence the component is working right now. |
| `DEGRADED` | amber | Partially working, or something needs attention. |
| `NOT_READY` | red | A confirmed failure or unusable component. Never used merely because there is currently no evidence of activity (e.g. no aircraft in range) — that is `READY` with an explanatory reason, or at most `DEGRADED`. |
| `NOT_INSTALLED` | gray | Hardware/feature intentionally absent by design (no AHRS board yet, or a band the operator disabled) or a settings-effected "off" band. Not a failure. |
| `UNKNOWN` | gray | Health has not yet been determined (e.g. immediately after startup). |

`readiness.Rollup(states...)` computes an aggregate: the worst of the *informative* states,
where `NOT_INSTALLED`/`UNKNOWN` are excluded from "worst" unless every input is one of those
two (in which case the aggregate is `UNKNOWN`, not falsely `READY`). This is what keeps a
healthy dual-band receiver with no AHRS board installed reading Overall `READY`, not dragged
down by a gray tile.

Per-component records live in `readiness/health.go`:

- **`RadioHealth`** (978/1090) wraps the existing, tested `sdrassign.BandStatus` — it does not
  re-derive assignment/ambiguity/conflict logic, only maps it to `ComponentState`
  (`StateFromBandStatus`) and adds frame totals, message rates, and (978 only) FIS-B tower and
  weather-product counts. A user-disabled band reads `NOT_INSTALLED` (gray), not a failure.
- **`GPSHealth`** distinguishes "present without a fix" (`DEGRADED` — routine at startup or
  indoors) from "missing/unresponsive device" (`NOT_READY`).
- **`GDL90Health`** never reports `NOT_READY` for zero connected clients; `ForeFlightClientDetected`
  is only ever set from evidence that actually identifies a ForeFlight client, since most GDL90
  clients do not self-identify — its absence means "not confirmed," not "ForeFlight is not
  connected."
- **`SystemHealth`** covers version/commit/uptime, CPU temperature, throttling/undervoltage
  (`readiness.GetThrottled`, parsing `vcgencmd get_throttled`'s bitmask — see
  `readiness/vcgencmd.go`), and failed systemd units (`readiness.ListFailedUnits`).
- **`StorageHealth`** — see below.
- **`TimeHealth`** — see below.
- **`FutureHardwareHealth`** (AHRS, barometer, fan-controller integration) always reports
  `NOT_INSTALLED` and carries **no sensor fields at all** — there is nothing for a UI to
  misread as operational data, by construction, not by convention.

`readiness.BuildHealthReport` assembles all of the above into one `HealthReport` and computes
`Overall` via `Rollup`. The temporary overlay's `StorageHealth` is deliberately **excluded**
from the `Overall` rollup inputs: it is a small tmpfs expected to run at high utilization by
design, and folding it in would make a normal system read as degraded. Its own health is still
reported in full under `TemporaryOverlay` for its own dashboard tile.

## Trusted time (`readiness/timetrust.go`)

Five states, matching the mission requirement:

- **`UNSYNCHRONIZED`** — no trusted source has ever been confirmed since startup.
- **`NETWORK_SYNCED`** — currently trusted via NTP. Not required in flight.
- **`GNSS_SYNCED`** — currently trusted via the GNSS receiver's own UTC time. This is the
  required offline, in-flight fallback.
- **`DEGRADED`** — a trusted source was previously established but has since gone stale or been
  lost (including a would-be backward correction that was rejected). The last trusted value is
  still the best available estimate; new data cannot currently be trusted without review.
- **`INVALID`** — concrete evidence of a wrong clock, not merely an unconfirmed or stale one
  (today, specifically: a rejected backward correction after recording had already started).

### Trust gates

A `GNSSTimeSample` is accepted (`EvaluateSample`) only if, in order: GNSS hardware is present,
its NMEA checksum validated, its date/time fields were parseable, its sentence status indicates
a valid fix, its UTC value falls within a configured plausible range (default
2024-01-01–2035-01-01), its fix quality is acceptable, and the sample is fresh (default: within
5 seconds of being captured). `TimeTrust.ObserveGNSS` additionally requires a configurable
number of **consecutive** accepted samples (default 3) that agree with each other within a
tolerance (default 2 seconds) before the first trust is established — a single valid-looking
sample is not enough.

### Clock correction policy

Once trust is (re-)established, `TimeTrust` decides one of:

- **Step once** — a discrepancy at or above a configurable threshold (default 5s) steps the
  system clock immediately. This happens **at most once per boot** per discrepancy episode; a
  second large discrepancy in the same session is reported (and logged) but not applied
  automatically.
- **Slew** — a small forward discrepancy is corrected without the "large step" bookkeeping
  (and may recur, unlike a step). **Known limitation:** this package only decides that a slew
  is the right response; it does not implement true gradual sub-second clock discipline (e.g.
  via `adjtimex`) — today's caller applies a slew the same way as a step, a direct clock set.
- **Reject backward** — a correction that would move the clock backward, once recording has
  begun, is never applied. The discrepancy is recorded and time moves to `INVALID`, so recorded
  data can never have out-of-order timestamps silently introduced. `isRecordingActive()` (see
  `main/health.go`) is the single hook this guard reads; it currently always returns `false`
  because automatic recording is not yet enabled (see below).

Every decision — applied or not — is appended to a bounded (20-entry) event log
(`TimeHealth.RecentEvents`), each with the old/new UTC value, source, and reason, satisfying the
"record the old time, new time, source, and reason" requirement without an unbounded log.

### Integration point

`main/gps.go`'s RMC handler builds one `readiness.GNSSTimeSample` per sentence and calls
`timeTrust.ObserveGNSS`. Because `processNMEALineLow` has already validated the sentence's NMEA
checksum and its `A`/`V` status field by the time this code runs, `ChecksumValid` and
`StatusValid` are always `true` at that call site; `AcceptableFix` uses the same "A" (active
navigation receiver) signal, since RMC carries no separate fix-type field to check
independently. The pre-existing `stratuxClock.SetRealTimeReference` / `GPSLastGPSTimeStratuxTime`
bookkeeping (used elsewhere for `isGPSClockValid()`) is untouched — only the actual `date -s`
decision changed.

ZDA sentences are requested from the GPS module (`main/gps.go`'s UBX config writer) but not
parsed; RMC alone already carries date, time, and status, so the trust model is built on the
data path that already exists rather than adding a second, currently-unused UTC source.

## Persistent storage certification (`readiness/storage.go`)

`StorageHealth` distinguishes the protected temporary overlay from the dedicated persistent
partition — see the fstab/mount evidence in [architecture.md](architecture.md) for the current
partition layout (`/dev/mmcblk0p3`, ext4, UUID `fa3cfa53-8933-4263-a19b-25227dbf13e6`, mounted at
`/var/lib/stratux-data`). `main/health.go` certifies both paths (`PersistentDataPath`,
`PersistentDataExpectedUUID` — override these if a deployment uses a different card layout) and
reports them as separate health-API fields (`Storage`, `TemporaryOverlay`); **`DiskBytesFree` in
`/getStatus` is unchanged** and continues to reflect `/` as it always has, for backward
compatibility with existing clients.

Certification checks: present (the path exists), mounted (via `findmnt`, using its
`-P` pairs output rather than fixed-column output — column-based parsing silently misaligns
when a field is empty, which is routine for exactly the mount types this matters most for:
overlay and tmpfs), read-only vs. read-write, filesystem UUID match (skipped for the overlay,
which has none), byte and inode utilization (via `statfs(2)` directly, not the `du` package
`main/` uses elsewhere, to get inode counts), and a real write test (create/write/remove a
marker file — a filesystem can report itself read-write while still being unwritable in
practice).

Thresholds (`readiness.StorageThresholds`, all configurable, not hardcoded):

| Threshold | Default | Effect |
|---|---|---|
| Warn | 80% used | `DEGRADED` |
| Critical | 90% used | `DEGRADED` (a stricter warning) |
| Recording prohibited | 95% used, or read-only/unmounted/wrong-UUID/failed write test | `NOT_READY`, `RecordingAllowed = false` |

## Health API

`GET /getHealth` (registered in `main/managementinterface.go`, alongside the existing
`GET /getStatus`) returns the current `readiness.HealthReport` as JSON, recomputed on its own
5-second ticker (`healthUpdateInterval` in `main/health.go`) — slower than the 1-second
`/status` WebSocket tick, since readiness is a higher-level, slower-moving judgment than raw
counters. This is a new, additive endpoint: no existing `/getStatus` field was renamed, removed,
or reinterpreted, and existing clients are unaffected.

See `readiness/health.go`'s struct definitions for the authoritative field list; the four
fixture scenarios covered by tests (`readiness/health_test.go`) — healthy, no-current-traffic,
degraded, and missing-hardware — double as the schema's worked examples.

## Dashboard (`web/plates/readiness.html` / `js/readiness.js`)

A new "Readiness" page, following the existing AngularJS "plate" convention (one HTML partial +
controller, registered in `web/js/main.js` and `web/index.html`, same as every other page). It
polls `GET /getHealth` every 5 seconds (matching the backend's recompute interval) rather than
opening another WebSocket. Tile colors are derived directly from each component's
`ComponentState` via the same mapping documented above — the frontend does not re-derive
health, it only renders what the backend already decided.

## Diagnostic bundle (`readiness/diagnostics.go`)

`BuildDiagnosticBundle` assembles a snapshot (health report, sanitized settings, version/commit,
a bounded window of recent log lines) and `WriteDiagnosticBundle` writes it as timestamped JSON
under `/var/lib/stratux-data/diagnostics`, pruning to a configurable retention count.

**Privacy rule:** `SanitizeSettings` recursively removes any settings-map key matching a broad,
case-insensitive pattern (`password|passphrase|secret|token|credential|private[_-]?key|
authorized[_-]?key|ssh`) at every nesting level (including inside arrays of objects, e.g.
`WiFiClientNetworks`). This is deliberately overinclusive: a new settings field whose name
happens to contain one of those substrings is excluded by default. A falsely-excluded harmless
field only loses a little diagnostic detail; a falsely-included secret is a credential leak.
Log lines are **not** independently sanitized by this package — a log line is unstructured
text, and the caller assembling `recentLogLines` is responsible for filtering anything a user
may have pasted into a field that later got logged.

A failed diagnostic write returns an error to its caller and otherwise has no effect — it must
never disrupt ADS-B/GDL90 operation.

## Recording foundation (`recording` package)

Schema and storage only — **no automatic recording is enabled**, and nothing in this package is
wired into the running daemon. `recording.Sample` carries every field the mission's schema
specifies; AHRS- and barometer-derived fields (`PitchDeg`, `BankDeg`, `VerticalAccelG`, `GLoad`,
`PressureAltitudeFt`) are pointers so their absence serializes as JSON `null` — a real,
level-flight bank angle of exactly `0` must stay distinguishable from "no AHRS installed."
`recording.Store` is an append-only, size-rotated, retention-bounded JSON-Lines store,
independent of the existing SQLite-backed traffic/situation log (`main/datalog.go`, which serves
a different purpose). `recording.CSVExporter` is real and tested; `GPXExporter`/`KMLExporter`
are defined (so the `Exporter` interface shape is fixed) but return `ErrExportNotImplemented`
rather than a silent no-op or a half-correct format.

**Prerequisite for enabling automatic recording** (future work, not part of this change):
`TimeTrust` must be reporting `GNSS_SYNCED` (or `NETWORK_SYNCED`) — validated on real hardware,
not simulated — and `isRecordingActive()` in `main/health.go` must be changed from its current
hardcoded `false` to reflect real state, at which point the backward-correction guard described
above is already wired to protect it.

## Overlay vs. persistent storage — do not confuse the two

- **Temporary overlay** (`/overlay/rwdata`, tmpfs, ~250 MiB, mounted at `/`): the writable layer
  of the protected read-only-root architecture (`init=/sbin/init-overlay`). Expected to run at
  high utilization by design. **Never** the recording capacity.
- **Persistent data partition** (`/dev/mmcblk0p3`, ext4, mounted at `/var/lib/stratux-data`): the
  only intended persistent application-data area (`recordings/`, `diagnostics/`, `exports/`,
  `health/`). Survives reboot; the overlay does not.

Nothing in this change disables the overlay, creates a persistent OS-wide overlay, converts the
root filesystem to read/write, or weakens sudden-power-loss resilience — the overlay's own
protection is untouched; a new partition was certified and reported on, not altered.

## System timezone

The appliance is standardized on UTC:

- `image_build/config` (stratux's own build input, copied into the `pi-gen` submodule checkout
  by `image_build/build.sh` before each build) overrides `pi-gen`'s own `TIMEZONE_DEFAULT`
  (previously falling through to `pi-gen`'s default, `Europe/London`). This is **not** an edit
  to the `pi-gen` submodule itself — that is the unmodified upstream `RPi-Distro/pi-gen` tool,
  not stratux's to change.
- `debian/stratux-pre-start.sh` idempotently corrects an already-flashed card's timezone to UTC
  on every boot, using the same `overlayctl unlock`/`lock` pattern already used elsewhere in
  that script (and in `main/networksettings.go`) to persist a change through the protected
  read-only root. A card built before this fix is corrected without a reflash; this is a no-op
  once corrected.

Stored timestamps (health events, and future recording data) are UTC regardless of the system
timezone setting — this fix is about the appliance's own clock display and log timestamps
matching UTC too, not a prerequisite for correct stored data.

## Hardware validation checklist (before trusting this in flight)

- [ ] `GET /getHealth` reachable and returns a well-formed report on real hardware.
- [ ] `Storage` reports the correct UUID, mount, and utilization for the actual
      `/var/lib/stratux-data` partition (not the overlay).
- [ ] `TemporaryOverlay` reports ~250 MiB total, distinct from `Storage`.
- [ ] `Time` reaches `GNSS_SYNCED` after a cold boot with a stale/build-time clock, and the
      resulting step is logged exactly once (`RecentEvents`).
- [ ] A live GNSS signal loss after sync moves `Time` to `DEGRADED`, not silently back to a
      falsely-trusted state.
- [ ] `AHRS`/`Baro`/`Fan` all read `NOT_INSTALLED` with no numeric fields present, until the
      corresponding hardware exists.
- [ ] The dashboard never shows red solely because there is no current aircraft/tower in range.
- [ ] Existing regression checklist in [building.md](building.md) / hardware docs still passes:
      deterministic SDR assignment, 978/1090/FIS-B decoding, GDL90 output, GPYes positioning,
      Wi-Fi AP, existing web pages/APIs, boot without AHRS, boot with persistent storage absent.

## Known limitations

- Clock "slewing" is not a true gradual adjustment (see above) — it is applied as a direct
  clock set, the same mechanism as a hard step, just without the once-per-boot restriction.
- GPX/KML export are interface-only; only CSV export is implemented.
- Automatic flight recording is not enabled by this change.
- `PersistentDataExpectedUUID` (`main/health.go`) is hardcoded to the development card's
  provisioned UUID; a different card/partition layout needs this value updated (ideally via a
  future settings field) rather than a code change.
- This foundation has been validated by unit tests and a full daemon build; hardware validation
  (the checklist above, on the actual development card) is tracked separately — see the PR for
  current status.
