#!/bin/bash -e
#
# Fails (non-zero exit) if a clean-install Stratux image contains SSH HOST
# private key material. A public, downloadable clean-install image must
# never ship pre-generated server host keys - every device must generate
# its own on first boot (see stratux-ssh-hostkeys.service and
# docs/known-limitations.md for the full history of this defect).
#
# This checks only SSH *host* keys (server identity: /etc/ssh/ssh_host_*).
# It never inspects, flags, or otherwise judges SSH *user* authorized_keys
# files (client access grants) - those are an entirely separate, unrelated
# mechanism and this script must not conflate the two.
#
# Usage: verify-no-host-keys.sh <path-to-raw-.img>
# Requires: sudo (loop-mounts the image read-only), losetup, mount.

set -euo pipefail

if [ "${1:-}" = "" ] || [ ! -f "$1" ]; then
    echo "usage: $0 <raw .img file>" >&2
    exit 2
fi
IMG="$1"

MNT="$(mktemp -d)"
LOOPDEV=""

cleanup() {
    if mountpoint -q "$MNT" 2>/dev/null; then
        sudo umount "$MNT" || true
    fi
    if [ -n "$LOOPDEV" ]; then
        sudo losetup -d "$LOOPDEV" || true
    fi
    rmdir "$MNT" 2>/dev/null || true
}
trap cleanup EXIT

LOOPDEV="$(sudo losetup -fP --show "$IMG")"
# Standard pi-gen layout: p1 = FAT32 boot, p2 = ext4 root. Root is what can
# ever contain /etc/ssh/ssh_host_*.
sudo mount -o ro "${LOOPDEV}p2" "$MNT"

FOUND=0
for keytype in rsa dsa ecdsa ed25519; do
    keyfile="$MNT/etc/ssh/ssh_host_${keytype}_key"
    if sudo test -s "$keyfile"; then
        echo "FAIL: found a pre-generated SSH HOST private key baked into the clean image: etc/ssh/ssh_host_${keytype}_key" >&2
        echo "      clean-install images must never ship reusable host key material - keys must be generated per-device on first boot." >&2
        FOUND=1
    fi
done

if [ "$FOUND" -ne 0 ]; then
    echo "SSH host key sanitization check: FAILED" >&2
    exit 1
fi

echo "SSH host key sanitization check: OK (no host private key material found)"
