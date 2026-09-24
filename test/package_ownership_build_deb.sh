#!/bin/bash
# package_ownership_build_deb.sh: builds a stratux .deb with the REAL `make dpkg` recipe
# from a source tree, in a throw-away Debian 12 container, running the build as an
# arbitrary non-root uid:gid. Only the compiled artifacts are stubbed (placeholder
# files where the Go/C binaries would be, `make -o all`); everything the package
# ownership depends on - the staging tree, the copies, the modes, the maintainer
# scripts, the units and the dpkg-deb invocation - is the repository's own recipe.
#
#   test/package_ownership_build_deb.sh SRC_TREE OUT_DEB UID:GID [VERSION]
#
# With LEGACY_DEB=/path the same staging tree is ALSO repackaged the way the recipe
# built it BEFORE ownership normalization (plain `dpkg-deb -b`, the builder uid in the
# archive, and the pre-normalization postinst from test/fixtures), at VERSION_LEGACY
# (default: lower than VERSION), for upgrade/downgrade tests.
#
# Needs Docker and network (once, to build the cached test image). Never touches a device.
set -eu
SRC=${1:?usage: $0 SRC_TREE OUT_DEB UID:GID [VERSION]}
OUT=${2:?}
IDS=${3:?}
VERSION=${4:-9.9.9~ownership}
HERE=$(cd "$(dirname "$0")" && pwd)
command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1 || { echo "SKIP: docker not available"; exit 77; }
SRC=$(readlink -f "$SRC")
OUTDIR=$(dirname "$(readlink -f "$OUT")"); OUTNAME=$(basename "$OUT")
. "$HERE/package_ownership_image.sh"; ensure_pkgtest_image
docker run --rm -i -v "$SRC":/src:ro -v "$OUTDIR":/out -v "$HERE/fixtures/package_ownership":/fx:ro \
	-e HOSTIDS="$(id -u):$(id -g)" -e IDS="$IDS" -e VERSION="$VERSION" -e OUTNAME="$OUTNAME" \
	-e LEGACY_NAME="${LEGACY_DEB:+$(basename "$LEGACY_DEB")}" -e VERSION_LEGACY="${VERSION_LEGACY:-9.9.8~ownership-legacy}" \
	"$IMG" bash -s <<'CONTAINER'
set -eu
u=${IDS%:*}; g=${IDS#*:}
install -d -o "$u" -g "$g" -m 755 /work
setpriv --reuid="$u" --regid="$g" --clear-groups bash -eu <<'BUILDER'
umask 022
cd /work
cp -r /src/Makefile /src/debian /src/scripts /src/web /src/softrf /src/ogn .
mkdir mapdata && cp /src/mapdata/download_mapdata.sh mapdata/ && printf 'stub mbtiles\n' > mapdata/osm.mbtiles
rm -f ogn/ogn-rx-eu_*; cp ogn/ddb.json.copy ogn/ddb.json
# stand-ins for the compiled artifacts (`make -o all` skips building them)
mkdir dump1090 rtl-ais
for f in stratuxrun fancontrol epaperd dump1090/dump1090 rtl-ais/rtl_ais ogn/ogn-rx-eu_x86 libdump978.so; do printf 'stub %s\n' "$f" > "$f"; done
make -o all VERSIONSTR="$VERSION" ARCH=amd64 dpkg >/tmp/make.log 2>&1 || { cat /tmp/make.log; exit 1; }
if [ -n "${LEGACY_NAME:-}" ]; then
	# The pre-normalization package: same tree, plain dpkg-deb -b (builder uid recorded),
	# old postinst, lower version.
	cp /fx/postinst.pre-normalization /tmp/dpkg-stratux/stratux/DEBIAN/postinst
	chmod 755 /tmp/dpkg-stratux/stratux/DEBIAN/postinst
	sed -i "s/^Version: .*/Version: $VERSION_LEGACY/" /tmp/dpkg-stratux/stratux/DEBIAN/control
	dpkg-deb -b /tmp/dpkg-stratux/stratux /work/legacy.deb >/dev/null
fi
BUILDER
cp "/work/stratux-$VERSION-amd64.deb" "/out/$OUTNAME"
[ -z "${LEGACY_NAME:-}" ] || cp /work/legacy.deb "/out/$LEGACY_NAME"
chown "$HOSTIDS" "/out/$OUTNAME" ${LEGACY_NAME:+"/out/$LEGACY_NAME"}
CONTAINER
[ -s "$OUT" ] || { echo "FAIL: no package produced"; exit 1; }
echo "built $OUT (builder $IDS)"
