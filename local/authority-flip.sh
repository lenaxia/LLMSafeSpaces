#!/usr/bin/env bash
# Copyright (C) 2026 Michael Kao
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Epic 69 US-69.13 (design 0055 M4) — the authority flip drill.
# Companion to docs/runbooks/authority-flip.md (ordered procedure,
# verification, rollback, alert triage live there; this is the
# committed mechanical half).
#
#   preflight <workspaceId> [--park]   GET in-flight count; non-zero
#                                      exit if >0 unless --park given
#   park <workspaceId> <reason>        hold in-flight outbox entries
#                                      (mode_transition; no auto-retry)
#   unpark <workspaceId>               re-arm the parks (rollback drain)
#   flip <on|off> [workspaceId]        preflight, then print (EXECUTE=1
#                                      to run) the helm step toggling
#                                      AGENTD_STATE_AUTHORITY
#   rollback <workspaceId>             flag off -> unpark; the ledger
#                                      back-drains via the 0052 path
#
# Env: API_BASE (default http://127.0.0.1:8080), ADMIN_TOKEN (required),
# RELEASE (lss), NS (llmsafespaces), EXECUTE (0 = dry-run print).
#
# The helm step assumes this script owns the api.extraEnv flag entries
# (indexes 0/1, V2 delivery first — AGENTD_STATE_AUTHORITY requires
# OPENCODE_V2_DELIVERY=1; ValidateDeliveryFlags makes the illegal combo
# a boot error). `off` writes value "0" rather than removing the entry —
# idempotent, no list surgery.

set -euo pipefail

API_BASE="${API_BASE:-http://127.0.0.1:8080}"
ADMIN_TOKEN="${ADMIN_TOKEN:?ADMIN_TOKEN (admin bearer) is required}"
RELEASE="${RELEASE:-lss}"
NS="${NS:-llmsafespaces}"
EXECUTE="${EXECUTE:-0}"

log() { printf '[authority-flip] %s\n' "$*"; }
ok() { printf '\033[32m[authority-flip] ok\033[0m — %s\n' "$*"; }
warn() { printf '\033[33m[authority-flip] warn\033[0m — %s\n' "$*" >&2; }
die() { printf '\033[31m[authority-flip] FAIL\033[0m — %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
usage: local/authority-flip.sh <command> [args]
  preflight <workspaceId> [--park]   check in-flight ledger deliveries
  inflight <workspaceId>            print the raw ledger_in_flight count
  park <workspaceId> <reason>        park in-flight outbox entries
  unpark <workspaceId>               re-arm parked entries (rollback)
  flip <on|off> [workspaceId]        toggle AGENTD_STATE_AUTHORITY
  rollback <workspaceId>             flag off + unpark (0052 back-drain)
env: API_BASE ADMIN_TOKEN RELEASE NS EXECUTE
see: docs/runbooks/authority-flip.md
EOF
}

admin_get() { curl -sfm 15 -H "Authorization: Bearer ${ADMIN_TOKEN}" "${API_BASE}${1}"; }

admin_post() { # path json
    curl -sfm 15 -X POST -H "Authorization: Bearer ${ADMIN_TOKEN}" \
        -H 'Content-Type: application/json' -d "$2" "${API_BASE}${1}"
}

inflight_count() { # workspaceId -> number
    local body
    body=$(admin_get "/api/v1/admin/authority/inflight/$1") \
        || die "in-flight read failed for $1 (API unreachable / auth rejected)"
    jq -r '.inFlight' <<<"$body"
}

do_preflight() { # workspaceId allow-park
    local n
    n=$(inflight_count "$1")
    if [ "$n" = "0" ]; then
        ok "preflight $1: ledger_in_flight=0 — safe to flip"
    elif [ "$2" = "1" ]; then
        warn "preflight $1: ledger_in_flight=${n} — park first: $0 park $1 <reason>"
    else
        die "preflight $1: ledger_in_flight=${n} (drain or park before flipping; --park to acknowledge)"
    fi
}

do_park() { # workspaceId reason
    local body
    body=$(admin_post /api/v1/admin/authority/park \
        "{\"workspaceId\":\"$1\",\"reason\":\"$2\"}") || die "park failed for $1 (outbox unreachable?)"
    ok "parked $1: $(jq -c . <<<"$body") (entries held visible; no auto-retry)"
}

do_unpark() { # workspaceId
    local body
    body=$(admin_post /api/v1/admin/authority/unpark \
        "{\"workspaceId\":\"$1\",\"reason\":\"rollback\"}") || die "unpark failed for $1"
    ok "unparked $1: $(jq -c . <<<"$body") (mode_transition parks re-armed to pending)"
}

helm_flip() { # on|off
    local mode="$1" auth
    auth=$([ "$mode" = "on" ] && echo 1 || echo 0)
    log "helm step (values form — the canonical procedure; never kubectl set env):"
    cat <<EOF
  api:
    extraEnv:
      - name: OPENCODE_V2_DELIVERY
        value: "1"
      - name: AGENTD_STATE_AUTHORITY
        value: "${auth}"
EOF
    local cmd=(helm upgrade "$RELEASE" helm -n "$NS" --reuse-values
        --set 'api.extraEnv[0].name=OPENCODE_V2_DELIVERY' --set 'api.extraEnv[0].value=1'
        --set 'api.extraEnv[1].name=AGENTD_STATE_AUTHORITY' --set "api.extraEnv[1].value=${auth}")
    if [ "$EXECUTE" = "1" ]; then
        ok "EXECUTE=1 — running: ${cmd[*]}"
        "${cmd[@]}"
        helm rollout status deployment/"${RELEASE}"-api -n "$NS" --timeout=300s
    else
        log "dry-run (EXECUTE=1 to execute): ${cmd[*]}"
    fi
}

do_flip() { # on|off [workspaceId]
    local mode="$1" ws="${2:-}"
    [ "$mode" = "on" ] || [ "$mode" = "off" ] || die "flip mode must be on|off (got: $mode)"
    if [ -n "$ws" ]; then
        do_preflight "$ws" 0
    else
        warn "no workspaceId given — skipping the in-flight preflight (pass one to check)"
    fi
    helm_flip "$mode"
}

do_rollback() { # workspaceId
    log "rollback $1: flag off first, then unpark — the ledger back-drains via the 0052 path (user-visible loss: none)"
    helm_flip off
    do_unpark "$1"
    log "verify: contract-events answers 501 (flag off); parked sends drain via the 0052 outbox path"
}

[ $# -ge 1 ] || { usage >&2; exit 2; }
cmd="$1"
shift
case "$cmd" in
preflight)
    [ $# -ge 1 ] || { usage >&2; die "preflight needs <workspaceId>"; }
    do_preflight "$1" "$([ "${2:-}" = "--park" ] && echo 1 || echo 0)"
    ;;
inflight)
    [ $# -ge 1 ] || { usage >&2; die "inflight needs <workspaceId>"; }
    ok "inflight $1: ledger_in_flight=$(inflight_count "$1")"
    ;;
park)
    [ $# -ge 2 ] || { usage >&2; die "park needs <workspaceId> <reason>"; }
    do_park "$1" "$2"
    ;;
unpark)
    [ $# -ge 1 ] || { usage >&2; die "unpark needs <workspaceId>"; }
    do_unpark "$1"
    ;;
flip)
    [ $# -ge 1 ] || { usage >&2; die "flip needs <on|off> [workspaceId]"; }
    do_flip "$1" "${2:-}"
    ;;
rollback)
    [ $# -ge 1 ] || { usage >&2; die "rollback needs <workspaceId>"; }
    do_rollback "$1"
    ;;
*)
    usage >&2
    die "unknown command: $cmd"
    ;;
esac
