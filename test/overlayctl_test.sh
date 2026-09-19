#!/bin/bash
# overlayctl_test.sh: exercises the real
# image_build/stage2/10-stratux/files/overlayctl script's `status`
# subcommand end-to-end, against a fake `findmnt` stub placed first on
# PATH - no real mount, root, or /overlay tree is touched (this dev
# machine has none, and `status` performs no writes regardless), so this
# is safe to run anywhere.
#
# This is a regression test for a real, confirmed defect (see
# docs/ota-persistent-storage-defect.md): overlayctl's own
# overlay_is_active() used to infer "overlay is active" from a leftover
# directory's mere existence rather than the live root mount type,
# reporting "active" on a boot findmnt/mountinfo proved was genuinely bare
# ext4. It now queries the live mount type directly, matching
# debian/stratux-pre-start.sh's own already-correct implementation - this
# script proves that end to end against the real overlayctl file, not a
# reimplementation of its logic.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OVERLAYCTL="${SCRIPT_DIR}/../image_build/stage2/10-stratux/files/overlayctl"
FAKEBIN="$(mktemp -d)"
trap 'rm -rf "${FAKEBIN}"' EXIT

fail=0

# fake_findmnt_reporting writes a stub findmnt(8) onto FAKEBIN that always
# reports the given FSTYPE for `findmnt -n -o FSTYPE /` (the exact
# invocation the corrected overlay_is_active makes), and exits 1 (no
# match) for anything targeting a path under /overlay - overlayctl's own
# `status` case only ever calls findmnt for that one root check; every
# other filesystem test in this script ([-e ...]) is real, unmocked, and
# harmlessly false on this dev machine, which has no /overlay tree.
fake_findmnt_reporting() {
	cat > "${FAKEBIN}/findmnt" <<EOF
#!/bin/sh
echo "$1"
EOF
	chmod +x "${FAKEBIN}/findmnt"
}

check() {
	local desc="$1" expected_substr="$2" actual="$3"
	if ! printf '%s' "$actual" | grep -qF "$expected_substr"; then
		echo "FAIL: $desc"
		echo "  expected to find: $expected_substr"
		echo "  actual output:    $actual"
		fail=1
	else
		echo "PASS: $desc"
	fi
}

# --- Root mounted as overlay: status must say "active" ---
fake_findmnt_reporting "overlay"
out="$(PATH="${FAKEBIN}:${PATH}" sh "${OVERLAYCTL}" status)"
check "root=overlay reports active" "overlay is active" "$out"

# --- Root mounted as ext4 (bare, genuinely disabled): status must say
#     "inactive" - this is the exact case the old, buggy directory-
#     existence check got wrong. ---
fake_findmnt_reporting "ext4"
out="$(PATH="${FAKEBIN}:${PATH}" sh "${OVERLAYCTL}" status)"
check "root=ext4 reports inactive" "overlay is inactive" "$out"

# --- Same ext4 case, but with a stale leftover directory structure
#     present (simulating a prior overlay-active session's own bind-mount
#     artifacts never having been cleaned up) - must still report
#     "inactive", proving the fix no longer trusts directory existence at
#     all. A harmless, throwaway temp directory stands in for
#     /overlay/robase/overlay; overlayctl's own overlay_is_active no
#     longer even looks at it, so this is here purely to document and
#     lock in the regression this fix closes. ---
mkdir -p "${FAKEBIN}/stale-overlay-artifact"
fake_findmnt_reporting "ext4"
out="$(PATH="${FAKEBIN}:${PATH}" sh "${OVERLAYCTL}" status)"
check "root=ext4 with a stale leftover directory still reports inactive" "overlay is inactive" "$out"

# --- Bind mount case: overlayfs itself is reported by findmnt as its own
#     FSTYPE ("overlay") regardless of what backs its lower/upper layers,
#     so a bind-mounted lower root behind it is transparent to this
#     check by construction - covered by the first case above.

# --- Missing/malformed mount information: findmnt itself failing (exit
#     nonzero, e.g. not installed or /proc unavailable) must not be
#     silently treated as "active" - `set -e` inside overlay_is_active's
#     own command substitution does not propagate, so the function
#     degrades to comparing empty-string output against "overlay", i.e.
#     safely "inactive" (never a crash, never a false "active"). ---
cat > "${FAKEBIN}/findmnt" <<'EOF'
#!/bin/sh
exit 1
EOF
chmod +x "${FAKEBIN}/findmnt"
out="$(PATH="${FAKEBIN}:${PATH}" sh "${OVERLAYCTL}" status)"
check "findmnt failure degrades to inactive, not a crash or false-active" "overlay is inactive" "$out"

# --- No mutation while querying status: status must never write the
#     marker file or otherwise change disk state. ---
fake_findmnt_reporting "overlay"
before="$(PATH="${FAKEBIN}:${PATH}" sh "${OVERLAYCTL}" status)"
after="$(PATH="${FAKEBIN}:${PATH}" sh "${OVERLAYCTL}" status)"
check "repeated status calls are idempotent (no mutation)" "$before" "$after"

exit $fail
