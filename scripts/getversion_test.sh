#!/bin/bash
# getversion_test.sh: regression test for getversion.sh's tag-to-version
# translation - in particular, the release-candidate tilde translation
# added for v2.0.0-rc1 (see getversion.sh's own doc comment for the
# full rationale: git tags cannot contain "~", but Debian's version
# comparison needs it for a prerelease to correctly sort before its own
# final release).
#
# Runs getversion.sh against a disposable, throwaway git repository
# tagged with each case below - never against this actual repository's
# own tag history, so it is unaffected by (and never affects) whatever
# tag is nearest in a real checkout.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GETVERSION="$SCRIPT_DIR/getversion.sh"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

fail=0

check() {
	local tag="$1" want="$2"
	repo="$TMPDIR/repo-$tag"
	mkdir -p "$repo"
	(
		cd "$repo"
		git init -q
		git config user.email test@example.invalid
		git config user.name test
		git commit -q --allow-empty -m "init"
		git tag "$tag"
	)
	got=$(cd "$repo" && "$GETVERSION")
	if [ "$got" != "$want" ]; then
		echo "FAIL: tag $tag -> got %q$got%q, want %q$want%q" | tr '%' '"'
		fail=1
	else
		echo "ok: tag $tag -> $got"
	fi
}

# New three-part semver RC tags: hyphen -> tilde translation applies.
check "v2.0.0-rc1" "2.0.0~rc1"
check "v2.0.0-rc12" "2.0.0~rc12"
check "v2.0.0-beta3" "2.0.0~beta3"
check "v2.0.0-alpha1" "2.0.0~alpha1"

# A final, non-prerelease three-part tag: unchanged, no translation.
check "v2.0.0" "2.0.0"
check "v2.1.3" "2.1.3"

# The project's older, already-deployed two-part "X.Y-preN" convention:
# deliberately left untouched by this fix - see getversion.sh's own
# comment on why.
check "v2.0-pre5" "2.0-pre5"
check "v2.0-pre" "2.0-pre"

# A tag with no leading "v": the leading-"v" strip is a no-op, and the
# translation still applies where the shape matches.
check "2.0.0-rc1" "2.0.0~rc1"

if [ "$fail" -ne 0 ]; then
	echo "getversion_test.sh: FAILED"
	exit 1
fi
echo "getversion_test.sh: all cases passed"
