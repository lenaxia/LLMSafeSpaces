#!/usr/bin/env bash
# pi-bump-gate.sh — the S6 pi fixture gate (the pi twin of the opencode
# #942 gate in ci.yml; scoping report §7 / story split S6).
#
# Usage: pi-bump-gate.sh <base_ref> <head_ref>
#
# pi's RPC surface has no protocol-version handshake — every decoder is
# pinned to fixtures captured against ARG PI_VERSION in
# runtimes/pi/Dockerfile (pkg/agent/pi/testdata/REFRESH.md). This gate
# makes the bump->refresh pairing mechanical: if the pinned VERSION
# changed between the two refs but NOTHING under pkg/agent/pi/testdata/
# changed, fail. The in-package drift gates (driftgate_test.go) then
# diff the refreshed fixtures against rpc_surface.json.
#
# A missing Dockerfile on the BASE ref (the pin landing for the first
# time) is a vacuous pass — the gate cannot compare what does not exist
# yet; the head's landing PR carries the initial fixtures by
# construction.
set -uo pipefail

BASE="${1:?usage: pi-bump-gate.sh <base_ref> <head_ref>}"
HEAD="${2:?usage: pi-bump-gate.sh <base_ref> <head_ref>}"

pin_of() {
  git show "$1:runtimes/pi/Dockerfile" 2>/dev/null \
    | grep -oE '^ARG PI_VERSION=[0-9.]+' | head -1 | cut -d= -f2
}

OLD="$(pin_of "$BASE")"
NEW="$(pin_of "$HEAD")"
echo "pi pin: ${BASE}=${OLD:-none} ${HEAD}=${NEW:-none}"

if [ -n "$OLD" ] && [ -n "$NEW" ] && [ "$OLD" != "$NEW" ]; then
  if ! git diff --name-only "$BASE...$HEAD" -- 'pkg/agent/pi/testdata/' | grep -q .; then
    echo "::error::PI_VERSION bumped ${OLD} -> ${NEW} without a fixture refresh (pkg/agent/pi/testdata/). See the refresh runbook: pkg/agent/pi/testdata/REFRESH.md" >&2
    exit 1
  fi
fi

echo "pi fixture-freshness gate: ok"
