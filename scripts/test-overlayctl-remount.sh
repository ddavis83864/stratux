#!/bin/bash
#
# Regression test for a defect found during physical clean-install validation
# of stratux-ssh-hostkeys.service: overlayctl's lock/unlock/enable/disable
# actions remount /overlay/robase (a bind mount of the same block device
# init-overlay's pivot_root leaves separately mounted at /overlay/pivot -
# see docs/ota.md) without the "bind" mount option. On real hardware this
# fails "mount point is busy" (EBUSY) the moment anything tries to relock
# the overlay after unlocking it, because a plain remount targets the whole
# shared superblock rather than just the one bind-mounted view, and
# /overlay/pivot still holds a live reference to it. mount(8) documents that
# altering a bind mount's own flags requires "bind" in the remount options.
#
# This was NOT caught by scripts/test-ssh-hostkeys.sh, which stubs
# overlayctl out entirely and never performs a real mount - it only proves
# the *script's own control flow* is correct, not that the underlying OS
# mechanism it calls actually succeeds on real hardware. Physically booting
# the corrected image on a real device is what caught this (see
# docs/ssh-host-keys.md); this test captures it so it can run in CI too.
#
# Two checks:
#   1. A static invariant: every remount in overlayctl's enable/disable/
#      lock/unlock actions must include "bind" when targeting the active
#      overlay's bind-mounted base - this can never silently regress.
#   2. A functional check: actually build the same three-mount topology
#      (an ext4 image mounted read-only as "pivot", bind-mounted again as
#      "robase", with an overlay filesystem using "robase" as its lowerdir
#      - exactly init-overlay's own real structure) and run the real
#      overlayctl script's unlock/lock actions against it, using only
#      synthetic, disposable loop-mounted images. Requires sudo.
#
# Usage: scripts/test-overlayctl-remount.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OVERLAYCTL="$SCRIPT_DIR/../image_build/stage2/10-stratux/files/overlayctl"

PASS=0
FAIL=0
ok()  { PASS=$((PASS+1)); echo "PASS: $1"; }
bad() { FAIL=$((FAIL+1)); echo "FAIL: $1"; }

echo "=== test_static_invariant_ro_remounts_use_bind_rw_remounts_stay_plain ==="
# Extract the "yes" (overlay active) branch specifically - the inactive
# branch legitimately remounts plain "/" (not a bind mount) either way.
active_branch="$(awk '/overlay_active="yes"/,/^else/' "$OVERLAYCTL")"
sets_bind="$(grep -c 'overlay_remount_ro_bind="bind,"' <<<"$active_branch")"
# Exactly 3 "...ro" remounts of $overlay_base must route through the bind
# variable (enable's re-lock, disable's re-lock, and lock itself) - the 3
# "rw" remounts of $overlay_base (enable's unlock, disable's unlock, and
# unlock itself) must stay plain, since a bind remount cannot escalate a
# mount that inherited a locked-readonly flag from its still-read-only
# source. (modify_boot_cmdline's own /boot/firmware remounts are unrelated
# and correctly excluded by grepping for "overlay_base" specifically.)
# These three greps intentionally match literal variable-name strings as
# they appear in overlayctl's own source, not expand them in this shell.
# shellcheck disable=SC2016
ro_bind_sites="$(grep -c 'remount,${overlay_remount_ro_bind}ro "\$overlay_base"' "$OVERLAYCTL")"
# shellcheck disable=SC2016
plain_rw_sites="$(grep -c 'remount,rw "\$overlay_base"' "$OVERLAYCTL")"
# shellcheck disable=SC2016
bound_rw_sites="$(grep -c 'remount,${overlay_remount_ro_bind}rw' "$OVERLAYCTL")"
if [ "$sets_bind" -ge 1 ] && [ "$ro_bind_sites" -eq 3 ] && [ "$plain_rw_sites" -eq 3 ] && [ "$bound_rw_sites" -eq 0 ]; then
    ok "all 3 ro-remounts of \$overlay_base use the bind variable, all 3 rw-remounts stay plain, active branch sets bind,"
else
    bad "expected 3 bind ro-remounts + 3 plain rw-remounts + 0 bound rw-remounts of \$overlay_base (got ro_bind=$ro_bind_sites plain_rw=$plain_rw_sites bound_rw=$bound_rw_sites, active-branch-sets-bind=$sets_bind)"
fi

echo "=== test_functional_lock_unlock_roundtrip_on_real_bind_overlay_topology ==="
WORKDIR="$(mktemp -d)"
LOOPDEV=""
cleanup() {
    sudo umount "$WORKDIR/combined" 2>/dev/null || true
    sudo umount /overlay/robase 2>/dev/null || true
    sudo umount "$WORKDIR/pivot" 2>/dev/null || true
    [ -n "${OVERLAY_CREATED_BY_TEST:-}" ] && sudo rm -rf /overlay 2>/dev/null || true
    [ -n "$LOOPDEV" ] && sudo losetup -d "$LOOPDEV" 2>/dev/null || true
    rm -rf "$WORKDIR"
}
trap cleanup EXIT

mkdir -p "$WORKDIR/pivot" "$WORKDIR/upper/data" "$WORKDIR/upper/work" "$WORKDIR/combined"
dd if=/dev/zero of="$WORKDIR/fs.img" bs=1M count=32 status=none
mkfs.ext4 -q -F "$WORKDIR/fs.img"
LOOPDEV="$(sudo losetup -f --show "$WORKDIR/fs.img")"

# Fake up the two overlayctl-relevant paths at fixed, absolute locations
# via bind mounts so the real overlayctl script (which hardcodes
# /overlay/robase) can be pointed at our synthetic topology without ever
# touching the real /overlay on this machine. Skipped if /overlay already
# exists and is non-empty on the test host (defensive - never true in CI
# or a normal dev machine, but this test must never touch a real /overlay).
if [ -e /overlay ] && [ -n "$(ls -A /overlay 2>/dev/null)" ]; then
    echo "SKIP: /overlay already exists and is non-empty on this host - refusing to touch it"
    ok "skipped functional test safely (host already has a populated /overlay)"
else
    # /overlay itself is a plain local directory (never a mount) - only
    # /overlay/robase (the bind-mounted view of the real device, exactly
    # matching init-overlay's own structure) and /overlay/pivot-equivalent
    # (this test's $WORKDIR/pivot, standing in for init-overlay's real
    # /overlay/pivot) are actual mounts.
    sudo mkdir -p /overlay
    OVERLAY_CREATED_BY_TEST=1
    sudo mount -o ro "$LOOPDEV" "$WORKDIR/pivot"
    sudo mkdir -p /overlay/robase
    sudo mount --bind "$WORKDIR/pivot" /overlay/robase

    # overlay_is_active() checks for -e /overlay/robase/overlay - the
    # underlying image needs that path to exist for the script to take the
    # "active" branch at all. Must be created on the read-only pivot image
    # itself (mkfs default layout has no such directory), so do it before
    # binding read-only: remount rw briefly, create it, remount ro again.
    sudo mount -o remount,rw "$WORKDIR/pivot"
    sudo mkdir -p "$WORKDIR/pivot/overlay"
    sudo mount -o remount,ro "$WORKDIR/pivot"

    # shellcheck disable=SC2140 # intentional multi-segment concatenation
    # building one comma-separated -o argument, the same pattern
    # init-overlay's own overlay-mount line uses.
    sudo mount -t overlay -o upperdir="$WORKDIR/upper/data",workdir="$WORKDIR/upper/work",lowerdir=/overlay/robase overlay "$WORKDIR/combined"

    # This is a genuinely fresh boot state: /overlay/robase has never been
    # made writable before now, exactly like a real device's first boot -
    # the case that matters most, since a bind remount cannot escalate a
    # mount that inherited a locked-readonly flag from its still-read-only
    # source (only a plain remount can, which is why unlock stays plain).
    unlock_out="$(sudo "$OVERLAYCTL" unlock 2>&1)"
    unlock_rc=$?
    state_after_unlock="$(findmnt -no OPTIONS /overlay/robase)"

    if [ "$unlock_rc" -eq 0 ] && [[ "$state_after_unlock" == rw,* ]]; then
        ok "overlayctl unlock succeeds on a genuinely fresh (never-before-writable) mount and /overlay/robase becomes rw"
    else
        bad "overlayctl unlock failed or did not produce rw (rc=$unlock_rc, state=$state_after_unlock, output=$unlock_out)"
    fi

    # Write through the durable path, exactly as stratux-generate-ssh-hostkeys
    # does between its own unlock and lock calls.
    echo "fixture-not-a-real-key" | sudo tee /overlay/robase/testfile >/dev/null
    write_ok=$?

    lock_out="$(sudo "$OVERLAYCTL" lock 2>&1)"
    lock_rc=$?
    state_after_lock="$(findmnt -no OPTIONS /overlay/robase)"

    if [ "$lock_rc" -eq 0 ] && [[ "$state_after_lock" == ro,* ]]; then
        ok "overlayctl lock succeeds and /overlay/robase becomes ro (the exact operation that failed with EBUSY before this fix)"
    else
        bad "overlayctl lock failed or did not produce ro (rc=$lock_rc, state=$state_after_lock, output=$lock_out)"
    fi

    if [ "$write_ok" -eq 0 ] && sudo test -s /overlay/robase/testfile && [ "$(sudo cat /overlay/robase/testfile)" = "fixture-not-a-real-key" ]; then
        ok "write made between unlock and lock persists and is readable after relock"
    else
        bad "write between unlock and lock did not persist correctly"
    fi

    sudo umount "$WORKDIR/combined" 2>/dev/null || true
    sudo umount /overlay/robase 2>/dev/null || true
    [ -n "${OVERLAY_CREATED_BY_TEST:-}" ] && sudo rm -rf /overlay 2>/dev/null || true
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
