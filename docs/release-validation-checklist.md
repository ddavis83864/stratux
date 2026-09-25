# Release-candidate validation checklist

The complete gate an RC must pass before it can be marked ready and merged, tagged, and
(after explicit owner approval) published as a GitHub prerelease. See a specific release's
own notes (`docs/releases/<version>.md`) for the actual, dated results against each item.

## Software gates

- [ ] Full production Go test suite green.
- [ ] Race-detector tests green (native ARM64 or the project's established native-amd64
      Docker toolchain, whichever the change under test requires).
- [ ] `go vet` clean.
- [ ] `gofmt -l` clean (no unformatted files).
- [ ] JavaScript syntax checks clean on every changed `web/` file.
- [ ] Version-consistency check green (`scripts/getversion_test.sh`).
- [ ] Package build succeeds from a clean checkout.
- [ ] Image build succeeds from a clean checkout.
- [ ] Configuration Backup compatibility tests green (including legacy-schema
      compatibility).
- [ ] OTA upgrade/rollback/reset tests green.
- [ ] Wi-Fi Administration transaction tests green.
- [ ] Power/shutdown tests green.
- [ ] Storage Lifecycle tests green.
- [ ] Recording/Automatic Flight Recording tests green.
- [ ] Alerting/CPA tests green.
- [ ] Readiness/Preflight tests green.
- [ ] Diagnostics-sanitization tests green.
- [ ] Secret/coordinate/privacy scan of a generated diagnostics bundle: clean.
- [ ] Artifact content scan (package and image): no unexpected files, no private data.
- [ ] SBOM validates as well-formed and contains no credentials or build-host paths.
- [ ] Checksum manifest validates against the actual published files.

## Build integrity

- [ ] At least two clean native ARM64 `.deb` builds from the same immutable commit compared;
      every difference explained (expected metadata vs. meaningful drift).
- [ ] Embedded commit/version verified against the exact tag being built.

## Live-device validation (configured Stratux, via OTA only)

- [ ] Pre-upgrade baseline captured (version, OTA state, overlay state, Wi-Fi config,
      persistent data, subsystem health).
- [ ] OTA upgrade to the exact tagged `.deb` completes cleanly.
- [ ] Post-upgrade version/commit match the tag.
- [ ] Overlay protected, OTA idle, zero failed units, zero unexplained restarts.
- [ ] Persistent data and configuration preserved and compared against the baseline.
- [ ] ForeFlight/GDL90 (or equivalent EFB) reconnects.
- [ ] Focused acceptance pass (Configuration Backup no-op preview, diagnostics generation,
      recording start/stop, Wi-Fi status, controlled-shutdown availability, alerting/CPA
      status, AFR status, AHRS profile).
- [ ] 30-minute stability window on the exact tagged build: zero anomalies.

## Clean-install image validation (separate, expendable test card)

- [ ] Target card explicitly identified and owner-authorized before writing.
- [ ] Image written and verified.
- [ ] First boot: default SSID/address, package version and commit correct, no private data
      present, overlay protected, persistent-data partition initializes correctly.
- [ ] Both SDRs, GPS, and (if fitted) AHRS/baro/fan detected correctly.
- [ ] Dashboard reachable; Readiness/Preflight reports honestly for a fresh device.
- [ ] ForeFlight/GDL90 connects.
- [ ] One controlled reboot: clean.
- [ ] One controlled shutdown + power restoration: clean.
- [ ] Clean second-boot behavior confirmed.

## Publication gates

- [ ] Draft GitHub Release created with only verified public artifacts attached.
- [ ] Release description covers RC status, hardware compatibility, installation, upgrade,
      verification, rollback/recovery, included/excluded features, known limitations, and
      the aviation disclaimer.
- [ ] Explicit owner approval obtained before publishing as a prerelease.
- [ ] After publishing: every asset redownloaded and rehashed against `SHA256SUMS`.
- [ ] Confirmed no private artifact was published.
- [ ] Confirmed PR #15 remains untouched.
