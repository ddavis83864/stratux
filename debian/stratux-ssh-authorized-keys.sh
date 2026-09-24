#!/bin/bash
#
# stratux-ssh-authorized-keys.sh: restore the administrator's persistent SSH
# public keys into the volatile home directory. Run at boot by
# stratux_ssh_authorized_keys.service (before ssh.service) and on demand by
# `systemctl restart stratux_ssh_authorized_keys.service`.
#
# Why this exists: root is a protected overlay whose writable layer is RAM
# (tmpfs), so anything written to /home/pi/.ssh/authorized_keys is discarded
# at every reboot (docs/ssh-authorized-keys.md). The authoritative copy of the
# keys therefore lives on the dedicated persistent data partition:
#
#   /var/lib/stratux-data/ssh/authorized_keys      (root-owned; the source)
#   /home/pi/.ssh/authorized_keys                  (volatile; the target)
#
# Semantics:
#   - No source file (or data partition not mounted): a normal no-op. Nothing
#     is created and any keys already in the target are left alone.
#   - Source present and valid: it is authoritative. The target is replaced
#     with exactly its contents (so removing a key from the source and
#     re-running revokes it, no reboot needed).
#   - Source present but unsafe (ownership/permissions/type) or containing
#     malformed key material: rejected as a whole, the existing target is
#     left untouched, a diagnostic is logged, and the exit status is non-zero.
#     This never blocks sshd (the unit has no dependency in either direction).
#   - A zero-byte source is ignored with a warning: it is far more likely a
#     truncated write than an intentional "revoke everything". To revoke all
#     keys on purpose, leave a comment line (e.g. "# no keys") in the file.
#
# The source is opened ONCE and every safety check is made on that open file
# descriptor, then the content is read from the same descriptor, so the file
# that was validated is the file that is installed. Keys are validated per
# entry with the platform's own `ssh-keygen -l`, which understands the full
# authorized_keys syntax including leading options; option NAMES are not
# validated (sshd itself skips an entry with bad options at login time).
# Public keys are never printed, only their SHA256 fingerprints.
#
# The target is installed atomically: written to a private temporary file in
# ~/.ssh with the final owner and mode, then renamed into place. A failure at
# any step leaves the previous target intact.
#
# This script does not touch sshd_config, host keys, StrictModes or any other
# SSH policy, and never handles private keys (a source containing private key
# material is rejected).
#
# Test hooks: honored ONLY when STRATUX_SSH_KEYS_TESTING=1 (never set by the
# unit), so a stray environment variable cannot redirect a real run.
#   STRATUX_TEST_ROOT            prefix for the data mount and lock file
#   STRATUX_TEST_HOME_ROOT       prefix for the target home directory
#   STRATUX_TEST_SRC_UID         uid the source must be owned by (default 0)
#   STRATUX_TEST_TARGET_USER     user to install the key for (default pi)
#   STRATUX_TEST_SKIP_MOUNTCHECK 1 = do not require a real mount point

set -u
umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C

TAG="stratux-ssh-authorized-keys"
DATA_MOUNT=/var/lib/stratux-data
LOCK=/run/stratux-ssh-authorized-keys.lock
TARGET_USER=pi
HOME_ROOT=
SRC_UID=0
SKIP_MOUNTCHECK=

if [ "${STRATUX_SSH_KEYS_TESTING:-}" = 1 ]; then
	DATA_MOUNT="${STRATUX_TEST_ROOT:-}${DATA_MOUNT}"
	LOCK="${STRATUX_TEST_ROOT:-}${LOCK}"
	HOME_ROOT="${STRATUX_TEST_HOME_ROOT:-}"
	SRC_UID="${STRATUX_TEST_SRC_UID:-0}"
	TARGET_USER="${STRATUX_TEST_TARGET_USER:-pi}"
	SKIP_MOUNTCHECK="${STRATUX_TEST_SKIP_MOUNTCHECK:-}"
fi

# Bounds: a handful of keys is normal; refuse anything absurd rather than
# spend boot time on it.
MAX_BYTES=65536
MAX_ENTRIES=256
KEYGEN_TIMEOUT=5
# Overall time budget for validation, counted from after the lock is taken. It
# must stay well inside the unit's TimeoutStartSec (asserted by a test), so this
# script always ends itself, changing nothing, before systemd would kill it.
BUDGET_SECONDS=10
LOCK_WAIT_SECONDS=5

SRC_DIR="${DATA_MOUNT}/ssh"
SRC="${SRC_DIR}/authorized_keys"

info() { echo "$TAG: $*"; }
warn() { echo "$TAG: WARNING: $*" >&2; }
fail() { echo "$TAG: ERROR: $*" >&2; exit 1; }

WORK=
TMPTARGET=
cleanup() {
	[ -n "$TMPTARGET" ] && rm -f -- "$TMPTARGET"
	[ -n "$WORK" ] && rm -rf -- "$WORK"
}
trap cleanup EXIT

# Serialize with any concurrent run (boot vs. a manual restart).
if command -v flock >/dev/null 2>&1; then
	mkdir -p -- "$(dirname -- "$LOCK")" 2>/dev/null
	exec 9>"$LOCK" && flock -w "$LOCK_WAIT_SECONDS" 9 || warn "could not take $LOCK; continuing"
fi
SECONDS=0

# --- 1. Is there anything to do? -------------------------------------------
# The persistent copy is only trusted when it really comes from the dedicated
# data partition mount, never from a same-named directory in the RAM overlay.
if [ -z "$SKIP_MOUNTCHECK" ] && ! mountpoint -q -- "$DATA_MOUNT"; then
	info "persistent data partition is not mounted at $DATA_MOUNT; nothing to restore"
	exit 0
fi
if [ ! -e "$SRC" ] && [ ! -L "$SRC" ]; then
	info "no persistent SSH key file at $SRC; nothing to restore"
	exit 0
fi

# --- 2. Is the source safe to trust? ---------------------------------------
# Modes are compared as octal; anything writable by group or other is unsafe
# because it would let a non-root user choose who may log in.
unsafe_mode() { [ $(( 8#$1 & 8#022 )) -ne 0 ]; }

if [ -L "$SRC_DIR" ] || [ ! -d "$SRC_DIR" ]; then
	fail "$SRC_DIR is not a plain directory; refusing to use $SRC"
fi
read -r dir_uid dir_mode < <(stat -c '%u %a' -- "$SRC_DIR") || fail "cannot stat $SRC_DIR"
[ "$dir_uid" = "$SRC_UID" ] || fail "$SRC_DIR is owned by uid $dir_uid, expected $SRC_UID; refusing to use $SRC"
unsafe_mode "$dir_mode" && fail "$SRC_DIR has unsafe mode $dir_mode (group/world writable); refusing to use $SRC"

[ -L "$SRC" ] && fail "$SRC is a symbolic link; refusing to follow it"
[ -f "$SRC" ] || fail "$SRC is not a regular file; refusing to use it"

# Open once, then check the open descriptor (not the path) and read from it.
exec 3<"$SRC" || fail "cannot open $SRC"
FD=/proc/self/fd/3
read -r fd_id fd_uid fd_mode fd_links fd_size fd_type < <(stat -L -c '%d:%i %u %a %h %s %F' -- "$FD") || fail "cannot stat the open $SRC"
path_id=$(stat -c '%d:%i' -- "$SRC" 2>/dev/null) || fail "$SRC vanished while being opened"
[ "$path_id" = "$fd_id" ] || fail "$SRC changed while being opened; refusing to use it"
case "$fd_type" in
	"regular file"|"regular empty file") ;;
	*) fail "$SRC is not a regular file ($fd_type)" ;;
esac
[ "$fd_uid" = "$SRC_UID" ] || fail "$SRC is owned by uid $fd_uid, expected $SRC_UID; refusing to use it"
unsafe_mode "$fd_mode" && fail "$SRC has unsafe mode $fd_mode (group/world writable); refusing to use it"
[ "$fd_links" = 1 ] || fail "$SRC has $fd_links hard links; refusing to use it"
if [ "$fd_size" -eq 0 ]; then
	warn "$SRC is empty (likely a truncated write); ignoring it and leaving the current keys in place. To revoke every key on purpose, put a comment line such as '# no keys' in the file"
	exit 0
fi
[ "$fd_size" -le "$MAX_BYTES" ] || fail "$SRC is $fd_size bytes (limit $MAX_BYTES); refusing to use it"

# --- 3. Read it and validate every entry -----------------------------------
WORK=$(mktemp -d) || fail "cannot create a work directory"
head -c $(( MAX_BYTES + 1 )) <&3 >"$WORK/source" || fail "cannot read $SRC"
exec 3<&-
[ "$(stat -c %s -- "$WORK/source")" -eq "$fd_size" ] || fail "$SRC changed while being read; refusing to use it"

# No NUL/control characters (tab and CR are tolerated), and never private keys.
tr -d '\000-\010\013\014\016-\037\177' <"$WORK/source" | cmp -s - "$WORK/source" \
	|| fail "$SRC contains control or NUL characters; refusing to use it"
grep -q -e 'PRIVATE KEY' "$WORK/source" && fail "$SRC contains private key material; refusing to use it (only public keys belong here)"

entries=0
lineno=0
: >"$WORK/fingerprints"
while IFS= read -r line || [ -n "$line" ]; do
	lineno=$(( lineno + 1 ))
	trimmed=${line#"${line%%[![:space:]]*}"}
	case "$trimmed" in
		''|'#'*) continue ;;
	esac
	entries=$(( entries + 1 ))
	[ "$SECONDS" -le "$BUDGET_SECONDS" ] || fail "validating $SRC exceeded its ${BUDGET_SECONDS}s time budget; nothing was changed"
	[ "$entries" -le "$MAX_ENTRIES" ] || fail "$SRC has more than $MAX_ENTRIES key entries; refusing to use it"
	printf '%s\n' "$line" >"$WORK/entry"
	if ! fp=$(timeout "$KEYGEN_TIMEOUT" ssh-keygen -l -f "$WORK/entry" 2>/dev/null) || [ -z "$fp" ]; then
		fail "$SRC line $lineno is not a valid authorized_keys entry; nothing was changed"
	fi
	# "<bits> SHA256:<fingerprint> <comment> (<type>)": log only the fingerprint.
	echo "${fp%%$'\n'*}" | awk '{print $2}' >>"$WORK/fingerprints"
done <"$WORK/source"

# --- 4. Locate and prepare the target --------------------------------------
pw=$(getent passwd "$TARGET_USER") || fail "user $TARGET_USER does not exist"
IFS=: read -r _ _ t_uid t_gid _ t_home _ <<<"$pw"
HOME_DIR="${HOME_ROOT}${t_home}"
SSH_DIR="${HOME_DIR}/.ssh"
TARGET="${SSH_DIR}/authorized_keys"

[ -d "$HOME_DIR" ] && [ ! -L "$HOME_DIR" ] || fail "home directory $HOME_DIR is missing or not a plain directory; not creating it"

if [ -L "$SSH_DIR" ]; then
	fail "$SSH_DIR is a symbolic link; refusing to write through it"
elif [ -e "$SSH_DIR" ]; then
	[ -d "$SSH_DIR" ] || fail "$SSH_DIR exists but is not a directory"
else
	install -d -m 0700 -o "$t_uid" -g "$t_gid" -- "$SSH_DIR" || fail "cannot create $SSH_DIR"
fi
# StrictModes needs the directory owned by the user and not writable by others.
chown -h "$t_uid:$t_gid" -- "$SSH_DIR" && chmod 0700 -- "$SSH_DIR" || fail "cannot set ownership/mode on $SSH_DIR"

# Leftovers from a run that was killed between create and rename.
find "$SSH_DIR" -maxdepth 1 -type f -name '.authorized_keys.stratux.*' -delete 2>/dev/null

# --- 5. Install atomically -------------------------------------------------
if [ -f "$TARGET" ] && [ ! -L "$TARGET" ] && cmp -s -- "$WORK/source" "$TARGET"; then
	chown -h "$t_uid:$t_gid" -- "$TARGET" && chmod 0600 -- "$TARGET" || fail "cannot set ownership/mode on $TARGET"
	info "$TARGET already matches the persistent file ($entries key(s)); nothing to change"
else
	TMPTARGET=$(mktemp -- "$SSH_DIR/.authorized_keys.stratux.XXXXXX") || fail "cannot create a temporary file in $SSH_DIR"
	cat -- "$WORK/source" >"$TMPTARGET" \
		&& chown -h "$t_uid:$t_gid" -- "$TMPTARGET" \
		&& chmod 0600 -- "$TMPTARGET" \
		&& mv -f -- "$TMPTARGET" "$TARGET" \
		|| fail "could not install $TARGET; the previous file (if any) was left in place"
	TMPTARGET=
	info "installed $TARGET from the persistent file ($entries key(s))"
fi

# Prove the end state rather than assume it.
read -r r_uid r_gid r_mode < <(stat -c '%u %g %a' -- "$TARGET") || fail "cannot verify $TARGET"
[ "$r_uid" = "$t_uid" ] && [ "$r_gid" = "$t_gid" ] && [ "$r_mode" = 600 ] \
	|| fail "$TARGET ended up with owner $r_uid:$r_gid mode $r_mode, expected $t_uid:$t_gid 600"

while IFS= read -r fp; do
	info "  authorized key $fp"
done <"$WORK/fingerprints"
exit 0
