#!/usr/bin/env bash
# Epic 70 US-70.2 — two-tier revision / conditional-pull cluster e2e rows.
#
# Runs on the nightly (after the US-70.1 delivery rows) and on the US-70.0
# delivery pool (BEFORE the fault-seam arming — this suite POSTs
# pod-bootstrap repeatedly and would burn the 8-fault budget). Same
# conventions as local/us-70-secret-delivery-e2e.sh /
# local/us-70-faults-e2e.sh; shared helpers + env contract in
# lib/us70-common.sh. Rows:
#
#   REV-0 — contract feature-detect (W15 mixed-fleet gate): POST
#           pod-bootstrap with contractVersion:2 through a
#           kubectl-minted SA token (the #1187 F4 mint technique); a 200
#           whose .secrets carries a numeric .revision.seq means the v2
#           envelope contract is live. A bare-array .secrets means the
#           deployed API predates US-70.2 → warn + loud-skip ALL rows
#           (exit 0 — a mixed fleet / pre-merge main running this suite
#           stays green, never a silent pass).
#   REV-1 — identical revisions from any replica (AC-13 fragment, I2/I6):
#           scale the API to 2 replicas, port-forward each api POD
#           directly (18091/18092 — distinct from the main service
#           forward), POST the same v2 bootstrap through both → both 200,
#           identical .secrets.revision (seq+manifestHash+batchHash) and
#           identical canonicalized entries. The DB revision row is the
#           single seq writer; replicas only read it, so identical row
#           state must yield identical revisions on any replica.
#   REV-2 — conditional pull: v2 WITHOUT clientManifestHash → 200
#           envelope; WITH the current manifestHash → HTTP 304 + ETag
#           "<seq>:<manifestHash>" + empty body; after a fresh bind the
#           OLD hash → 200 (not 304) with a STRICTLY greater seq
#           (monotonic mint); the NEW hash → 304 again.
#   REV-3 — anchored spawned_rev (I4): force a revisioned re-pull (pod
#           delete → recreate → boot bootstrap; a live bind converges via
#           the legacy push, which intentionally clears the anchor —
#           cmd/workspace-agentd/secrets.go reloadSecretsHandler), then
#           assert .status.secretsDelivery.spawnedRev is the anchored
#           <seq>:<manifestHash>:<contentHash> form — exactly 3
#           colon-separated components, the FIRST equal to REV-2's final
#           seq (string compare on cut -d: -f1) — and the bound var is
#           present in the child env.
#   REV-4 — one builder, one truth (design 0052 Phase-2 acceptance,
#           API-side): after a fresh bind + convergence (the bind-push
#           path built its batch through the one builder), a pull-path
#           re-pull (200) CONTAINS the just-bound var's entry and its
#           revision seq >= REV-2's final seq. (Pod-side push/pull
#           byte-identity beyond this is pinned by Go tests; this row
#           pins the API-side truth.)
#
# Environment (beyond lib/us70-common.sh):
#   WS_BASE         - distinct workspace prefix (default e2esrev0-…; the
#                     longest CR name is 31 chars, inside the ≤33-char
#                     audit-truncation budget)
#   REV_PF_PORT_1 / REV_PF_PORT_2 - local ports for the two api-pod
#                     port-forwards (default 18091/18092 — distinct from
#                     PORTFWD_PORT so the row tolerates the main harness
#                     forward instead of colliding with it)
set -Eeuo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$SCRIPT_DIR/lib/us70-common.sh"

WS_BASE="${WS_BASE:-e2e57000-0000-4000-8000-000000000000}"
REV_PF_PORT_1="${REV_PF_PORT_1:-18091}"
REV_PF_PORT_2="${REV_PF_PORT_2:-18092}"

PASS=0
SKIP=0
declare -a SKIPPED_ROWS=()

skip_row() { # row msg...
    local row="$1"; shift
    warn "SKIPPED ${row}: $*"
    SKIP=$((SKIP + 1))
    SKIPPED_ROWS+=("${row}")
}

# Row-scoped pod port-forwards (the lib's cleanup only knows the main
# service forward PF_PID). API_SCALED guards the replica restore on a
# mid-row die so the fleet is never left at 2 replicas.
declare -a REV_PF_PIDS=()
API_SCALED=0

rev_cleanup() {
    local pid
    for pid in "${REV_PF_PIDS[@]:-}"; do
        [[ -n "${pid}" ]] || continue
        kill "${pid}" 2>/dev/null || true
        wait "${pid}" 2>/dev/null || true
    done
    if (( API_SCALED )); then
        kc scale deployment/llmsafespaces-api --replicas=1 >/dev/null 2>&1 || true
    fi
    cleanup   # the lib's EXIT body (main service port-forward)
}
trap rev_cleanup EXIT

# mint_token ws — fresh SA token for the workspace bootstrap SA, API
# audience (the real TokenReview path; api/internal/handlers/
# pod_bootstrap.go validates minted tokens exactly like projected ones).
mint_token() {
    kubectl --context "${CTX}" -n "${NS}" create token "workspace-$1" \
        --audience=llmsafespace-api
}

v2_body() { # ws [clientManifestHash] → v2 bootstrap request JSON
    if [[ $# -ge 2 && -n "$2" ]]; then
        jq -nc --arg w "$1" --arg h "$2" '{workspaceID:$w,contractVersion:2,clientManifestHash:$h}'
    else
        jq -nc --arg w "$1" '{workspaceID:$w,contractVersion:2}'
    fi
}

# bootstrap_post port token body hdrfile bodyfile → echoes the HTTP
# status; headers land in hdrfile (ETag), body in bodyfile.
bootstrap_post() {
    curl -sm 30 -D "$4" -o "$5" -w '%{http_code}' -X POST \
        -H "Authorization: Bearer $2" -H 'Content-Type: application/json' \
        -d "$3" "http://127.0.0.1:$1/internal/v1/pod-bootstrap" || true
}

etag_of() { # hdrfile → the ETag header value exactly as served (quoted)
    tr -d '\r' <"$1" | awk 'tolower($1)=="etag:" {sub(/^[^:]*:[ \t]*/, ""); print; exit}'
}

wait_local_livez() { # port
    local i
    for i in $(seq 1 20); do
        curl -sfm 2 "http://127.0.0.1:$1/livez" >/dev/null 2>&1 && return 0
        sleep 1
    done
    return 1
}

# pf_pod local_port pod — background per-pod port-forward, tracked so the
# cleanup trap tears it down alongside the lib's main forward.
pf_pod() {
    kc port-forward "pod/$2" "$1:8080" >/dev/null 2>&1 &
    REV_PF_PIDS+=($!)
}

# ensure_api_reachable — scaling the API 2→1 may delete the exact pod the
# MAIN service port-forward targets; re-establish it when /livez died.
ensure_api_reachable() {
    local i
    for i in $(seq 1 5); do
        curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1 && return 0
        if [[ -n "${PF_PID:-}" ]]; then
            kill "${PF_PID}" 2>/dev/null || true
            wait "${PF_PID}" 2>/dev/null || true
        fi
        kc -n "${NS}" port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
        PF_PID=$!
        sleep 2
    done
    curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1
}

TMPD=$(mktemp -d)

total_start=$(date +%s%3N)
harness_start

# -----------------------------------------------------------------------------
# REV-1 setup + REV-0 probe — the probe needs a workspace (the minted SA
# belongs to workspace-<name>), so it runs inline in REV-1's workspace.
# -----------------------------------------------------------------------------
WS1=$(ws_id 1)
log "REV-1 setup — create + bind workspace ${WS1}, wait Active + converged"
seed_workspace "${WS1}" "${USER_ID}"
bind_env "${WS1}" "SD_REV1" "rev1-first-value"
wait_phase "${WS1}" Active 240 || die "REV-1: workspace never Active"
secrets_converged "${WS1}" 120 || die "REV-1: secretsDelivery not healthy/converged"

TOKEN=$(mint_token "${WS1}") || die "REV-0: kubectl create token failed (needs create serviceaccounts/token RBAC)"
[[ -n "${TOKEN}" ]] || die "REV-0: minted an empty token"

log "REV-0 — contract feature-detect: v2 pod-bootstrap via the minted SA token"
CODE=$(bootstrap_post "${PORTFWD_PORT}" "${TOKEN}" "$(v2_body "${WS1}")" "${TMPD}/rev0.hdr" "${TMPD}/rev0.json")
[[ "${CODE}" == "200" ]] \
    || die "REV-0: v2 bootstrap probe returned ${CODE}: $(head -c 300 "${TMPD}/rev0.json")"

CONTRACT_LIVE=false
if jq -e '.secrets.revision.seq | type == "number"' "${TMPD}/rev0.json" >/dev/null 2>&1; then
    ok "REV-0 PASS: envelope contract live (revision=$(jq -c '.secrets.revision' "${TMPD}/rev0.json"))"
    PASS=$((PASS + 1))
    CONTRACT_LIVE=true
elif jq -e '.secrets | type == "array"' "${TMPD}/rev0.json" >/dev/null 2>&1; then
    warn "REV-0: the deployed API answered the v2 probe with today's bare-array secrets — it predates the US-70.2 contract"
else
    die "REV-0: .secrets is neither a revisioned envelope nor a bare array: $(head -c 300 "${TMPD}/rev0.json")"
fi

if [[ "${CONTRACT_LIVE}" != "true" ]]; then
    for ROW in REV-1 REV-2 REV-3 REV-4; do
        skip_row "${ROW}" "deployed API predates the US-70.2 bootstrap contract (bare-array probe) — mixed fleet / pre-merge main"
    done
    total_ms=$(( $(date +%s%3N) - total_start ))
    log "US-70.2 revisions suite complete — pass=${PASS} skip=${SKIP} fail=0 (${total_ms}ms)"
    warn "loud skips: ${SKIPPED_ROWS[*]}"
    warn "skipped rows are contract gates (the deploy must speak the v2 envelope), tracked above — never a silent pass"
    exit 0
fi

# -----------------------------------------------------------------------------
# REV-1 — identical revisions from any replica (scale to 2, both pods)
# -----------------------------------------------------------------------------
log "REV-1 — scale the API to 2 replicas → identical revision from BOTH pods"
kc scale deployment/llmsafespaces-api --replicas=2
API_SCALED=1
kc rollout status deployment/llmsafespaces-api --timeout=180s >/dev/null

# Same selector the faults suite's reconnect/scale-to-0 rows use
# (helm/templates/_helpers.tpl pins app.kubernetes.io/component: api).
mapfile -t API_PODS < <(kc get pods -l app.kubernetes.io/component=api \
    --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}' \
    | tr ' ' '\n' | grep -v '^$' | sort)
((${#API_PODS[@]} == 2)) \
    || die "REV-1 FAIL: expected exactly 2 Running api pods after scale-out, got ${#API_PODS[@]}: ${API_PODS[*]:-<none>}"
ok "api pods enumerated: ${API_PODS[*]}"

pf_pod "${REV_PF_PORT_1}" "${API_PODS[0]}"
pf_pod "${REV_PF_PORT_2}" "${API_PODS[1]}"
wait_local_livez "${REV_PF_PORT_1}" || die "REV-1: pod forward :${REV_PF_PORT_1} never reached /livez"
wait_local_livez "${REV_PF_PORT_2}" || die "REV-1: pod forward :${REV_PF_PORT_2} never reached /livez"

REV1_BODY=$(v2_body "${WS1}")
CODE_A=$(bootstrap_post "${REV_PF_PORT_1}" "${TOKEN}" "${REV1_BODY}" "${TMPD}/rev1a.hdr" "${TMPD}/rev1a.json")
CODE_B=$(bootstrap_post "${REV_PF_PORT_2}" "${TOKEN}" "${REV1_BODY}" "${TMPD}/rev1b.hdr" "${TMPD}/rev1b.json")
[[ "${CODE_A}" == "200" ]] || die "REV-1 FAIL: pod ${API_PODS[0]} returned ${CODE_A}: $(head -c 300 "${TMPD}/rev1a.json")"
[[ "${CODE_B}" == "200" ]] || die "REV-1 FAIL: pod ${API_PODS[1]} returned ${CODE_B}: $(head -c 300 "${TMPD}/rev1b.json")"

REV_A=$(jq -c '.secrets.revision' "${TMPD}/rev1a.json")
REV_B=$(jq -c '.secrets.revision' "${TMPD}/rev1b.json")
[[ -n "${REV_A}" && "${REV_A}" == "${REV_B}" ]] \
    || die "REV-1 FAIL: .secrets.revision diverged across replicas: ${REV_A:-<empty>} vs ${REV_B:-<empty>}"

jq -S '.secrets.entries | sort_by(.type, .secretId)' "${TMPD}/rev1a.json" >"${TMPD}/rev1a.canon"
jq -S '.secrets.entries | sort_by(.type, .secretId)' "${TMPD}/rev1b.json" >"${TMPD}/rev1b.canon"
cmp -s "${TMPD}/rev1a.canon" "${TMPD}/rev1b.canon" \
    || die "REV-1 FAIL: canonicalized entries diverged across replicas"
ok "REV-1 PASS: both replicas served identical revision ${REV_A} + identical canonicalized entries ($(jq '.secrets.entries | length' "${TMPD}/rev1a.json") entries)"
PASS=$((PASS + 1))

# Row teardown: kill the pod forwards, restore 1 replica, re-establish the
# main service forward if the scale-in deleted its backing pod.
for pid in "${REV_PF_PIDS[@]}"; do
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
done
REV_PF_PIDS=()
kc scale deployment/llmsafespaces-api --replicas=1
API_SCALED=0
kc rollout status deployment/llmsafespaces-api --timeout=180s >/dev/null
ensure_api_reachable || die "REV-1: main API port-forward unreachable after replica restore"
ok "REV-1: api restored to 1 replica, pod port-forwards torn down"

# -----------------------------------------------------------------------------
# REV-2 — conditional pull: 304/ETag, monotonic seq on change, 304 again
# -----------------------------------------------------------------------------
log "REV-2 — conditional pull: 200 envelope → 304+ETag → bind → 200 (seq bump) → 304"

CODE=$(bootstrap_post "${PORTFWD_PORT}" "${TOKEN}" "$(v2_body "${WS1}")" "${TMPD}/rev2a.hdr" "${TMPD}/rev2a.json")
[[ "${CODE}" == "200" ]] || die "REV-2: unconditional v2 pull returned ${CODE}"
REV2_SEQ1=$(jq -r '.secrets.revision.seq' "${TMPD}/rev2a.json")
REV2_MH1=$(jq -r '.secrets.revision.manifestHash' "${TMPD}/rev2a.json")
[[ "${REV2_SEQ1}" =~ ^[0-9]+$ ]] || die "REV-2: revision.seq not numeric (${REV2_SEQ1})"
[[ -n "${REV2_MH1}" ]] || die "REV-2: revision.manifestHash empty"

CODE=$(bootstrap_post "${PORTFWD_PORT}" "${TOKEN}" "$(v2_body "${WS1}" "${REV2_MH1}")" "${TMPD}/rev2b.hdr" "${TMPD}/rev2b.json")
[[ "${CODE}" == "304" ]] \
    || die "REV-2 FAIL: unchanged manifest + clientManifestHash → ${CODE}, expected exactly 304"
ETAG=$(etag_of "${TMPD}/rev2b.hdr")
[[ "${ETAG}" == "\"${REV2_SEQ1}:${REV2_MH1}\"" ]] \
    || die "REV-2 FAIL: 304 ETag '${ETAG}' != \"${REV2_SEQ1}:${REV2_MH1}\""
[[ ! -s "${TMPD}/rev2b.json" ]] || die "REV-2 FAIL: the 304 carried a body: $(cat "${TMPD}/rev2b.json")"
ok "REV-2: unchanged manifest → 304 + ETag \"${REV2_SEQ1}:${REV2_MH1:0:12}…\" + empty body"

bind_env "${WS1}" "SD_REV2" "rev2-second-value"
secrets_converged "${WS1}" 180 || die "REV-2: secretsDelivery not converged after bind"

CODE=$(bootstrap_post "${PORTFWD_PORT}" "${TOKEN}" "$(v2_body "${WS1}" "${REV2_MH1}")" "${TMPD}/rev2c.hdr" "${TMPD}/rev2c.json")
[[ "${CODE}" == "200" ]] \
    || die "REV-2 FAIL: stale clientManifestHash after a bind → ${CODE}, expected 200 (not 304)"
REV2_SEQ2=$(jq -r '.secrets.revision.seq' "${TMPD}/rev2c.json")
REV2_MH2=$(jq -r '.secrets.revision.manifestHash' "${TMPD}/rev2c.json")
[[ "${REV2_SEQ2}" =~ ^[0-9]+$ ]] || die "REV-2: post-bind revision.seq not numeric (${REV2_SEQ2})"
(( REV2_SEQ2 > REV2_SEQ1 )) \
    || die "REV-2 FAIL: seq not monotonic after a value-affecting bind (${REV2_SEQ2} ≤ ${REV2_SEQ1})"
[[ "${REV2_MH2}" != "${REV2_MH1}" ]] \
    || warn "REV-2: manifestHash unchanged across the bind (unexpected for a new env var)"

CODE=$(bootstrap_post "${PORTFWD_PORT}" "${TOKEN}" "$(v2_body "${WS1}" "${REV2_MH2}")" "${TMPD}/rev2d.hdr" "${TMPD}/rev2d.json")
[[ "${CODE}" == "304" ]] \
    || die "REV-2 FAIL: the new manifestHash re-presented → ${CODE}, expected 304 again"
ETAG=$(etag_of "${TMPD}/rev2d.hdr")
[[ "${ETAG}" == "\"${REV2_SEQ2}:${REV2_MH2}\"" ]] \
    || die "REV-2 FAIL: second 304 ETag '${ETAG}' != \"${REV2_SEQ2}:${REV2_MH2}\""
ok "REV-2 PASS: 304 → bind → 200 seq ${REV2_SEQ1}<${REV2_SEQ2} (monotonic mint) → 304"
PASS=$((PASS + 1))

# -----------------------------------------------------------------------------
# REV-3 — anchored spawned_rev (<seq>:<manifestHash>:<contentHash>)
# -----------------------------------------------------------------------------
# A live bind converges through the legacy push, and reload-secrets
# intentionally clears the rev anchor (the pushed set supersedes the pull
# it came from — W2). The anchored form is therefore observable only
# after a revisioned PULL: delete the pod (the F5b technique) so the
# recreation bootstraps from the API. The recreated pod pulls the current
# revision — EnsureRevision is idempotent on an unchanged manifest, so
# the anchored seq equals REV-2's final 200 seq exactly.
log "REV-3 — pod delete → revisioned re-pull → anchored spawnedRev with seq prefix ${REV2_SEQ2}"
POD1=$(pod_of "${WS1}")
[[ -n "${POD1}" ]] || die "REV-3: no pod to delete"
kc delete pod "${POD1}" --ignore-not-found >/dev/null 2>&1 || true
wait_phase "${WS1}" Active 240 || die "REV-3: workspace never re-Active after pod delete"
secrets_converged "${WS1}" 180 || die "REV-3: secretsDelivery not converged after pod recreate"

ANCHOR_OK=false
SPAWN_REV=""
for _i in $(seq 1 40); do
    SPAWN_REV=$(kc get workspace "${WS1}" -o jsonpath='{.status.secretsDelivery.spawnedRev}' 2>/dev/null || echo "")
    if [[ -n "${SPAWN_REV}" \
        && "$(awk -F: '{print NF}' <<<"${SPAWN_REV}")" == 3 \
        && "$(cut -d: -f1 <<<"${SPAWN_REV}")" =~ ^[0-9]+$ \
        && "$(cut -d: -f2 <<<"${SPAWN_REV}")" =~ ^[0-9a-f]+$ \
        && "$(cut -d: -f3 <<<"${SPAWN_REV}")" =~ ^[0-9a-f]+$ \
        && "$(cut -d: -f1 <<<"${SPAWN_REV}")" == "${REV2_SEQ2}" ]]; then
        ANCHOR_OK=true
        break
    fi
    sleep 3
done
[[ "${ANCHOR_OK}" == "true" ]] \
    || die "REV-3 FAIL: spawnedRev not the anchored <seq>:<manifestHash>:<contentHash> with seq=${REV2_SEQ2} (last: '${SPAWN_REV}')"
ok "REV-3: spawnedRev anchored (${SPAWN_REV:0:24}…; seq prefix $(cut -d: -f1 <<<"${SPAWN_REV}") == REV-2 final seq ${REV2_SEQ2})"

if env_in_child "${WS1}" "SD_REV2=rev2-second-value"; then
    ok "REV-3 PASS: anchored spawnedRev + SD_REV2 present in the child env"
    PASS=$((PASS + 1))
else
    die "REV-3 FAIL: SD_REV2 missing from the child env after the revisioned re-pull"
fi

# -----------------------------------------------------------------------------
# REV-4 — identical batch across delivery paths (pull vs bind-push)
# -----------------------------------------------------------------------------
log "REV-4 — fresh bind (push path) → pull-path re-pull contains the entry, seq >= ${REV2_SEQ2}"
bind_env "${WS1}" "SD_REV4" "rev4-cross-path-value"
secrets_converged "${WS1}" 180 || die "REV-4: secretsDelivery not converged after bind"

CODE=$(bootstrap_post "${PORTFWD_PORT}" "${TOKEN}" "$(v2_body "${WS1}")" "${TMPD}/rev4.hdr" "${TMPD}/rev4.json")
[[ "${CODE}" == "200" ]] || die "REV-4: pull-path re-pull returned ${CODE}"
REV4_ENTRY=$(jq -r --arg v "SD_REV4" \
    '.secrets.entries[] | select(.type == "env-secret" and ((.metadata.var_name // "") == $v)) | .name' \
    "${TMPD}/rev4.json" | head -1)
[[ -n "${REV4_ENTRY}" ]] \
    || die "REV-4 FAIL: the pulled envelope lacks the just-bound SD_REV4 entry (bind-push and pull paths disagree)"
REV4_SEQ=$(jq -r '.secrets.revision.seq' "${TMPD}/rev4.json")
[[ "${REV4_SEQ}" =~ ^[0-9]+$ ]] || die "REV-4: revision.seq not numeric (${REV4_SEQ})"
(( REV4_SEQ >= REV2_SEQ2 )) \
    || die "REV-4 FAIL: pulled seq ${REV4_SEQ} < REV-2 seq ${REV2_SEQ2} — the paths do not share the one revision row"
ok "REV-4 PASS: pull contains '${REV4_ENTRY}' (seq ${REV4_SEQ} ≥ ${REV2_SEQ2}) — one builder, one truth across paths"
PASS=$((PASS + 1))

# -----------------------------------------------------------------------------
total_ms=$(( $(date +%s%3N) - total_start ))
log "US-70.2 revisions suite complete — pass=${PASS} skip=${SKIP} fail=0 (${total_ms}ms)"
if (( ${#SKIPPED_ROWS[@]} > 0 )); then
    warn "loud skips: ${SKIPPED_ROWS[*]}"
    warn "skipped rows are contract gates (the deploy must speak the v2 envelope), tracked above — never a silent pass"
fi
