# Known limitations

This is the release-engineering supplement to the README's own
[Safety, Certification, and Operational Disclaimer](../README.md#safety-certification-and-operational-disclaimer)
— that section is authoritative for every aviation/safety limitation; if anything here
appears to conflict with it, the README section controls. This document adds the
limitations specific to *this build/release*, not restated aviation limitations.

## Aviation

Stratux — upstream and every capability this fork adds — is supplemental,
non-certified situational-awareness equipment. It does not replace required aircraft
instruments, certified navigation/traffic/weather equipment, or official aviation weather/
NOTAM sources, and its use never relieves the pilot in command of their own
see-and-avoid and situational-awareness responsibilities. See the README section linked
above for the complete, authoritative statement (traffic limitations, weather limitations,
GPS/AHRS limitations, and the no-certification statement).

## This release specifically

- **First formal release.** `v2.0.0-rc2` is this fork's first published, tagged,
  checksummed, independently-verifiable release (`v2.0.0-rc1` was superseded before
  publication - see [releases/v2.0.0-rc1.md](releases/v2.0.0-rc1.md)). Everything before it
  was build-it-yourself only; treat this RC with the scrutiny appropriate to a first
  release, not an N-th point release with a long track record.
- **Release candidate, not stable.** See [release-process.md](release-process.md) for exactly
  what separates an RC from stable, and what has to happen before this line is promoted.
- **No OTA version-ordering enforcement.** This codebase's OTA mechanism (`docs/ota.md`)
  records whatever `Version:`/embedded-commit an uploaded package declares; it does not
  itself refuse an older or unrelated package. The Debian package version is still built to
  compare correctly (`2.0.0~rc1` sorts before `2.0.0`, verified with
  `dpkg --compare-versions`) for anyone using standard Debian tooling, but the running
  daemon does not use that comparison to gate an OTA upload today.
- **SSH host keys are unique per device, generated on first boot.** Fixed after `v2.0.0-rc2`
  (which shipped a build-time-baked, shared key set — see that release's own notes for the
  original defect). The clean-install image now ships with no SSH host private key material
  at all; `stratux-ssh-hostkeys.service` generates a fresh, unique key set the first time each
  device boots, before `ssh.service` is allowed to accept connections. See
  [ssh-host-keys.md](ssh-host-keys.md) for the full lifecycle (clean install, OTA upgrade,
  recovery/reflash, and card-cloning implications).
- **Default SSH password.** The public clean-install image still ships with SSH enabled and
  the pi-gen-standard default password (host *keys* are now unique per device — see above;
  the *password* is a separate, still-shared default). Standard Raspberry Pi OS practice, not
  unique to this project, but explicitly called out here: change it (or disable password
  auth) before relying on the device on an untrusted network.
- **Reproducibility is functional, not bit-for-bit.** Two independent builds of this release
  from the same commit produced content-identical files except for one deliberately
  timestamped cache-busting file (`stratux.appcache`); the packages themselves differ only
  in incidental build-time file-modification metadata, not behavior. See this release's own
  notes (`docs/releases/v2.0.0-rc2.md`) for the exact comparison evidence.
- **Image build performance/timing is untested at scale.** The `pi-gen`-based image build
  (`image_build/`) is this project's own established mechanism, but this release is the
  first time it has been exercised as part of a formal, checksum-verified release process.
- **Optional features remain disabled by default** in both the `.deb` and the clean-install
  image: Automatic Flight Recording, and (excluded entirely from this release) the FIS-B
  weather cache. Traffic/system alerting and closure-rate/CPA alerting are enabled by
  default with conservative thresholds, matching their own documentation.

## Feature-specific limitations

- **Browser/app audio-alert path.** Not every alert type is supported identically across
  the browser, iPad, and Bose A30 audio paths - see [alerting.md](alerting.md) for the
  exact per-path constraints.
- **No fan tachometer.** The fan-controller hardware has no rotation-feedback sensor, so
  fan operation is never electronically confirmed by this project, only inferred from
  commanded duty cycle and thermal trend. The dashboard states this plainly rather than
  implying a confirmation that doesn't exist - see [ahrs-baro-fan-health.md](ahrs-baro-fan-health.md)
  and [preflight-readiness.md](preflight-readiness.md).
- **Wi-Fi Admin's reconnection proof is IP-address-based, not kernel-interface-based** -
  see [wifi-administration-hardening.md](wifi-administration-hardening.md).
- **Configuration Backup's explicit non-goals** (credentials, Wi-Fi/network configuration,
  OS-level settings) - see [configuration-backup-restore.md](configuration-backup-restore.md).

See [CHANGELOG.md](../CHANGELOG.md) for the full feature-to-doc mapping.
