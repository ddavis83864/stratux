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
