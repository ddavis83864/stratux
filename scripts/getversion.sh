#!/bin/bash
# getversion.sh: derives the version string embedded in stratuxrun
# (main.stratuxVersion, via the Makefile's LFLAGS) and used as the
# Debian package's own Version: field (Makefile's dpkg target does the
# same VERSION substitution). There is no separate hardcoded version
# constant anywhere else in the tree - this script's output IS the
# version, for every one of those consumers.
#
# The nearest reachable tag names the version (e.g. "v2.0.0-rc1" ->
# "2.0.0-rc1"), with one deliberate exception: a trailing prerelease
# suffix of the form "-rcN"/"-preN"/"-betaN"/"-alphaN" is translated
# from a hyphen to a tilde ("2.0.0-rc1" -> "2.0.0~rc1") before being
# used as the actual package/displayed version.
#
# Why: git tag names cannot contain "~" (`git check-ref-format` rejects
# it outright), but Debian's own version-comparison algorithm treats
# "~" and "-" very differently for exactly this case. Verified directly
# with dpkg --compare-versions:
#   2.0.0~rc1  <  2.0.0       (correct: a prerelease sorts BEFORE its
#                              own eventual final release)
#   2.0.0-rc1  >  2.0.0       (WRONG: a plain hyphen sorts AFTER it -
#                              a real "2.0.0" final release would then
#                              look like a downgrade from "2.0.0-rc1")
# So the git tag stays human-readable and git-legal ("v2.0.0-rc1"), and
# only the package-facing string gets the Debian-correct translation.
# A final, non-prerelease tag (e.g. "v2.0.0") is returned unchanged -
# nothing to translate.
VER=$(git describe --tags --abbrev=0)
if [ "${VER:0:1}" == "v" ]; then
	VER=${VER:1}
fi
# Translate a trailing -rcN/-preN/-betaN/-alphaN (case-insensitive) into
# the Debian-correct ~rcN/~preN/~betaN/~alphaN form - but ONLY for this
# fork's new three-part "X.Y.Z-rcN" semver-style tags. Deliberately
# does NOT touch the project's older, already-deployed "X.Y-preN"
# two-part convention (e.g. "2.0-pre5", still reachable as the nearest
# tag from older commits/branches) - that format's own already-observed,
# already-documented "2.0-pre5" string is out of scope for this fix and
# must not silently change underneath anything still building from it.
VER=$(echo "$VER" | sed -E 's/^([0-9]+\.[0-9]+\.[0-9]+)-((rc|pre|beta|alpha)[0-9]+)$/\1~\2/I')
echo $VER
