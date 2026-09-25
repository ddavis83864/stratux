#!/bin/bash
# genprovenance.sh: emits a JSON build-provenance record for a release
# artifact set. Every field is either read from the local git checkout
# or passed in via environment variables the calling CI workflow
# already has (GitHub Actions' own context) - no network access, no
# credentials, no build-host filesystem paths.
#
# Required environment variables (all provided automatically by
# .github/workflows/release.yml when run there):
#   STRATUX_REPO_URL       - e.g. https://github.com/ddavis83864/stratux
#   STRATUX_TAG            - e.g. v2.0.0-rc1
#   STRATUX_RUN_ID         - GitHub Actions run id
#   STRATUX_RUN_URL        - link to the run
#   STRATUX_RUNNER_ARCH    - e.g. arm64
#   STRATUX_RUNNER_OS      - e.g. Linux
# Everything else (commit, toolchain versions, artifact names/sizes/
# hashes) is derived here.
#
# Usage: ./scripts/genprovenance.sh file1 [file2 ...] > provenance.json
set -euo pipefail

commit=$(git rev-parse HEAD)
go_version="unknown"
if command -v go >/dev/null 2>&1; then
	go_version=$(go version)
fi
dpkg_deb_version="unknown"
if command -v dpkg-deb >/dev/null 2>&1; then
	dpkg_deb_version=$(dpkg-deb --version | head -1)
fi

artifacts_json="[]"
if [ "$#" -gt 0 ]; then
	entries=()
	for f in "$@"; do
		if [ ! -f "$f" ]; then
			echo "genprovenance.sh: warning: $f does not exist, skipping" >&2
			continue
		fi
		name=$(basename "$f")
		size=$(stat -c%s "$f" 2>/dev/null || stat -f%z "$f")
		sha=$(sha256sum "$f" | awk '{print $1}')
		entries+=("{\"name\":\"$name\",\"sizeBytes\":$size,\"sha256\":\"$sha\"}")
	done
	artifacts_json=$(printf '%s\n' "${entries[@]}" | paste -sd, -)
	artifacts_json="[$artifacts_json]"
fi

cat <<EOF
{
  "repository": "${STRATUX_REPO_URL:-unknown}",
  "tag": "${STRATUX_TAG:-unknown}",
  "commit": "$commit",
  "workflowRunId": "${STRATUX_RUN_ID:-unknown}",
  "workflowRunUrl": "${STRATUX_RUN_URL:-unknown}",
  "runnerArchitecture": "${STRATUX_RUNNER_ARCH:-unknown}",
  "runnerOs": "${STRATUX_RUNNER_OS:-unknown}",
  "toolchain": {
    "go": "$go_version",
    "dpkgDeb": "$dpkg_deb_version"
  },
  "generatedAtUtc": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "artifacts": $artifacts_json
}
EOF
