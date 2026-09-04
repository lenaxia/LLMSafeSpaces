#!/usr/bin/env bash
# Epic 69 pool evidence leg (US-69.13 follow-ups #1218/#1219): the two
# cluster-bound evidence rows the close-out routed to the delivery pool —
#
#   A. ADMISSION-ID MATRIX (#1219): local/spike-admission-id.sh against a
#      live pinned pod — baseline / fresh-unique / duplicate-reuse against
#      opencode's own V2 prompt endpoint. Outcome feeds the oracle
#      deletion rule (fresh-unique accept + duplicate 409 => delete).
#   B. AUTHORITY-FLIP OPERATIONAL DRILL (#1218): the park/unpark/in-flight
#      admin endpoints against the DEPLOYED API + a real pod's statusz
#      drain signal. The load-scale zero-loss verdict is pinned in-repo
#      (api/internal/services/outbox/rollback_drill_test.go); this leg
#      proves the operational path at cluster scale.
#
# Results append to /tmp/us69-evidence.txt (workflow artifact) and the
# step FAILS the pool on a probe error (not on a matrix outcome — the
# outcome is data; the disposition rule is applied by the tracker).
set -euo pipefail
cd "$(dirname "$0")/.."

# shellcheck source=local/lib/us70-common.sh
source local/lib/us70-common.sh

EVIDENCE="${US69_EVIDENCE:-/tmp/us69-evidence.txt}"
# A literal valid UUID (the workspaces.id column is uuid-typed): derived
# from WS_BASE's shape but hardcoded — string-mangling WS_BASE produced an
# invalid 12-char first group (pool run 33574913376).
WS_ADMIT="${WS_ADMIT:-e2e569ad-0000-4000-8000-000000000069}"
OC_PF_PORT="${OC_PF_PORT:-14096}"

log_evidence() { printf '%s\n' "$*" | tee -a "${EVIDENCE}"; }

harness_start

# G8: the pool's first registrant is admin; if a leg ordering changed
# that, promote through the DB the pool already owns.
ROLE=$(curl -sfm 10 "http://127.0.0.1:${PORTFWD_PORT}/api/v1/auth/me" \
    -H "Authorization: Bearer ${AUTH_TOKEN}" | jq -r '.user.role // .role // empty')
if [[ "${ROLE}" != "admin" ]]; then
    kc exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
        psql -U llmsafespaces -d llmsafespaces -c \
        "UPDATE users SET role='admin' WHERE id='${OWNER_ID}'" >/dev/null
    login_harness_user
    log_evidence "[setup] harness user promoted to admin (was: ${ROLE:-unknown})"
fi

log_evidence "=== Epic 69 evidence leg — $(date -u +%FT%TZ) ==="
log_evidence "[env] image_tag=${IMAGE_TAG:-ci} runtime=${RUNTIME_REF} opencode_pin=$(kubectl --context "${CTX}" -n "${NS}" get workspace -o jsonpath='{.items[0].status.imageTag}' 2>/dev/null | head -c 64)"

# ---------------------------------------------------------------------------
# A. Admission-ID matrix (#1219)
# ---------------------------------------------------------------------------
ok "A. admission-ID matrix: seeding ${WS_ADMIT}"
seed_workspace "${WS_ADMIT}"
wait_phase "${WS_ADMIT}" Active 240 || die "admission workspace never went Active"

PASSWORD=$(kc get secret "workspace-pw-${WS_ADMIT}" -o jsonpath='{.data.password}' | base64 -d)
[[ -n "${PASSWORD}" ]] || die "no workspace password secret"

# curl -f under pipefail would kill the script with curl's exit code
# before die can diagnose (pool run 33578936807: exit 22, no body).
# Capture code+body, retry once (session create races the just-Active
# pod's first readiness), and print the body on failure.
SESSION=""
for attempt in 1 2; do
    SB=$(mktemp)
    SC=$(curl -sm 15 -o "${SB}" -w '%{http_code}' -X POST "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS_ADMIT}/sessions/new" \
        -H "Authorization: Bearer ${AUTH_TOKEN}" -H "Content-Type: application/json" \
        -d '{"title":"us69 admission matrix"}' || echo 000)
    SESSION=$(jq -r '.sessionId // .id // empty' "${SB}" 2>/dev/null || true)
    if [[ "${SC}" == "200" || "${SC}" == "201" ]] && [[ -n "${SESSION}" && "${SESSION}" != "null" ]]; then
        rm -f "${SB}"; break
    fi
    [[ "${attempt}" == "2" ]] && die "session create failed (HTTP ${SC}): $(cat "${SB}" 2>/dev/null | head -c 300)"
    warn "session create HTTP ${SC}, retrying: $(head -c 200 "${SB}" 2>/dev/null)"
    rm -f "${SB}"; sleep 5
done
[[ -n "${SESSION}" && "${SESSION}" != "null" ]] || die "session create returned no id"

POD=$(kc get pod -l "llmsafespaces.dev/workspace=${WS_ADMIT}" -o jsonpath='{.items[0].metadata.name}')
[[ -n "${POD}" ]] || die "no pod for ${WS_ADMIT}"

kc port-forward "pod/${POD}" "${OC_PF_PORT}:4096" >/dev/null 2>&1 &
OC_PF_PID=$!
trap 'kill ${OC_PF_PID} 2>/dev/null || true' EXIT
for i in $(seq 1 20); do
    curl -sfm 1 -o /dev/null "http://127.0.0.1:${OC_PF_PORT}/" 2>/dev/null && break
    sleep 1
done

log_evidence "--- admission-ID matrix (pod ${POD}, session ${SESSION}) ---"
if bash local/spike-admission-id.sh 127.0.0.1 "${OC_PF_PORT}" "${PASSWORD}" "${SESSION}" 2>&1 | tee -a "${EVIDENCE}"; then
    ok "A. admission-ID matrix probe completed"
else
    die "admission-ID probe errored (harness failure, not a matrix outcome)"
fi
kill ${OC_PF_PID} 2>/dev/null || true

# ---------------------------------------------------------------------------
# B. Authority-flip operational drill (#1218)
# ---------------------------------------------------------------------------
ok "B. authority-flip operational drill"
# API_BASE must NOT carry the /api/v1 prefix — authority-flip.sh's
# admin_post paths already include it (first live run doubled it:
# POST /api/v1/api/v1/admin/authority/park -> 404 -> "outbox unreachable").
FLIP_ENV=(API_BASE="http://127.0.0.1:${PORTFWD_PORT}" ADMIN_TOKEN="${AUTH_TOKEN}")

INFLIGHT=$(curl -sfm 15 -H "Authorization: Bearer ${AUTH_TOKEN}"     "http://127.0.0.1:${PORTFWD_PORT}/api/v1/admin/authority/inflight/${WS_ADMIT}" | jq -r '.inFlight')
log_evidence "drain signal (ledger_in_flight on a live pod): ${INFLIGHT}"

PARK_OUT=$(env "${FLIP_ENV[@]}" bash local/authority-flip.sh park "${WS_ADMIT}" "pool evidence drill" 2>&1 | tee -a "${EVIDENCE}")
UNPARK_OUT=$(env "${FLIP_ENV[@]}" bash local/authority-flip.sh unpark "${WS_ADMIT}" 2>&1 | tee -a "${EVIDENCE}")

# The subcommands embed the JSON verdicts ({parked:N} / {unparked:N}).
P_N=$(grep -oE '"parked":[0-9]+' <<<"${PARK_OUT}" | grep -oE '[0-9]+' || echo "?")
U_N=$(grep -oE '"unparked":[0-9]+' <<<"${UNPARK_OUT}" | grep -oE '[0-9]+' || echo "?")
[[ "${P_N}" != "?" && "${U_N}" != "?" ]] || die "park/unpark output lacked JSON verdicts"
[[ "${P_N}" == "${U_N}" ]] || die "park/unpark round-trip mismatch (${P_N} vs ${U_N})"
log_evidence "round-trip: parked=${P_N} unparked=${U_N} (consistent)"

# Post-drain: in-flight is readable and the workspace still Active.
wait_phase "${WS_ADMIT}" Active 60 || die "workspace left Active during the drill"
log_evidence "--- flip drill PASS: endpoints live, drain signal readable, round-trip consistent ---"

log_evidence "=== Epic 69 evidence leg complete ==="
ok "Epic 69 evidence leg complete (results: ${EVIDENCE})"
