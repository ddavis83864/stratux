#!/bin/bash
#
# Tests for image_build/stage2/10-stratux/files/stratux-generate-ssh-hostkeys,
# the first-boot SSH host key generator (see docs/ssh-host-keys.md).
#
# Every test runs the real script against a throwaway temp directory via its
# STRATUX_TEST_ROOT/STRATUX_TEST_FORCE_OVERLAY/STRATUX_TEST_OVERLAYCTL test
# hooks (unset in every real invocation - see the script's own comments).
# No test ever reads, generates near, or references any real device's SSH
# host keys; every key here is a disposable fixture created fresh in
# mktemp -d and deleted at exit. No test prints private key contents.
#
# Usage: scripts/test-ssh-hostkeys.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GEN_SCRIPT="$SCRIPT_DIR/../image_build/stage2/10-stratux/files/stratux-generate-ssh-hostkeys"

PASS=0
FAIL=0
WORKDIRS=()

cleanup() {
    for d in "${WORKDIRS[@]}"; do
        rm -rf "$d"
    done
}
trap cleanup EXIT

newroot() {
    local d
    d="$(mktemp -d)"
    WORKDIRS+=("$d")
    mkdir -p "$d/etc/ssh" "$d/overlay/robase/etc/ssh"
    echo "$d"
}

# Guards against this test suite itself ever leaking key material: every
# assertion below inspects size/permissions/fingerprints only, but this is
# a hard backstop - if any private key marker ever appears in this script's
# own captured output, that is itself a failure, independent of anything
# else being tested.
assert_no_secret_material() {
    local text="$1"
    if grep -q "PRIVATE KEY" <<<"$text"; then
        echo "  ANOMALY: private key marker text found in captured test output"
        return 1
    fi
    return 0
}

ok()   { PASS=$((PASS+1)); echo "PASS: $1"; }
bad()  { FAIL=$((FAIL+1)); echo "FAIL: $1"; }

run_gen() {
    # $1: root dir. $2: 0 or 1 for STRATUX_TEST_FORCE_OVERLAY. $3 (optional):
    # overlayctl stub path.
    local root="$1" overlay="$2" stub="${3:-}"
    STRATUX_TEST_ROOT="$root" \
    STRATUX_TEST_FORCE_OVERLAY="$overlay" \
    STRATUX_TEST_OVERLAYCTL="${stub:-/bin/true}" \
        "$GEN_SCRIPT"
}

echo "=== test_first_boot_generates_full_set (non-overlay) ==="
r="$(newroot)"
out="$(run_gen "$r" 0 2>&1)"
assert_no_secret_material "$out" && ok "no secret material in captured output" || bad "secret material leaked in output"
if [ -s "$r/etc/ssh/ssh_host_rsa_key" ] && [ -s "$r/etc/ssh/ssh_host_ecdsa_key" ] && [ -s "$r/etc/ssh/ssh_host_ed25519_key" ] \
   && [ -s "$r/etc/ssh/ssh_host_rsa_key.pub" ] && [ -s "$r/etc/ssh/ssh_host_ecdsa_key.pub" ] && [ -s "$r/etc/ssh/ssh_host_ed25519_key.pub" ]; then
    ok "first boot generates full key set (3 private + 3 public, all non-empty)"
else
    bad "first boot did not generate a full key set"
fi

echo "=== test_permissions ==="
priv_ok=1
for f in ssh_host_rsa_key ssh_host_ecdsa_key ssh_host_ed25519_key; do
    mode="$(stat -c %a "$r/etc/ssh/$f")"
    [ "$mode" = "600" ] || priv_ok=0
done
pub_ok=1
for f in ssh_host_rsa_key.pub ssh_host_ecdsa_key.pub ssh_host_ed25519_key.pub; do
    mode="$(stat -c %a "$r/etc/ssh/$f")"
    [ "$mode" = "644" ] || pub_ok=0
done
[ "$priv_ok" = 1 ] && ok "private key files are mode 600" || bad "a private key file is not mode 600"
[ "$pub_ok" = 1 ] && ok "public key files are mode 644" || bad "a public key file is not mode 644"

echo "=== test_second_boot_does_not_replace ==="
before_hash="$(sha256sum "$r"/etc/ssh/ssh_host_*_key | sort)"
before_mtime="$(stat -c %Y "$r/etc/ssh/ssh_host_ed25519_key")"
sleep 1
run_gen "$r" 0 >/dev/null 2>&1
after_hash="$(sha256sum "$r"/etc/ssh/ssh_host_*_key | sort)"
after_mtime="$(stat -c %Y "$r/etc/ssh/ssh_host_ed25519_key")"
if [ "$before_hash" = "$after_hash" ] && [ "$before_mtime" = "$after_mtime" ]; then
    ok "second boot leaves existing keys byte-identical and untouched (mtime unchanged)"
else
    bad "second boot modified an existing key set"
fi

echo "=== test_interrupted_partial_generation_recovered ==="
r2="$(newroot)"
# Simulate a prior run that was interrupted after generating only the
# ed25519 key: seed a fixture (never real key material) for just that type.
echo "fixture-not-a-real-key" > "$r2/etc/ssh/ssh_host_ed25519_key"
chmod 600 "$r2/etc/ssh/ssh_host_ed25519_key"
partial_before="$(cat "$r2/etc/ssh/ssh_host_ed25519_key")"
run_gen "$r2" 0 >/dev/null 2>&1
partial_after="$(cat "$r2/etc/ssh/ssh_host_ed25519_key")"
if [ "$partial_before" = "$partial_after" ] \
   && [ -s "$r2/etc/ssh/ssh_host_rsa_key" ] && [ -s "$r2/etc/ssh/ssh_host_ecdsa_key" ]; then
    ok "interrupted/partial key set is completed without touching the existing partial key"
else
    bad "partial-generation recovery did not behave as expected"
fi

echo "=== test_two_independent_instances_produce_different_fingerprints ==="
r3="$(newroot)"
r4="$(newroot)"
run_gen "$r3" 0 >/dev/null 2>&1
run_gen "$r4" 0 >/dev/null 2>&1
fp3="$(ssh-keygen -lf "$r3/etc/ssh/ssh_host_ed25519_key.pub" | awk '{print $2}')"
fp4="$(ssh-keygen -lf "$r4/etc/ssh/ssh_host_ed25519_key.pub" | awk '{print $2}')"
if [ -n "$fp3" ] && [ "$fp3" != "$fp4" ]; then
    ok "two independently generated instances have different fingerprints ($fp3 != $fp4)"
else
    bad "two independent instances produced the same fingerprint - not unique per install"
fi

echo "=== test_overlay_active_writes_through_robase_and_calls_unlock_lock_in_order ==="
r5="$(newroot)"
stub="$r5/overlayctl-stub"
callog="$r5/overlayctl-calls.log"
cat > "$stub" <<STUB
#!/bin/sh
echo "\$1" >> "$callog"
exit 0
STUB
chmod +x "$stub"
run_gen "$r5" 1 "$stub" >/dev/null 2>&1
calls="$(cat "$callog" 2>/dev/null | tr '\n' ',' )"
if [ "$calls" = "unlock,lock," ]; then
    ok "overlay-active path calls overlayctl unlock then lock, in order"
else
    bad "overlay-active path called overlayctl in the wrong order or not at all (got: $calls)"
fi
if [ -s "$r5/overlay/robase/etc/ssh/ssh_host_ed25519_key" ] && [ ! -e "$r5/etc/ssh/ssh_host_ed25519_key" ]; then
    ok "overlay-active path writes keys through /overlay/robase, not the ephemeral /etc path"
else
    bad "overlay-active path did not write keys to the persistent robase location"
fi

echo "=== test_overlay_active_second_boot_never_unlocks ==="
rm -f "$callog"
run_gen "$r5" 1 "$stub" >/dev/null 2>&1
if [ ! -s "$callog" ]; then
    ok "overlay-active path never calls overlayctl again once a complete key set already exists"
else
    bad "overlay-active path unnecessarily unlocked the overlay on an already-initialized boot"
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]
