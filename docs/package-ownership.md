# Package file ownership

Everything the Stratux `.deb` installs under `/opt/stratux`, plus its systemd units and udev
rules, is **`root:root`**, whoever built the package. Root executes those files at boot
(`stratuxrun`, `epaperd`, `fancontrol`, `stratux-pre-start.sh`, the SSH key helper, …), so an
unprivileged owner would let that account replace root-run code without any privilege check.

## What went wrong

`dpkg-deb -b` records the **numeric uid/gid of the account that ran the build** in the package's
data archive. CI builds as uid 1001 and a developer/owner build as uid 1000, so installed files
were owned by an arbitrary, usually non-existent, uid; on the reference device the directories
were `pi:pi` (uid 1000, from the original image) and the OTA-installed files uid 1001.
`dpkg --verify` does not report ownership, and dpkg re-owns files on upgrade but **never** an
already-existing directory or an existing conffile (`stratux.service`), so nothing repaired it.

Severity: hygiene / defense in depth. `pi` already has passwordless `sudo`, every Stratux process
runs as root, and the root overlay makes live tampering non-persistent. The point is that the
package must not depend on who happened to build it.

## The fix (two layers)

**A. Normalize the archive** — `Makefile` builds with `dpkg-deb --root-owner-group -b`
(dpkg ≥ 1.19; Debian 12 ships 1.21). Every entry is `0/0` for any builder uid:gid. Fresh
installs are correct from the archive.

**B. Migrate existing installs** — `debian/postinst.dpkg` runs `stratux_normalize_ownership`
before anything else, i.e. **before the `STRATUX_OTA_INSTALL` early exit** (an OTA install on
the bare root filesystem is the only path whose changes persist). It:

- visits only the paths dpkg lists for this package (`/var/lib/dpkg/info/stratux.list`) that are
  under `/opt/stratux`, in `/lib/systemd/system/*.service` or in `/etc/udev/rules.d/*.rules`;
- does `chown -h root:root -- <path>` per path, only when the path is not already `0:0`;
- skips symlinks, paths containing `..`, and anything under a symlinked parent;
- never recurses, never changes a mode, never touches an unlisted file;
- leaves the **contents** of `/opt/stratux/mapdata` alone (user-uploaded data; the directory
  itself becomes `root:root` and stays `0777`);
- is best-effort: a failure prints a warning and never fails the install; it is idempotent (a
  normalized device issues no `chown` at all).

`chown -R root:root /opt/stratux` (or any blind equivalent) is deliberately **not** used.

The SSH helper's own `chown root:root …` in the Pi block of the postinst is now redundant with
this migration; it is kept as a harmless second line of defense.

## The invariant

Everything in the archive is `root:root`, nothing is setuid/setgid/sticky, and nothing is group-
or world-writable **except `/opt/stratux/mapdata/`** (an explicit one-entry allowlist; users
upload map data there as `pi`). Root never auto-executes anything from a user-writable
directory: no unit, maintainer script or boot helper, and no Go `exec`, references scripts in
`mapdata/`, `ogn/` or `softrf/` (those are manual helpers, and `ogn/`/`softrf/` are root-owned
anyway). If that ever changes, the executable must move out of the writable directory first.

## Rollback and downgrade (the boundary)

- The OTA backup is a numeric-owner tar of the tree *as it was*. Rolling back restores the
  previous — legacy — owners; that is a faithful restore, not a regression. The next upgrade
  repairs them again.
- Downgrading to a pre-normalization package re-creates that package's file owners; upgrading
  again repairs them. Nothing fails and no user data is touched in either direction.

## Tests

| Script | What it proves |
| --- | --- |
| `test/package_ownership_static_test.sh` | Makefile flag, no bare `dpkg-deb -b` anywhere, migration placement/shape, ShellCheck |
| `test/package_ownership_archive_test.sh [--deb X]` | real `make dpkg` under uid:gid 1000, 1001, 4242:4243 and 0 → identical `0/0` archives; the invariant and privileged-path checks; the old way is *detected* |
| `test/package_ownership_lifecycle_test.sh X.deb` | fresh install, legacy upgrade (normal and OTA-style), idempotency, rollback, downgrade, removal/purge, user-data preservation, hostile inputs to the migration |
| `test/package_ownership_mutation_test.sh` | each way of breaking the fix is caught by a test |

The last three need Docker; the lifecycle test accepts the exact CI artifact
(`PLATFORM=linux/arm64` under QEMU). None of them touches a device.

## Physical acceptance (Raspberry Pi 4 Model B Rev 1.4, one device)

The exact CI artifact was deployed through the normal Stratux OTA path (`POST /updateUpload`,
no manual `dpkg`, no manual `chown`, no `overlayctl unlock`, no extra reboot) to the reference
device, which still carried the legacy ownership.

- **Artifact:** `stratux-2.0.0~rc2-arm64.deb`, 86,322,004 bytes, SHA-256
  `64939e06c53a050d9c5d323df08d0f3103f3670c1359f21d41c001c459b0d232`, built from
  `381bed1e980cc117139a4b8830828becb8b37ef4` (CI run 35959915339), `vcs.modified=false`; its
  data archive (180 entries) and control archive (6) are all `0/0`. The device reported that
  commit in the upload response and, after the update, in `getStatus`.
- **OTA sequence** (observed from outside): `idle` → `staged` → `disable_requested` → reboot into a
  bare ext4 root (boot `72fa382a…`, seen over SSH with root filesystem type `ext4`) → reboot into
  the protected overlay (boot `4d5806a4…`) with the new build and stage `idle`. About 1 min 35 s
  from `disable_requested` to the new build answering HTTP. Pre-update boot `06a9fa2a…`.
- **Census** (`/var/lib/dpkg/info/stratux.list` filtered by the migration's own scope rules, 172
  paths, taken before and after with the same script):

  | | before | after |
  | --- | --- | --- |
  | migration scope, non-`root:root` | 168 (18 directories and `stratux.service` at `pi:pi` 1000:1000, 149 files at 1001:1001) | **0** |
  | migration scope, already `root:root` | 2 (the SSH helper and its unit) | 170 |
  | `mapdata/` contents (2 package files) | 1001:1001 | 0:0 |

  No mode or type changed on any of the 172 paths. `/opt/stratux`, `/opt/stratux/bin` and
  `stratux.service`: `pi:pi` 0755/0755/0644 before, `root:root` after. `mapdata`: `pi:pi` 0777
  before, `root:root` 0777 after.
- **Which layer did what:** dpkg never re-owns an existing directory or conffile, so the 18
  directories and `stratux.service` can only have been repaired by the postinst migration. The 149
  files are re-extracted by dpkg from the normalized archive as well, so the census alone cannot
  say which layer repaired them. The two `mapdata/` files are excluded from the migration and
  became `root:root` through dpkg extraction only. No `could not set root ownership` warning was
  found in any log; `dpkg --audit` and `dpkg --verify stratux` were clean.
- **Data:** the device had no user-uploaded `mapdata` files and no unlisted files under
  `/opt/stratux`, so those two cases were exercised only by the lab test, not physically. Every
  file on the data partition that was hashed (SSH key file, calibration profiles, export,
  recording, diagnostic, stored package) was byte-, owner- and mode-identical afterwards. The only
  difference was the runtime marker `power-session.json`, which the running system rewrites at
  boot.
- **Privileged-path invariant:** 100 paths checked (everything the units execute, `bin/*`, `cfg/*`,
  the libraries, the units, the udev rules, and every parent up to `/`, with `/lib` judged by its
  target `/usr/lib`): all `root:root` and not group/world-writable. As `pi`, `/opt/stratux`,
  `bin`, `cfg`, `lib` and `/lib/systemd/system` and the root-executed files are not writable;
  `mapdata` is (intended). No setuid/setgid file under `/opt/stratux`. Nothing was exploited or
  replaced to test this.
- **Regression:** SSH persistence (fresh key login worked straight after the update without
  re-adding a key; the helper found the volatile copy already matching; password authentication
  and sshd policy unchanged), e-paper (`RUNNING`, 0 BUSY timeouts, 0 consecutive failures), AHRS,
  barometer, GDL90, fan and storage `READY`, 1090 ES receiving from uptime 129 s, UAT `READY`
  (served by the external radio), zero failed units, OTA `idle`, overlay active with no disable
  marker. GPS was `DEGRADED` (indoors, no fix), as before the update.
- **Power:** `get_throttled` read `0x50000` (undervoltage and throttling *have occurred*, not
  active) after the update, from a single kernel `Undervoltage detected!` about 10 s into the boot;
  there was no recurrence over the following minutes. The health report's `System: DEGRADED`
  comes from that sticky flag. It was not investigated further and is not attributed to this
  change.

Not exercised on hardware: an OTA rollback, a downgrade, removal, and a user-uploaded `mapdata`
file (all covered by the lab test). The device's wall clock is unsynchronized, so log timestamps
from the device are unreliable; the sequence above uses this workstation's clock.

Evidence (kept on the device under `/var/lib/stratux-data/acceptance/package-ownership/` with a
byte-identical host copy): the before/after census, the machine-derived ownership diff, the data
manifests, the OTA transition log, the invariant output and the health snapshots. Manifest
SHA-256 `4eebfc1f1bbb03d8aeb9c2208563a0ab4634a718e0ae725ab439af2a50ec727e`.
