#!/bin/bash
#
# Tests for scripts/verify-no-host-keys.sh, the release-pipeline check that
# fails a build if a clean image ships SSH host private key material.
#
# Builds two small, throwaway, synthetic disk images (standard DOS
# partition table, FAT32 boot + ext4 root, matching the real image
# layout) - one with a deliberately injected FIXTURE key (not real key
# material, just distinguishable non-secret bytes) at the host-key path,
# one without - and confirms the checker fails on the first and passes on
# the second. Also confirms a user authorized_keys file is never flagged.
# Requires sudo (loop-mount); uses only synthetic, disposable images.
#
# Usage: scripts/test-verify-no-host-keys.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="$SCRIPT_DIR/verify-no-host-keys.sh"

PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL: $1"; }

WORKDIR="$(mktemp -d)"
LOOPDEV=""
cleanup() {
    if [ -n "$LOOPDEV" ]; then
        sudo umount "$WORKDIR/mnt" 2>/dev/null || true
        sudo losetup -d "$LOOPDEV" 2>/dev/null || true
    fi
    rm -rf "$WORKDIR"
}
trap cleanup EXIT
mkdir -p "$WORKDIR/mnt"

build_image() {
    # $1: output path. $2: "with_key" or "without_key".
    local img="$1" mode="$2"
    dd if=/dev/zero of="$img" bs=1M count=64 status=none
    sudo parted -s "$img" mklabel msdos mkpart primary fat32 1MiB 9MiB mkpart primary ext4 9MiB 100%

    local loopdev
    loopdev="$(sudo losetup -fP --show "$img")"
    sudo mkfs.vfat -n BOOT "${loopdev}p1" >/dev/null
    sudo mkfs.ext4 -q -F "${loopdev}p2"

    sudo mount "${loopdev}p2" "$WORKDIR/mnt"
    sudo mkdir -p "$WORKDIR/mnt/etc/ssh" "$WORKDIR/mnt/root/.ssh"
    if [ "$mode" = "with_key" ]; then
        # A deliberately obvious, non-secret FIXTURE string - never real
        # key material - injected purely to prove the checker's detection
        # logic actually fires on a populated host-key path.
        echo "FIXTURE-NOT-A-REAL-SSH-HOST-KEY-$(date +%s)" | sudo tee "$WORKDIR/mnt/etc/ssh/ssh_host_rsa_key" >/dev/null
        sudo chmod 600 "$WORKDIR/mnt/etc/ssh/ssh_host_rsa_key"
    fi
    # In both modes, plant an unrelated user authorized_keys file - proves
    # the checker distinguishes host keys from user client-access keys and
    # never flags this.
    echo "ssh-ed25519 AAAAFIXTURE fixture@test" | sudo tee "$WORKDIR/mnt/root/.ssh/authorized_keys" >/dev/null
    sudo umount "$WORKDIR/mnt"
    sudo losetup -d "$loopdev"
}

echo "=== building synthetic test images (no real key material) ==="
build_image "$WORKDIR/with_key.img" with_key
build_image "$WORKDIR/without_key.img" without_key

echo "=== test_detects_injected_host_key (should FAIL) ==="
if "$CHECKER" "$WORKDIR/with_key.img" >"$WORKDIR/out1.log" 2>&1; then
    bad "checker passed an image with an injected host key - should have failed"
else
    if grep -q "ssh_host_rsa_key" "$WORKDIR/out1.log"; then
        ok "checker correctly fails and identifies the injected host key"
    else
        bad "checker failed but did not identify which key - output: $(cat "$WORKDIR/out1.log")"
    fi
fi

echo "=== test_passes_clean_image (should PASS) ==="
if "$CHECKER" "$WORKDIR/without_key.img" >"$WORKDIR/out2.log" 2>&1; then
    ok "checker correctly passes a clean image with no host key material"
else
    bad "checker failed a genuinely clean image - output: $(cat "$WORKDIR/out2.log")"
fi

echo "=== test_never_flags_user_authorized_keys ==="
if grep -qi "authorized_keys" "$WORKDIR/out1.log" "$WORKDIR/out2.log"; then
    bad "checker output mentions authorized_keys - it must only ever judge host keys"
else
    ok "checker output never references the unrelated user authorized_keys file"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
