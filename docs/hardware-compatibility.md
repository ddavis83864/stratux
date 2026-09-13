# Hardware compatibility matrix (this release)

Full detail lives in [hardware/README.md](hardware/README.md) and its linked pages
(SDR/bands, GPS, sensors, OGN/AIS receivers) — this page is a release-scoped summary of what
this fork's own additions specifically require, on top of the upstream baseline.

| Component | Required for | This release's requirement | Notes |
|---|---|---|---|
| Raspberry Pi (per upstream support list) | Everything | Same as upstream baseline | This release changes no OS/board-level requirement. |
| 2× RTL-SDR-compatible dongle | Dual-band 978/1090 reception | Same as upstream | Band assignment by EEPROM serial prefix — see [sdr-and-bands.md](sdr-and-bands.md). |
| GPS | Position, trusted time, Automatic Flight Recording trigger | Same as upstream | AFR needs nothing beyond GPS already required for position. |
| ICM-20948 AHRS | AHRS/attitude, calibration profiles | Only if you use AHRS features | Health reporting and calibration profiles are software-only on top of hardware you already have. |
| BMP280 barometer | Barometric altitude, baro health | Only if fitted | Same. |
| PWM-controlled fan | Fan health reporting | Only if fitted | Same. |
| Wi-Fi (built-in AP) | Dashboard access, Wi-Fi Administration | Always | No new hardware requirement — this release replaces *how* Wi-Fi is changed, not what's needed to run it. |
| USB power bank / power source | Power/thermal health | Any | Reporting is honest about what USB-power-bank hardware can't tell you (no battery percentage, no automatic shutdown) — see [power-shutdown-resilience.md](power-shutdown-resilience.md). |
| Audio output (browser/iPad/Bose A30) | Alert audio | Optional | Documented limitations in [alerting.md](alerting.md) — not all paths support all alert types identically. |

## What this release adds no hardware requirement for

Readiness/health model, preflight workflow, Configuration Backup, Storage Lifecycle,
Automatic Flight Recording (beyond GPS already listed), traffic/CPA alerting (beyond
existing traffic sources), and Wi-Fi Administration Hardening are all software-only on top
of the table above — see [CHANGELOG.md](../CHANGELOG.md) for the full feature list and each
feature's own "hardware requirement" column.

## Community buyer's guide

What to actually buy (not just what the firmware recognizes) is maintained upstream at the
[Supported Hardware wiki page](https://github.com/stratux/stratux/wiki/Supported-Hardware)
and applies equally to this fork, since the underlying hardware-integration code is shared.
