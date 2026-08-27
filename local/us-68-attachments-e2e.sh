#!/usr/bin/env bash
# Epic 68 US-68.6 — cluster-level attachment e2e rows (E2, E10, E11).
#
# Complements local/test.sh (same harness conventions: kind cluster,
# port-forwarded API, postgres-seeded users/API keys, kubectl exec into
# the workspace pod).
#
#   E2  — Persistence: upload → suspend → resume → file still present,
#         byte-identical (PVC survives the pod).
#   E10 — Multi-tenant: two users, two workspaces, simultaneous uploads —
#         cross-user upload denied (404), no cross-workspace path leakage.
#   E11 — Chaos: pod killed mid-upload → clean 5xx to the client; pod
#         restarts; retry succeeds; exactly one intact file, no .tmp.
#
# SIDECAR MODE GATE (as-built, D1): in agentd-sidecar deployments the
# sidecar's /workspace mount is read-only, so uploads fail cleanly with
# 5xx by design. When sidecar mode is detected this script asserts that
# clean-fail behavior and SKIPS E2/E10/E11 with an explicit message
# instead of failing — the rows require single-container mode
# (`controller.agentdSidecar.enabled=false`; executed weekly + on dispatch
# by .github/workflows/e2e-attachments-single-container.yml).
#
# Environment (same as local/test.sh):
#   CLUSTER_NAME  - kind cluster name (default llmsafespaces)
#   NS            - namespace (default llmsafespaces)
#   PORTFWD_PORT  - local port for the API port-forward (default 18081)
set -Eeuo pipefail

if [[ -t 0 ]]; then
    BOLD=$'\033[1m'; RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf "%s==>%s %s\n" "${CYAN}${BOLD}" "${RESET}" "$*"; }
ok()   { printf "%s ✓%s %s\n" "${GREEN}" "${RESET}" "$*"; }
warn() { printf "%s !%s %s\n" "${YELLOW}" "${RESET}" "$*" >&2; }
die()  { printf "%s ✗%s %s\n" "${RED}${BOLD}" "${RESET}" "$*" >&2; exit 1; }

CLUSTER_NAME="${CLUSTER_NAME:-llmsafespaces}"
CTX="kind-${CLUSTER_NAME}"
NS="${NS:-llmsafespaces}"
PORTFWD_PORT="${PORTFWD_PORT:-18081}"
USER_A="e2e-att-user-a"
USER_B="e2e-att-user-b"
KEY_A="lsp_e2eattusera0000000000000000000001"
KEY_B="lsp_e2eattuserb0000000000000000000002"
WS_A="e2e0a000-0000-0000-0000-0000000000a1"
WS_B="e2e0b000-0000-0000-0000-0000000000b2"

cleanup() {
    if [[ -n "${PF_PID:-}" ]]; then
        kill "${PF_PID}" 2>/dev/null || true
        wait "${PF_PID}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# Regression guard (PR #1099): the first real execution died on
# 'invalid input syntax for type uuid' because a seed carried a
# 9-char first group. Fail fast, at the source, before any cluster
# interaction — postgres would otherwise reject the seed INSERT
# minutes later.
uuid_ok() { [[ "$1" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]]; }
for _ws in "${WS_A}" "${WS_B}"; do
    uuid_ok "${_ws}" || die "seed ${_ws} is not a valid UUID (8-4-4-4-12 lowercase hex)"
done

kc() { kubectl --context "${CTX}" "$@"; }

wait_phase() { # ws phase timeout_s
    local ws="$1" want="$2" timeout_s="$3" i phase
    for ((i = 0; i < timeout_s; i += 3)); do
        phase=$(kc -n "${NS}" get workspace "${ws}" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [[ "${phase}" == "${want}" ]] && return 0
        sleep 3
    done
    warn "workspace ${ws} never reached phase=${want} (last: ${phase:-<empty>})"
    return 1
}

pod_of() { kc -n "${NS}" get workspace "$1" -o jsonpath='{.status.podName}'; }

upload() { # ws key filename localfile -> echoes "<http-status>TAB<body>"
    # Status+body travel together: callers capture via $(upload ...) and a
    # command substitution is a SUBSHELL — an UPLOAD_STATUS side-assignment
    # would never reach the parent (set -u then kills the run; found by the
    # first real single-container execution, run 33117487609).
    local ws="$1" key="$2" filename="$3" localfile="$4" extra="${5:-}"
    local out status
    out="$(mktemp)"
    status=$(curl -sm 60 ${extra} -X POST \
        -H "Authorization: Bearer ${key}" \
        -F "file=@${localfile};filename=${filename}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${ws}/uploads" \
        -o "${out}" -w "%{http_code}" || true)
    printf '%s\t' "${status}"
    cat "${out}" 2>/dev/null || true
    rm -f "${out}"
}

# upload_do captures upload() output and splits it into UPLOAD_STATUS/BODY.
upload_do() { # same args as upload
    local captured
    captured="$(upload "$@")"
    UPLOAD_STATUS="${captured%%$'\t'*}"
    BODY="${captured#*$'\t'}"
    [[ -n "${UPLOAD_STATUS}" ]]
}

exec_ws() { # ws cmd...
    local pod
    pod=$(pod_of "$1")
    shift
    kc -n "${NS}" exec "${pod}" -c workspace -- "$@"
}

seed_workspace() { # ws user_id
    local ws="$1" user_id="$2"
    kc -n "${NS}" delete workspace "${ws}" --ignore-not-found >/dev/null 2>&1 || true
    cat <<EOF | kc -n "${NS}" apply -f - >/dev/null
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: ${ws}
  labels:
    user-id: ${user_id}
spec:
  owner:
    userID: ${user_id}
  runtime: python:3.11
  storage:
    size: 1Gi
    accessMode: ReadWriteOnce
EOF
}

seed_user() { # user_id api_key ws
    local user_id="$1" api_key="$2" ws="$3"
    kc -n "${NS}" exec "${PGPOD}" -- env PGPASSWORD="${PG_PWD}" \
        psql -U llmsafespaces -d llmsafespaces -v ON_ERROR_STOP=1 -c "
INSERT INTO users (id, username, email, password_hash, role)
VALUES ('${user_id}', '${user_id}', '${user_id}@example.test', 'unused-by-api-key-auth', 'user')
ON CONFLICT (id) DO NOTHING;

INSERT INTO api_keys (id, user_id, key, name, active)
VALUES ('${user_id}-key', '${user_id}', '${api_key}', 'e2e-att-key', true)
ON CONFLICT (id) DO UPDATE SET key=EXCLUDED.key, active=true;

INSERT INTO workspaces (id, name, user_id, namespace, runtime, storage_size)
VALUES ('${ws}', 'e2e-att-${user_id}', '${user_id}', '${NS}', 'python:3.11', '1Gi')
ON CONFLICT (id) DO UPDATE SET name=EXCLUDED.name, user_id=EXCLUDED.user_id;
" >/dev/null
}

# -----------------------------------------------------------------------------
# Setup: probes, port-forward, users, workspaces
# -----------------------------------------------------------------------------
log "Epic 68 attachments e2e — API probes via port-forward"

kc -n "${NS}" port-forward svc/llmsafespaces-api "${PORTFWD_PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 10); do
    curl -sfm 1 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null 2>&1 && break
    sleep 1
done
curl -sfm 2 "http://127.0.0.1:${PORTFWD_PORT}/livez" >/dev/null || die "API /livez unreachable"

PGPOD=$(kc -n "${NS}" get pod -l app=postgres -o jsonpath='{.items[0].metadata.name}')
[[ -n "${PGPOD}" ]] || die "postgres pod not found"
PG_PWD="${PG_PWD:-$(kc -n "${NS}" get secret llmsafespaces-credentials -o jsonpath='{.data.postgres-password}' 2>/dev/null | base64 -d 2>/dev/null || echo changeme)}"

seed_user "${USER_A}" "${KEY_A}" "${WS_A}"
seed_user "${USER_B}" "${KEY_B}" "${WS_B}"
seed_workspace "${WS_A}" "${USER_A}"
seed_workspace "${WS_B}" "${USER_B}"
ok "users + workspaces seeded"

log "  waiting for ${WS_A} and ${WS_B} to reach Active"
wait_phase "${WS_A}" Active 180 || { kc -n "${NS}" describe workspace "${WS_A}"; die "${WS_A} timeout"; }
wait_phase "${WS_B}" Active 180 || { kc -n "${NS}" describe workspace "${WS_B}"; die "${WS_B} timeout"; }
ok "both workspaces Active"

POD_A=$(pod_of "${WS_A}")
[[ -n "${POD_A}" ]] || die "no pod for ${WS_A}"

# -----------------------------------------------------------------------------
# Sidecar mode gate (as-built D1): uploads require single-container mode.
# -----------------------------------------------------------------------------
SIDECAR_CONTAINERS=$(kc -n "${NS}" get pod "${POD_A}" -o jsonpath='{.spec.containers[*].name}')
if [[ "${SIDECAR_CONTAINERS}" == *"agentd"* ]]; then
    warn "agentd SIDECAR mode detected — /workspace is read-only in the sidecar (design epic-68 D1 as-built)."
    log "Sidecar clean-fail assertion: upload must fail 5xx and write nothing"
    printf 'sidecar mode: uploads unavailable\n' > /tmp/us67-sidecar.txt
    upload_do "${WS_A}" "${KEY_A}" "sidecar.txt" /tmp/us67-sidecar.txt
    case "${UPLOAD_STATUS}" in
        5*)
            ok "upload rejected cleanly with ${UPLOAD_STATUS} in sidecar mode"
            ;;
        *)
            die "sidecar-mode upload returned ${UPLOAD_STATUS} — expected clean 5xx (D1). Body: ${BODY}"
            ;;
    esac
    FILES=$(exec_ws "${WS_A}" ls /workspace/uploads 2>/dev/null || true)
    [[ -z "${FILES}" ]] || die "sidecar-mode upload wrote files despite RO mount: ${FILES}"
    ok "no files written in sidecar mode"
    warn "SKIPPED E2/E10/E11: these rows require single-container mode"
    warn "(helm: --set controller.agentdSidecar.enabled=false; runs weekly via e2e-attachments-single-container.yml)"
    exit 0
fi
ok "single-container mode confirmed (containers: ${SIDECAR_CONTAINERS})"

# -----------------------------------------------------------------------------
# E2 — Persistence: upload → suspend → resume → file present + identical
# -----------------------------------------------------------------------------
log "E2 — upload → suspend → resume → file persists on the PVC"
printf 'e2 persistence payload — epic 67\n' > /tmp/us67-e2.txt
E2_SUM=$(sha256sum /tmp/us67-e2.txt | cut -d' ' -f1)

upload_do "${WS_A}" "${KEY_A}" "notes-e2.txt" /tmp/us67-e2.txt
[[ "${UPLOAD_STATUS}" == "201" ]] || die "E2 upload returned ${UPLOAD_STATUS}: ${BODY}"
E2_PATH=$(printf '%s' "${BODY}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["path"])' 2>/dev/null || true)
[[ "${E2_PATH}" == /workspace/uploads/* ]] || die "E2 upload path unexpected: ${E2_PATH}"
ok "uploaded → ${E2_PATH}"

curl -sfm 10 -X POST -H "Authorization: Bearer ${KEY_A}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS_A}/suspend" >/dev/null \
    || die "suspend call failed"
wait_phase "${WS_A}" Suspended 180 || die "workspace never reached Suspended"
ok "suspended"

curl -sfm 30 -X POST -H "Authorization: Bearer ${KEY_A}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS_A}/activate" >/dev/null \
    || die "activate call failed"
wait_phase "${WS_A}" Active 240 || die "workspace never re-reached Active"
ok "resumed"

E2_POD_SUM=$(exec_ws "${WS_A}" sha256sum "${E2_PATH}" 2>/dev/null | cut -d' ' -f1 || true)
[[ "${E2_POD_SUM}" == "${E2_SUM}" ]] || die "E2: file missing or altered after resume (pod sha ${E2_POD_SUM:-<gone>}, want ${E2_SUM})"
ok "file byte-identical after suspend/resume (sha256 ${E2_SUM:0:12}…)"

# -----------------------------------------------------------------------------
# E10 — Multi-tenant: simultaneous uploads, cross-user denial, no leakage
# -----------------------------------------------------------------------------
log "E10 — two users, two workspaces, simultaneous uploads"
printf 'tenant A payload\n' > /tmp/us67-a.txt
printf 'tenant B payload\n' > /tmp/us67-b.txt

# Simultaneous uploads (concurrency exercise). Each upload() call mktemps
# its own response file, so the in-flight calls cannot race; the assertions
# come from the sequential re-issues below (which also capture the paths).
upload "${WS_A}" "${KEY_A}" "tenant-a.txt" /tmp/us67-a.txt >/dev/null &
UP_A_PID=$!
upload "${WS_B}" "${KEY_B}" "tenant-b.txt" /tmp/us67-b.txt >/dev/null &
UP_B_PID=$!
wait "${UP_A_PID}" || true
wait "${UP_B_PID}" || true

# Sequential re-issues pin the per-tenant statuses and capture the paths.
upload_do "${WS_A}" "${KEY_A}" "tenant-a.txt" /tmp/us67-a.txt
[[ "${UPLOAD_STATUS}" == "201" ]] || die "E10: user A upload returned ${UPLOAD_STATUS}: ${BODY}"
PATH_A=$(printf '%s' "${BODY}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["path"])' 2>/dev/null || true)
upload_do "${WS_B}" "${KEY_B}" "tenant-b.txt" /tmp/us67-b.txt
[[ "${UPLOAD_STATUS}" == "201" ]] || die "E10: user B upload returned ${UPLOAD_STATUS}: ${BODY}"
PATH_B=$(printf '%s' "${BODY}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["path"])' 2>/dev/null || true)
ok "both tenants uploaded (${PATH_A}, ${PATH_B})"

upload_do "${WS_B}" "${KEY_A}" "evil.txt" /tmp/us67-a.txt
[[ "${UPLOAD_STATUS}" == "403" ]] || die "E10: cross-user upload returned ${UPLOAD_STATUS} (want 403): ${BODY}"
ok "cross-user upload denied (403)"

LEAK=$(exec_ws "${WS_A}" sh -c "ls /workspace/uploads | grep -v notes-e2 | grep -v tenant-a || true")
[[ -z "${LEAK}" ]] || die "E10: workspace A contains foreign files: ${LEAK}"
ok "no cross-workspace file leakage"

# -----------------------------------------------------------------------------
# E11 — Chaos: pod killed mid-upload → clean 5xx; retry; exactly one file
# -----------------------------------------------------------------------------
log "E11 — pod killed mid-upload"
dd if=/dev/zero of=/tmp/us67-chaos.bin bs=1M count=6 2>/dev/null

curl -sm 120 --limit-rate 300k -X POST \
    -H "Authorization: Bearer ${KEY_A}" \
    -F "file=@/tmp/us67-chaos.bin;filename=chaos.bin" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS_A}/uploads" \
    -o /tmp/us67-chaos-resp.json -w "%{http_code}" > /tmp/us67-chaos-status.txt &
CHAOS_PID=$!
sleep 4
POD_BEFORE=$(pod_of "${WS_A}")
kc -n "${NS}" delete pod "${POD_BEFORE}" --wait=false >/dev/null
log "  deleted pod ${POD_BEFORE} mid-upload"
wait "${CHAOS_PID}" || true
CHAOS_STATUS=$(cat /tmp/us67-chaos-status.txt)
case "${CHAOS_STATUS}" in
    5*|000)
        ok "client saw clean failure (${CHAOS_STATUS})"
        ;;
    201)
        # The upload finished before the pod died — acceptable timing
        # outcome; the assertions below still hold (retry, one intact file).
        warn "upload completed before the kill took effect (got 201); continuing"
        ;;
    *)
        die "E11: unexpected mid-kill status ${CHAOS_STATUS}: $(cat /tmp/us67-chaos-resp.json 2>/dev/null)"
        ;;
esac

wait_phase "${WS_A}" Active 300 || die "workspace pod never came back after kill"
ok "pod restarted and workspace Active again"

upload_do "${WS_A}" "${KEY_A}" "chaos.bin" /tmp/us67-chaos.bin
[[ "${UPLOAD_STATUS}" == "201" ]] || die "E11: retry upload returned ${UPLOAD_STATUS}: ${BODY}"
ok "retry succeeded after restart"

TMP_FILES=$(exec_ws "${WS_A}" sh -c 'ls /workspace/uploads/*.tmp 2>/dev/null || true')
[[ -z "${TMP_FILES}" ]] || die "E11: .tmp residue on disk: ${TMP_FILES}"
ok "no .tmp residue"

CHAOS_COUNT=$(exec_ws "${WS_A}" sh -c 'ls /workspace/uploads | grep -c "^.*-chaos.bin$" || true')
# 1 if the mid-flight upload aborted (expected), 2 if it completed AND the
# retry stored a second uuid — both are contract-legal (D19: retry = new
# uuid); what is NEVER legal is a partial file or residue.
[[ "${CHAOS_COUNT}" == "1" || "${CHAOS_COUNT}" == "2" ]] || die "E11: unexpected chaos.bin count ${CHAOS_COUNT}"
INTACT=$(exec_ws "${WS_A}" sh -c 'for f in /workspace/uploads/*-chaos.bin; do [ "$(stat -c%s "$f")" = "6291456" ] || exit 1; done; echo intact')
[[ "${INTACT}" == "intact" ]] || die "E11: partial (non-6MiB) chaos.bin present — atomicity violated"
ok "all stored chaos.bin copies are complete 6 MiB files (atomic-or-absent)"

# -----------------------------------------------------------------------------
log "E2/E10/E11 complete — all green"
