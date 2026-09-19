#!/bin/bash
# persistent_data_partition_test.sh: exercises the real
# image_build/stage2/10-stratux/files/provision-data-partition script
# against real, disposable loop-mounted disk images - proving the actual
# parted/mkfs arithmetic and commands work, not just that the script
# parses. Requires root/CAP_SYS_ADMIN (losetup, parted, mkfs.ext4) - run
# in CI (a real VM runner, not a restricted container) or on a Linux
# development machine with sudo; skips itself cleanly if those tools or
# privileges are unavailable rather than failing the whole test suite.
#
# Every loop device and temp file this script creates is cleaned up via a
# trap, including on failure/interruption - see cleanup() below.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROVISION_SCRIPT="${SCRIPT_DIR}/../image_build/stage2/10-stratux/files/provision-data-partition"
WORKDIR="$(mktemp -d)"
LOOPDEV=""
fail=0

cleanup() {
	if [ -n "$LOOPDEV" ]; then
		sudo losetup -d "$LOOPDEV" 2>/dev/null || true
	fi
	rm -rf "$WORKDIR"
}
trap cleanup EXIT INT TERM

for tool in losetup parted mkfs.ext4 blockdev lsblk sudo; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "SKIP: $tool not available - cannot run real loop-device tests here"
		exit 0
	fi
done
if ! sudo -n true 2>/dev/null; then
	echo "SKIP: passwordless sudo not available - cannot create loop devices here"
	exit 0
fi

check() {
	local desc="$1" ok="$2"
	if [ "$ok" = "1" ]; then
		echo "PASS: $desc"
	else
		echo "FAIL: $desc"
		fail=1
	fi
}

# make_test_image creates a sparse disk image of the given size (in MiB)
# with a stock-like 2-partition layout (a small FAT32 boot partition, a
# small ext4 root partition using only a fraction of the image - the rest
# is the "unallocated space" a real freshly-flashed pi-gen image leaves
# beyond its own minimal built rootfs), attaches it as a loop device with
# partition scanning, and echoes the loop device path.
make_test_image() {
	local size_mib="$1" img="${WORKDIR}/disk.img"
	# rm -f first: `dd ... seek=N count=0` only *extends* a file (via a
	# sparse hole) to reach the seek offset, it never truncates or zeroes
	# existing content, since count=0 means zero write operations happen
	# at all. Reusing the same path across cases without this would
	# leave whatever a *previous* case actually wrote on "disk"
	# (verified directly: real bytes, not merely a stale cache read)
	# physically intact underneath the new partition table - and two
	# cases using the same image size and root cap put their partition
	# 3 at the identical byte offset, so a later case's "freshly
	# unformatted" partition can land squarely on top of an earlier
	# case's real, still-intact stratux-data filesystem. This was a
	# real, reproduced bug in this test harness, not the script under
	# test - see the Case 6 investigation.
	rm -f "$img"
	dd if=/dev/zero of="$img" bs=1M seek="$size_mib" count=0 status=none
	parted -s "$img" mklabel msdos
	parted -s "$img" mkpart primary fat32 1MiB 257MiB
	parted -s "$img" mkpart primary ext4 257MiB 1281MiB
	local dev
	dev="$(sudo losetup -f --show -P "$img")"
	sudo mkfs.vfat "${dev}p1" >/dev/null
	sudo mkfs.ext4 -F "${dev}p2" >/dev/null
	echo "$dev"
}

partition_count() {
	sudo lsblk -no NAME "$1" | tail -n +2 | wc -l
}

# settle_udev: mirrors provision-data-partition's own helper of the same
# name - after this test harness makes its own out-of-band partition
# table changes (simulating an interruption mid-provisioning) and calls
# partprobe, a new partition's device node and udev-probed FSTYPE/LABEL
# can lag partprobe returning. This mirrors defensive hardening added
# to the script under test itself; it did not turn out to be the cause
# of this suite's own Case 6 flakiness during development (that was a
# stale-fixture bug in make_test_image, fixed separately), but is cheap
# insurance worth keeping regardless.
settle_udev() {
	if command -v udevadm >/dev/null 2>&1; then
		sudo udevadm settle --timeout=5 2>/dev/null || true
	else
		sleep 1
	fi
}

# --- Case 1: a card at/above the minimum size gets bounded root growth
#     plus a real, correctly-labeled ext4 data partition. ---
LOOPDEV="$(make_test_image 20480)" # 20 GiB image
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out1.log" 2>&1
RC=$?
cat "${WORKDIR}/out1.log"
check "provision succeeds (exit 0) on a card at the minimum size" "$([ "$RC" -eq 0 ] && echo 1 || echo 0)"
check "reports RESULT=provisioned" "$(grep -q '^RESULT=provisioned' "${WORKDIR}/out1.log" && echo 1 || echo 0)"
check "creates exactly 3 partitions" "$([ "$(partition_count "$LOOPDEV")" -eq 3 ] && echo 1 || echo 0)"
DATA_PART="${LOOPDEV}p3"
check "data partition device exists" "$([ -b "$DATA_PART" ] && echo 1 || echo 0)"
DATA_LABEL="$(sudo blkid -s LABEL -o value "$DATA_PART" 2>/dev/null)"
check "data partition is labeled stratux-data" "$([ "$DATA_LABEL" = "stratux-data" ] && echo 1 || echo 0)"
DATA_FSTYPE="$(sudo blkid -s TYPE -o value "$DATA_PART" 2>/dev/null)"
check "data partition is ext4" "$([ "$DATA_FSTYPE" = "ext4" ] && echo 1 || echo 0)"
ROOT_SIZE_MIB=$(( $(sudo blockdev --getsize64 "${LOOPDEV}p2") / 1024 / 1024 ))
# Root started at 257MiB and should now end at 257+8192=8449MiB, i.e. be
# ~8192MiB (allow +/- a few MiB for parted's own alignment rounding).
check "root partition grew to approximately its 8192MiB cap (got ${ROOT_SIZE_MIB}MiB)" "$([ "$ROOT_SIZE_MIB" -ge 8180 ] && [ "$ROOT_SIZE_MIB" -le 8200 ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 2: idempotence - running again against a card that already has
#     3 partitions must never touch the table again. ---
LOOPDEV="$(make_test_image 20480)"
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > /dev/null 2>&1
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out2.log" 2>&1
RC=$?
check "second run succeeds (exit 0)" "$([ "$RC" -eq 0 ] && echo 1 || echo 0)"
check "second run reports RESULT=already-provisioned (recognizes its own completed work)" "$(grep -q '^RESULT=already-provisioned' "${WORKDIR}/out2.log" && echo 1 || echo 0)"
check "second run leaves exactly 3 partitions (no new one created)" "$([ "$(partition_count "$LOOPDEV")" -eq 3 ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 3: an existing valid third-partition layout (simulating the
#     operational device's own pre-existing, already-in-use
#     /dev/mmcblk0p3 - genuinely formatted and labeled, unlike this
#     script's own unformatted just-after-mkpart state, so it is
#     observably distinguishable from an interrupted attempt) is never
#     touched, even on a "first" invocation. ---
LOOPDEV="$(make_test_image 20480)"
sudo parted -s "$LOOPDEV" mkpart primary ext4 1281MiB 100%
sudo partprobe "$LOOPDEV"
settle_udev
# Actually format it - parted's own "ext4" argument to mkpart is only a
# partition-table type hint, it does not create a filesystem. Using a
# different filesystem/label than this script would ever produce
# (LABEL=stratux-data) proves the fixture is a genuine foreign layout,
# not merely an unformatted partition indistinguishable from this
# script's own interrupted-before-mkfs state (that case is covered
# separately in Case 6).
sudo mkfs.ext4 -F -L foreign-data "${LOOPDEV}p3" >/dev/null # ext4 labels cap at 16 bytes
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out3.log" 2>&1
check "a pre-existing, genuinely foreign 3-partition layout is reported skipped, not modified" "$(grep -q '^RESULT=skipped-unexpected-layout' "${WORKDIR}/out3.log" && echo 1 || echo 0)"
check "pre-existing layout still has exactly 3 partitions" "$([ "$(partition_count "$LOOPDEV")" -eq 3 ] && echo 1 || echo 0)"
FOREIGN_LABEL3="$(sudo blkid -s LABEL -o value "${LOOPDEV}p3" 2>/dev/null)"
check "pre-existing foreign partition's own filesystem/label is untouched" "$([ "$FOREIGN_LABEL3" = "foreign-data" ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 4: below the minimum supported size, root grows to fill the
#     card and no data partition is created - the documented fallback.
#     The marker path is overridden to a plain WORKDIR file (never
#     mounted) rather than the real partition itself: the script resizes
#     partition 2 with `parted resizepart 2 100%`, which - like resizing
#     any other in-use block device - must never be attempted against an
#     actively mounted partition, so this test must not mount it either. ---
LOOPDEV="$(make_test_image 4096)" # 4 GiB image, well under a 16 GiB minimum
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	STRATUX_PERSISTENCE_UNSUPPORTED_MARKER="${WORKDIR}/stratux-persistence-unsupported" \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out4.log" 2>&1
check "below-minimum card reports the documented fallback result" "$(grep -q '^RESULT=grew-root-only-below-minimum-card-size' "${WORKDIR}/out4.log" && echo 1 || echo 0)"
check "below-minimum card still has exactly 2 partitions (no data partition)" "$([ "$(partition_count "$LOOPDEV")" -eq 2 ] && echo 1 || echo 0)"
check "below-minimum card writes the clear unsupported-persistence marker" "$([ -f "${WORKDIR}/stratux-persistence-unsupported" ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 5: interrupted after root resize but before partition 3 is
#     created (PART_COUNT still 2 on retry) - must safely redo the
#     (idempotent) root resize and complete provisioning, not get stuck
#     or double-shrink/corrupt anything. ---
LOOPDEV="$(make_test_image 20480)"
sudo parted -s "$LOOPDEV" resizepart 2 8449MiB # exactly what a real first attempt would have done
sudo partprobe "$LOOPDEV"
settle_udev
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out5.log" 2>&1
RC=$?
check "resuming after an interrupted root-resize-only completes successfully" "$([ "$RC" -eq 0 ] && grep -q '^RESULT=provisioned' "${WORKDIR}/out5.log" && echo 1 || echo 0)"
check "resumed provisioning still yields exactly 3 partitions" "$([ "$(partition_count "$LOOPDEV")" -eq 3 ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 6: interrupted after partition 3 is created but before mkfs
#     (the marker is already consumed by init-overlay regardless of
#     RESULT=, so this is the one remaining chance to complete it) -
#     must format it, not strand it forever as "someone else's layout". ---
LOOPDEV="$(make_test_image 20480)"
sudo parted -s "$LOOPDEV" resizepart 2 8449MiB
sudo parted -s "$LOOPDEV" mkpart primary ext4 8449MiB 100%
sudo partprobe "$LOOPDEV"
settle_udev
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out6.log" 2>&1
RC=$?
check "resuming after an interrupted mkpart-before-mkfs completes and formats it" "$([ "$RC" -eq 0 ] && grep -q '^RESULT=provisioned' "${WORKDIR}/out6.log" && echo 1 || echo 0)"
DATA_LABEL6="$(sudo blkid -s LABEL -o value "${LOOPDEV}p3" 2>/dev/null)"
check "the resumed-and-formatted partition is correctly labeled" "$([ "$DATA_LABEL6" = "stratux-data" ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 7: already fully provisioned (a repeat invocation after
#     everything already succeeded, e.g. the marker survived a reboot
#     between mkfs and init-overlay removing it) - must recognize this
#     and do nothing, never reformat and destroy existing data. ---
LOOPDEV="$(make_test_image 20480)"
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > /dev/null 2>&1
sudo mkdir -p "${WORKDIR}/mnt7"
sudo mount "${LOOPDEV}p3" "${WORKDIR}/mnt7"
echo "sentinel-must-survive" | sudo tee "${WORKDIR}/mnt7/sentinel.txt" > /dev/null
sudo umount "${WORKDIR}/mnt7"
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 FSTYPE_DETECT_RETRIES=2 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out7.log" 2>&1
check "repeat invocation on an already-provisioned card reports already-provisioned" "$(grep -q '^RESULT=already-provisioned' "${WORKDIR}/out7.log" && echo 1 || echo 0)"
sudo mount "${LOOPDEV}p3" "${WORKDIR}/mnt7"
check "repeat invocation never reformats - the sentinel file survives" "$([ -f "${WORKDIR}/mnt7/sentinel.txt" ] && echo 1 || echo 0)"
sudo umount "${WORKDIR}/mnt7"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 8: a real hardware-validation finding - filesystem-type
#     detection for the boot partition can transiently return empty very
#     early in boot (before udevd itself is even running), even though
#     the filesystem genuinely exists and is detected correctly moments
#     later. A real device was incorrectly rejected as "not vfat" by
#     this exact race under the first version of this fix (which used
#     `lsblk`, itself sourced from the udev database - unpopulated that
#     early regardless of retry count, since no daemon is running to
#     build it). The corrected fix uses `blkid -p` (direct superblock
#     probing, no udev dependency at all) with a bounded retry only on
#     the device node itself appearing. Proves blkid_probe_field actually
#     retries and succeeds, rather than merely asserting the fix exists:
#     a fake `blkid` shim placed first in PATH returns empty for
#     partition 1's TYPE on its first 2 calls, then delegates to the real
#     blkid for every other call (all other fields/devices, and this same
#     call once past its fake-failure count) - exactly simulating the
#     transient race, never a permanently absent filesystem. ---
CASE8_BINDIR="${WORKDIR}/case8-fakebin"
mkdir -p "$CASE8_BINDIR"
REAL_BLKID="$(command -v blkid)"
CASE8_COUNTER="${WORKDIR}/case8-blkid-calls"
echo 0 > "$CASE8_COUNTER"
cat > "${CASE8_BINDIR}/blkid" << EOF
#!/bin/sh
# Fakes exactly one call shape - "-p -s TYPE -o value <path ending in
# p1>" - empty for its first 2 invocations, then delegates to the real
# blkid for that same call and unconditionally for every other call
# shape.
if [ "\$1" = "-p" ] && [ "\$2" = "-s" ] && [ "\$3" = "TYPE" ] && [ "\$4" = "-o" ] && [ "\$5" = "value" ] && [ "\${6%p1}" != "\$6" ]; then
	n=\$(cat "$CASE8_COUNTER")
	n=\$((n + 1))
	echo "\$n" > "$CASE8_COUNTER"
	if [ "\$n" -le 2 ]; then
		exit 0
	fi
fi
exec "$REAL_BLKID" "\$@"
EOF
chmod +x "${CASE8_BINDIR}/blkid"

LOOPDEV="$(make_test_image 20480)"
OUT8=$(sudo env PATH="${CASE8_BINDIR}:${PATH}" \
	MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 \
	FSTYPE_DETECT_RETRIES=5 FSTYPE_DETECT_RETRY_DELAY_SECONDS=0 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" 2>&1)
RC8=$?
echo "$OUT8" > "${WORKDIR}/out8.log"
check "transient boot-partition TYPE race: retried and succeeded (exit 0)" "$([ "$RC8" -eq 0 ] && echo 1 || echo 0)"
check "transient boot-partition TYPE race: reports RESULT=provisioned, not skipped-unexpected-layout" "$(grep -q '^RESULT=provisioned' "${WORKDIR}/out8.log" && echo 1 || echo 0)"
check "transient boot-partition TYPE race: the fake blkid was actually exercised at least twice" "$([ "$(cat "$CASE8_COUNTER")" -ge 2 ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

exit $fail
