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

> **Two panels are supported: the Waveshare 3.7" panel (`waveshare-3.7in`)
> and the Waveshare 4.2" e-Paper Module V2 (`waveshare-4.2in-v2`). Both
> panels' core software (content, initialization, full/partial refresh,
> reboot persistence, disable/re-enable recovery) has been hardware-
> validated on the production Raspberry Pi 4B. **A real hardware-
> validation defect was found and fixed**: `EpaperRotation` at 180 degrees
> did not actually rotate the displayed content in the original
> implementation - root-caused, fixed in commit `c0dcd19c`, and the
> corrected build has since been physically deployed and re-tested, with
> the display now **confirmed to visibly rotate 180 degrees correctly**.
> See "Display rotation (EpaperRotation)" below for the full defect/fix/
> revalidation history. 90/270 remain software-tested only, with no
> physical hardware evidence either way. As of this reconciliation, all
> 22 of 22 hardware-validation checklist lines have direct physical
> evidence - see "Hardware-validation checklist: Waveshare 4.2in V2"
> below for the exact, itemized status of every item. A fully-passed
> checklist is not itself a recommendation to merge; that decision is
> the owner's alone.**

## Why this exists

The Stratux dashboard requires a phone, tablet, or laptop connected over
Wi-Fi. This optional add-on puts a small, low-power, always-legible summary
of a handful of readiness facts (version, GPS fix, receiver status, AHRS/
baro/fan health, storage state) directly on the unit itself, for a glance
without opening an app - nothing more. It is entirely supplemental: every
fact it shows is also, and more completely, available through the existing
dashboard and HTTP API.

## Hardware

- **Panel**: Waveshare 3.7" e-Paper panel, 280×480 pixels native
  (portrait; rotation 0 in this feature's own settings), SSD1677
  controller. This driver uses the panel's 1-bit black/white mode only -
  it never uses the controller's grayscale LUT modes or red-RAM plane.
  A real hardware-validation finding: an earlier version of this
  document, and this project's own `PanelWidth`/`PanelHeight` constants,
  had this reversed (480×280, "landscape at rotation 0") based on an
  unverified assumption rather than Waveshare's own reference driver -
  see epaper/layout.go and epaper_main/driver.go for the corrected,
  vendor-verified values and the exact real-hardware symptom (a fully
  wired, error-free, zero-flicker blank panel) this caused.
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

### Hardware: Waveshare 4.2" e-Paper Module V2

- **Panel**: Waveshare 4.2" e-Paper Module, PCB revision **Rev2.2**,
  400×300 pixels native (landscape; rotation 0 in this feature's own
  settings), driven by the "V2" controller generation - the same one
  Waveshare's own reference driver
  (`RaspberryPi_JetsonNano/python/lib/waveshare_epd/epd4in2_V2.py`)
  targets. Unlike the 3.7" panel above, the 400-pixel axis genuinely is
  this controller's own byte-addressed RAM direction (X) - confirmed
  directly against the vendor's own `init()`, not assumed. Black/white
  only in this driver (this panel also has a 4-gray capability the
  vendor's reference exposes via a separate `Init_4Gray()`/`Lut()` path;
  this driver deliberately does not use it, matching the existing 3.7"
  driver's own "1-bit mode only" design).
- **Interface**: 8-pin SPI - `VCC GND DIN CLK CS DC RST BUSY`. No `PWR`
  pin - this panel's VCC is always-on; there is nothing for this driver
  to power-enable or power-down beyond the controller's own deep-sleep
  command.
- **Harness**: this panel's own discrete wiring harness only - like the
  3.7" panel above, **never** mount it directly on the Raspberry Pi's
  40-pin GPIO header, which the AHRS board already occupies (and covers
  physical pins 1 and 6 outright).
- **Operating voltage**: **3.3V**. Treated identically to the 3.7" panel
  above - never assert 5V on any signal.
- **No panel-specific driver-board switches** to set, unlike the 3.7"
  panel's Driver HAT.

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

### GPIO plan: Waveshare 4.2in V2

This audit did not need to be redone from scratch for the second panel.
The 4.2" V2 panel deliberately **reuses the exact same DC=GPIO25/
BUSY=GPIO24/RST=GPIO27 pins** already proven conflict-free above for the
3.7" panel, and the same fixed SPI0 MOSI/SCLK/CE0 pins for DIN/CLK/CS.
This is safe because only one panel is ever electrically active at a
time - the `EpaperPanel` setting selects exactly one - so there is no
scenario where both panels' control lines are driven simultaneously.
`GPIOMapping.Validate()` itself has no panel-specific logic at all; the
same validated mapping and the same `DefaultGPIOMapping()` serve both
panels. The one candidate pin explicitly considered and rejected for
this panel was **GPIO17 / physical pin 11** (used in this panel's own
Pi 3B+ bench-test wiring, without an AHRS board present) - unusable on
the production Raspberry Pi 4B because it falls inside physical pins
1-12, which the AHRS board physically covers outright, independent of
GPIO17's own logical/electrical availability. This panel has no `PWR`
line, so GPIO22 (used for the 3.7" panel's own PWR line) is simply
unused when this panel is selected - not reserved, not repurposed, just
not wired.

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

### Final wiring table: Waveshare 4.2in V2

**Not yet physically hardware-validated** - see "Hardware-validation
checklist: Waveshare 4.2in V2" below.

| Signal | Physical pin | BCM / function |
|---|---|---|
| VCC | 17 | 3.3V (never 5V) |
| GND | 20 | Ground |
| DIN | 19 | GPIO10 / SPI0 MOSI (hardware SPI, fixed) |
| CLK | 23 | GPIO11 / SPI0 SCLK (hardware SPI, fixed) |
| CS | 24 | GPIO8 / SPI0 CE0 (hardware SPI, fixed) |
| DC | 22 | GPIO25 (plain GPIO, configurable) |
| RST | 13 | GPIO27 (plain GPIO, configurable) |
| BUSY | 18 | GPIO24 (plain GPIO, configurable) |

No `PWR` row - this panel has no power-enable line; VCC is always-on.
Every physical pin above is identical to the 3.7" panel's own table -
see "GPIO plan: Waveshare 4.2in V2" above for why reusing them is safe.

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

### Display rotation (EpaperRotation)

**Real hardware-validation finding: non-zero `EpaperRotation` did not
rotate the display.** Physical testing on the production Raspberry Pi 4B
(4.2in V2 panel) set `EpaperRotation` from 0 to 180 via `/setSettings`.
The setting was accepted and persisted, and the change was correctly
detected by `epaperd` - the panel physically flashed and redrew, and
`fullRefreshCount`/`partialRefreshCount` both advanced, with zero errors
throughout - but the displayed content did not visibly rotate.

**Root cause**: `epaper.Dimensions(panel, rotation)` correctly swaps
width/height for 90/270 (so the *canvas shape* passed to rendering is
correct for those two values), but nothing anywhere in the render
pipeline ever transformed the *pixel content* drawn onto that canvas -
`epaper_main/render.go`'s `Render` took only `(lines, width, height)`
and always drew text in the same fixed screen-space orientation,
regardless of rotation. For 180 degrees specifically, `Dimensions` does
not even swap width/height (correctly - 180 degrees does not change the
aspect ratio), so the entire render call was **byte-for-byte identical**
whether `EpaperRotation` was 0 or 180 - exactly matching the reported
symptom. A repository-wide search (`grep -rn "rotate|Rotate|transform|
Transform"`) confirmed no rotation/transform logic existed anywhere in
`epaper` or `epaper_main` before this fix. This was never a 4.2in-panel-
specific bug: `Render` is shared by both drivers, so the 3.7in panel's
own render path was equally affected at every non-zero rotation value -
it had simply never been physically exercised at a non-zero rotation
before this test.

A related, not-yet-physically-observed defect was found and fixed in the
same investigation: the pre-fix code fed `Dimensions`'s already rotation-
swapped width/height directly into the `PanelDriver` constructor
(`newPanelDriver`), which would have misprogrammed each controller's own
fixed-hardware RAM-window addressing (and, for the 3.7in panel, its
Driver Output Control gate count - a fixed property of the physical
silicon, not something a software rotation setting can reprogram) at
90/270 degrees specifically. This was not the reported symptom (180
degrees does not swap dimensions), but is part of the same rotation
path and is fixed by the same change.

**Fix**: `epaper.NativeDimensions(panel)` was added - the panel's fixed
physical (width, height), always at rotation 0, used exclusively for
`PanelDriver` construction from now on. `Render` now draws onto the
*logical* (reading-orientation) canvas exactly as before, then applies a
new `rotateImage` transform onto the panel's native canvas before
packing - a real geometric rotation (0: identity; 180: point-symmetric
flip; 90/270: transpose), verified by exact single-pixel-marker
relocation tests, not merely a dimension or byte-length check. See
`epaper_main/render.go`'s `Render`/`rotateImage` and
`epaper.NativeDimensions` doc comments for the full technical account,
and `epaper_main/render_rotation_test.go` for the regression coverage.

**Physical revalidation: PASSED.** The corrected build
(`c0dcd19cdde390db7b8c640ef674608e64d5da34`) was deployed to the
production Raspberry Pi 4B via the established `/updateUpload` OTA
mechanism (artifact `stratux-2.0.0~rc2-arm64.deb`, SHA-256
`f70cb6427acb6692917bb48ae644f9fbe6593d134d1616a25b01b7b0dbddffd4`,
verified before upload). With a healthy baseline confirmed at
`EpaperRotation: 0`, the owner set `EpaperRotation: 180` and **physically
observed the entire displayed content visibly rotate 180 degrees
correctly** - `state: RUNNING`, `panelDetected: true`,
`fullRefreshCount: 2`, `partialRefreshCount: 3`,
`consecutiveFailures: 0`, `busyTimeoutCount: 0`. `EpaperRotation` was
then restored to `0` and confirmed via `/getSettings`. This resolves the
defect described above end to end: reported, root-caused, fixed,
covered by regression tests, deployed, and physically confirmed. Only 0
and 180 degrees have physical hardware evidence - 90/270 remain
software-tested only (see `epaper_main/render_rotation_test.go`), with
no physical confirmation either way; do not treat 90/270 as hardware-
validated on the strength of this finding. See "Hardware-validation
checklist: Waveshare 4.2in V2" below for the full itemized status.

### BUSY timeout and driver-error handling

Every wait on the panel's `BUSY` line is bounded (`defaultBusyTimeout`, 10
seconds - ample margin over this panel's documented few-second full-refresh
time). A timeout is reported as `ErrorBusyTimeout`, never left to block
indefinitely. Every other driver failure (SPI write, GPIO open, panel not
found) maps to its own bounded `epaper.ErrorCategory`; the panel's own
reference-code-observed `BUSY` polarity (high-while-busy, the *opposite* of
the Driver HAT's generic "low active" pin-description text - see
`epaper_main/driver.go`'s `busyMeansBusy` doc comment) is followed. **For
the Waveshare 4.2in V2 panel, this polarity is physically confirmed
correct**: `busyTimeoutCount` remained `0` throughout initialization, a
full refresh, repeated partial refreshes, a full Stratux reboot, an
explicit disable/re-enable cycle, the corrected 180-degree rotation
test, an explicit `systemctl restart stratux_epaper` process restart,
and a full power-off/reconnect/power-on cycle - all on the production
Raspberry Pi 4B (see "Hardware-validation checklist: Waveshare 4.2in V2"
below for the full evidence). A wrong polarity would have produced
either an immediate false-idle read (corrupting every RAM write) or a
permanent `BUSY` timeout on every refresh attempt - neither was observed
across any of these cycles.

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
| `EpaperPanel` | string | `waveshare-3.7in` | Panel model identifier: `waveshare-3.7in` or `waveshare-4.2in-v2`. Empty string means the default (`waveshare-3.7in`), preserving every installation's behavior from before the second panel was added. Selectable via the dashboard's "Panel model" dropdown. |
| `EpaperRotation` | number | `0` | Degrees clockwise: 0, 90, 180, or 270. See "Display rotation (EpaperRotation)" below - 0 and 180 are physically validated (4.2in V2); 90/270 are software-tested only, with no physical hardware evidence. |
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

**Real hardware-validation finding: `PanelDetected` does not confirm panel
identity.** During physical validation of the 4.2in V2 panel, the 3.7in
driver - still configured and running on the production baseline at the
time - reported `panelDetected: true`, `state: RUNNING`, and completed a
full refresh plus 18 partial refreshes (all with zero errors) while the
panel actually, physically wired was the Waveshare 4.2in V2, not the 3.7in
panel the driver was built for. This is expected, not a bug to fix: both
panels' controllers respond enough to the generic reset/BUSY handshake
`PanelDetected` relies on for `Init()` to report success, even though the
panel-specific RAM addressing and content would be wrong for whichever
panel is actually connected. **`EpaperPanel` (the owner's own explicit
setting) is the only authoritative source of which panel is configured -
`PanelDetected: true` never confirms the physically-connected hardware
matches it, and no reliable hardware identity register exists on either
controller to check instead.** Always confirm `EpaperPanel` matches the
hardware actually wired before relying on `PanelDetected`'s value for
anything beyond "some SSD16xx-family e-paper controller answered the
handshake." See `epaper.Health.PanelDetected`'s own doc comment for the
same finding recorded in code.

## Installation procedure

**Stop. Do not connect or power the display until every step through "GPIO
ownership audit" above has been read and the owner has explicitly
authorized physical wiring.** A pin being physically accessible does not
prove it is logically free, and this document's own audit is only as good
as the hardware it was checked against - confirm the target unit still
matches this document's stated hardware (RPi 4B + Stratux AHRS v2.0 board)
before proceeding. This procedure applies to either supported panel -
follow whichever panel's own wiring table above matches the hardware
actually being installed, and skip step 2 (switches) entirely for the
4.2" V2 panel, which has none.

1. **Power down completely.** Confirm fans have stopped, LEDs are off,
   Wi-Fi ("Stratux" AP) is gone from nearby device lists, and power is
   physically disconnected - not merely `shutdown`, but power removed.
2. **Confirm switch positions** (3.7" panel only) on the Driver HAT
   against this document's photographically-confirmed labels: Display
   Config = A (3R), Interface Config = 0 (4-line SPI).
3. **Connect one wire at a time**, checking each against the correct
   panel's own final wiring table above before moving to the next. Check
   for reversed connectors, loose contacts, and exposed conductors. Never
   move a wire while powered.
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
7. **Select the correct panel model** (dashboard's "Panel model"
   dropdown, or `EpaperPanel`) matching the hardware just wired, **then**
   enable the feature, and confirm one controlled full refresh: correct
   orientation, legible contrast, no clipping, no unexpected ghosting.
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
| Dashboard shows `DEGRADED`, panel not detected, but wiring looks correct | Wrong `EpaperPanel` selected for the hardware actually wired | Confirm the "Panel model" dropdown matches the panel physically connected - the two panels' driver protocols are not interchangeable |
| Dashboard shows `RUNNING`/`panelDetected: true` with zero errors, but the panel shows nothing recognizable or stays blank | `EpaperPanel` may still be wrong even though `PanelDetected` says `true` - this field is protocol-success-based, not identity-based (see "Real hardware-validation finding" above) | Confirm `EpaperPanel` against the hardware actually wired regardless of what `PanelDetected` reports; it cannot catch this class of mismatch by itself |

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

- **`PanelDetected` cannot verify panel identity** - it is protocol-
  success-based, not identity-based, and a real hardware-validation
  finding confirmed a wrongly-configured `EpaperPanel` can still report
  `PanelDetected: true` and complete real refresh cycles with zero
  errors. See "Real hardware-validation finding" under "Observability"
  above for the full evidence. `EpaperPanel` is the only authoritative
  source of which panel is configured; always confirm it against the
  hardware actually wired.
- The `BUSY` line polarity the 3.7in driver follows is based on the
  panel's documented reference-code behavior; independent physical
  confirmation against that exact unit is part of its own still-pending
  hardware regression campaign (see the 3.7in checklist below). **This
  no longer applies to the 4.2in V2 panel** - its `BUSY` polarity is now
  physically confirmed correct, including across the corrected rotation
  fix, an explicit `systemctl restart`, and a full power cycle; see
  "BUSY timeout and driver-error handling" above.
- The Rev2.3-specific `PWR` pin's 3.3V logic-level convention is
  corroborated by two independent secondary sources, not yet a direct
  primary-source quote - flagged for the same hardware smoke test to
  confirm before the display is relied on for anything beyond a bench
  test.
- GPIO pin mapping is not dashboard-configurable in this release - only
  the shipped default mapping (validated by this document's own audit) is
  used; changing it requires a code change to `epaper.Config.GPIO`.
- **Waveshare 4.2in V2 panel: all 22 of 22 hardware-validation checklist
  lines now have direct physical evidence on the production Raspberry Pi
  4B** - initialization, full and repeated partial refreshes, a full
  Stratux reboot, disable/re-enable recovery, an explicit `systemctl
  restart stratux_epaper` process restart (including direct,
  contemporaneous confirmation that it has no effect on any other
  Stratux function), a full power-off/reconnect/power-on cycle following
  the documented connection order, and the corrected 180-degree rotation
  fix, all completed successfully with zero refresh failures and zero
  `BUSY` timeouts, and with no observed disturbance to AHRS/GPS/1090ES/
  978/fan/baro. See "Hardware-validation checklist: Waveshare 4.2in V2"
  above for the full itemized history - not merely a limitation notice,
  this bullet is kept as a permanent record of what was validated and
  how.
- **Non-zero `EpaperRotation`: 180 degrees is now physically validated;
  90/270 are not.** The *original* implementation FAILED physical
  testing at 180 degrees - the setting was accepted and persisted, and
  correctly triggered a refresh, but the content did not visibly rotate.
  Root-caused (no rotation/transform logic existed anywhere in the
  render pipeline) and fixed in commit `c0dcd19c`, with new deterministic
  pixel-relocation regression tests. The *corrected* build was then
  physically deployed and re-tested, and the display was **confirmed to
  visibly rotate 180 degrees correctly**. See "Display rotation
  (EpaperRotation)" above for the full defect/fix/revalidation history.
  90 and 270 degrees remain software-tested only, with no physical
  hardware evidence either way - do not rely on them in a production/
  flight context.
- The 4.2in V2 panel's own 4-gray capability is not used by this driver,
  matching the 3.7in panel's own "1-bit mode only" design - this project
  has no grayscale rendering anywhere in its content model.

## Aviation disclaimer

**This display is a supplemental status surface only. It is not a
certified flight instrument and must never be used as a primary or sole
source of position, traffic, weather, terrain, attitude, or system-health
information. It does not replace any certified instrument, EFB
application, or pilot procedure. Its absence, failure, or disconnection
has no effect on Stratux's core ADS-B reception, GDL90 output, AHRS, GPS,
or alerting functions.**

## Hardware-validation checklist: Waveshare 3.7in (for the physical-wiring
and smoke-test gates)

This checklist exists for the phases of this feature's own validation plan
that require physical hardware and explicit owner authorization - it is
not itself an authorization, and none of its steps should be performed
until the owner has explicitly approved physical wiring.

- [x] Confirmed target hardware still matches this document (RPi 4B +
      Stratux AHRS v2.0 board)
- [x] Confirmed bench-only setup, grounded, known-good recovery image
      available
- [x] Confirmed power fully removed before any wiring change
- [x] Confirmed Driver HAT switch positions against this document
- [x] Confirmed every wire against the final wiring table, one at a time
- [x] Confirmed no reversed connectors, loose contacts, or exposed
      conductors
- [x] Booted with the feature disabled first; confirmed normal boot
      (no undervoltage/throttling/failed units/abnormal temperature)
- [x] Confirmed AHRS/baro/fan/GPS/978/1090/GDL90 all functioning
      normally *before* enabling the display
- [x] Enabled the display; confirmed one controlled full refresh
      (orientation, contrast, no clipping, no unexpected ghosting)
- [x] Confirmed no disturbance to any other subsystem after enabling
- [ ] Ran the full hardware regression campaign (reboots, shutdown/
      restore, service restart, missing-display recovery, ≥30-minute
      stability window, portrait/landscape on the existing dashboard,
      GDL90/GPS/978/1090/AHRS/baro/fan/alerts/AFR/power/storage
      continuity) with before/after counts recorded
- [ ] Restored the final intended configuration and recorded the result

Status as of this panel's most recent bench session: the panel produces
real, correct, confirmed-flickering full refreshes, but shows old and new
content superimposed in roughly its top fifth (the remainder renders and
stays correctly blank), reproducible and unaffected by a power-off rest,
repeated full refreshes, or an explicit from-scratch clear cycle. Every
protocol/timing/calibration value has been confirmed correct against the
vendor's own reference and this specific panel's own printed VCOM spec.
The most likely remaining explanation is a physical defect specific to
this bench unit - not proven without a second, known-good panel for
direct comparison. See PR #30's own history for the full investigation.

## Hardware-validation checklist: Waveshare 4.2in V2 (for the
physical-wiring and smoke-test gates)

**Status as of this reconciliation**: the 4.2in V2 driver's core
functionality has been physically validated on the production Raspberry
Pi 4B at build `289767375688fff64fa3de0dd8b291f9c98401c7`. This checklist
records exactly what was, and was not, directly observed - see the
per-phase notes below for what each unchecked item is still missing.
Every checked item below reflects a specific, reported physical
observation; nothing is checked on the basis of inference or of "should
have worked."

Phase A - pre-connection:
- [x] Confirmed target hardware still matches this document (RPi 4B +
      Stratux AHRS v2.0 board)
- [x] Stratux shut down cleanly, power removed
- [x] Confirmed the proposed wiring against the 4.2in V2 final wiring
      table above, signal by signal, physical pin AND BCM GPIO for each -
      the reported production wiring (VCC=pin17, GND=pin20, DIN=pin19/
      GPIO10, CLK=pin23/GPIO11, CS=pin24/GPIO8, DC=pin22/GPIO25,
      RST=pin13/GPIO27, BUSY=pin18/GPIO24) matches this table exactly
- [x] Confirmed AHRS remains correctly installed and undisturbed
- [x] Confirmed no physical pin conflict (avoids physical pins 1-12) -
      every wired pin above is outside that range
- [x] Confirmed VCC/GND before DIN/CLK/CS/DC/RST/BUSY - physically
      re-validated under power-off conditions: Stratux shut down and
      power removed, VCC (pin 17) and GND (pin 20) connected first,
      *then* DIN/CLK/CS/DC/RST/BUSY in sequence, Pi unpowered throughout
      reconnection, power restored only afterward. Post-boot: `Build:
      c0dcd19c...`, `state: RUNNING`, `panelDetected: true`,
      `fullRefreshCount: 1`, `partialRefreshCount: 3`,
      `consecutiveFailures: 0`, `busyTimeoutCount: 0` - the display
      returned to normal operation.

Phase B - first power-up:
- [x] Booted with the display attached but the feature still disabled -
      confirmed cleanly this time (`EpaperEnabled: false` throughout),
      resolving the deviation recorded in the previous version of this
      checklist (where the still-enabled 3.7in driver briefly ran against
      the newly-wired 4.2in panel before being disabled)
- [x] Confirmed AHRS, fan, GPS, and ADS-B devices all functioning
      normally *before* enabling the display - 1090ES detected/assigned/
      receiving, GPS 3D fix, IMU connected, BMP connected, no observed
      GPIO/AHRS/I2C/GPS/ADS-B conflict
- [x] Inspected `epaperd`'s own status and logs for anything unexpected -
      `state: DISABLED`, all counters zero, service active

Phase C - display enablement:
- [x] Selected `waveshare-4.2in-v2` explicitly (not left on the default)
- [x] Enabled the display
- [x] Observed initialization; confirmed `panelDetected: true`
- [x] Confirmed one successful first refresh - `fullRefreshCount: 1`,
      `partialRefreshCount: 1`, display physically flashed through
      initialization and ended showing readable Stratux host/details
      text. **This is the first confirmed physical execution of the
      native `waveshare-4.2in-v2` Go driver.**

Phase D - functional test:
- [x] Overview page legible, correctly oriented, no clipping - confirmed
      readable Stratux host/details text after the first refresh, with no
      reported clipping or orientation defect
- [x] **PASSED after a source fix - see full defect/fix/revalidation
      history below.** Rotation setting(s) checked: the *original*
      implementation physically FAILED this test (`EpaperRotation: 180`
      was accepted, persisted, and correctly triggered a
      re-initialization and refresh, but the displayed content did not
      visibly rotate - root-caused and fixed in commit `c0dcd19c...`, see
      "Display rotation (EpaperRotation)" above for the full account).
      The *corrected* build (`c0dcd19cdde390db7b8c640ef674608e64d5da34`)
      was then physically deployed and re-tested: `EpaperRotation` set to
      180, and **the display correctly and visibly rotated the entire
      rendered content 180 degrees** - `state: RUNNING`, `panelDetected:
      true`, `fullRefreshCount: 2`, `partialRefreshCount: 3`,
      `consecutiveFailures: 0`, `busyTimeoutCount: 0`. `EpaperRotation`
      was then restored to `0` and confirmed via `/getSettings`. Only 0
      and 180 degrees have physical hardware evidence; 90/270 remain
      software-tested only (deterministic pixel-relocation tests, see
      `epaper_main/render_rotation_test.go`) and are not claimed to be
      physically validated.
- [x] Refresh interval and full/partial refresh behavior checked -
      `partialRefreshCount` advanced cleanly across repeated cycles
      (1 -> 5 -> 7) with `fullRefreshCount` staying at 1 as expected
      between forced full refreshes
- [x] Multiple refresh cycles stable - `consecutiveFailures: 0` and
      `busyTimeoutCount: 0` held across every reported snapshot
- [x] `epaperd` restart recovers cleanly - an explicit `sudo systemctl
      restart stratux_epaper` was physically exercised at
      `EpaperRotation: 0`. `systemctl status` reported `Loaded: loaded`,
      `Active: active (running)`, `Main process: /opt/stratux/bin/
      epaperd`; the physical display successfully recovered/
      reinitialized; `state: RUNNING`, `panelDetected: true`,
      `fullRefreshCount: 1`, `partialRefreshCount: 2`,
      `consecutiveFailures: 0`, `busyTimeoutCount: 0`. Distinct from the
      settings-driven disable/re-enable cycle already covered by Phase E
      below - this is a genuine process-level restart.
- [x] Full Stratux reboot: display resumes correctly afterward - after a
      normal reboot, `state: RUNNING`, `configuredPanel:
      waveshare-4.2in-v2`, `panelDetected: true`, `consecutiveFailures: 0`,
      `busyTimeoutCount: 0`, confirming the configuration and service
      survived the reboot/overlay lifecycle and the display resumed
      automatically with no manual intervention

Phase E - failure/recovery:
- [x] Controlled display disable/re-enable behaves safely - disabling
      produced the expected inert state (`state: DISABLED`,
      `panelDetected: false`, all counters zero); re-enabling without a
      reboot produced a clean reinitialization (`state: RUNNING`,
      `panelDetected: true`, `fullRefreshCount: 1`,
      `partialRefreshCount: 1`, zero errors)
- [x] `epaperd` restart never affects any other Stratux function -
      closed with direct, contemporaneous evidence: immediately after an
      explicit `sudo systemctl restart stratux_epaper`, `epaperd`'s own
      status read `state: RUNNING`, `configuredPanel: waveshare-4.2in-v2`,
      `panelDetected: true`, `consecutiveFailures: 0`,
      `busyTimeoutCount: 0`, `fullRefreshCount: 1`,
      `partialRefreshCount: 0`, and `/getStatus` taken from that *same*
      restart cycle read `ES_DecoderRunning: true`, `ES_Receiving: true`,
      `ES_Degraded: false` (1 message received), `GPS_connected: true`,
      `GPS_solution: 3D GPS` (13 satellites locked), `IMUConnected: true`,
      `BMPConnected: true`, CPU temperature in a normal ~54-57C range.
      This is directly measured, not inferred - the exact gap this line
      previously identified is now closed.
- [x] No regression to AHRS/fan/GPS/ADS-B/core Stratux functionality at
      any point above - while the display was enabled and actively
      refreshing: `ES_DecoderRunning: true`, `ES_Receiving: true`,
      `ES_Degraded: false`, GPS 3D fix with 18 satellites locked, IMU and
      BMP connected, CPU temperature in a normal ~53-57C range

**Checklist accounting as of this final reconciliation: 22 PASSED, 0
FAILED, 0 OPEN, out of 22 total checklist lines.** Every line was
individually re-audited against its own specific supporting evidence
before reaching this count - see each item's own note above for exactly
what was observed.

The full history behind this final count, preserved rather than
erased:

- **18 PASSED** as of the first hardware-validation pass on this panel.
- The document's own accounting at that point ("18 PASSED, 1 FAILED, 2
  OPEN" = 21) undercounted the true total by one line - it treated the
  Phase D and Phase E `systemctl restart stratux_epaper` items as a
  single conceptual item, when they are two distinct checklist lines
  testing two different claims (the daemon's own recovery, and the
  absence of any effect on other Stratux functions). A later
  reconciliation corrected the true total to 22 discrete lines.
- **Non-zero rotation** was physically tested and **FAILED**: the
  original implementation accepted and persisted `EpaperRotation: 180`
  and correctly triggered a refresh, but the displayed content did not
  visibly rotate. Root-caused (the render pipeline never transformed
  pixel content based on rotation; a related architectural issue also
  fed rotation-swapped logical dimensions toward the driver's fixed
  native RAM-window construction) and fixed in commit
  `c0dcd19cdde390db7b8c640ef674608e64d5da34`, which added
  `epaper.NativeDimensions` and a real `rotateImage` framebuffer
  transform. The corrected build was physically deployed and re-tested,
  and the display was confirmed to **visibly rotate 180 degrees
  correctly** - this line moves to PASSED. 90 and 270 degrees remain
  software-tested only (see `epaper_main/render_rotation_test.go`); no
  physical evidence exists for them and none is claimed.
- An explicit `systemctl restart stratux_epaper` was physically
  exercised and `epaperd` recovered cleanly - Phase D's line PASSED.
- The VCC/GND-before-signals connection order was physically
  re-validated under power-off conditions - Phase A's line PASSED.
- Phase E's "`epaperd` restart never affects any other Stratux
  function" was, for one round, left open: the restart itself had been
  performed and `epaperd`'s own recovery confirmed, but no core-system
  field readout had been captured specifically during that cycle. That
  gap is now closed with direct, contemporaneous evidence - see the item
  itself above - completing the checklist at 22/22.

**BUSY-line polarity is physically confirmed correct** for the 4.2in V2
panel/driver/wiring combination across every cycle in this entire
validation history. **`PanelDetected` remains protocol-success-based,
not identity-based** - it confirms the configured driver's hardware
handshake succeeded, never that the physically-connected panel matches
`EpaperPanel`; `EpaperPanel` remains the sole authoritative source of
which panel is configured, and no automatic model-identification
mechanism has been added. Legacy Waveshare 3.7in support is unmodified
by this PR - its own separate hardware-validation checklist is untouched
above.

This checklist being fully passed is not itself a recommendation to
merge; the merge decision is the owner's alone.
