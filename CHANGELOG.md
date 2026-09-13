# Changelog

All notable changes to this fork are documented here. This file starts with `v2.0.0-rc1`,
the fork's first formal, tagged, independently-verifiable release — everything merged before
that tag is listed once, below, as this release's starting inventory rather than as a
sequence of prior "releases" that never formally existed.

Format loosely follows [Keep a Changelog](https://keepachangelog.com/); versions follow the
scheme described in [docs/release-process.md](docs/release-process.md).

## [v2.0.0-rc1] — release candidate

Base: `upstream/stratux` `v2.0-pre5` (`7398769595c0d41fef489267294508b1264fc5f7`). Everything
below is this fork's own work on top of that base, merged into `master` via PRs #1–#22
(PR #15 excluded — see below).

### Included capabilities

| Capability | Source | Docs | Default | Hardware requirement | Validation |
|---|---|---|---|---|---|
| Dual-band 978 UAT + 1090 ES baseline | PR #1 | `docs/architecture.md`, `docs/sdr-and-bands.md` | Enabled per configured SDR | 2× RTL-SDR (band-assigned) | Hardware-validated (upstream baseline + this fork's ongoing use) |
| Readiness/health model, trusted GNSS time, persistent-data certification | PR #2 | `docs/readiness-and-time-trust.md` | Always on | None beyond GPS for time trust | Hardware-validated |
| AHRS/barometer/fan health integration | PR #3 | `docs/ahrs-baro-fan-health.md` | Always on | ICM-20948 AHRS, BMP280 baro, PWM fan (as fitted) | Hardware-validated |
| Named aircraft calibration profiles | PR #4 | `docs/aircraft-calibration-profiles.md` | No active profile by default | AHRS | Hardware-validated |
| Network mutex/concurrency fix | PR #5 | — | N/A (bugfix) | None | Race-tested |
| Preflight readiness workflow | PR #6, #7 | `docs/preflight-readiness.md`, `docs/recording.md` | Always on (supplemental, non-blocking) | None | Hardware-validated |
| Traffic and system-health alerting | PR #8 | `docs/alerting.md` | Enabled, conservative thresholds; audio requires opt-in | Browser/iPad/Bose A30 audio path has documented limitations | Hardware-validated |
| Configuration Backup and Restore | PR #9, #10 | `docs/configuration-backup-restore.md` | On-demand only | None | Hardware-validated |
| `/setSettings` hardening (malformed-input panic fix) | PR #11 | — | N/A (bugfix) | None | Regression-tested |
| Power/thermal health + controlled-shutdown resilience | PR #12 | `docs/power-shutdown-resilience.md` | Always on; shutdown is manual, two-step-confirmed | Raspberry Pi `get_throttled` | Hardware-validated |
| Storage Lifecycle inventory foundation | PR #13 | `docs/storage-lifecycle.md` | Observational only, no automatic eviction | None | Hardware-validated |
| Automatic Flight Recording | PR #14 | `docs/automatic-flight-recording.md` | **Disabled by default** | GPS | Hardware-validated |
| docs/README feature description + hero image | PR #16, #17 | `README.md` | N/A | N/A | N/A |
| Monotonic-clock and settings-mutation data-race fixes | PR #18 | — | N/A (bugfix) | None | Race-tested |
| Closure-rate/CPA traffic-trend alerting | PR #19 | `docs/alerting.md` | Enabled, conservative | None beyond existing traffic sources | Hardware-validated |
| Wi-Fi Administration Hardening | PR #20 | `docs/wifi-administration-hardening.md` | Replaces the old unguarded `/setSettings` Wi-Fi path entirely | Wi-Fi AP (built-in) | Hardware-validated — see PR #20 for the full validation record, including two real defects found and fixed on real hardware |
| README aviation safety disclaimer consolidation | PR #21 | `README.md` | N/A | N/A | N/A |
| OTA `StageFailed` bounded-retry + `/resetOTA` | PR #22 | `docs/ota.md` | Always on | None | Regression-tested (deterministic; no hardware failure-injection performed) |

### Explicitly excluded from this release

- **PR #15 (Rolling FIS-B Weather Cache)** — remains open, draft, unmerged. Its field
  validation is blocked on FIS-B ground-station reception this fork has not yet obtained; it
  is deliberately not part of this release candidate. No `fisbcache` code, settings section,
  dashboard page, or documentation from PR #15 is present in this release — verified by
  commit-reachability and file-tree inspection, not merely by PR state (see
  `docs/releases/v2.0.0-rc1.md` for the exact verification).
- Internet-sourced weather of any kind (this fork adds no internet-weather client).
- LCD/e-paper display support.
- Carbon-monoxide hardware integration (not implemented in this codebase).
- Any aircraft-control or maneuver-guidance function.
- Any claim of FAA certification or suitability as a primary flight instrument.

### Known limitations

See [docs/known-limitations.md](docs/known-limitations.md) for the consolidated list;
individual feature docs also carry their own scoped limitations (e.g. alerting's browser-audio
constraints, Wi-Fi Admin's IP-address-based-not-kernel-interface confirmation proof).
