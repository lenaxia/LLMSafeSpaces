#!/usr/bin/env bash
# Smoke test for the base runtime image. Reports each required binary/tool
# individually and runs ALL checks (does not exit on the first failure) so a
# CI failure pinpoints the exact missing component instead of a bare exit 1.
#
# Exit non-zero only if one or more HARD-required checks fail. JVM tools
# (java/maven/gradle) are SOFT: the Dockerfile installs them best-effort, but
# mise's java plugin cannot always pre-install on every architecture. They are
# available on-demand via mise at runtime and never fail this build.
set -uo pipefail

FAILED=()

record() {
	# record <status> <label>: print a line and, on FAIL, append to FAILED.
	if [ "$1" = "OK" ]; then
		echo "OK   $2"
	else
		echo "FAIL $2"
		FAILED+=("$2")
	fi
}

# verify <label> <cmd...>: run a command; record OK/FAIL.
verify() {
	local label="$1"; shift
	if "$@" >/dev/null 2>&1; then
		record OK "$label"
	else
		record FAIL "$label"
	fi
}

# verify_any <label> <command-string>: pass if the compound command (with ||)
# succeeds. Used for fallbacks like `mise which pip || mise which pip3`.
verify_any() {
	local label="$1"; shift
	if sh -c "$*" >/dev/null 2>&1; then
		record OK "$label"
	else
		record FAIL "$label"
	fi
}

echo "=== mise tools installed (diagnostic) ==="
mise ls 2>/dev/null || echo "(mise ls unavailable)"
echo
echo "=== smoke checks ==="

# Internal binaries (built into the image)
verify redact           which redact
verify workspace-agentd which workspace-agentd

# Core shell + dev tools (apt)
verify bash    which bash
verify curl    which curl
verify file    which file
verify git     which git
verify_any gnupg "which gnupg || which gpg"
verify jq      which jq
verify less    which less
verify make    which make
verify gcc     which gcc
verify openssl which openssl
verify ps      which ps
verify rsync   which rsync
verify sqlite3 which sqlite3
verify ssh     which ssh
verify ssh-keygen which ssh-keygen
verify_any vim "which vim.tiny || which vim"
verify zstd   which zstd

# DB clients
verify psql  which psql
verify mysql which mysql

# Cloud CLIs
verify aws   which aws

# Agent runtime
verify opencode which opencode
verify mise     which mise

# GitHub CLI
verify gh       which gh

# Language runtimes (mise-managed, baked into the image layer — HARD required,
# matching the Dockerfile's FATAL-on-failure install list).
verify python mise which python
# Python on amd64 frequently ships pip3 without a `pip` shim; accept either.
verify_any pip "mise which pip || mise which pip3"
verify node  mise which node
verify npm   mise which npm
verify cargo mise which cargo
verify go    mise which go

# JVM tools are SOFT (best-effort pre-install; available via mise at runtime).
for t in java mvn gradle; do
	if mise which "$t" >/dev/null 2>&1; then
		echo "OK   $t"
	else
		echo "WARN $t not pre-installed (available via mise at runtime)"
	fi
done

echo
echo "=== summary ==="
if [ "${#FAILED[@]}" -eq 0 ]; then
	echo "All hard-required checks passed."
	exit 0
fi
echo "FAILED checks (${#FAILED[@]}):"
for f in "${FAILED[@]}"; do
	echo "  - $f"
done
exit 1
