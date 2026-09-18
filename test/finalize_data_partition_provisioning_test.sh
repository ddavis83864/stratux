#!/bin/bash
# finalize_data_partition_provisioning_test.sh: exercises the real,
# unmodified /sbin/finalize-data-partition-provisioning script - the
# durable, bounded, idempotent state machine deciding whether the
# first-boot persistent-data provisioning retry marker may be removed
# yet - against plain files in a disposable temp directory (no loop
# devices needed: this script only ever touches ordinary files, never a
# block device directly).
#
# This is the fault-injection suite for the hardware-validation finding:
# a real device's own marker was removed and a reboot issued even though
# provisioning silently failed (its own log never got written because
# root was still read-only at the time), with zero durable evidence left
# behind and no bounded retry. Every case below either resumes safely or
# stops safely with durable evidence - see each check's own comment for
# which specific interruption point it covers.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FINALIZE_SCRIPT="${SCRIPT_DIR}/../image_build/stage2/10-stratux/files/finalize-data-partition-provisioning"
WORKDIR="$(mktemp -d)"
fail=0

cleanup() {
	rm -rf "$WORKDIR"
}
trap cleanup EXIT INT TERM

check() {
	local desc="$1" ok="$2"
	if [ "$ok" = "1" ]; then
		echo "PASS: $desc"
	else
		echo "FAIL: $desc"
		fail=1
	fi
}

# run_finalize: invokes the real script with a fresh, isolated set of
# env-var-overridden paths under $WORKDIR/$1, and the given RESULT_LINE
# ($2) and MAX_PROVISION_ATTEMPTS ($3, default 3). Echoes the script's
# own FINALIZE= line. All of the script's own file paths are overridden
# so no case here ever touches a real system path.
run_finalize() {
	local case_dir="${WORKDIR}/$1" result_line="$2" max_attempts="${3:-3}"
	mkdir -p "$case_dir"
	env \
		STRATUX_PROVISION_STATE_FILE="${case_dir}/state" \
		STRATUX_PROVISION_MARKER="${case_dir}/marker" \
		STRATUX_FSTAB_PATH="${case_dir}/fstab" \
		STRATUX_PERSISTENT_DATA_MOUNTPOINT="${case_dir}/mnt/stratux-data" \
		STRATUX_PERSISTENCE_UNSUPPORTED_MARKER="${case_dir}/unsupported-marker" \
		MAX_PROVISION_ATTEMPTS="$max_attempts" \
		"$FINALIZE_SCRIPT" "$result_line"
}

# --- Case 1: a clean, successful provisioning outcome - the fstab entry
#     must be written exactly once, the marker and state file removed,
#     no unsupported-persistence marker written. ---
CASE=case1
mkdir -p "${WORKDIR}/${CASE}"
touch "${WORKDIR}/${CASE}/marker"
touch "${WORKDIR}/${CASE}/fstab"
OUT=$(run_finalize "$CASE" "RESULT=provisioned DATA_PART_DEV=/dev/mmcblk0p3 ROOT_END_MIB=8449")
check "success: reports FINALIZE=success" "$([ "$OUT" = "FINALIZE=success" ] && echo 1 || echo 0)"
check "success: marker removed" "$([ ! -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"
check "success: state file removed" "$([ ! -f "${WORKDIR}/${CASE}/state" ] && echo 1 || echo 0)"
check "success: fstab entry written exactly once" "$([ "$(grep -c 'stratux-data' "${WORKDIR}/${CASE}/fstab")" -eq 1 ] && echo 1 || echo 0)"
check "success: fstab entry carries Before=stratux.service and nofail" "$(grep -q 'x-systemd.before=stratux.service' "${WORKDIR}/${CASE}/fstab" && grep -q 'nofail' "${WORKDIR}/${CASE}/fstab" && echo 1 || echo 0)"
check "success: no unsupported-persistence marker written" "$([ ! -f "${WORKDIR}/${CASE}/unsupported-marker" ] && echo 1 || echo 0)"
check "success: no leftover .tmp file (atomic write left nothing behind)" "$([ -z "$(find "${WORKDIR}/${CASE}" -name '*.tmp' 2>/dev/null)" ] && echo 1 || echo 0)"

# --- Case 2: already-provisioned (idempotent resume/repeat) - a
#     pre-existing fstab entry must never be duplicated. ---
CASE=case2
mkdir -p "${WORKDIR}/${CASE}"
touch "${WORKDIR}/${CASE}/marker"
echo "LABEL=stratux-data  ${WORKDIR}/${CASE}/mnt/stratux-data  ext4  defaults,noatime,nofail,x-systemd.device-timeout=5s,x-systemd.before=stratux.service  0  2" > "${WORKDIR}/${CASE}/fstab"
OUT=$(run_finalize "$CASE" "RESULT=already-provisioned DATA_PART_DEV=/dev/mmcblk0p3")
check "already-provisioned: reports FINALIZE=success" "$([ "$OUT" = "FINALIZE=success" ] && echo 1 || echo 0)"
check "already-provisioned: fstab entry not duplicated" "$([ "$(grep -c 'stratux-data' "${WORKDIR}/${CASE}/fstab")" -eq 1 ] && echo 1 || echo 0)"
check "already-provisioned: marker removed" "$([ ! -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"

# --- Case 3: below-minimum-card-size fallback - success, but no fstab
#     entry is expected (no data partition exists in this branch). ---
CASE=case3
mkdir -p "${WORKDIR}/${CASE}"
touch "${WORKDIR}/${CASE}/marker"
touch "${WORKDIR}/${CASE}/fstab"
OUT=$(run_finalize "$CASE" "RESULT=grew-root-only-below-minimum-card-size")
check "below-minimum: reports FINALIZE=success" "$([ "$OUT" = "FINALIZE=success" ] && echo 1 || echo 0)"
check "below-minimum: marker removed" "$([ ! -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"
check "below-minimum: no fstab entry written (no data partition exists)" "$([ ! -s "${WORKDIR}/${CASE}/fstab" ] && echo 1 || echo 0)"

# --- Case 4: unexpected/custom layout - must NEVER be retried, even on
#     the very first attempt, since nothing about the layout changes
#     between reboots. Exhausted immediately, with a clear durable
#     reason. ---
CASE=case4
mkdir -p "${WORKDIR}/${CASE}"
touch "${WORKDIR}/${CASE}/marker"
OUT=$(run_finalize "$CASE" "RESULT=skipped-unexpected-layout" 3)
check "unexpected layout: reports FINALIZE=exhausted on the very first attempt" "$([ "$OUT" = "FINALIZE=exhausted" ] && echo 1 || echo 0)"
check "unexpected layout: marker removed (never retried)" "$([ ! -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"
check "unexpected layout: unsupported-persistence marker written" "$([ -s "${WORKDIR}/${CASE}/unsupported-marker" ] && echo 1 || echo 0)"
check "unexpected layout: reason mentions the unexpected layout, not a generic message" "$(grep -q 'skipped-unexpected-layout' "${WORKDIR}/${CASE}/unsupported-marker" && echo 1 || echo 0)"
# The state file is intentionally removed once a terminal outcome
# (success or permanent failure) is reached - nothing is left to resume,
# and the unsupported-persistence marker (checked above) already carries
# the durable, human-readable reason for the terminal case.
check "unexpected layout: state file removed (terminal outcome, nothing left to resume)" "$([ ! -f "${WORKDIR}/${CASE}/state" ] && echo 1 || echo 0)"

# --- Case 5: the exact hardware-observed failure mode - an empty
#     RESULT_LINE (provision-data-partition's own invocation produced no
#     parseable result, e.g. because its log never got written). This
#     must be BOUNDED-RETRIED, not treated as permanent immediately -
#     the underlying cause may be transient. Simulates "on the next boot
#     after each partial state" by re-invoking against the SAME case
#     directory (the marker/state files left behind by the previous
#     call ARE the resumption state a real reboot would see). ---
CASE=case5
mkdir -p "${WORKDIR}/${CASE}"
touch "${WORKDIR}/${CASE}/marker"
MAX=3

OUT=$(run_finalize "$CASE" "" "$MAX")
check "empty result, attempt 1: reports FINALIZE=retry (budget not exhausted)" "$([ "$OUT" = "FINALIZE=retry" ] && echo 1 || echo 0)"
check "empty result, attempt 1: marker PRESERVED, not removed" "$([ -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"
check "empty result, attempt 1: state file records ATTEMPTS=1" "$(grep -q '^ATTEMPTS=1$' "${WORKDIR}/${CASE}/state" 2>/dev/null && echo 1 || echo 0)"
check "empty result, attempt 1: state file records failed-retryable" "$(grep -q '^STAGE=failed-retryable$' "${WORKDIR}/${CASE}/state" 2>/dev/null && echo 1 || echo 0)"
check "empty result, attempt 1: no unsupported-persistence marker yet (retryable, not final)" "$([ ! -f "${WORKDIR}/${CASE}/unsupported-marker" ] && echo 1 || echo 0)"

OUT=$(run_finalize "$CASE" "" "$MAX")
check "empty result, attempt 2 (simulated next boot): still FINALIZE=retry" "$([ "$OUT" = "FINALIZE=retry" ] && echo 1 || echo 0)"
check "empty result, attempt 2: marker STILL preserved" "$([ -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"
check "empty result, attempt 2: state file records ATTEMPTS=2 (resumed from the prior attempt's own persisted count)" "$(grep -q '^ATTEMPTS=2$' "${WORKDIR}/${CASE}/state" 2>/dev/null && echo 1 || echo 0)"

OUT=$(run_finalize "$CASE" "" "$MAX")
check "empty result, attempt 3 (budget exhausted): reports FINALIZE=exhausted" "$([ "$OUT" = "FINALIZE=exhausted" ] && echo 1 || echo 0)"
check "empty result, attempt 3: marker finally removed (no more retries)" "$([ ! -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"
check "empty result, attempt 3: unsupported-persistence marker now written" "$([ -s "${WORKDIR}/${CASE}/unsupported-marker" ] && echo 1 || echo 0)"
check "empty result, attempt 3: reason mentions the attempt count" "$(grep -q '3 attempt' "${WORKDIR}/${CASE}/unsupported-marker" && echo 1 || echo 0)"
check "empty result, attempt 3: state file removed (terminal outcome, nothing left to resume)" "$([ ! -f "${WORKDIR}/${CASE}/state" ] && echo 1 || echo 0)"

# --- Case 6: LAST_ERROR is bounded and sanitized - a long, multi-line,
#     adversarial RESULT_LINE must never blow up the state file or leak
#     more than a short, single-line summary. ---
CASE=case6
mkdir -p "${WORKDIR}/${CASE}"
touch "${WORKDIR}/${CASE}/marker"
LONG_MULTILINE_GARBAGE="line one of garbage
line two of garbage
$(head -c 1000 < /dev/zero | tr '\0' 'x')"
run_finalize "$CASE" "$LONG_MULTILINE_GARBAGE" 1 > /dev/null
LAST_ERROR_LINE="$(grep '^LAST_ERROR=' "${WORKDIR}/${CASE}/unsupported-marker" 2>/dev/null; grep -c $'\n' "${WORKDIR}/${CASE}/unsupported-marker" 2>/dev/null)"
UNSUPPORTED_LINE_COUNT="$(wc -l < "${WORKDIR}/${CASE}/unsupported-marker" 2>/dev/null || echo 0)"
UNSUPPORTED_LEN="$(wc -c < "${WORKDIR}/${CASE}/unsupported-marker" 2>/dev/null || echo 0)"
check "sanitized error: unsupported-marker is exactly one line (no embedded newlines)" "$([ "$UNSUPPORTED_LINE_COUNT" -eq 1 ] && echo 1 || echo 0)"
check "sanitized error: unsupported-marker is bounded, not unboundedly long" "$([ "$UNSUPPORTED_LEN" -lt 600 ] && echo 1 || echo 0)"

# --- Case 7: MAX_PROVISION_ATTEMPTS=1 - proves the bound is actually
#     configurable and enforced from the very first attempt when set
#     that low (a degenerate but valid bound). ---
CASE=case7
mkdir -p "${WORKDIR}/${CASE}"
touch "${WORKDIR}/${CASE}/marker"
OUT=$(run_finalize "$CASE" "" 1)
check "MAX_PROVISION_ATTEMPTS=1: exhausts on the very first attempt" "$([ "$OUT" = "FINALIZE=exhausted" ] && echo 1 || echo 0)"
check "MAX_PROVISION_ATTEMPTS=1: marker removed" "$([ ! -f "${WORKDIR}/${CASE}/marker" ] && echo 1 || echo 0)"

exit $fail
