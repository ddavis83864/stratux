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

- **First formal release.** `v2.0.0-rc1` is this fork's first tagged, checksummed,
  independently-verifiable release. Everything before it was build-it-yourself only; treat
  this RC with the scrutiny appropriate to a first release, not an N-th point release with a
  long track record.
- **Release candidate, not stable.** See [release-process.md](release-process.md) for exactly
  what separates an RC from stable, and what has to happen before this line is promoted.
- **No OTA version-ordering enforcement.** This codebase's OTA mechanism (`docs/ota.md`)
  records whatever `Version:`/embedded-commit an uploaded package declares; it does not
  itself refuse an older or unrelated package. The Debian package version is still built to
  compare correctly (`2.0.0~rc1` sorts before `2.0.0`, verified with
  `dpkg --compare-versions`) for anyone using standard Debian tooling, but the running
  daemon does not use that comparison to gate an OTA upload today.
- **Default SSH credentials.** The public clean-install image ships with SSH enabled and the
  pi-gen-standard default password. Standard Raspberry Pi OS practice, not unique to this
  project, but explicitly called out here: change it (or disable password auth) before
  relying on the device on an untrusted network.
- **Shared SSH host keys across images built from the same process.** This project's
  `image_build/stage2` deliberately disables `regenerate_ssh_host_keys` (the systemd unit
  that would otherwise generate fresh host keys on a cloned image's first boot) and bakes in
  the keys generated at image-*build* time instead. This is an intentional, pre-existing
  tradeoff (the accompanying comment: minimizing SD-card writes on first boot, since a
  power-loss-prone embedded appliance can be bricked by an interrupted write) — not a defect
  introduced by this release, and not changed by it. The practical consequence: every device
  flashed from the exact same published image shares the same SSH host keys, unlike a normal
  Raspberry Pi OS install. If this matters for your deployment, regenerate host keys
  yourself after first boot (`ssh-keygen -A` after removing the existing host key files, then
  reboot) — this is not currently automated.
- **Reproducibility is functional, not bit-for-bit.** Two independent builds of this release
  from the same commit produced content-identical files except for one deliberately
  timestamped cache-busting file (`stratux.appcache`); the packages themselves differ only
  in incidental build-time file-modification metadata, not behavior. See this release's own
  notes (`docs/releases/v2.0.0-rc1.md`) for the exact comparison evidence.
- **Image build performance/timing is untested at scale.** The `pi-gen`-based image build
  (`image_build/`) is this project's own established mechanism, but this release is the
  first time it has been exercised as part of a formal, checksum-verified release process.
- **Optional features remain disabled by default** in both the `.deb` and the clean-install
  image: Automatic Flight Recording, and (excluded entirely from this release) the FIS-B
  weather cache. Traffic/system alerting and closure-rate/CPA alerting are enabled by
  default with conservative thresholds, matching their own documentation.

## Feature-specific limitations

Each feature's own documentation carries its own scoped limitations (e.g. alerting's
browser-audio-path constraints, Wi-Fi Admin's IP-address-based rather than kernel-
interface-based reconnection proof, Configuration Backup's explicit non-goals). See
[CHANGELOG.md](../CHANGELOG.md) for the full feature-to-doc mapping.
