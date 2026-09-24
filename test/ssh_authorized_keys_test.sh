#!/bin/bash
# ssh_authorized_keys_test.sh: behavior tests for
# debian/stratux-ssh-authorized-keys.sh, the helper that restores the
# administrator's persistent SSH public keys into the volatile home directory
# (docs/ssh-authorized-keys.md).
#
# Runs unprivileged against throwaway directories via the helper's test hooks
# (honored only with STRATUX_SSH_KEYS_TESTING=1). Ownership checks that need a
# real root-owned source, a real `pi` user, a real mount and a real sshd are
# covered by test/ssh_authorized_keys_systemd_lab.sh. Every key used here is
# generated ephemerally at run time; nothing secret is committed.
set -u
cd "$(dirname "$0")/.." || exit 1
HELPER="$PWD/debian/stratux-ssh-authorized-keys.sh"

fail=0
pass_n=0
check() {
	if [ "$2" = 1 ]; then echo "PASS: $1"; pass_n=$((pass_n + 1)); else echo "FAIL: $1"; fail=1; fi
}
ok() { [ "$1" = "$2" ] && echo 1 || echo 0; }

command -v ssh-keygen >/dev/null || { echo "SKIP: ssh-keygen not available"; exit 0; }

ME=$(id -un)
MY_UID=$(id -u)
MY_HOME=$(getent passwd "$ME" | cut -d: -f6)
ROOT_TMP=$(mktemp -d)
trap 'rm -rf "$ROOT_TMP"' EXIT

# Ephemeral keys: A, B, C (ed25519) and R (rsa) plus a private key blob.
KEYS="$ROOT_TMP/keys"; mkdir -p "$KEYS"
for k in A B C; do ssh-keygen -q -t ed25519 -N '' -C "test-$k" -f "$KEYS/$k" >/dev/null; done
ssh-keygen -q -t rsa -b 2048 -N '' -C test-R -f "$KEYS/R" >/dev/null
pubA=$(cat "$KEYS/A.pub"); pubB=$(cat "$KEYS/B.pub"); pubC=$(cat "$KEYS/C.pub"); pubR=$(cat "$KEYS/R.pub")
bodyA=$(awk '{print $2}' "$KEYS/A.pub")
fpA=$(ssh-keygen -lf "$KEYS/A.pub" | awk '{print $2}')

# Fresh sandbox per case: T/root (data mount prefix), T/home (home prefix).
new_env() {
	T=$(mktemp -d -p "$ROOT_TMP")
	DATA="$T/root/var/lib/stratux-data"
	SRCDIR="$DATA/ssh"; SRC="$SRCDIR/authorized_keys"
	HOMEDIR="$T/home$MY_HOME"; SSHDIR="$HOMEDIR/.ssh"; TARGET="$SSHDIR/authorized_keys"
	mkdir -p "$DATA" "$HOMEDIR"
	chmod 755 "$DATA"
}
with_src() { # write the source (stdin) as a well-formed persistent file
	mkdir -p "$SRCDIR"; chmod 755 "$SRCDIR"; cat >"$SRC"; chmod 644 "$SRC"
}
run() { # extra env assignments as args; sets RC, OUT (stdout+stderr)
	OUT=$(env STRATUX_SSH_KEYS_TESTING=1 STRATUX_TEST_ROOT="$T/root" STRATUX_TEST_HOME_ROOT="$T/home" \
		STRATUX_TEST_SRC_UID="$MY_UID" STRATUX_TEST_TARGET_USER="$ME" STRATUX_TEST_SKIP_MOUNTCHECK=1 \
		"$@" timeout 15 bash "$HELPER" 2>&1); RC=$?   # 124 = the helper hung: always a failure
}
mode() { stat -c %a "$1"; }
littered() { find "$SSHDIR" -maxdepth 1 -name '.authorized_keys.stratux.*' 2>/dev/null | grep -q . && echo 1 || echo 0; }

bash -n "$HELPER" && echo "PASS: helper is valid bash" || { echo "FAIL: helper syntax"; fail=1; }
[ -x "$HELPER" ] && echo "PASS: helper is executable" || { echo "FAIL: helper is not executable"; fail=1; }

# --- 1. Missing source: a normal no-op --------------------------------------
new_env; run
check "missing source: exit 0" "$(ok $RC 0)"
check "missing source: nothing created (no ~/.ssh, no target, no persistent file/dir)" "$([ ! -e "$SSHDIR" ] && [ ! -e "$SRCDIR" ] && echo 1 || echo 0)"
check "missing source: informational (not alarming) message" "$(echo "$OUT" | grep -q 'nothing to restore' && ! echo "$OUT" | grep -qi 'error\|warning' && echo 1 || echo 0)"
# ...and it must not erase a volatile key that is already there.
new_env; mkdir -p "$SSHDIR"; chmod 700 "$SSHDIR"; echo "$pubA" >"$TARGET"; chmod 600 "$TARGET"; run
check "missing source does not erase an existing volatile key" "$([ "$RC" = 0 ] && [ "$(cat "$TARGET")" = "$pubA" ] && echo 1 || echo 0)"

# --- 2. Valid single key ----------------------------------------------------
new_env; echo "$pubA" | with_src; run
check "single key: exit 0" "$(ok $RC 0)"
check "single key: contents identical to the source" "$(cmp -s "$SRC" "$TARGET" && echo 1 || echo 0)"
check "single key: ~/.ssh is 0700 and owned by the target user" "$([ "$(mode "$SSHDIR")" = 700 ] && [ "$(stat -c %u "$SSHDIR")" = "$MY_UID" ] && echo 1 || echo 0)"
check "single key: authorized_keys is 0600 and owned by the target user" "$([ "$(mode "$TARGET")" = 600 ] && [ "$(stat -c %u "$TARGET")" = "$MY_UID" ] && [ "$(stat -c %g "$TARGET")" = "$(id -g)" ] && echo 1 || echo 0)"
check "single key: the source itself is not modified" "$([ "$(cat "$SRC")" = "$pubA" ] && echo 1 || echo 0)"
check "no temporary files left behind" "$(ok "$(littered)" 0)"
check "logs the fingerprint but never the public key body" "$(echo "$OUT" | grep -q -F "$fpA" && ! echo "$OUT" | grep -q -F "$bodyA" && echo 1 || echo 0)"

# --- 3. Multiple keys, exactly retained -------------------------------------
new_env; printf '%s\n%s\n%s\n%s\n' "$pubA" "$pubB" "$pubC" "$pubR" | with_src; run
check "multiple keys (ed25519 x3 + rsa): installed byte-for-byte" "$([ "$RC" = 0 ] && cmp -s "$SRC" "$TARGET" && echo 1 || echo 0)"
check "multiple keys: reports 4 entries" "$(echo "$OUT" | grep -q '4 key(s)' && echo 1 || echo 0)"

# --- 4. Comments and blank lines preserved ----------------------------------
new_env; printf '# administrator keys\n\n%s\n   \n  # indented comment\n%s\n\n' "$pubA" "$pubB" | with_src; run
check "comments and blank lines: preserved exactly" "$([ "$RC" = 0 ] && cmp -s "$SRC" "$TARGET" && echo 1 || echo 0)"
new_env; printf '%s' "$pubA" | with_src; run
check "no trailing newline on the last entry is accepted" "$([ "$RC" = 0 ] && cmp -s "$SRC" "$TARGET" && echo 1 || echo 0)"
new_env; printf '# no keys\n' | with_src; mkdir -p "$SSHDIR"; echo "$pubA" >"$SSHDIR/authorized_keys"; run
check "comment-only source is a valid authoritative file: revokes every key" "$([ "$RC" = 0 ] && [ "$(cat "$TARGET")" = '# no keys' ] && echo 1 || echo 0)"

# --- 5. Legitimate authorized_keys options ----------------------------------
new_env
{
	printf 'command="/usr/bin/id -un",no-pty,no-port-forwarding %s\n' "$pubA"
	printf 'from="10.0.0.0/8,*.example.com",restrict %s\n' "$pubB"
	printf 'environment="GREETING=hello world",no-agent-forwarding %s\n' "$pubC"
	printf 'cert-authority,principals="ops" %s\n' "$pubR"
	printf 'no-X11-forwarding,permitopen="host:22" %s\n' "$pubA"
} | with_src; run
check "authorized_keys options (command=, from=, quoted spaces, cert-authority, permitopen=): accepted" "$([ "$RC" = 0 ] && cmp -s "$SRC" "$TARGET" && echo 1 || echo 0)"
check "options: 5 entries counted, key bodies not logged" "$(echo "$OUT" | grep -q '5 key(s)' && ! echo "$OUT" | grep -q -F "$bodyA" && echo 1 || echo 0)"

# --- 6. Malformed key material ----------------------------------------------
badcases=(
	"truncated base64|ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI comment"
	"unknown key type|ssh-nothing AAAAC3NzaC1lZDI1NTE5AAAAIGVsbG8= comment"
	"garbage line|this is not a key at all"
	"options with no key|command=\"/bin/true\",no-pty"
)
for c in "${badcases[@]}"; do
	name=${c%%|*}; line=${c#*|}
	new_env; mkdir -p "$SSHDIR"; chmod 700 "$SSHDIR"; echo "$pubB" >"$TARGET"; chmod 600 "$TARGET"
	printf '%s\n%s\n' "$pubA" "$line" | with_src; run
	check "malformed ($name): non-zero exit with a diagnostic naming the line" "$([ "$RC" != 0 ] && echo "$OUT" | grep -q 'line 2 is not a valid' && echo 1 || echo 0)"
	check "malformed ($name): target NOT replaced (previous key intact, even though line 1 was valid)" "$([ "$(cat "$TARGET")" = "$pubB" ] && echo 1 || echo 0)"
	check "malformed ($name): no temporary files left" "$(ok "$(littered)" 0)"
done

# --- 7. Private key material ------------------------------------------------
new_env; { cat "$KEYS/A"; } | with_src; run
check "a source containing a PRIVATE key is rejected and nothing is installed" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo "$OUT" | grep -qi 'private key' && echo 1 || echo 0)"
new_env; { echo "$pubA"; echo "-----BEGIN OPENSSH PRIVATE KEY-----"; } | with_src; run
check "private key header hidden among valid keys is rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"

# --- 8. Source ownership / permissions / type -------------------------------
new_env; echo "$pubA" | with_src; run STRATUX_TEST_SRC_UID=$((MY_UID + 1))
check "source not owned by the expected uid (root in production): rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo "$OUT" | grep -q 'expected' && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; chmod 664 "$SRC"; run
check "group-writable source (0664): rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo "$OUT" | grep -q 'unsafe mode' && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; chmod 666 "$SRC"; run
check "world-writable source (0666): rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; chmod 602 "$SRC"; run
check "world-writable source (0602): rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; chmod 600 "$SRC"; run
check "a stricter source mode (0600) is fine" "$([ "$RC" = 0 ] && cmp -s "$SRC" "$TARGET" && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; chmod 775 "$SRCDIR"; run
check "group-writable ssh/ directory: rejected (a non-root user could swap the file)" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; chmod 757 "$SRCDIR"; run
check "world-writable ssh/ directory: rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; run STRATUX_TEST_SRC_UID=$((MY_UID + 1))
check "wrong-owner directory or file never leaves a partial target" "$([ ! -e "$SSHDIR" ] || [ "$(ls -A "$SSHDIR" | wc -l)" = 0 ] && echo 1 || echo 0)"

new_env; echo "$pubA" >"$T/real"; mkdir -p "$SRCDIR"; chmod 755 "$SRCDIR"; ln -s "$T/real" "$SRC"; run
check "source is a symlink to a valid file: rejected, not followed" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo "$OUT" | grep -q 'symbolic link' && echo 1 || echo 0)"
new_env; mkdir -p "$SRCDIR"; chmod 755 "$SRCDIR"; ln -s "$T/does-not-exist" "$SRC"; run
check "dangling symlink source: rejected (and not treated as 'absent')" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; mkdir -p "$SRCDIR/authorized_keys"; chmod 755 "$SRCDIR"; run
check "source is a directory: rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; mkdir -p "$SRCDIR"; chmod 755 "$SRCDIR"; mkfifo "$SRC"; run
check "source is a FIFO: rejected promptly without blocking on it (a hang here would stall boot)" "$([ "$RC" != 0 ] && [ "$RC" != 124 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; ln "$SRC" "$T/second-link"; run
check "source with a second hard link: rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo "$OUT" | grep -q 'hard links' && echo 1 || echo 0)"
new_env; echo "$pubA" >"$T/real-dir-target"; mkdir -p "$T/root/var/lib"; ln -s "$T/realssh" "$SRCDIR" 2>/dev/null; mkdir -p "$T/realssh"; echo "$pubA" >"$T/realssh/authorized_keys"; chmod 644 "$T/realssh/authorized_keys"; chmod 755 "$T/realssh"; run
check "ssh/ directory is a symlink: rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"

# --- 9. Empty / oversized / control characters ------------------------------
new_env; : | with_src; mkdir -p "$SSHDIR"; chmod 700 "$SSHDIR"; echo "$pubB" >"$TARGET"; chmod 600 "$TARGET"; run
check "zero-byte source: ignored with a warning, current key kept, exit 0" "$([ "$RC" = 0 ] && [ "$(cat "$TARGET")" = "$pubB" ] && echo "$OUT" | grep -qi 'warning.*empty' && echo 1 || echo 0)"
new_env; head -c 70000 /dev/zero | tr '\0' '#' | with_src; run
check "oversized source (>64 KiB): rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; { for _ in $(seq 1 300); do echo "$pubA"; done; } | with_src; run
check "more than 256 key entries: rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; printf '%s\n' "$pubA" | with_src; printf 'ssh-ed25519 AAAA\001evil\n' >>"$SRC"; run
check "control character in the source: rejected" "$([ "$RC" != 0 ] && [ ! -e "$TARGET" ] && echo 1 || echo 0)"
new_env; printf '%s\r\n' "$pubA" | with_src; run
check "CRLF line endings are tolerated and retained unchanged" "$([ "$RC" = 0 ] && cmp -s "$SRC" "$TARGET" && echo 1 || echo 0)"

# --- 10. Atomic replacement -------------------------------------------------
new_env; mkdir -p "$SSHDIR"; chmod 700 "$SSHDIR"; printf '%s\n%s\n' "$pubA" "$pubB" >"$TARGET"; chmod 600 "$TARGET"; cp "$TARGET" "$T/before"
printf '%s\nnot-a-key\n' "$pubC" | with_src; run
check "atomic: invalid replacement leaves the previous target byte-identical" "$([ "$RC" != 0 ] && cmp -s "$T/before" "$TARGET" && [ "$(mode "$TARGET")" = 600 ] && echo 1 || echo 0)"
check "atomic: no temporary files after the failed replacement" "$(ok "$(littered)" 0)"
printf '%s\n' "$pubC" | with_src; run
check "atomic: a valid new source then replaces the target completely" "$([ "$RC" = 0 ] && [ "$(cat "$TARGET")" = "$pubC" ] && echo 1 || echo 0)"
new_env; mkdir -p "$SSHDIR"; touch "$SSHDIR/.authorized_keys.stratux.STALE1" "$SSHDIR/.authorized_keys.stratux.STALE2"; echo "$pubA" | with_src; run
check "stale temporary files from a killed earlier run are cleaned up" "$([ "$RC" = 0 ] && ok "$(littered)" 0)"
new_env; mkdir -p "$SSHDIR"; ln -s "$T/elsewhere" "$TARGET"; echo "$pubA" | with_src; run
check "a symlink at the target is replaced (not written through)" "$([ "$RC" = 0 ] && [ ! -L "$TARGET" ] && [ "$(cat "$TARGET")" = "$pubA" ] && [ ! -e "$T/elsewhere" ] && echo 1 || echo 0)"

# --- 11. Revocation ---------------------------------------------------------
new_env; printf '%s\n%s\n' "$pubA" "$pubB" | with_src; run
first=$RC; hasB=$(grep -c -F "$(awk '{print $2}' "$KEYS/B.pub")" "$TARGET")
printf '%s\n' "$pubA" | with_src; run
check "revocation: A+B applied, then only A: target is exactly A" "$([ "$first" = 0 ] && [ "$hasB" = 1 ] && [ "$RC" = 0 ] && [ "$(cat "$TARGET")" = "$pubA" ] && echo 1 || echo 0)"
check "revocation: the revoked key B is gone from the target" "$(grep -q -F "$(awk '{print $2}' "$KEYS/B.pub")" "$TARGET" && echo 0 || echo 1)"

# --- 12. Permissions are corrected, not trusted -----------------------------
new_env; mkdir -p "$SSHDIR"; chmod 777 "$SSHDIR"; echo "$pubA" >"$TARGET"; chmod 666 "$TARGET"; echo "$pubA" | with_src; run
check "a loose pre-existing ~/.ssh (0777) and target (0666) are tightened to 0700/0600" "$([ "$RC" = 0 ] && [ "$(mode "$SSHDIR")" = 700 ] && [ "$(mode "$TARGET")" = 600 ] && echo 1 || echo 0)"
new_env; mkdir -p "$T/elsewhere"; mkdir -p "$HOMEDIR"; ln -s "$T/elsewhere" "$SSHDIR"; echo "$pubA" | with_src; run
check "the .ssh directory is a symlink: refused, nothing written through it" "$([ "$RC" != 0 ] && [ ! -e "$T/elsewhere/authorized_keys" ] && echo 1 || echo 0)"
new_env; rmdir "$HOMEDIR"; echo "$pubA" | with_src; run
check "missing home directory: error, and the home is not created" "$([ "$RC" != 0 ] && [ ! -e "$HOMEDIR" ] && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src; run STRATUX_TEST_TARGET_USER=no-such-user-xyz
check "unknown target user: error, nothing installed" "$([ "$RC" != 0 ] && [ ! -e "$SSHDIR" ] && echo 1 || echo 0)"

# --- 13. Idempotency --------------------------------------------------------
new_env; printf '# keys\n%s\n%s\n' "$pubA" "$pubB" | with_src; run; ino1=$(stat -c %i "$TARGET"); sum1=$(sha256sum <"$TARGET")
run; rc2=$RC; run; rc3=$RC; ino3=$(stat -c %i "$TARGET"); sum3=$(sha256sum <"$TARGET")
check "idempotent: three runs, same result, exit 0 each time" "$([ "$rc2" = 0 ] && [ "$rc3" = 0 ] && [ "$sum1" = "$sum3" ] && echo 1 || echo 0)"
check "idempotent: no duplicated keys introduced (2 entries after 3 runs)" "$([ "$(grep -c '^ssh-ed25519' "$TARGET")" = 2 ] && echo 1 || echo 0)"
check "idempotent: an up-to-date target is not rewritten (same inode)" "$([ "$ino1" = "$ino3" ] && echo "$OUT" | grep -q 'nothing to change' && echo 1 || echo 0)"

# --- 14. Mount guard and hook isolation -------------------------------------
new_env; echo "$pubA" | with_src
OUT=$(env STRATUX_SSH_KEYS_TESTING=1 STRATUX_TEST_ROOT="$T/root" STRATUX_TEST_HOME_ROOT="$T/home" STRATUX_TEST_SRC_UID="$MY_UID" STRATUX_TEST_TARGET_USER="$ME" bash "$HELPER" 2>&1); RC=$?
check "data path that is not a real mount point: no-op (a RAM-overlay directory is never trusted), exit 0" "$([ "$RC" = 0 ] && [ ! -e "$SSHDIR" ] && echo "$OUT" | grep -q 'not mounted' && echo 1 || echo 0)"
new_env; echo "$pubA" | with_src
OUT=$(env STRATUX_TEST_ROOT="$T/root" STRATUX_TEST_HOME_ROOT="$T/home" STRATUX_TEST_SRC_UID="$MY_UID" STRATUX_TEST_TARGET_USER="$ME" STRATUX_TEST_SKIP_MOUNTCHECK=1 bash "$HELPER" 2>&1); RC=$?
check "test hooks are ignored unless STRATUX_SSH_KEYS_TESTING=1 (real /var/lib/stratux-data is used)" "$(echo "$OUT" | grep -q '/var/lib/stratux-data' && ! echo "$OUT" | grep -q "$T" && [ ! -e "$SSHDIR" ] && echo 1 || echo 0)"

# --- 15. Static properties --------------------------------------------------
check "helper never references sshd_config, StrictModes, host keys or authorizedkeysfile settings" "$(grep -v '^\s*#' "$HELPER" | grep -q -i -E 'sshd_config|StrictModes|ssh_host|AuthorizedKeysFile|AuthorizedKeysCommand|PermitRoot|PasswordAuthentication' && echo 0 || echo 1)"
check "helper does not create the persistent file or directory (no writes under the source path)" "$(grep -v '^\s*#' "$HELPER" | grep -E '>\s*"?\$SRC|mkdir[^|;]*\$SRC|touch[^|;]*\$SRC|install[^|;]*\$SRC_DIR' | grep -q . && echo 0 || echo 1)"

echo
echo "NOTE: a source file owned by a DIFFERENT uid inside a correctly-owned directory cannot be created unprivileged; that case is covered by test/ssh_authorized_keys_systemd_lab.sh (real root and a real pi user)."
echo "$pass_n checks passed"
exit $fail
