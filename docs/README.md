# Stratux Developer Documentation

These are the **developer-facing** docs that track the code (build, architecture, APIs,
hardware integration). User-facing how-tos live in the
[project wiki](https://github.com/stratux/stratux/wiki) — see
[the main README](../README.md#docs-in-repository-vs-in-wiki) for the rationale of that split.

## Start here

- **[architecture.md](architecture.md)** — what the daemon is, the three signal-source
  patterns, fusion and output, the web UI. Read this first.
- **[building.md](building.md)** — build targets, CI/release workflows, repo organization, and
  the OTA update process.
- **[dev-setup.md](dev-setup.md)** — setting up a development environment (remote on a Pi, or
  local Linux).
- **[readiness-and-time-trust.md](readiness-and-time-trust.md)** — the unified component health
  model, trusted GNSS time synchronization, persistent-storage certification, the `/getHealth`
  API, the readiness dashboard, the diagnostic bundle, and the flight-recording foundation.
- **[ota.md](ota.md)** — the `.deb` OTA update mechanism: the overlay-disable marker's proven
  persistent location (with mount/device evidence), and the deterministic, resumable install
  state machine built on it.
- **[ahrs-baro-fan-health.md](ahrs-baro-fan-health.md)** — the live health model for the
  ICM-20948 AHRS, BMP280 barometer, and dual-fan PWM controller, wired into the readiness
  model above.
- **[aircraft-calibration-profiles.md](aircraft-calibration-profiles.md)** — persistent,
  named AHRS calibration profiles so the same Stratux can move between aircraft without
  overwriting a single global calibration.
- **[preflight-readiness.md](preflight-readiness.md)** — the simplified, supplemental preflight
  checklist built on top of the readiness health model: state definitions, the blocking-vs-
  caution decision policy, startup grace periods, and the manual-acknowledgement workflow.
- **[recording.md](recording.md)** — the on-demand recording subsystem and the durable,
  versioned session-level Preflight metadata captured once at recording start.
- **[alerting.md](alerting.md)** — conservative, supplemental traffic-proximity and
  system-health notices: threshold policy, hysteresis, duplicate suppression, health
  transitions, settings, and browser-audio limitations. Not collision avoidance.
- **[traffic-cpa-alerting.md](traffic-cpa-alerting.md)** — an additive, off-by-default
  closure-rate/closest-point-of-approach trend input to the alerting subsystem above:
  the coordinate/relative-motion model, TCPA/CPA equations, freshness and confidence
  rules, and the exact policy that lets it only ever raise an existing alert one tier
  early, never lower or suppress one. Not collision avoidance, TCAS, or ACAS.
- **[configuration-backup-restore.md](configuration-backup-restore.md)** — backing up and
  restoring supported application configuration (radio enablement, alert settings,
  calibration profiles): export allowlist, checksums, the validate/preview/confirm/apply
  flow, transactional rollback, and what is deliberately excluded (credentials,
  recordings, diagnostics, OS configuration).
- **[power-shutdown-resilience.md](power-shutdown-resilience.md)** — power/thermal-health
  reporting built on the Raspberry Pi's own `get_throttled` signal, the honest capability
  limits on typical USB-power-bank hardware (no battery percentage, no automatic shutdown),
  the manual two-step confirmed controlled-shutdown flow, and the previous-session
  clean/unclean marker.
- **[storage-lifecycle.md](storage-lifecycle.md)** — the shared, observational-only storage
  inventory/quota/retention-planning/atomic-write foundation used by
  [automatic-flight-recording.md](automatic-flight-recording.md) (now merged) and, in an
  unmerged pull request, a rolling FIS-B weather cache: namespace ownership, pressure
  classification, and interrupted-write recovery. This foundation package itself performs
  no automatic eviction.
- **[automatic-flight-recording.md](automatic-flight-recording.md)** — the opt-in,
  disabled-by-default detection state machine that can start/stop the existing manual
  recording subsystem on the operator's behalf from conservative, GNSS-derived movement
  thresholds.

## Interfaces (for EFB / app / tool developers)

- **[integration/README.md](integration/README.md)** — the transport map (GDL90, FLARM/NMEA,
  X-Plane, CoT) and the `Capability` routing bitmask. Start here for output integration.
  - [integration/gdl90.md](integration/gdl90.md) — GDL90 over UDP, recognizing Stratux, sleep
    mode, traffic lifecycle, ForeFlight specifics.
  - [integration/other-transports.md](integration/other-transports.md) — FLARM/NMEA
    (TCP/UDP/serial/BLE), X-Plane, Cursor-on-Target.
- **[http-api.md](http-api.md)** — the full HTTP + WebSocket JSON API.
- **[settings-reference.md](settings-reference.md)** — every `stratux.conf` / `/getSettings`
  field.

## Hardware

- **[hardware/README.md](hardware/README.md)** — overview + the udev-recognized USB device
  table.
  - [hardware/sdr-and-bands.md](hardware/sdr-and-bands.md) — RTL-SDR dongles and band
    assignment.
  - [hardware/gps.md](hardware/gps.md) — GPS/GNSS receivers.
  - [hardware/sensors.md](hardware/sensors.md) — barometric and IMU/AHRS sensors.
  - [hardware/ogn-ais-receivers.md](hardware/ogn-ais-receivers.md) — OGN trackers (RX/TX),
    AIS, and external ADS-B receivers (Ping/Pong/UATRadio).

## Tech debt

Point-in-time audits tracking cleanup backlogs. Findings are candidates, not commitments —
re-verify before acting.

- **[tech-debt/modernization.md](tech-debt/modernization.md)** — outdated dependencies,
  deprecated language/stdlib usage, EOL frameworks (AngularJS), and aging build/CI/packaging
  tooling, with a prioritized backlog and suggested sequencing.
- **[tech-debt/dead-code.md](tech-debt/dead-code.md)** — functions, types, and fields that are
  defined but never referenced.

## Doc conventions

- Keep these docs in sync with the code in the same PR — they are reviewed alongside code
  changes.
- Cite code by package/file where it helps (`main/sdr.go`, `main/traffic.go`, …) rather than
  pinning line numbers that drift.
- User how-tos and buyer's-guide content belong in the wiki; cross-link instead of
  duplicating.
