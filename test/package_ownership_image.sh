#!/bin/bash
# Sourced by the package ownership tests: builds (once, then cached) the Debian 12 test
# image: make, the runtime libraries the stratux package depends on, and ShellCheck.
ensure_pkgtest_image() {
	IMG=stratux-pkgtest:bookworm
	docker image inspect "$IMG" >/dev/null 2>&1 && return 0
	docker build -q -t "$IMG" - >/dev/null <<'DOCKERFILE'
FROM debian:bookworm
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends make libncurses6 librtlsdr0 openssh-client shellcheck >/dev/null && rm -rf /var/lib/apt/lists/*
DOCKERFILE
}
