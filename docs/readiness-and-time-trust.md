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

## Corrections from the two-hour endurance test

A live two-hour endurance run on the deployed baseline surfaced five defects, all rooted in the
same underlying mistake repeated in different places: a monotonic-only or never-set
`time.Time` value being serialized as if it were a trustworthy wall-clock instant, and — for the
clock-correction frequency — a direct clock set with no deadband or rate limiting being invoked
on every accepted sample instead of only when a correction was actually warranted. None of these
were found in the two-hour test's core reception/decoding path; all are confined to the
health/time-reporting and clock-discipline layers.

1. **Receiver timestamps** (`LastFrameTime`/`LastFrameAge` reading `0001-01-01T00:00:00Z`/`0`
   despite live reception): `main/gen_gdl90.go` already tracked `lastUATMessageTime`/
   `lastESMessageTime` correctly on the monotonic clock; `main/health.go`'s `updateHealth()`
   simply never read them, passing a literal `time.Time{}` into `BuildRadioHealth` for both
   bands instead. Fixed by reading `lastMessageTime(MSGCLASS_UAT/ES)` and computing age as
   `nowMono.Sub(lastFrameMono)` — immune to any wall-clock step in between — plus a best-effort
   wall-clock display value via `monoToWallOptional` (see below).
2. **Time-health fields** (`CurrentUTC`/`LastSyncTime`/event `At` all reading year-1 dates, some
   with a real-looking time-of-day): `TimeTrust.Snapshot`/`decideAndSync` were storing the
   *monotonic* `now` parameter into fields meant to hold wall-clock UTC (`LastSyncTime` read the
   wrong internal field entirely; `Decision.At` was the monotonic reading; `CurrentUTC` had no
   wall-clock input at all). Fixed by giving `Snapshot` a second, wall-clock parameter and
   sourcing every wall-clock field from it or from the already-correct-but-unexposed
   `lastSyncUTC`.
3. **Clock-correction frequency** (a "slew" roughly every 100ms, 0.3–6ms offsets): every accepted
   GNSS sample invoked a direct clock set, unconditionally, once trust was established. Fixed
   with `TimeTrustConfig.Deadband` (50ms default — below this, no clock action and no event) and
   `MinCorrectionInterval` (30s default — above the deadband but too soon since the last
   correction, deferred rather than applied). `ClockActionSlew` is renamed
   `ClockActionPeriodicCorrection`, and its doc comment explicitly disclaims genuine kernel
   slewing — see "Known limitations."
4. **Client timestamps** (`LastClientActivity`/`LastSeen` reading year-1 despite real clients
   associated): `BuildGDL90Health`/`BuildForeFlightDetection` were never given a real timestamp
   to report in the first place — the parameter existed but no caller ever passed one. Fixed by
   (a) adding `main/network.go`'s `lastNetworkClientActivityMono()`, which aggregates
   `LastPingResponse`/`LastPongResponse` across every tracked client, wired into
   `GDL90Health.LastNetworkClientActivity`; and (b) making `ForeFlightDetection.LastSeen` always
   explicitly unavailable, since no application-layer evidence identifying ForeFlight
   specifically exists anywhere in this project — see `BuildForeFlightDetection`'s doc comment.
   The two are deliberately named and typed so they cannot be confused: generic network liveness
   is not evidence a specific EFB app was seen.
5. **Storage accounting** (`df` reporting ~162 MiB/1% used while `/getHealth` reported ~1.2 GiB/
   6% used, at the same instant): `readiness.StatfsResult.UsedBytes()` was computed from statfs's
   `Bavail` (blocks available to an *unprivileged* process), while `df`'s Used column — confirmed
   directly on the live device with `df -B1`/`stat -f`/`tune2fs -l` — is `Total-Bfree` (all free
   blocks, including the ext4 root-reserved percentage). Since `stratuxrun` always runs as root,
   `Bavail`-based accounting understated real capacity for the daemon's own writes by exactly the
   reserved-blocks percentage (~1 GiB on the live device). Fixed by adding `FreeBytes` (`Bfree`)
   alongside the existing `AvailableBytes` (`Bavail`) and switching `UsedBytes`/
   `UtilizationPercent`/recording admission to the `FreeBytes`-based model — see "Persistent
   storage certification" below.

### The `OptionalTime` pattern

Every wall-clock field above that can legitimately be "not yet known" (`RadioHealth.LastFrameTime`,
`TimeHealth.CurrentUTC`/`LastSyncTime`, `GDL90Health.LastNetworkClientActivity`/
`LastClientActivity`, `ClientObservability.LastSeen`) is now `readiness.OptionalTime`
(`readiness/optionaltime.go`): a `{Time time.Time; Valid bool}` wrapper whose `MarshalJSON`
emits `null` when unavailable and byte-identical RFC3339 output to a plain `time.Time` otherwise.
This is the single mechanism behind "never serialize `0001-01-01` for an unavailable timestamp" —
a consumer checks for `null`/`IsZero()` instead of pattern-matching a magic date. Fields that
also need a monotonic-domain, wall-clock-step-immune duration (`LastFrameAgeSeconds`,
`LastSyncSourceAgeSeconds`) are added as new, explicitly-named companion fields (`*float64`,
`nil` when unavailable) alongside the pre-existing `time.Duration` fields, which are kept for
backward compatibility rather than changed in place.

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
- **`GDL90Health`** never reports `NOT_READY` for zero connected clients. `ForeFlightDetection`
  (a `ClientObservability` record: `State` — `DETECTED`/`NOT_DETECTED`/`UNKNOWN`/`UNSUPPORTED` —
  plus `Reason` and `DetectionBasis`) replaces a bare "is ForeFlight connected" bool with an
  honest account of what can actually be concluded. Stratux's client tracking
  (`main/network.go`) is built entirely on ICMP echo-reply/destination-unreachable liveness
  probing — it never receives or parses any application-identifying signal from an EFB client —
  so `DETECTED` is unreachable with the protocol as currently implemented; zero clients
  associated is real evidence for `NOT_DETECTED`; one or more clients present is honestly
  `UNSUPPORTED`, never a guess that it's specifically ForeFlight. The legacy
  `ForeFlightClientDetected` bool is kept for existing consumers, always exactly
  `(State == DETECTED)`.
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
- **Within the deadband — no action, no event** — an offset smaller than
  `TimeTrustConfig.Deadband` (default 50ms, chosen above a Raspberry Pi 4's typical
  parts-per-million oscillator drift and above ordinary GNSS fix jitter) is routine noise, not a
  real discrepancy: no clock action is taken and, deliberately, no health event is recorded
  either — trust state (source, `LastSyncTime`, offset) still advances silently, so `State`/
  `RecordingAllowed` stay current without the event log filling up with noise.
- **Periodic correction** — an offset at or above the deadband but below the large-step threshold
  is applied as a direct clock set, honestly named `ClockActionPeriodicCorrection` rather than
  "slew" — see "Known limitations" for why a genuine gradual slew is not implemented. Unlike the
  once-per-boot step, this may recur, but is rate-limited: an above-deadband offset that arrives
  less than `TimeTrustConfig.MinCorrectionInterval` (default 30s) after the last applied
  correction is deferred (`ClockActionNone`, reason says so, **and an event is still recorded**,
  unlike the deadband case, since it reflects a real above-threshold discrepancy an operator may
  want visibility into if it persists) rather than applied again immediately. Together, the
  deadband and the rate limit are what replaced a naive "correct on every accepted sample"
  policy — which produced a direct clock set roughly every 100ms during the two-hour endurance
  test — with at most one applied correction per `MinCorrectionInterval` for any
  above-deadband, below-large-step discrepancy.
- **Reject backward** — a correction that would move the clock backward, once recording has
  begun, is never applied. The discrepancy is recorded and time moves to `INVALID`, so recorded
  data can never have out-of-order timestamps silently introduced. `isRecordingActive()` (see
  `main/health.go`) is the single hook this guard reads; it currently always returns `false`
  because automatic recording is not yet enabled (see below).

Every decision that produces an event — applied, deferred, rejected, or a suppressed repeat large
step — is appended to a bounded (20-entry) event log (`TimeHealth.RecentEvents`), each with the
old/new UTC value (wall-clock, never the monotonic reading — see "Corrections from the two-hour
endurance test" above), source, and reason. Within-deadband observations are the one case that
deliberately does not add an event, by design, not by omission.

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
`/var/lib/stratux-data`). `main/health.go` certifies both `PersistentDataPath`
(`/var/lib/stratux-data`, constant) and reports them as separate health-API fields (`Storage`,
`TemporaryOverlay`); **`DiskBytesFree` in `/getStatus` is unchanged** and continues to reflect
`/` as it always has, for backward compatibility with existing clients.

**Expected UUID — configurable, or safely discoverable, never silently accepted.** The
partition's expected filesystem UUID is `globalSettings.PersistentDataUUID`, an ordinary
settings field (`GET`/`POST /getSettings` /`/setSettings`, like everything else in
`globalSettings`) — set it explicitly for a known installation. If it is left empty,
`main/health.go`'s `ensurePersistentDataUUID()` pins it automatically **the first time**
`PersistentDataPath` is found mounted as a structurally-valid filesystem
(`readiness.DiscoverableMount`: present, mounted, read-write, and — the critical gate — exactly
the expected filesystem type, `ext4`). This is deliberately not "trust whatever's mounted":
an overlay or tmpfs mount at the same path (or a read-only one) never passes the type/read-write
gate and is never pinned. Once a UUID is set — by either path — every later check is the same
strict exact-match comparison in `readiness.EvaluateStorage` as before; discovery only ever runs
once, not on every health tick.

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

### Accounting model: `Bfree` vs `Bavail` (root-reserved blocks)

ext4 reserves a percentage of blocks that only a privileged (root) process may use —
`tune2fs -l` on the live device confirms this. `statfs(2)` reports both: `Bfree` (all free
blocks, including the reserved ones) and `Bavail` (free blocks available to an *unprivileged*
process, i.e. `Bfree` minus the reserved percentage). `readiness.StatfsResult` captures both as
`FreeBytes` and `AvailableBytes` respectively.

**`df`'s Used column is `Total-Bfree`, not `Total-Bavail`** — verified directly against the live
device with exact byte-level arithmetic (`df -B1`, `stat -f`, `tune2fs -l`). Because `stratuxrun`
(and the recording process it hosts) always runs as root, the reserved-blocks percentage is
space it can actually still write — an `Bavail`-based model understated real capacity by exactly
that percentage (~1 GiB on the live device, the gap behind the endurance test's ~162 MiB/1%
vs. ~1.2 GiB/6% discrepancy). This package therefore standardizes on the `FreeBytes`-based model
everywhere it appears:

- `StatfsResult.UsedBytes()` / `UtilizationPercent()` — matches `df`, not an unprivileged view.
- `StatfsResult.AvailableForRecording()` — equals `FreeBytes`, documented as root-aware; a
  deployment that ever ran the recording process as a non-root user would need `AvailableBytes`
  instead, but `stratuxrun` does not.
- `main/recordingapi.go`'s `availablePersistentBytes()` (the existing, preserved 100 MiB
  minimum-free-space guard checked before starting a recording and before every sample) — reads
  `StorageHealth.FreeBytes`, not `AvailableBytes`, for the same reason.

Both raw numbers (`FreeBytes` and `AvailableBytes`) are still exposed on `StorageHealth` so an
operator (or a future consumer with different privilege assumptions) can see the reserved-blocks
split directly, rather than trusting only this package's chosen model.

**The temporary overlay (tmpfs) needs no special-casing here**: tmpfs has no root-reserved-blocks
concept at all, so the kernel reports `Bfree == Bavail` for it — the two accounting models
coincide automatically for that mount. The persistent partition (ext4) is the only mount where
the two numbers meaningfully differ.

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

## Diagnostic bundle (`readiness/diagnostics.go`, wired via `main/diagnosticsapi.go`)

`BuildDiagnosticBundle` assembles a snapshot (health report, sanitized settings, version/commit,
a bounded window of recent log lines) and `WriteDiagnosticBundle` writes it as timestamped JSON
under `/var/lib/stratux-data/diagnostics`, pruning to a configurable retention count. The write
is atomic (temp file + rename) and the filename carries nanosecond precision, so concurrent
requests cannot collide on a name or observe a partial file.

**Integrated, on-demand, via `POST /generateDiagnostics` / `GET /getDiagnostics` /
`GET /downloadDiagnostics`** (see `docs/http-api.md`) — nothing generates a bundle automatically;
every bundle exists because it was explicitly requested. The log excerpt is read directly from
`/var/log/stratux.log` at request time, bounded to the last 8 MiB read regardless of on-disk file
size, with any line matching a credential-shaped pattern (password/passphrase/token/private
key/`Authorization:` header/SSH key) dropped outright.

**Privacy rule:** `SanitizeSettings` recursively removes any settings-map key matching a broad,
case-insensitive pattern (`password|passphrase|secret|token|credential|private[_-]?key|
authorized[_-]?key|ssh`) at every nesting level (including inside arrays of objects, e.g.
`WiFiClientNetworks`). This is deliberately overinclusive: a new settings field whose name
happens to contain one of those substrings is excluded by default. A falsely-excluded harmless
field only loses a little diagnostic detail; a falsely-included secret is a credential leak.
A bundle deliberately does **not** include precise current GPS coordinates by default - only
what `HealthReport` already exposes (fix type, satellite counts, accuracy), not a location fix
someone could use to place the device.

A failed diagnostic write returns an error to its caller and otherwise has no effect — it must
never disrupt ADS-B/GDL90 operation; a retention-pruning failure after a successful write is
reported as a partial success, not a failure.

## Recording foundation (`recording` package, wired via `main/recordingapi.go`)

**Automatic flight recording remains disabled** — nothing here runs unless a client explicitly
calls `POST /startRecording` (see `docs/http-api.md`). `recording.Sample` carries every field the
mission's schema specifies, plus `TimeTrustState` (the steady-state trust level at sample time);
AHRS- and barometer-derived fields (`PitchDeg`, `BankDeg`, `VerticalAccelG`, `GLoad`,
`PressureAltitudeFt`) are pointers so their absence serializes as JSON `null` — a real,
level-flight bank angle of exactly `0` must stay distinguishable from "no AHRS installed," and on
this hardware revision (no AHRS/BMP280 board installed) they are always `null`.
`recording.Store` is an append-only, size-rotated, retention-bounded JSON-Lines store,
independent of the existing SQLite-backed traffic/situation log (`main/datalog.go`, which serves
a different purpose). `recording.CSVExporter` is real and tested; `GPXExporter`/`KMLExporter`
are defined (so the `Exporter` interface shape is fixed) but return `ErrExportNotImplemented`
rather than a silent no-op or a half-correct format - `POST /exportRecording?format=gpx|kml`
surfaces this as `501 Not Implemented`, not a fabricated file.

Each session gets its own subdirectory under `/var/lib/stratux-data/recordings/<id>/`; a
background goroutine samples once per second while active, reading GPS/health/message-count state
under those subsystems' own existing locks - it never blocks the decode or GDL90 send paths. A
documented 100 MiB minimum-free-space threshold on the persistent partition is checked before
starting and before every sample; falling below it transitions the session to an explicit `error`
state (visible via `GET /getRecordingStatus`) rather than filling the partition or crashing the
daemon. An active session is flushed and closed both on `POST /stopRecording` and on daemon
shutdown.

**Automatic** flight recording (as opposed to this on-demand, explicitly-triggered path) is still
future work, not part of this change, and still requires `isRecordingActive()` in `main/health.go`
(currently hardcoded `false`) to reflect real state before it could be turned on - the
backward-correction guard described above is already wired to protect it once it is.

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
- [ ] An external (non-SDR) low-power 978 UAT receiver reads `READY`, not missing/`NOT_READY`,
      even though legacy `/getStatus`'s `UAT_Assigned` is `false` for it (it was never
      SDR-assigned in the first place) — see `StateFromBandStatus`'s `ExternallySatisfied` check.
- [ ] Existing regression checklist in [building.md](building.md) / hardware docs still passes:
      deterministic SDR assignment, 978/1090/FIS-B decoding, GDL90 output, GPYes positioning,
      Wi-Fi AP, existing web pages/APIs, boot without AHRS, boot with persistent storage absent.

## Known limitations

- **No genuine kernel-level clock slewing.** `ClockActionPeriodicCorrection` is a direct clock
  set (`date -s`), the same mechanism as a hard step, just without the once-per-boot restriction,
  gated by a deadband and rate-limited (see "Clock correction policy"). This is a deliberate
  choice, not an oversight: implementing true gradual discipline (e.g. `adjtimex`) would add an
  untested, low-level syscall interface to a live flight-adjacent system, cannot be exercised
  deterministically in unit tests without touching the host clock, and the deadband/rate-limit
  combination already eliminates the problem that motivated looking at slewing in the first place
  (correction *frequency*, not the mechanism of any individual correction). If a real slew is
  ever implemented, `ClockActionPeriodicCorrection`'s doc comment is where that would need to
  change first — the name and comment exist specifically so this package never claims a
  capability it does not have.
- GPX/KML export are interface-only; only CSV export is implemented.
- Automatic flight recording is not enabled by this change.
- Discovery pins on the *first* structurally-valid ext4 mount found at `PersistentDataPath` if
  no UUID is configured; if that first mount is somehow the wrong ext4 filesystem (e.g. during
  bring-up with an unintended card), the operator must clear `PersistentDataUUID` via
  `/setSettings` to let it re-pin, rather than it self-correcting automatically.
- This foundation has been validated by unit tests and a full daemon build; hardware validation
  (the checklist above, on the actual development card) is tracked separately — see the PR for
  current status.
