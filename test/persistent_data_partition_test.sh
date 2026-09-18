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

# --- Case 1: a card at/above the minimum size gets bounded root growth
#     plus a real, correctly-labeled ext4 data partition. ---
LOOPDEV="$(make_test_image 20480)" # 20 GiB image
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 \
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
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > /dev/null 2>&1
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out2.log" 2>&1
RC=$?
check "second run succeeds (exit 0)" "$([ "$RC" -eq 0 ] && echo 1 || echo 0)"
check "second run reports RESULT=skipped-unexpected-layout (already 3 partitions)" "$(grep -q '^RESULT=skipped-unexpected-layout' "${WORKDIR}/out2.log" && echo 1 || echo 0)"
check "second run leaves exactly 3 partitions (no new one created)" "$([ "$(partition_count "$LOOPDEV")" -eq 3 ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 3: an existing valid third-partition layout (simulating the
#     operational device's own pre-existing /dev/mmcblk0p3) is never
#     touched, even on a "first" invocation. ---
LOOPDEV="$(make_test_image 20480)"
sudo parted -s "$LOOPDEV" mkpart primary ext4 1281MiB 100%
sudo partprobe "$LOOPDEV"
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out3.log" 2>&1
check "a pre-existing 3-partition layout is reported skipped, not modified" "$(grep -q '^RESULT=skipped-unexpected-layout' "${WORKDIR}/out3.log" && echo 1 || echo 0)"
check "pre-existing layout still has exactly 3 partitions" "$([ "$(partition_count "$LOOPDEV")" -eq 3 ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

# --- Case 4: below the minimum supported size, root grows to fill the
#     card and no data partition is created - the documented fallback. ---
LOOPDEV="$(make_test_image 4096)" # 4 GiB image, well under a 16 GiB minimum
sudo env MIN_CARD_BYTES=$((16*1024*1024*1024)) ROOT_CAP_MIB=8192 \
	"$PROVISION_SCRIPT" "$LOOPDEV" "${LOOPDEV}p2" > "${WORKDIR}/out4.log" 2>&1
check "below-minimum card reports the documented fallback result" "$(grep -q '^RESULT=grew-root-only-below-minimum-card-size' "${WORKDIR}/out4.log" && echo 1 || echo 0)"
check "below-minimum card still has exactly 2 partitions (no data partition)" "$([ "$(partition_count "$LOOPDEV")" -eq 2 ] && echo 1 || echo 0)"
sudo losetup -d "$LOOPDEV"
LOOPDEV=""

exit $fail
