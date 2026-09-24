#!/bin/bash
# package_ownership_archive_test.sh: the ownership contract of the stratux .deb ARCHIVE.
#
#   test/package_ownership_archive_test.sh            builds the package with the REAL
#                                                     `make dpkg` recipe under several
#                                                     non-root builder uid:gid pairs and
#                                                     checks each one, and that they agree
#   test/package_ownership_archive_test.sh --deb X    checks one already-built package
#                                                     (for example the exact CI artifact)
#   SRC=/path/to/tree test/package_ownership_archive_test.sh   use another source tree
#                                                     (the mutation test does this)
#
# The contract is an INVARIANT, not a hard-coded builder uid:
#   1. every entry of the data and the control archive is owned by 0:0, whoever built it;
#   2. no entry is setuid/setgid/sticky;
#   3. nothing is group- or world-writable except an explicit allowlist (mapdata/ only,
#      which must stay 0777: users upload map data there as `pi`);
#   4. everything a shipped unit executes as root exists in the package, is executable,
#      and does not live in the writable allowlist;
#   5. nothing root runs automatically references scripts in the user-writable
#      directories (mapdata/, and ogn/ + softrf/, which hold manual flashing helpers);
#   6. packages built by different uid:gid pairs are identical (path/type/mode/uid/gid).
# It also proves its own sensitivity: the same tree packaged the old way (plain
# `dpkg-deb -b`) MUST be reported as violating the invariant.
#
# Opt-in like the other package tests: needs Docker (+ network once, for the cached test
# image). Prints SKIP and exits 0 without Docker. Never touches a device.
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
SRC=${SRC:-$(cd "$HERE/.." && pwd)}
DEB=""
if [ "${1:-}" = "--deb" ]; then DEB=${2:-}; [ -f "$DEB" ] || { echo "FAIL: --deb needs an existing file"; exit 1; }; fi
command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available"; exit 0; }
. "$HERE/package_ownership_image.sh"; ensure_pkgtest_image
W=$(mktemp -d)
trap 'docker run --rm -v "$W":/w debian:bookworm rm -rf /w/* >/dev/null 2>&1; rm -rf "$W"' EXIT
if [ -n "$DEB" ]; then
	cp "$DEB" "$W/candidate.deb"
	MODE=deb
else
	MODE=build
	for ids in 1000:1000 1001:1001 4242:4243 0:0; do
		n=b-${ids/:/-}
		# The 1001 build also emits the pre-normalization package, to prove the checks can fail.
		if [ "$ids" = 1001:1001 ]; then export LEGACY_DEB="$W/old-1001-1001.deb"; else unset LEGACY_DEB; fi
		"$HERE/package_ownership_build_deb.sh" "$SRC" "$W/$n.deb" "$ids" >/dev/null || { echo "FAIL: build as $ids failed"; exit 1; }
	done
fi

OUT=$(mktemp)
docker run --rm -i -v "$W":/debs:ro -v "$SRC":/src:ro -e MODE=$MODE "$IMG" bash -s <<'CONTAINER' 2>&1 | tee "$OUT"
set -u
fail=0
check() { if [ "$2" = 1 ]; then echo "PASS: $1"; else echo "FAIL: $1"; fail=1; fi; }
ALLOW_WRITABLE="/opt/stratux/mapdata/"          # the ONLY user-writable path of the package

# lst DEB -> "mode uid gid path" per data-archive entry, path normalized to absolute
lst() { dpkg-deb --fsys-tarfile "$1" | tar -tvf - --numeric-owner | awk '{split($2,o,"/"); p=$6; for(i=7;i<=NF;i++){ if($i=="->")break; p=p" "$i }; sub(/^\./,"",p); if(p=="")p="/"; print $1, o[1], o[2], p}'; }
clst() { dpkg-deb --ctrl-tarfile "$1" | tar -tvf - --numeric-owner | awk '{split($2,o,"/"); print o[1]"/"o[2], $6}'; }

# violations DEB: prints one line per breach of the ownership invariant
violations() {
	lst "$1" | awk -v allow="$ALLOW_WRITABLE" '
		$2!=0 || $3!=0 { print "not root-owned: " $0 }
		substr($1,4,1)~/[sS]/ || substr($1,7,1)~/[sS]/ || substr($1,10,1)~/[tT]/ { print "setuid/setgid/sticky: " $0 }
		substr($1,1,1)!="l" && (substr($1,6,1)=="w" || substr($1,9,1)=="w") && $4!=allow && index($4,allow)!=1 { print "group/world-writable outside the allowlist: " $0 }
		substr($1,1,1)!="l" && (substr($1,6,1)=="w" || substr($1,9,1)=="w") && index($4,allow)==1 && $4!=allow { print "group/world-writable file inside the allowlisted dir: " $0 }'
	clst "$1" | awk '$1!="0/0" { print "control archive not root-owned: " $0 }'
}

inspect() {
	local d=$1 tag=$2 v n
	v=$(violations "$d")
	check "[$tag] ownership invariant holds: every entry 0:0, no setuid/setgid/sticky, nothing group/world-writable outside the allowlist" "$([ -z "$v" ] && echo 1 || echo 0)"
	[ -z "$v" ] || echo "$v" | head -5 | sed 's/^/        /'
	n=$(lst "$d" | wc -l)
	check "[$tag] the archive is not trivially empty ($n entries)" "$([ "$n" -gt 100 ] && echo 1 || echo 0)"
	check "[$tag] mapdata stays a world-writable directory (0777) so users can upload as pi" "$([ "$(lst "$d" | awk '$4=="/opt/stratux/mapdata/"{print $1}')" = drwxrwxrwx ] && echo 1 || echo 0)"
	check "[$tag] /opt/stratux, /opt/stratux/bin and the units are present and 0755/0755/0644 root-owned" "$(lst "$d" | awk '$4=="/opt/stratux/"{a=($1=="drwxr-xr-x"&&$2==0&&$3==0)} $4=="/opt/stratux/bin/"{b=($1=="drwxr-xr-x"&&$2==0&&$3==0)} $4=="/lib/systemd/system/stratux.service"{c=($1=="-rw-r--r--"&&$2==0&&$3==0)} END{print (a&&b&&c)?1:0}')"

	# --- privileged-path invariant, derived from what the package's own units run as root
	local x=/tmp/x-$tag; rm -rf "$x"; mkdir -p "$x"; dpkg-deb -x "$d" "$x"; dpkg-deb -e "$d" "$x/DEBIAN"
	local execs missing=0 inallow=0 nonexec=0 p total=0
	execs=$(grep -hE '^Exec(Start|StartPre|StartPost|Stop|StopPost|Reload)=' "$x"/lib/systemd/system/*.service | grep -oE '/opt/stratux/[^ ]+' | sort -u)
	for p in $execs; do
		total=$((total+1))
		[ -f "$x$p" ] || { missing=$((missing+1)); echo "        missing: $p"; continue; }
		[ -x "$x$p" ] || nonexec=$((nonexec+1))
		case "$p" in /opt/stratux/mapdata/*) inallow=$((inallow+1));; esac
	done
	check "[$tag] every /opt/stratux path the units execute as root is shipped and executable ($total paths)" "$([ "$total" -ge 5 ] && [ $missing = 0 ] && [ $nonexec = 0 ] && echo 1 || echo 0)"
	check "[$tag] none of them lives in the user-writable directory" "$([ $inallow = 0 ] && echo 1 || echo 0)"
	# parents of each executed path are root-owned and not writable by anyone else
	local badparent=0 q
	for p in $execs; do q=$p; while q=$(dirname "$q"); [ "$q" != / ]; do
		lst "$d" | awk -v q="$q/" '$4==q{ f=1; if($2!=0||$3!=0||substr($1,6,1)=="w"||substr($1,9,1)=="w") bad=1 } END{exit (f&&!bad)?0:1}' || badparent=$((badparent+1))
	done; done
	check "[$tag] every directory on the path to them is root-owned and not group/world-writable" "$([ $badparent = 0 ] && echo 1 || echo 0)"
	# nothing root runs automatically calls into the writable dirs
	local refs
	refs=$(grep -lE 'mapdata|/opt/stratux/softrf|/opt/stratux/ogn/[A-Za-z0-9_-]+\.sh|softrf/[A-Za-z0-9_-]+\.sh' "$x"/lib/systemd/system/*.service "$x/DEBIAN/preinst" "$x/DEBIAN/prerm" "$x"/opt/stratux/bin/stratux-pre-start.sh "$x"/opt/stratux/bin/stratux-wifi.sh "$x"/opt/stratux/bin/stratux-ssh-authorized-keys.sh 2>/dev/null)
	check "[$tag] no unit, maintainer script or boot helper references scripts in mapdata/, ogn/ or softrf/" "$([ -z "$refs" ] && echo 1 || echo 0)"
	[ -z "$refs" ] || echo "$refs" | sed 's/^/        /'
	grep -vE '^\s*#' "$x/DEBIAN/postinst" | grep -E 'mapdata' | grep -vqE 'continue' && refs=bad || refs=""
	check "[$tag] postinst mentions mapdata only to EXCLUDE it from the ownership migration" "$([ -z "$refs" ] && echo 1 || echo 0)"
}

if [ "$MODE" = deb ]; then
	inspect /debs/candidate.deb candidate
	echo "(candidate) $(dpkg-deb -f /debs/candidate.deb Package Version Architecture | tr '\n' ' ')"
else
	for d in /debs/b-*.deb; do inspect "$d" "$(basename "$d" .deb)"; done
	# Source-level counterpart of the 'nothing root runs references the writable dirs' check:
	# the Go daemon (root) must not exec anything from mapdata/, softrf/ or ogn/.
	ex=$(grep -rn 'exec\.Command' --include=*.go /src 2>/dev/null | grep -E 'mapdata|softrf|/ogn/' )
	check "no Go code exec()s a program from mapdata/, softrf/ or ogn/" "$([ -z "$ex" ] && echo 1 || echo 0)"
	# Same tree, four different builders: the archives must be IDENTICAL in path/type/mode/uid/gid.
	ref=$(ls /debs/b-*.deb | head -1); lst "$ref" | sort > /tmp/ref.lst; clst "$ref" | sort > /tmp/ref.clst
	same=1
	for d in /debs/b-*.deb; do
		lst "$d" | sort | cmp -s - /tmp/ref.lst || { same=0; echo "        data archive differs: $d"; }
		clst "$d" | sort | cmp -s - /tmp/ref.clst || { same=0; echo "        control archive differs: $d"; }
	done
	check "packages built as $(ls /debs/b-*.deb | sed 's#.*/b-##;s#\.deb##;s#-#:#' | tr '\n' ' ')have identical path/type/mode/uid/gid listings (builder-independent)" "$same"
	# Sensitivity: the old way of packaging the same tree must be caught.
	v=$(violations /debs/old-1001-1001.deb)
	check "SENSITIVITY: the pre-normalization package (plain dpkg-deb -b under uid 1001) is reported as violating the invariant ($(echo "$v" | grep -c 'not root-owned') non-root entries)" "$([ "$(echo "$v" | grep -c 'not root-owned')" -gt 100 ] && echo 1 || echo 0)"
	check "SENSITIVITY: ... and it really recorded the builder uid 1001 (so the fix is not vacuous)" "$([ "$(lst /debs/old-1001-1001.deb | awk '$2==1001 && $3==1001' | wc -l)" -gt 100 ] && echo 1 || echo 0)"
fi
exit $fail
CONTAINER
rc=${PIPESTATUS[0]}
n=$(grep -c '^PASS' "$OUT"); f=$(grep -c '^FAIL' "$OUT"); rm -f "$OUT"
echo "$n checks passed, $f failed"
[ "$n" -ge 10 ] && [ "$f" = 0 ] && [ "$rc" = 0 ] && exit 0
echo "FAIL: ownership archive test did not complete cleanly (container exit $rc)"; exit 1
