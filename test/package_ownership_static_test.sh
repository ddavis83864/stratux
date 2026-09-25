#!/bin/bash
# package_ownership_static_test.sh: source-level guards for the package-ownership fix that
# need no Docker and no built package (the behavior itself is proven by
# package_ownership_archive_test.sh and package_ownership_lifecycle_test.sh):
#   - the Makefile builds the .deb with --root-owner-group, and NOTHING else in the tree
#     builds a stratux .deb with a bare `dpkg-deb -b`;
#   - the postinst defines the ownership migration and runs it BEFORE the first
#     systemctl call and BEFORE the STRATUX_OTA_INSTALL early exit;
#   - the migration is per-path (never recursive), non-following, mode-preserving, scoped
#     and best-effort, and the tree contains no blind recursive chown of /opt/stratux;
#   - the shell scripts are syntactically valid (bash and dash) and ShellCheck-clean.
# Usage: test/package_ownership_static_test.sh [SRC_TREE]     (default: this repository)
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
cd "${1:-$HERE/..}" || exit 1
fail=0
check() { if [ "$2" = 1 ]; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }
POST=debian/postinst.dpkg

check "Makefile packages with 'dpkg-deb --root-owner-group -b'" "$(grep -qE '^\s*dpkg-deb --root-owner-group -b \$\(DEBPKG_BASE\)' Makefile && echo 1 || echo 0)"
bare=$(grep -rnE 'dpkg-deb( +-[-a-zA-Z]+)* +(-b|--build)\b' --exclude-dir=.git --exclude-dir=test --exclude-dir=docs --exclude-dir=dump1090 --exclude-dir=dump978 . 2>/dev/null | grep -v -- '--root-owner-group' | grep -vE '^\./[^:]*:[0-9]+:\s*#')
check "no build recipe or script anywhere builds a .deb without --root-owner-group" "$([ -z "$bare" ] && echo 1 || echo 0)"
[ -z "$bare" ] || echo "$bare" | sed 's/^/        /'

def=$(grep -n '^stratux_normalize_ownership() {' $POST | head -1 | cut -d: -f1)
call=$(grep -n '^stratux_normalize_ownership || true' $POST | head -1 | cut -d: -f1)
first_sc=$(grep -n '^ *systemctl ' $POST | head -1 | cut -d: -f1)
ota=$(grep -n 'STRATUX_OTA_INSTALL' $POST | grep -v '^[0-9]*:\s*#' | head -1 | cut -d: -f1)
check "postinst defines the migration function and calls it (line $def / $call)" "$([ -n "$def" ] && [ -n "$call" ] && [ "$def" -lt "$call" ] && echo 1 || echo 0)"
check "the migration runs before the first systemctl call (line $first_sc) - units are enabled/reloaded on normalized files" "$([ -n "$call" ] && [ -n "$first_sc" ] && [ "$call" -lt "$first_sc" ] && echo 1 || echo 0)"
check "the migration runs before the STRATUX_OTA_INSTALL early exit (line $ota) - the exit is taken on the only persistent (OTA) path" "$([ -n "$call" ] && [ -n "$ota" ] && [ "$call" -lt "$ota" ] && echo 1 || echo 0)"

fn=$(awk '/^stratux_normalize_ownership\(\) \{/{f=1} f{print} f&&/^\}/{exit}' $POST)
code=$(printf '%s\n' "$fn" | grep -vE '^\s*#')
check "migration reads the dpkg file list of THIS package (no filesystem walk: no find/ls/glob)" "$(printf '%s' "$code" | grep -q '/var/lib/dpkg/info/stratux.list' && ! printf '%s' "$code" | grep -qE '\bfind\b|\bls\b|\bxargs\b' && echo 1 || echo 0)"
check "migration never recurses (no chown -R / --recursive)" "$(printf '%s' "$code" | grep -qE 'chown +(-[a-zA-Z]*R|--recursive)' && echo 0 || echo 1)"
check "migration never follows a symlink (chown -h, symlinks skipped)" "$(printf '%s' "$code" | grep -q 'chown -h' && printf '%s' "$code" | grep -q '\[ -L "\$f" \]' && echo 1 || echo 0)"
check "migration never changes a mode (no chmod)" "$(printf '%s' "$code" | grep -q 'chmod' && echo 0 || echo 1)"
check "migration sets exactly root:root" "$(printf '%s' "$code" | grep -q 'chown -h root:root -- "\$f"' && echo 1 || echo 0)"
check "migration is scoped: /opt/stratux, the shipped units, the shipped udev rules only" "$(printf '%s' "$code" | grep -q '/opt/stratux|/opt/stratux/\*' && printf '%s' "$code" | grep -q '/lib/systemd/system/\*.service' && printf '%s' "$code" | grep -q '/etc/udev/rules.d/\*.rules' && echo 1 || echo 0)"
check "migration excludes the user-data directory contents (/opt/stratux/mapdata/*)" "$(printf '%s' "$code" | grep -q '/opt/stratux/mapdata/\*) continue' && echo 1 || echo 0)"
check "migration is best-effort: returns 0 and its call cannot fail the install" "$(printf '%s' "$code" | grep -q 'return 0' && sed -n "${call}p" $POST | grep -q '|| true' && echo 1 || echo 0)"

blind=$(grep -rnE 'chown +(-[a-zA-Z]*R[a-zA-Z]*|--recursive)[^#]*(/opt/stratux|STRATUX_HOME|DEBPKG)' Makefile debian scripts 2>/dev/null | grep -vE ':[0-9]+:\s*#')
check "no blind recursive chown of /opt/stratux exists in the Makefile, debian/ or scripts/" "$([ -z "$blind" ] && echo 1 || echo 0)"

check "postinst is valid bash and dash syntax" "$(bash -n $POST && dash -n $POST && echo 1 || echo 0)"
check "the test scripts are valid bash" "$(for f in "$HERE"/package_ownership_*.sh; do bash -n "$f" || exit 1; done && echo 1 || echo 0)"
if command -v shellcheck >/dev/null 2>&1; then SC="shellcheck"; elif command -v docker >/dev/null 2>&1 && docker image inspect stratux-pkgtest:bookworm >/dev/null 2>&1; then SC=docker-shellcheck; else SC=""; fi
if [ -n "$SC" ]; then
	files="debian/postinst.dpkg"
	if [ "$SC" = shellcheck ]; then out=$(shellcheck -s bash $files 2>&1); else out=$(docker run --rm -v "$PWD":/s:ro -w /s stratux-pkgtest:bookworm shellcheck -s bash $files 2>&1); fi
	# The pre-existing script has findings unrelated to this change; the new function must add none.
	lines=$(printf '%s\n' "$out" | grep -oE 'line [0-9]+' | awk '{print $2}')
	inrange=0; for l in $lines; do [ "$l" -ge "$def" ] && [ "$l" -le "$call" ] && inrange=$((inrange+1)); done
	check "ShellCheck reports nothing inside the new migration function (lines $def-$call)" "$([ "$inrange" = 0 ] && echo 1 || echo 0)"
	[ "$inrange" = 0 ] || printf '%s\n' "$out" | head -20 | sed 's/^/        /'
else
	echo "SKIP: shellcheck not available (ShellCheck check not run)"
fi
exit $fail
