# OTA Update Mechanism: Overlay Transition and the Deterministic Install State Machine

This documents the `.deb`-based OTA update mechanism (`POST /updateUpload`), its interaction
with the protected read-only-root overlay, and the hardware evidence that pins down exactly one
correct way to request a bare-ext4 install boot.

## The defect this replaces

The original mechanism raced: `reboot` (as invoked from `debian/stratux-pre-start.sh`, most
likely resolving to `systemctl reboot`) returns asynchronously - the calling script kept
executing while shutdown proceeded in the background. The staging stage (copy `.deb` to the
ext4 lower layer, disable the overlay, reboot) and the install stage (`dpkg -i`, re-enable the
overlay, reboot) could both run within the same still-live, overlay-mounted session, before the
overlay had ever actually been disabled - because disabling/enabling the overlay only changes
state for the *next* boot, not the currently mounted filesystem. `dpkg -i` therefore installed
against the live 250 MiB tmpfs overlay, not the real ~9.5 GiB bare ext4 partition, and failed
with `ENOSPC` trying to back up a large existing file (`./opt/stratux/mapdata/osm.mbtiles`)
before overwriting it. Fixed by adding `sync`/`exit 0` immediately after every `reboot` call, so
the script actually halts once a reboot has been requested (see git history for the exact
before/after).

That fix alone was not sufficient: a subsequent live attempt still did not produce a version
change, and manually inspecting the sequence raised a second, distinct question - **is the
overlay-disable marker itself written to a location that actually survives the reboot?**

## The marker-location hypothesis, and why it needed proof rather than assumption

`overlayctl`'s `disable` action writes a marker at `$overlay_base/overlay/disable`, where
`$overlay_base` is `/overlay/robase/` when called with the overlay active. `/sbin/init-overlay`
checks for a marker at `/overlay/disable` at the very start of the *next* boot, before any
overlay/tmpfs setup happens - at that point, `/` is still the plain, freshly-mounted ext4
partition, so `/overlay/disable` is a real, top-level directory on the true filesystem.

The catch: while the overlay is *currently* active, there is more than one path that plausibly
looks like "the real underlying directory" - and they are not all the same thing. Two
init-overlay implementation details create a second, similarly-named, but *volatile* path:

- `init-overlay` creates a tmpfs at `/overlay` very early (`mount -n -t tmpfs none /overlay`),
  then builds `/overlay/robase` (a bind mount of the true root) and `/overlay/rwdata` under it.
- When it later does `mount -n --move` to relocate `robase` and `rwdata` by name into the newly
  built combined/overlay root, it **only moves those two named children** - it never explicitly
  relocates the original top-level `/overlay` tmpfs mount itself.
- After `pivot_root`, the old root (with that orphaned, still-mounted `/overlay` tmpfs inside
  it) ends up parked at `/overlay/pivot`. The result: `/overlay/pivot/overlay` is a tmpfs left
  behind by this bookkeeping, not the real underlying directory - even though the path *looks*
  like it should be equivalent to `/overlay/robase/overlay`.

Writing a marker to the wrong one of these two lookalike paths silently vanishes on reboot. This
had to be proven from mount identity evidence and empirical reboot survival, not assumed from
reading the shell scripts alone.

## The evidence

Captured directly from the live development target while the overlay was active:

```
$ stat -c '%n  dev=%d ino=%i' / /overlay /overlay/robase /overlay/robase/overlay /overlay/pivot /overlay/pivot/overlay /overlay/rwdata
/                        dev=22(0x16)     ino=2
/overlay                 dev=22(0x16)     ino=3       # part of the live overlay itself
/overlay/robase          dev=45826(0xb302) ino=2       # bind mount of the true ext4 root
/overlay/robase/overlay  dev=45826(0xb302) ino=31105   # <-- same device as robase: real, persistent
/overlay/pivot           dev=45826(0xb302) ino=2       # same device as robase (same underlying ext4 mount)
/overlay/pivot/overlay   dev=20(0x14)      ino=1       # <-- DIFFERENT device: a tmpfs shadow
/overlay/rwdata          dev=21(0x15)      ino=1       # the 250 MiB writable overlay layer (tmpfs)

$ blkid /dev/mmcblk0p2
/dev/mmcblk0p2: LABEL="rootfs" UUID="8f0772f1-f93a-4590-9833-cd3a800647e1" TYPE="ext4"
```

`/overlay/robase/overlay` and `/overlay/pivot` share device `45826` with the real mounted ext4
partition; `/overlay/pivot/overlay` reports device `20`, an unrelated tmpfs. This is the
automated check `ota.IsPersistent` encodes: a candidate marker location must share the
reference's device number and must not be a known-volatile filesystem type
(tmpfs/overlay/ramfs/devtmpfs/aufs/unionfs), regardless of what its path looks like.

### Empirical proof (harmless test markers, no package installed)

With the overlay active, a narrow `overlayctl unlock` / write / `sync` / `overlayctl lock`
sequence was used to write two differently-named, non-`disable` test files - one at each
candidate path - so the test could not accidentally trigger a real overlay-disable regardless of
outcome:

| Marker | Path | Device | Survived reboot? |
|---|---|---|---|
| `marker-test-good` | `/overlay/robase/overlay/` | 45826 (ext4) | **Yes** - identical content, same device and inode |
| `marker-test-bad` | `/overlay/pivot/overlay/` | 20 (tmpfs) | No - gone after reboot |

### Causation proof (the real `disable` marker, still no package involved)

The same narrow sequence was then used to write the actual `disable` marker at
`/overlay/robase/overlay/disable`, and the effect was confirmed directly from the *next* boot's
own root mount, not inferred:

```
# before reboot: /overlay/robase mount options: rw,relatime (after unlock) -> ro,relatime (after lock)
# next boot:
$ findmnt -n -o SOURCE,FSTYPE,OPTIONS /
/dev/mmcblk0p2  ext4  rw,noatime          <-- bare ext4, NOT overlay
```

The marker was then removed (bare ext4 root is directly writable - no remount needed) and a
final reboot confirmed the overlay's return:

```
$ findmnt -n -o SOURCE,FSTYPE,OPTIONS /
overlay  overlay  rw,noatime,lowerdir=/overlay/robase,upperdir=/overlay/rwdata/data,workdir=/overlay/rwdata/work,uuid=on
```

`stratux.service` was confirmed active/running/`NRestarts=0` in both the bare-ext4 and restored
overlay boots, and the version/build were unchanged throughout (`d3ac939607e0afe48b0c6dbfebbb673204ca0d0a`)
- these marker-only tests never touched the package.

## The deterministic OTA state machine

Implemented in the `ota` Go package (pure, hardware-free decision logic - `ota.Decide`) plus two
real-I/O halves: `main/ota.go` (the Go daemon: upload, verify, request-disable, post-reboot
verify/cleanup, hand off to rollback) and `debian/stratux-pre-start.sh` (the bare-ext4 install
half, which must run before the Go daemon exists on that boot).

### State and storage

All OTA state lives under `/var/lib/stratux-data/updates/` (the persistent data partition, never
`/boot/firmware` or the overlay):

- `state.json` - the current `ota.State` (stage, staged package path, expected SHA-256/version/
  commit, backup path, attempt count, last error), written atomically (temp file + rename) by
  both sides.
- `staged/` - the uploaded `.deb`, kept until a successful update's cleanup or a rollback.
- `backup/` - a `tar.gz` of `/opt/stratux`, the systemd unit files, and the udev rules, taken
  immediately before `dpkg -i` runs, used to restore if verification fails.

Both directories are pruned to `otaMaxRetainedPackages` (3) after a successful update.

### Stages

`idle -> staged -> disable_requested -> installing -> installed -> verifying -> complete`, with
`failed -> rolled_back` reachable from any stage that detects a problem. See `ota/state.go` for
the full definitions and `ota/decide.go` for the transition rules - every rule below is a real,
independently-tested case, not prose describing untested behavior:

- **Staged**: the uploaded package is verified as a well-formed `.deb` (`dpkg-deb -f`), its
  SHA-256 recorded, and its embedded commit extracted directly from the packaged
  `stratuxrun` binary (`dpkg-deb -x` to a temp dir, then scanned for a 40-hex-character run) -
  independent of the package's `Version` control field, which does not change per commit.
- **DisableRequested**: `requestOverlayDisable()` performs the proven narrow remount/write/
  sync/relock sequence - after first confirming the marker path shares `/overlay/robase`'s
  device number (`ota.IsPersistent`), refusing to proceed otherwise - then reboots. If root is
  still `overlay` when re-checked, that is `ActionAwaitReboot`, not a failure: the reboot simply
  has not happened yet.
- **Installing** (bare ext4 only): backs up the current install, runs `dpkg -i --force-depends`,
  and never runs it anywhere else - if this stage is ever reached while root reports `overlay`,
  the decision is `ActionRollback`, not an install attempt. A resumed "installing" state (e.g.
  after a power loss) re-derives success from real signals (dpkg status + the installed binary's
  actual commit) rather than blindly retrying or blindly trusting the stage name. Retries are
  bounded (`ota.MaxInstallAttempts`, 3) before giving up and rolling back - including a
  persistent `ENOSPC` on the real partition, not just the overlay-mistake case this whole
  mechanism exists to prevent.
- **Installed**: the disable marker has been removed and a reboot back to the overlay
  requested; `ActionAwaitRebootToOverlay` while still bare ext4, `ActionVerify` once back under
  the overlay.
- **Verifying**: the running daemon's own reported commit (`/getStatus`'s `Build` field) must
  match the expected commit before declaring success.
- **Complete**: staged/backup files are pruned and the state file cleared.
- **Failed / RolledBack**: the pre-install backup is extracted back over `/opt/stratux` and the
  unit files, `systemctl daemon-reload` is run, and the overlay is unconditionally re-enabled
  before the final reboot - rollback always restores overlay protection, regardless of which
  stage failed.

### Never against the overlay; never on package presence alone

Two mission requirements are structural, not just documented intent:

- `ota.Decide` returns `ActionRollback` (not `ActionInstall`) if the `installing` stage is ever
  observed while `RootFSType == "overlay"` - there is no code path that runs `dpkg -i` without
  first confirming bare ext4.
- Every stage that could resume after a package was staged (`Staged`, `DisableRequested`)
  re-verifies the file's actual SHA-256 against the recorded expected value before proceeding -
  the file merely existing is never treated as proof it is the right, uncorrupted package.

## A wedging retry-guard defect found by dry-run testing, before touching hardware

The install/rollback block in `debian/stratux-pre-start.sh` had never actually been executed -
only syntax-checked (`bash -n`) - before it was exercised end to end in an isolated sandbox (a
scratch directory tree with stubbed `overlayctl`/`dpkg`/`dpkg-query`/`reboot`/`systemctl`/
`findmnt` on `PATH`, each script invocation representing one simulated boot). That run surfaced a
real defect: the top-level guard only matched `Stage == "disable_requested"`, but a failed
`dpkg -i` advances `Stage` to `"installing"` *before* the retry reboot. On the next boot the guard
no longer matched anything, so the block never ran again - `Attempts` stayed frozen, the overlay
was never re-enabled, and the device would wedge on bare ext4 indefinitely after a single
transient install failure (an `ENOSPC`-class failure, precisely the kind this mechanism exists to
survive, being the most likely real-world trigger).

Fixed by widening the guard to also resume from `Stage == "installing"`, re-deriving success from
`dpkg-query`'s own status/version (in case a previous `dpkg -i` actually completed just before a
reboot or power loss cut the script off before it could record `"installed"` - the same
power-loss-safe philosophy `ota.Decide` already uses on the Go side) rather than blindly retrying
or blindly trusting the stage name, and reusing the single backup taken before the first attempt
instead of creating a redundant one per retry. Re-validated in the same sandbox across nine
scenarios: overlay still active (await-reboot, no dpkg touched), missing staged package, hash
mismatch, first-attempt success, a persistent failure exhausting all 3 attempts across separate
boots and then rolling back, a transient failure recovering on a later attempt with the backup
reused rather than duplicated, the power-loss-safe resume short-circuit (dpkg already healthy,
`dpkg -i` not re-invoked), and rollback genuinely restoring the pre-install backup's content. No
package was installed on real hardware to find or fix this - it was caught entirely by the kind of
loopback/namespace validation this mechanism was already meant to prefer over live reboot cycles.

## A missing runtime dependency found while staging the first real deployment

Before any package had been installed through this mechanism on real hardware, staging the first
real update surfaced a second defect, independent of the retry-guard fix above: the bare-ext4
install/rollback logic was written entirely around `jq`, but `jq` is not part of the base image
and is not a declared dependency of the `stratux` package (`Depends: libncurses6, librtlsdr0` -
no `jq`). The very first attempt to run the disable-marker-driven install would have failed
outright with `jq: command not found`, on an environment where installing `jq` via the .deb itself
is circular - the state-machine script must already work correctly *before* that .deb (or any
`jq` dependency it might declare) is installed.

Fixed by rewriting every `jq` call in the OTA section (`ota_json_get`/`ota_save_stage`/
`ota_begin_install`, and the ad hoc reads in the resume and rollback blocks) to use `python3`
instead - already present on the base image and used elsewhere on the device (e.g. writing
network config). Re-validated identically across the same nine sandboxed scenarios with the `jq`
stub removed entirely and every call site grepped to confirm no `jq` invocation remains.

## A maintainer-script reentrancy defect found on the first live install attempt

With both fixes above in place, the first real install attempt on hardware still failed - this
time from a defect neither sandbox run could have caught, because it depends on the package's own
maintainer scripts, which the sandbox stubs out. `dpkg -i`'s `postinst` calls `systemctl start
stratux` (and `preinst`/`prerm` call `systemctl stop`) as ordinary, correct package-maintenance
practice. But this `dpkg -i` is invoked from `stratux-pre-start.sh`, which is itself
`stratux.service`'s own `ExecStartPre` - so `systemctl start stratux` from inside that `dpkg -i`
starts the very same unit currently activating, which re-invokes `ExecStartPre` (this same script)
*while the outer `dpkg -i` is still running and holding dpkg's lock*. The reentrant invocation saw
`Stage=installing` and, with no way to know it was itself running inside another install already
in progress, took its own independent retry decision - calling `dpkg -i` again, which failed
immediately on dpkg's lock (`rc=2`), and then rebooted mid-install. This is a structural defect,
not specific to the manual bootstrap this first deployment required: any boot-driven install
would hit the identical recursion, since `postinst` always tries to restart the service it just
updated.

Confirmed from hardware evidence, not inferred: `systemctl status stratux` showed the failed
`ExecStartPre` chain with `dpkg`, `stratux.prerm`, and a nested `systemctl` all listed as "remains
running after unit stopped", and `Restart=always`/`StartLimitBurst=5`/`StartLimitIntervalUSec=10s`
(from `stratux.service`) explains why the device stabilized on its own after the burst limit was
hit, rather than looping indefinitely.

Fixed by gating every `systemctl start`/`stop`/`restart` in `preinst.dpkg`/`postinst.dpkg`/
`prerm.dpkg` behind a `STRATUX_OTA_INSTALL` environment variable, set only when
`stratux-pre-start.sh` invokes `dpkg -i` itself. This is safe precisely because that script always
reboots once `dpkg -i` returns (success, retry, or rollback) - the next boot's normal init
sequence starts the service correctly every time, so the maintainer scripts' own service control
is redundant during an OTA install and actively harmful. `systemctl daemon-reload` and `systemctl
enable` remain unconditional in `postinst` (they don't start anything, and unit-file/enablement
state should stay current regardless of who invoked the install). Verified directly by running
each maintainer script with a stubbed `systemctl` on `PATH`, with and without the environment
variable set, confirming zero `systemctl` invocations when set and the original behavior
unchanged when unset.

The live device recovered cleanly through this session's own designed rollback path once
`Attempts` reached its bound: files restored from the pre-install backup, overlay marker removed,
overlay confirmed restored (`findmnt`), and the daemon confirmed back at the pre-deployment
version and commit - `stratux.service` active with `NRestarts=0` throughout. The backup's own
scope was independently confirmed complete against the package's full file manifest
(`dpkg-deb --contents`): every shipped file falls under one of the three backed-up paths, so
nothing was left un-covered by the restore.

## Package-database reconciliation on rollback

A file-level rollback alone left a real reliability defect, not a cosmetic one: restoring
`/opt/stratux` and the unit files from the tar backup put the correct bytes back on disk, but
`dpkg -s stratux` kept reporting whatever state the interrupted install had left it in (e.g.
`install ok unpacked`) rather than the version actually present on disk. That mismatch is a false
premise for any later `dpkg -i` or `dpkg --configure -a`, which reasons about the package from
dpkg's own database, not from what files happen to be on disk.

The fix is deliberately narrow - never a copy of the whole `/var/lib/dpkg` hierarchy, which would
risk corrupting unrelated packages' state:

- **Backup** (`ota_dpkg_meta_backup`, taken once per transaction alongside the existing file tar,
  before the first `dpkg -i` attempt): the `stratux` package's own stanza in
  `/var/lib/dpkg/status` - extracted without reading, modifying, or even necessarily preserving
  the byte layout of any other package's stanza in that file - and its per-package control files
  under `/var/lib/dpkg/info/stratux.*`. Both are folded into the same backup tarball under a
  synthetic `ota-dpkg-meta/` member so one backup file remains one transaction's complete restore
  point.
- **Restore** (`ota_dpkg_meta_restore`, run as part of rollback, right after the file restore):
  splices the backed-up stanza back into `/var/lib/dpkg/status` in place of whatever is there now
  (or removes it if the package genuinely had no stanza before this transaction - the fresh-install
  case), and replaces `/var/lib/dpkg/info/stratux.*` with exactly the backed-up set (removing
  whatever the failed install left there first). Every other package's stanza and info files are
  never touched.
- **Power-loss safety**: the status-file splice is written atomically - temp file, `fsync`,
  `os.replace`, then `fsync` the containing directory - so a power loss during the write itself
  leaves `/var/lib/dpkg/status` either fully the old content or fully the new content, never torn.
  A power loss between the file restore and the metadata restore simply resumes both on the next
  boot, since `Stage` only advances to `rolled_back` after both steps complete.
- **Idempotent**: splicing the same stanza in twice, or re-copying the same info/ files twice,
  produces an identical result both times - repeated rollback invocation is safe by construction,
  not by an added guard.
- **Graceful degradation**: a backup taken by an older version of this script (before this fix
  existed) or one with a corrupt/incomplete `ota-dpkg-meta` member is detected and logged as a
  warning; the file-level restore still completes and the transaction still reaches `rolled_back`
  - `dpkg`'s database is left unreconciled in that one case, but the device is not left stuck, and
  the gap is visible in the log rather than silent.

The splice algorithm (`ExtractStanza`/`ReplaceStanza`) is implemented and unit-tested as pure logic
in the `ota` Go package (`ota/dpkgmeta.go`, `ota/dpkgmeta_test.go`) - stanza replacement in place,
removal, append-when-missing, exact-name matching (no `stratux-foo` collision with `stratux`),
idempotency, and neighbor-stanza preservation - and mirrored by an equivalent python3 script
embedded in `debian/stratux-pre-start.sh`, for the same reason the rest of the bare-ext4 state
machine is re-derived in bash rather than shared: `ExecStartPre` runs before the Go daemon exists
on a freshly-booted bare-ext4 system. Verified end to end (not just at the algorithm level) via the
sandboxed dry-run harness: a successful install reconciles dpkg's database to the new version; a
rollback reconciles it back to the exact prior version and status; the same rollback invoked twice
is a no-op the second time; a fresh install started after a completed rollback succeeds cleanly;
re-entering the `installing` stage when dpkg already reports the target version installed
short-circuits without invoking `dpkg -i` again; and a backup missing its metadata member degrades
to a logged warning without blocking the file-level restore.

## Known limitations

- The shell side re-derives (in bash) the same stage logic `ota.Decide` encodes in Go, and the
  same dpkg-metadata splice algorithm `ota.ExtractStanza`/`ota.ReplaceStanza` encode in Go, since
  `ExecStartPre` runs before the Go daemon exists on a freshly-booted bare-ext4 system. Keeping
  the bash and Go implementations in sync is a manual discipline, not enforced by a shared
  implementation.
- A backup taken before the package-database reconciliation fix existed (or one whose
  `ota-dpkg-meta` member is otherwise missing/corrupt) cannot have `dpkg`'s database reconciled
  from it - restore falls back to files-only, logs a warning, and still reaches `rolled_back`
  rather than getting stuck, but the operator should expect to run `dpkg --configure -a` or an
  equivalent manual fixup afterward in that specific case.
