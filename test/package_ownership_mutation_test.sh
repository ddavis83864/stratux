#!/bin/bash
# package_ownership_mutation_test.sh: proves the package-ownership tests can FAIL. Each
# mutation re-introduces one specific way the fix could be broken or weakened, in a
# throw-away COPY of the tree (the repository is never modified), and the matching
# tests must then fail. A mutation that no test catches is itself a failure.
#
#   test/package_ownership_mutation_test.sh [MUTATION_NAME ...]     (default: all)
#
# The un-mutated copy is run first as the control and must pass everything.
# Opt-in (Docker; ~40 s per mutation). Never touches a device.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$HERE/.." && pwd)
command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available"; exit 0; }
W=$(mktemp -d)
trap 'docker run --rm -v "$W":/w debian:bookworm rm -rf /w/* >/dev/null 2>&1; rm -rf "$W"' EXIT

fresh_copy() { # $1 = dir
	mkdir -p "$1/mapdata"
	cp -r "$REPO/Makefile" "$REPO/debian" "$REPO/scripts" "$REPO/web" "$REPO/softrf" "$REPO/ogn" "$1/"
	cp "$REPO/mapdata/download_mapdata.sh" "$1/mapdata/"
	[ -z "$(ls -d "$REPO"/main 2>/dev/null)" ] || cp -r "$REPO/main" "$1/"
}
# mutate DIR FILE OLD NEW: exact, must-match replacement (a stale mutation fails loudly)
mutate() { python3 - "$@" <<'PY'
import sys
d,f,old,new=sys.argv[1:5]
p=f"{d}/{f}"; s=open(p).read()
if old not in s: sys.exit(f"mutation target not found in {f}: {old!r}")
open(p,'w').write(s.replace(old,new,1))
PY
}

# run the applicable tests against a tree; prints the names of the ones that FAILED
run_tests() { # $1 = tree, $2 = kinds (space list of: static archive lifecycle)
	local t=$1 k failed=""
	for k in $2; do
		case $k in
			static)    "$HERE/package_ownership_static_test.sh" "$t" >"$W/out.$k" 2>&1 || failed="$failed static" ;;
			archive)   SRC=$t "$HERE/package_ownership_archive_test.sh" >"$W/out.$k" 2>&1 || failed="$failed archive" ;;
			lifecycle) if "$HERE/package_ownership_build_deb.sh" "$t" "$W/m.deb" 1001:1001 >/dev/null 2>"$W/out.$k"; then
			               "$HERE/package_ownership_lifecycle_test.sh" "$W/m.deb" >"$W/out.$k" 2>&1 || failed="$failed lifecycle"
			           else failed="$failed BUILD-ERROR"; fi ;;
		esac
	done
	echo "${failed# }"
}

overall=0
echo "=== control: the un-mutated tree passes every test ==="
fresh_copy "$W/control"
res=$(run_tests "$W/control" "static archive lifecycle")
if [ -z "$res" ]; then echo "PASS: control passes static, archive and lifecycle"; else echo "FAIL: control failed: $res"; overall=1; fi

P=debian/postinst.dpkg
run_mut() { # name kinds expect-one-of description ; the mutation is applied by the caller's function
	local name=$1 kinds=$2 expect=$3 desc=$4 res
	res=$(run_tests "$W/m-$name" "$kinds")
	local hit=0; for e in $expect; do case " $res " in *" $e "*) hit=1;; esac; done
	if [ "$hit" = 1 ]; then echo "PASS: mutation '$name' CAUGHT by: $res  -- $desc"
	else echo "FAIL: mutation '$name' NOT caught (failed: '${res:-none}', expected one of: $expect)  -- $desc"; overall=1; fi
}
want() { [ -z "${WANT:-}" ] || case " $WANT " in *" $1 "*) return 0;; *) return 1;; esac; }
WANT="$*"
M() { # name kinds expect file old new desc
	local name=$1 kinds=$2 expect=$3 file=$4 old=$5 new=$6 desc=$7
	want "$name" || return 0
	fresh_copy "$W/m-$name"; mutate "$W/m-$name" "$file" "$old" "$new" || { echo "FAIL: mutation '$name' could not be applied"; overall=1; return; }
	run_mut "$name" "$kinds" "$expect" "$desc"
}

echo "=== mutations of the archive normalization (Layer A) ==="
M no-root-owner-group   "static archive"  "static archive" Makefile "dpkg-deb --root-owner-group -b" "dpkg-deb -b" "package built the old way: builder uid leaks into the archive"
M mapdata-not-writable  "archive"         "archive"        Makefile "chmod a+rwx \$(STRATUX_HOME)/mapdata" "chmod 755 \$(STRATUX_HOME)/mapdata" "mapdata no longer world-writable (would break user uploads)"
M unit-group-writable   "archive"         "archive"        Makefile "chmod 644 \$(DEBPKG_BASE)/lib/systemd/system/stratux.service" "chmod 664 \$(DEBPKG_BASE)/lib/systemd/system/stratux.service" "a root-run unit becomes group-writable"
M unit-setuid           "archive"         "archive"        Makefile "chmod 644 \$(DEBPKG_BASE)/lib/systemd/system/stratux_fancontrol.service" "chmod 4644 \$(DEBPKG_BASE)/lib/systemd/system/stratux_fancontrol.service" "a package file becomes setuid"
M unit-runs-mapdata     "archive"         "archive"        debian/stratux_epaper.service "ExecStart=/opt/stratux/bin/epaperd" "ExecStart=/opt/stratux/mapdata/epaperd" "a root unit executes from the user-writable directory"

echo "=== mutations of the migration (Layer B) ==="
M migration-not-called  "static lifecycle" "static lifecycle" $P "stratux_normalize_ownership || true" "true" "the migration is defined but never called"
if want migration-after-ota; then
	fresh_copy "$W/m-migration-after-ota"
	mutate "$W/m-migration-after-ota" $P "stratux_normalize_ownership || true

systemctl daemon-reload" "systemctl daemon-reload" &&
	mutate "$W/m-migration-after-ota" $P "if [ -n \"\${STRATUX_OTA_INSTALL:-}\" ]; then
    exit 0
fi
" "if [ -n \"\${STRATUX_OTA_INSTALL:-}\" ]; then
    exit 0
fi
stratux_normalize_ownership || true
" && run_mut migration-after-ota "static lifecycle" "static lifecycle" "the migration moved BELOW the OTA early exit (never runs on OTA installs)" \
	|| { echo "FAIL: mutation 'migration-after-ota' could not be applied"; overall=1; }
fi
M migration-skips-ota   "lifecycle"        "lifecycle"        $P "stratux_normalize_ownership || true" '[ -z "${STRATUX_OTA_INSTALL:-}" ] && stratux_normalize_ownership || true' "the migration is skipped on OTA installs"
M migration-recursive   "static lifecycle" "static lifecycle" $P '        chown -h root:root -- "$f" 2>/dev/null || echo "WARNING: could not set root ownership on $f" >&2' '        chown -R root:root /opt/stratux 2>/dev/null; chown -h root:root -- "$f" 2>/dev/null || echo "WARNING: could not set root ownership on $f" >&2' "blind recursive chown of /opt/stratux (clobbers user data ownership)"
M migration-follows-symlinks "lifecycle"  "lifecycle"        $P '        [ -L "$f" ] && continue' '        :' "symlinks are followed (and chown without -h)"
# NOT a mutation: dropping only 'chown -h' is an EQUIVALENT mutant (the symlink skip and the
# canonical-path check both run first), so the layered guards are removed together instead.
if want migration-no-symlink-protection; then
	fresh_copy "$W/m-migration-no-symlink-protection"
	mutate "$W/m-migration-no-symlink-protection" $P '        [ -L "$f" ] && continue' '        :' &&
	mutate "$W/m-migration-no-symlink-protection" $P 'chown -h root:root -- "$f"' 'chown root:root -- "$f"' &&
	mutate "$W/m-migration-no-symlink-protection" $P '/opt/stratux|/opt/stratux/*) [ "$(readlink -f -- "$f" 2>/dev/null)" = "$f" ] || continue ;;' '/opt/stratux|/opt/stratux/*) ;;' &&
	run_mut migration-no-symlink-protection "lifecycle" "lifecycle" "every symlink guard removed: a symlink target owned by pi is re-owned through the link" \
	|| { echo "FAIL: mutation 'migration-no-symlink-protection' could not be applied"; overall=1; }
fi
M migration-no-parent-check "lifecycle"   "lifecycle"        $P '/opt/stratux|/opt/stratux/*) [ "$(readlink -f -- "$f" 2>/dev/null)" = "$f" ] || continue ;;' '/opt/stratux|/opt/stratux/*) ;;' "a symlinked parent directory is followed"
M migration-touches-mapdata "static lifecycle" "static lifecycle" $P '            /opt/stratux/mapdata/*) continue ;;
' '' "user data under mapdata/ is normalized too"
M migration-chmods      "lifecycle"        "lifecycle"        $P '        chown -h root:root -- "$f" 2>/dev/null ||' '        chmod 755 -- "$f" 2>/dev/null; chown -h root:root -- "$f" 2>/dev/null ||' "the migration changes modes (mapdata 0777 -> 0755)"
M migration-fails-install "lifecycle"      "lifecycle"        $P '|| echo "WARNING: could not set root ownership on $f" >&2' '|| return 1' "a chown failure aborts the install"
M migration-wrong-owner "lifecycle"        "lifecycle"        $P 'chown -h root:root -- "$f"' 'chown -h 1000:1000 -- "$f"' "normalizes to the wrong owner"
M migration-out-of-scope "lifecycle"       "lifecycle"        $P '            /lib/systemd/system/*.service|/etc/udev/rules.d/*.rules) ;;' '            /lib/systemd/system/*.service|/etc/udev/rules.d/*.rules|/home/*|/etc/*) ;;' "the scope widens to /home and /etc"
M migration-not-idempotent "lifecycle"     "lifecycle"        $P '        [ "$(stat -c '"'"'%u:%g'"'"' -- "$f" 2>/dev/null)" = "0:0" ] && continue
' '' "chown is issued even for already-correct paths"
echo
[ "$overall" = 0 ] && echo "ALL MUTATIONS CAUGHT" || echo "MUTATION TEST FAILED"
exit $overall
