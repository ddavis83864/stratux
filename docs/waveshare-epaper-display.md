# Waveshare E-Paper Display (optional, disabled by default)

> **This is a supplemental status surface only. It is not a certified flight
> instrument, not a moving map, not an artificial horizon, not a rapid
> traffic scope, not a ForeFlight/EFB substitute, not a safety-critical
> annunciator, not a cached-weather replay, and not an internet-weather
> client. Whether this display is enabled, connected, wired correctly, or
> working has no effect on 978 UAT / 1090 ES reception, GDL90 output, AHRS,
> GPS, alerting, or any other Stratux function.**

> **Disabled by default. Safe with no display connected. A crash, hang, or
> GPIO fault in this feature can never affect any other Stratux function -
> see "Architecture: fault isolation" below.**

## Why this exists

The Stratux dashboard requires a phone, tablet, or laptop connected over
Wi-Fi. This optional add-on puts a small, low-power, always-legible summary
of a handful of readiness facts (version, GPS fix, receiver status, AHRS/
baro/fan health, storage state) directly on the unit itself, for a glance
without opening an app - nothing more. It is entirely supplemental: every
fact it shows is also, and more completely, available through the existing
dashboard and HTTP API.

## Hardware

- **Panel**: Waveshare 3.7" e-Paper panel, 480×280 pixels, SSD1677
  controller. This driver uses the panel's 1-bit black/white mode only -
  it never uses the controller's grayscale LUT modes or red-RAM plane.
- **Driver board**: Waveshare **E-Paper Driver HAT Rev2.3** - a separate,
  universal driver board, *not* an all-in-one integrated HAT. Confirmed
  from Waveshare's own wiki (more current than a statically-hosted PDF
  manual that predates the Rev2.3 revision and its 9-pin connector).
- **Connector**: GH1.25 9-pin, labeled on the board `PWR BUSY RST DC CS CLK
  DIN GND VCC`. The 9th pin (`PWR`) is specific to Rev2.3; earlier
  revisions used an 8-pin PH2.0 connector without it.
- **Harness**: the Driver HAT's own 9-wire harness only. **Never** mount
  the Driver HAT directly on the Raspberry Pi's 40-pin GPIO header - on
  this hardware (RPi 4B + Stratux AHRS v2.0 board), the AHRS board
  physically occupies that header and additionally covers physical pins 1
  and 6 outright.
- **Wire colors** (harness, as received): PWR=Brown, BUSY=Purple,
  RST=White, DC=Green, CS=Orange, CLK=Yellow, DIN=Blue, GND=Black,
  VCC=Red.
- **Driver HAT switches**: Display Config = **A (3R)** - verbatim-confirmed
  against Waveshare's own wiki table for the 3.7" panel. Interface Config =
  **0 (4-line SPI)** - verbatim-confirmed twice (the documented default and
  example configuration). Do not assume these from a photograph alone;
  confirm the physical switch positions against the labels silkscreened on
  the board before powering on.
- **Operating voltage**: **3.3V**. Waveshare's own documentation states the
  Driver HAT's operating voltage as 3.3V explicitly. This driver treats the
  panel as a 3.3V logic device and never asserts 5V on any signal. The
  Rev2.3-specific `PWR` pin is documented (via two independent secondary
  sources, not yet a direct primary-source quote) as a 3.3V logic-level
  power-enable input, not a 5V rail - it is wired to a 3.3V GPIO here
  accordingly, and this is the first thing to double check during the
  physical-wiring gate (see "Installation procedure" below) before power is
  ever applied.

## GPIO ownership audit

Before any wiring is proposed, every GPIO this project's own software or
device-tree configuration claims on the target hardware (Raspberry Pi 4B +
Stratux AHRS v2.0 board) was enumerated and cross-checked against the
candidate wiring. A pin being physically accessible on the header does not
prove it is logically free - this audit traces actual ownership at the
source-code and device-tree level, not merely header geometry.

| Physical pin | BCM GPIO | Function | Current owner | Verdict for this display |
|---|---|---|---|---|
| 1, 6 | - | 3.3V / GND | Physically covered by the AHRS board | Excluded outright - never usable regardless of logical state |
| 3, 5 | GPIO2, GPIO3 | I2C1 SDA/SCL | AHRS/barometer I2C bus | Claimed - do not use |
| 7 | GPIO4 | GPIO | `sc16is752-i2c` device-tree overlay interrupt line | Claimed - do not use |
| 8, 10 | GPIO14, GPIO15 | UART TXD/RXD | Reserved for GPIO-attached GPS fallback | Claimed - do not use |
| 12 | GPIO18 | GPIO | Cooling fan PWM output | Claimed - do not use |
| 19, 21, 23 | GPIO10, GPIO9, GPIO11 | SPI0 MOSI/MISO/SCLK | Enabled by device-tree config, not driven by any existing Go code | MOSI/SCLK free for this display's DIN/CLK (hardware SPI0); MISO unused by this display and remains free |
| 24, 26 | GPIO8, GPIO7 | SPI0 CE0/CE1 | Enabled by device-tree config, not driven by any existing Go code | CE0 free for this display's CS (hardware SPI0); CE1 unused and remains free |
| 22, 18, 13, 15 | GPIO25, GPIO24, GPIO27, GPIO22 | Plain GPIO | Not claimed by any existing Go code, device-tree overlay, or documented hardware use | Free - used below for DC/BUSY/RST/PWR |

One open question was fully resolved during this audit: a possible RFM95/
SX1276 OGN transmit module, if physically populated, is documented
(`docs/hardware/ogn-ais-receivers.md`) as an **I²C TX module** - meaning
even when present, it shares the AHRS's I2C bus (already excluded above),
never SPI0. This closes out the audit with no remaining ambiguity.

This mapping is also encoded as executable validation, not just this
table: `epaper.GPIOMapping.Validate()` (`epaper/gpio.go`) rejects any of
the reserved BCM numbers above, any SPI0 hardware pin reused as plain
GPIO, any duplicate assignment, and any non-positive pin number - so a
future configuration change cannot silently reintroduce a conflict this
audit already ruled out. See `epaper/gpio_test.go` for the exercised
cases.

## Final wiring table

| Signal | Wire color | Physical pin | BCM / function |
|---|---|---|---|
| VCC | Red | 17 | 3.3V (never 5V) |
| GND | Black | 20 | Ground |
| DIN | Blue | 19 | GPIO10 / SPI0 MOSI (hardware SPI, fixed) |
| CLK | Yellow | 23 | GPIO11 / SPI0 SCLK (hardware SPI, fixed) |
| CS | Orange | 24 | GPIO8 / SPI0 CE0 (hardware SPI, fixed) |
| DC | Green | 22 | GPIO25 (plain GPIO, configurable) |
| BUSY | Purple | 18 | GPIO24 (plain GPIO, configurable) |
| RST | White | 13 | GPIO27 (plain GPIO, configurable) |
| PWR | Brown | 15 | GPIO22 (plain GPIO, configurable) |

DIN/CLK/CS are the Pi's own dedicated SPI0 hardware peripheral pins and are
not user-configurable. DC/BUSY/RST/PWR are plain GPIO lines; the mapping
above is `epaper.DefaultGPIOMapping()` and can be overridden only in code
(not through the dashboard - see "Configuration" below), always subject to
the same `Validate()` conflict check.

### Wiring diagram (schematic, not to scale)

```
 Raspberry Pi 4B 40-pin header               Driver HAT 9-pin harness
 (AHRS v2.0 board occupies pins 1,6
  and the header itself - harness
  only, never a direct HAT mount)

  pin 13 (GPIO27) ---- White ----> RST
  pin 15 (GPIO22) ---- Brown ----> PWR
  pin 17 (3.3V)   ---- Red   ----> VCC
  pin 18 (GPIO24) ---- Purple---> BUSY
  pin 19 (GPIO10) ---- Blue  ----> DIN   (SPI0 MOSI)
  pin 20 (GND)    ---- Black ----> GND
  pin 22 (GPIO25) ---- Green ----> DC
  pin 23 (GPIO11) ---- Yellow----> CLK   (SPI0 SCLK)
  pin 24 (GPIO8)  ---- Orange----> CS    (SPI0 CE0)
```

## Architecture: fault isolation

Following this project's established pattern for optional hardware
(`fancontrol_main/`), this feature is a **separate binary and systemd
service**, never linked into the main `stratuxrun` daemon:

- **`epaper`** (Go package, `epaper/`): a pure, hardware-free decision
  core - configuration validation and defaulting (`Config`/`Normalize`),
  GPIO conflict validation (`GPIOMapping.Validate`), the content model
  (`Content`, `MaterialChange`), the change-driven refresh policy
  (`Decide`), and page layout (`Layout`). No filesystem, GPIO, SPI, or
  clock calls of its own - every function takes already-known values and
  returns a decision, so it is fully exercised by `go test` with no
  Raspberry Pi, GPIO, or SPI bus attached.
- **`epaperd`** (binary, built from `epaper_main/`): the impure glue - an
  original, from-scratch SSD1677 command driver (`driver.go`) behind a
  small `Bus` interface, a `go-rpio`-backed GPIO/SPI0 implementation
  (`gpiobus.go`, reusing this project's existing `go-rpio` dependency -
  see `fancontrol_main/fancontrol.go`), `basicfont`-based text rendering
  (`render.go`, reusing the existing indirect `golang.org/x/image`
  dependency), an HTTP client that reads only the main daemon's existing
  stable, read-only APIs (`statussource.go` - `/getStatus`, `/getHealth`,
  `/getStorageLifecycle`, `/getAutoRecordStatus`, `/getPowerHealth`,
  `/getAlertSettings`, `/getAlerts`), and the service lifecycle
  (`main.go`).

Guarantees this architecture provides:

- **Disabled by default, and safe with no display connected.** The
  shipped default (`EpaperEnabled = false`) means `epaperd` touches no
  GPIO or SPI at all - it idles, re-checking its configuration
  periodically. Even enabled, if the panel never responds (nothing wired
  up), the service reports `NOT_DETECTED` and keeps idling; it never
  retries in a tight loop or escalates.
- **Never blocks any other subsystem.** Every HTTP call to the main
  daemon carries a bounded timeout (`NewStatusSource`'s `timeout`
  parameter). Every SPI/GPIO wait (`BUSY`) is bounded by
  `defaultBusyTimeout` (10 seconds). There are no unbounded queues or
  goroutines - the whole service is a single loop on a ticker.
- **A crash or panic here cannot reach the main daemon.** `epaperd` is
  never imported by, and never runs inside, `stratuxrun`. A panic inside
  a single refresh cycle is itself recovered (`refreshOnce`'s
  `defer`/`recover`) and reported as a bounded error category, not a
  process crash; systemd (`Restart=always`) restarts the process itself
  if it ever does exit.
- **Recovers cleanly from BUSY-stuck/SPI-errors/missing-device/crash/
  restart.** A `BUSY` timeout, an SPI write failure, or a missing panel
  each map to a specific `epaper.ErrorCategory`, self-reported via the
  status file (see "Configuration and observability" below) - never a
  raw, unbounded error string. On a config change, the previous driver
  and GPIO bus are cleanly torn down (`driver.Sleep()` +
  `closeGPIOBus()`) before a new one is opened.
- **No writes to persistent storage during normal operation.** All
  refresh-policy state (`epaper.PolicyState`) lives only in `epaperd`'s
  own memory; only the small, bounded status file under `/run` (tmpfs,
  RAM-backed, cleared on reboot) is written, exactly like
  `fancontrol_main`'s own status file.
- **Cleanly releases GPIO/SPI on stop**, and leaves the panel in a
  defined state on a controlled shutdown - see "Startup and shutdown
  behavior" below.

## Content, refresh policy, and ghosting mitigation

### What is shown

Three selectable pages (`epaper.Page*`), plus a one-line header (version +
short build) and disclaimer on every full refresh:

| Page | Content |
|---|---|
| `overview` (default) | Overall readiness, GPS fix, trusted time, overlay-protection state, storage pressure, Auto Record armed state, alerts enabled/muted |
| `receivers` | 978 UAT / 1090 ES receiving state, GDL90 client count, traffic target count |
| `health` | AHRS/baro/fan state, CPU temperature, undervoltage/throttle warning |

Every value shown is already available, in full, through the existing
dashboard and HTTP API - this page never introduces a new data source, a
coordinate, a credential, or any private value.

### Update frequency and material-change policy

A refresh is considered only if **both**: (a) the sampled content
materially changed since the last refresh, and (b) at least the configured
refresh interval has elapsed. The sample timestamp itself is deliberately
excluded from the change comparison - time always "changes" and must never
alone trigger a refresh (`epaper.MaterialChange`). This is a change-driven,
not a fixed-rate, display: a quiet system with nothing new to report simply
does not refresh, minimizing unnecessary panel wear.

### Full-vs-partial refresh and ghosting

E-paper partial updates accumulate visible "ghosting" over repeated use. A
full (flashing, slower, ghosting-clearing) refresh is forced every
`fullRefreshEvery` partial refreshes (default 20; configurable 1-200). The
very first refresh after startup is always full, since there is nothing on
the panel yet to partially update from (`epaper.Decide`).

### BUSY timeout and driver-error handling

Every wait on the panel's `BUSY` line is bounded (`defaultBusyTimeout`, 10
seconds - ample margin over this panel's documented few-second full-refresh
time). A timeout is reported as `ErrorBusyTimeout`, never left to block
indefinitely. Every other driver failure (SPI write, GPIO open, panel not
found) maps to its own bounded `epaper.ErrorCategory`; the panel's own
reference-code-observed `BUSY` polarity (high-while-busy, the *opposite* of
the Driver HAT's generic "low active" pin-description text - see
`epaper_main/driver.go`'s `busyMeansBusy` doc comment) is followed, with
final confirmation deferred to the hardware smoke test (Phase 10 of this
feature's own validation plan).

### Stale-data indication

If the sampled status data is older than `StaleDataThresholdSeconds` (30
seconds), the panel shows an explicit `** STATUS DATA STALE / OFFLINE **`
line rather than silently continuing to show a possibly-outdated last-known
value.

## Configuration and observability

### Settings (dashboard: "E-Paper Display", or `/setSettings`)

| Setting | Type | Default | Meaning |
|---|---|---|---|
| `EpaperEnabled` | bool | `false` | Master enable. Safe to leave `false` indefinitely, including with the display physically connected. |
| `EpaperPanel` | string | `waveshare-3.7in` | Panel model identifier. Only this one is supported today. |
| `EpaperRotation` | number | `0` | Degrees clockwise: 0, 90, 180, or 270. |
| `EpaperRefreshIntervalSeconds` | number | `15` | Minimum seconds between refreshes (floor: 5). |
| `EpaperFullRefreshEvery` | number | `20` | Partial refreshes between forced full refreshes (max: 200). |
| `EpaperPage` | string | `overview` | Which status page is shown - see the content table above. |

These six fields are wired into the existing `/setSettings`
validation-and-mutation machinery (`main/settingsvalidate.go`'s
`settingsFieldTypes`, `main/managementinterface.go`'s
`handleSettingsSetRequest`) exactly like every other setting - no new API
was introduced. Beyond the raw-type check every field gets, the four
fields with a real bounded value space (`EpaperPanel`, `EpaperPage`,
`EpaperRotation`, `EpaperRefreshIntervalSeconds`, `EpaperFullRefreshEvery`)
are also range/enum-validated at the API boundary - mirroring
`epaper.Normalize`'s own rules exactly - so an out-of-range or
unrecognized value is rejected outright rather than silently persisted
and then silently never applied on `epaperd`'s own next poll. The GPIO pin
mapping is **not** dashboard-configurable (only in code, via
`epaper.Config.GPIO`) - it is always subject to the same `Validate()`
conflict check either way.

A disabled configuration (`EpaperEnabled: false`) is always valid
regardless of every other field's contents (`epaper.Normalize`) -
deliberately, so a stale or unrecognized value in a future version's
config can never block an unrelated settings change, including a
Configuration Backup/Restore apply.

**Configuration Backup/Restore covers all six settings**, as its own
`EpaperSettingsSection` (`configbackup` package), mirroring
`TrafficCPASettingsSection`'s own established pattern exactly: a dedicated
`Document.EpaperSettings` field with its own section checksum (no
`SchemaVersion` bump needed - purely additive), its own
`validateEpaperSettings` (the same bounds as `epaper.Normalize`, with the
same "a disabled section is always valid" exception), and its own
historical-shape entry in `legacy.go` so a backup exported before this
section existed still validates and restores cleanly (the missing section
is backfilled with the safe, disabled default - not the bare zero value,
though for this section they happen to be identical, since a disabled
e-paper section requires no non-zero hysteresis-avoiding defaults the way
Automatic Flight Recording's thresholds do). Preview
(`/validateConfigurationBackup`) shows any changed e-paper field exactly
like every other section, via the same generic, reflection-based diff -
no e-paper-specific preview code was needed.

### Observability (`/getHealth`'s new `Epaper` field)

`epaperd` self-reports its own bounded health to
`/run/stratux-epaper/status.json` (tmpfs, atomically written) every cycle,
read by the main daemon exactly like `fancontrol_main`'s own status file
(`common/epaperstatus.go`, `main/epaperreadiness.go`). Never a raw
payload, coordinate, or credential - only small enums, counts, and
timestamps (`epaper.Health`).

`readiness.EpaperHealth` (exposed at `/getHealth`'s `Epaper` field, and on
the dashboard page) reports: `State`/`Reason`, `Enabled`,
`ServiceInstalled`/`ServiceActive` (the `stratux_epaper` systemd unit's own
state), `StatusAvailable`/`Malformed`, `ServiceState`/`PanelDetected`/
`LastErrorCategory` (from `epaperd`'s own self-report), `ConfiguredPanel`,
full/partial refresh counts, `BusyTimeoutCount`, `ConsecutiveFailures`, and
`LastUpdateTime`/staleness.

A **disabled or never-installed display always reports `NOT_INSTALLED`**,
which the existing readiness `Rollup` already excludes from the `Overall`
computation - this optional, non-safety-related display being off, or even
actively failing, never degrades overall system readiness in a way that
could be mistaken for a core radio/AHRS/GPS/GDL90 problem (see
`readiness/epaper.go`'s `BuildEpaperHealth` policy for the full state
table).

## Installation procedure

**Stop. Do not connect or power the display until every step through "GPIO
ownership audit" above has been read and the owner has explicitly
authorized physical wiring.** A pin being physically accessible does not
prove it is logically free, and this document's own audit is only as good
as the hardware it was checked against - confirm the target unit still
matches this document's stated hardware (RPi 4B + Stratux AHRS v2.0 board)
before proceeding.

1. **Power down completely.** Confirm fans have stopped, LEDs are off,
   Wi-Fi ("Stratux" AP) is gone from nearby device lists, and power is
   physically disconnected - not merely `shutdown`, but power removed.
2. **Confirm switch positions** on the Driver HAT against this document's
   photographically-confirmed labels: Display Config = A (3R), Interface
   Config = 0 (4-line SPI).
3. **Connect one wire at a time**, checking each against the final wiring
   table above before moving to the next. Check for reversed connectors,
   loose contacts, and exposed conductors. Never move a wire while
   powered.
4. **Reconcile against a photograph** of the completed harness before
   applying power, comparing every wire's physical-pin position against
   the table above.
5. **Apply power with the feature still disabled** (`EpaperEnabled:
   false`, the shipped default). Confirm normal boot: no undervoltage or
   thermal throttling, no failed systemd units, AHRS/baro/fan/GPS/
   978/1090/GDL90 all functioning exactly as before this hardware was
   added.
6. **Confirm the systemd unit is already running** (it is enabled and
   started automatically by the package, exactly like `stratux_fancontrol`
   - see "Startup and shutdown behavior" below): `systemctl status
   stratux_epaper` should show `active (running)`. No manual `systemctl
   enable` step is needed or expected.
7. **Enable the feature** in Settings or on the E-Paper Display dashboard
   page, and confirm one controlled full refresh: correct orientation,
   legible contrast, no clipping, no unexpected ghosting.
8. **Confirm no disturbance** to any other subsystem: radio reception,
   AHRS attitude, fan operation, GPS fix, GDL90/ForeFlight connectivity,
   and audio alerts all continue exactly as before.

If any step produces an unexpected result - excessive heat, undervoltage,
a fan failure, loss of AHRS/baro data, an SPI/GPIO conflict, a boot
failure, a repeated crash, or any radio/GDL90 interruption - **stop
immediately**, disconnect power, and do not proceed until the cause is
understood.

## Startup and shutdown behavior

- On `epaperd`'s own startup, before the first real content sample
  completes, the panel shows a fixed "Starting..." screen
  (`epaper.StartupLines`) - it is never left blank during startup.
- On a controlled stop (`SIGTERM`/`SIGINT`, including a full system
  shutdown), the panel is updated with a fixed "Stratux is shut down. Safe
  to remove power." screen (`epaper.ShutdownLines`) *before* the
  controller is put to sleep and `PWR` de-asserted - never after, so the
  shutdown message is the last thing left on the panel.
- The `stratux_epaper` systemd unit is **enabled and started
  automatically** by the package's post-install script, exactly like
  `stratux_fancontrol`. A manual, post-boot `systemctl enable` was found
  during hardware validation to persist only in the protected overlay's
  RAM-backed upper layer and be silently lost on the next reboot, so the
  unit itself is now always durably active - the owner-controlled,
  durably-persisted `EpaperEnabled` setting (step 7 above) is the only
  thing that decides whether it ever touches GPIO/SPI. A device that never
  had the display added still runs this service, but it idles: a bounded,
  cheap localhost HTTP poll every few seconds, no hardware access at all.

## Failure isolation and missing-display behavior

With the feature disabled (the default), or with nothing physically
connected, `epaperd` never touches GPIO or SPI - the service simply idles.
With the feature enabled but the panel not detected (the `BUSY` line never
settles during init, or an SPI probe fails), the service reports
`NOT_DETECTED` on the dashboard and keeps idling at the configured poll
interval - it never retries in a tight loop, never blocks, and never
affects any other subsystem. A driver-error case (an SPI write failure, a
`BUSY` timeout mid-refresh) is reported via `LastErrorCategory` and never
escalates past a `DEGRADED` dashboard/readiness state - this hardware is
purely supplemental, so even a confirmed fault here is reported as
attention-worthy, never as if it were a core radio/AHRS/GPS failure.

## Troubleshooting

| Symptom | Likely cause | Check |
|---|---|---|
| Dashboard shows `NOT_INSTALLED` | Feature disabled (`EpaperEnabled: false`) | Confirm `EpaperEnabled: true` in Settings; the `stratux_epaper` unit itself is enabled/running on every device regardless |
| Dashboard shows `NOT_READY` | Unit installed but not active | `systemctl status stratux_epaper`; check `journalctl -u stratux_epaper` for a startup error |
| Dashboard shows `DEGRADED`, panel not detected | Nothing wired up, a loose connection, or wrong switch positions | Re-check the wiring table and switch positions above; confirm power is applied only after a full re-check |
| Dashboard shows `DEGRADED`, an error category | A transient SPI/GPIO issue, or a genuine wiring fault | Note the exact `LastErrorCategory`; power down and re-check the connection named by the affected signal in the wiring table |
| Panel shows stale data | The main daemon's HTTP APIs are slow or unreachable | Check the main daemon's own health first - this is a symptom, not a separate fault |
| Panel ghosting is excessive | `EpaperFullRefreshEvery` set too high | Lower it (dashboard Settings panel) |

## Recovery and rollback

1. **Disable the feature** (`EpaperEnabled: false` via Settings or the
   dashboard page) - immediately safe, takes effect on `epaperd`'s next
   settings poll (at most `settings-poll`, default 15 seconds), and
   requires no reboot.
2. **Stop and disable the service** if needed:
   `sudo systemctl disable --now stratux_epaper`.
3. **Remove the wiring only after a controlled shutdown and power
   removal** - never while powered, per the installation procedure above.
4. **Revert or redeploy a prior artifact** if a packaging-level issue is
   suspected, per this project's existing OTA/rollback procedure
   (`docs/rollback-recovery.md`) - this feature introduces no change to
   any other subsystem's own rollback behavior.

At every step, disabling this feature has no effect on any other Stratux
function, and no effect on already-persisted settings, recordings,
calibration, or configuration.

## Known limitations

- The `BUSY` line polarity this driver follows is based on the panel's
  documented reference-code behavior, not yet independently confirmed
  against this exact physical unit - final confirmation is part of the
  hardware smoke test, not yet performed as of this writing.
- The Rev2.3-specific `PWR` pin's 3.3V logic-level convention is
  corroborated by two independent secondary sources, not yet a direct
  primary-source quote - flagged for the same hardware smoke test to
  confirm before the display is relied on for anything beyond a bench
  test.
- GPIO pin mapping is not dashboard-configurable in this release - only
  the shipped default mapping (validated by this document's own audit) is
  used; changing it requires a code change to `epaper.Config.GPIO`.

## Aviation disclaimer

**This display is a supplemental status surface only. It is not a
certified flight instrument and must never be used as a primary or sole
source of position, traffic, weather, terrain, attitude, or system-health
information. It does not replace any certified instrument, EFB
application, or pilot procedure. Its absence, failure, or disconnection
has no effect on Stratux's core ADS-B reception, GDL90 output, AHRS, GPS,
or alerting functions.**

## Hardware-validation checklist (for the physical-wiring and smoke-test
gates)

This checklist exists for the phases of this feature's own validation plan
that require physical hardware and explicit owner authorization - it is
not itself an authorization, and none of its steps should be performed
until the owner has explicitly approved physical wiring.

- [ ] Confirmed target hardware still matches this document (RPi 4B +
      Stratux AHRS v2.0 board)
- [ ] Confirmed bench-only setup, grounded, known-good recovery image
      available
- [ ] Confirmed power fully removed before any wiring change
- [ ] Confirmed Driver HAT switch positions against this document
- [ ] Confirmed every wire against the final wiring table, one at a time
- [ ] Confirmed no reversed connectors, loose contacts, or exposed
      conductors
- [ ] Booted with the feature disabled first; confirmed normal boot
      (no undervoltage/throttling/failed units/abnormal temperature)
- [ ] Confirmed AHRS/baro/fan/GPS/978/1090/GDL90 all functioning
      normally *before* enabling the display
- [ ] Enabled the display; confirmed one controlled full refresh
      (orientation, contrast, no clipping, no unexpected ghosting)
- [ ] Confirmed no disturbance to any other subsystem after enabling
- [ ] Ran the full hardware regression campaign (reboots, shutdown/
      restore, service restart, missing-display recovery, ≥30-minute
      stability window, portrait/landscape on the existing dashboard,
      GDL90/GPS/978/1090/AHRS/baro/fan/alerts/AFR/power/storage
      continuity) with before/after counts recorded
- [ ] Restored the final intended configuration and recorded the result
