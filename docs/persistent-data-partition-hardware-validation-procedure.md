# Persistent-Data Partition: Proposed Sacrificial-Card Hardware Validation Procedure

**This document is a proposal only. No step in it has been executed. Every step requires your
separate, explicit, per-step authorization before it runs - most importantly, no block device is
written to until you have confirmed its exact identity immediately beforehand.** See
`docs/persistent-data-partition.md` for the design this validates and
`docs/ota-persistent-storage-defect.md` for the incident it closes.

## Mandatory tooling safeguards (apply to every step below)

Both requirements below are drawn directly from a real incident during this design's own
sacrificial-card write: a working copy of the verified image was loop-mounted read-write (missing
`-o ro`) purely for read-only inspection (`cat`/`grep`/`find`/`diff`, no file ever written
deliberately). Mounting an ext4/vfat filesystem read-write updates on-disk metadata as a side
effect of the mount itself and of ordinary reads - the ext4 superblock's mount-count and
last-mounted timestamp, and file `atime` on every file touched - silently changing that copy's
bytes (identical size, different SHA-256) with no file content ever deliberately written. The
resulting checksum mismatch was printed but not gated on, and a stale copy was written to the card
before the mismatch was caught from the printed output and corrected. Both safeguards below exist
specifically to make that class of mistake structurally impossible to repeat, not just
harder to miss:

1. **Every image mount for inspection - at any point in this procedure, not only immediately
   before writing - MUST be read-only.** Always `mount -o ro <device> <mountpoint>` (or
   `losetup -r` before mounting), never a bare `mount` with no explicit read-only option. This
   applies to the raw `.img`, any loop-mounted partition of it, and any working copy derived from
   it - a copy that is itself never mounted read-write cannot silently drift.
2. **Every pre-write (and post-write read-back) checksum comparison MUST be a hard command gate
   that aborts on mismatch, never a printed value a human or a later step is expected to visually
   compare against another printed value.** A script that computes and echoes "expected: X" /
   "actual: Y" and then proceeds regardless is not a safety check - it is two log lines. Use
   `sha256sum -c` against a checksum file (exit code nonzero on mismatch, safe to `&&`-chain
   directly into the destructive command), e.g.:

   ```sh
   echo "492a647aae71c4c303231b3c645ebdd17d5d5235290a9c5548eff5b121c8c6f7  path/to/image.img" \
     > expected.sha256
   sha256sum -c expected.sha256 && sudo dd if=path/to/image.img of=/dev/sdX bs=4M conv=fsync status=progress
   ```

   or, if constructing the check inline rather than via a checksum file, an explicit
   `if [ "$actual" != "$expected" ]; then exit 1; fi` (or `set -e` with a command whose own exit
   code reflects the comparison) - never a bare comparison whose result is only displayed. The same
   hard-gate requirement applies to the post-write read-back verification in Step 1.3 below: read
   the written bytes back off the device and pipe the comparison through a gate that aborts (or at
   minimum, clearly fails) rather than one that only prints both values for a human to eyeball.

## Pre-conditions (confirmed before any of this begins)

- A card you have explicitly identified as spare and sacrificial - never inferred from a device
  path seen in a previous session.
- A build of this branch's artifact, produced via the trusted CI path, with its filename, size,
  and SHA-256 already recorded (see the PR for these).
- The operational device is not touched by any step below.

## Step 0: Device identity confirmation (repeated before every write)

Before the image is written, and again immediately before confirming the write command:

1. Run `lsblk` and show you the full output.
2. Record: device node, byte size, model, serial (if available), transport (USB/SD), removable
   flag, current filesystems/labels/UUIDs/mountpoints.
3. Confirm this device is not the host machine's own OS disk.
4. Unmount every partition on it.
5. **Ask you to approve that exact device node, by its identity as just shown - not a remembered
   path.**
6. Re-run `lsblk` immediately before the write command executes, and re-confirm nothing changed.

Only after your explicit approval of that exact, freshly-reconfirmed identity does step 1 begin.

## Step 1: Image write and verification

1. Compute and show the SHA-256 of the image file to be written, before writing - the exact same
   file path used in step 2, never a differently-named copy assumed to be identical.
2. **Hard-gate the write on that checksum** (see "Mandatory tooling safeguards" above -
   `sha256sum -c` chained with `&&`, or an explicit abort-on-mismatch check): the write command
   must not be reachable at all if the checksum does not match. Only once the gate passes, write
   the image to the approved device.
3. Read back and verify the write - either a full byte-for-byte comparison against the source
   image (for a card size matching the image exactly) or a hash of the written region, whichever
   is correct for the actual size relationship between the image and the card - by reading the
   bytes back off the physical device itself, not by trusting the write tool's own exit code alone.
   This comparison must also be a hard gate (see above), not a printed pair of values.
4. Confirm no errors were reported by the write tool itself.

## Step 2: First boot and partition provisioning

1. Insert the card into the test hardware (bench setup, per your existing safety conventions -
   grounded, no aircraft, known-good recovery path for the *test* hardware available).
2. Power on. Expect: first-boot partition provisioning
   (`docs/persistent-data-partition.md`'s own first-boot sequence) runs once, automatically,
   ending in one `reboot -f`.
3. After the device comes back up (second boot), confirm via SSH:
   - `lsblk` shows **exactly 3 partitions**: boot (FAT32), root (ext4, ~8 GiB), data (ext4,
     labeled `stratux-data`, consuming the remainder of the card).
   - `findmnt -T /var/lib/stratux-data` reports a genuine, dedicated ext4 mount (`Target` equals
     the path itself, `FSType` is `ext4`) - not the root overlay.
   - `findmnt -T /` reports `overlay` (protected mode restored normally - this first-boot sequence
     never leaves the device in bare-ext4 mode, unlike the OTA incident this design closes).
   - `blkid` confirms the data partition's label and filesystem type directly.
   - `PersistentDataUUID` (via `/getSettings`) is pinned, and matches the data partition's own
     real, freshly-generated UUID (via `blkid`) - not the root filesystem's UUID (which would
     indicate the incident's own false-discovery bug has recurred).
   - `systemctl --failed` reports no failed units.
   - No undervoltage/throttling (`/getHealth`'s `System` fields).

## Step 3: Persistence matrix

Using only supported product interfaces (never manual file writes), create one item in each
namespace the audit in `docs/persistent-data-partition.md` (B5) identifies as belonging under
`/var/lib/stratux-data`, and record its exact identity (filename, checksum, or a unique sentinel
value) before each of the following, then re-check it is unchanged after each:

- A harmless settings change (e.g. `DarkMode`).
- One test calibration profile (or equivalent safe fixture).
- One short recording.
- One export of that recording.
- One diagnostics bundle.
- The current power-session marker.
- OTA state (idle is itself a valid, checkable state).
- Storage Lifecycle's own inventory.
- One uniquely-named sentinel file placed directly under `/var/lib/stratux-data` for this test
  alone (outside any product namespace, purely as an unambiguous canary).

Verify all of the above survive, unchanged, across:

1. A warm reboot (`systemctl reboot`).
2. A second warm reboot.
3. A controlled shutdown (the existing two-step `/requestShutdown` + `/confirmShutdown` flow) and
   physical power restoration.
4. An overlay disable/enable cycle (`overlayctl disable`, reboot, confirm bare ext4 and the data
   partition both still correct, `overlayctl enable`, reboot, confirm protected mode restored and
   every item above still intact).
5. A `systemctl restart stratux`.

## Step 4: OTA validation

Using two trusted artifacts with distinguishable embedded commits (e.g. this branch's own build,
then a trivial follow-up commit rebuilt the same way):

1. Confirm `/getOTAStatus` is idle and no other operation (recording/restore) is active.
2. Upload the second artifact via the dashboard's existing update mechanism.
3. Confirm the upload is **accepted** this time (the fail-fast guard from
   `docs/ota-persistent-storage-defect.md` requires exactly the genuine dedicated mount this
   design now provides) - contrast with the original incident, where the identical upload
   sequence on a card without this partition was rejected (post-fix) or silently lost (pre-fix).
4. Confirm the device reboots into bare ext4, the pre-start script finds the staged package and
   state file (now genuinely on disk), installs, reboots back.
5. Confirm `findmnt -T /` reports `overlay` again (protected mode restored) and
   `findmnt -T /var/lib/stratux-data` still reports the same genuine dedicated mount as before.
6. Confirm the running commit matches the second artifact's own embedded commit.
7. Confirm every persistence-matrix item from Step 3 survived this entire OTA cycle unchanged.
8. Confirm `/getOTAStatus` returns to idle, with no retry loop and no false-success report.
9. If a safely, deliberately testable failure mode is available (e.g. an intentionally corrupted
   third artifact), confirm rollback restores the prior commit cleanly - only if this can be done
   without any unsafe condition; skip otherwise and say so plainly.

## Step 5: Stability window

1. Five warm reboots (beyond the ones already performed above, or continuing that count -
   whichever is clearer to report).
2. One additional controlled shutdown/physical power restoration.
3. At least 30 minutes of continuous operation, checked periodically: no failed units, no
   unexpected restarts, no throttling/undervoltage, stable mount identity (`findmnt` reporting the
   same `stratux-data` label/partition throughout), overlay protected, OTA idle.
4. Confirm core receivers (978/1090), GDL90/ForeFlight connectivity, AHRS, barometer, fan, GPS, and
   power monitoring are all unaffected throughout - the same regression scope this mission's
   earlier e-paper and OTA work already established as the standard check.

## What this procedure will not do

- Never simulate power loss.
- Never touch the operational device.
- Never proceed past Step 0 without your explicit, freshly-confirmed approval of the exact device.
- Never merge or mark any PR ready as a result of this procedure alone - that remains a separate
  decision after you review the recorded evidence.
