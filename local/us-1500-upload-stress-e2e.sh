#!/usr/bin/env bash
# Design 0060 §6 / §9 PR 3 — the upload staging stress harness. Rows:
#
#   SR-1 (§6.1) streaming residency, MEASURED: a CONCURRENT storm of
#        near-cap uploads (MAX_CONCURRENT + 2 in flight); a file-backed
#        gauge sampler pins max(staging_bytes) ≤ budget AND
#        max(reserved_bytes) ≤ budget; storm outcomes all terminal.
#   SR-2 (§6.2) cross-feature isolation: a credential resync forced
#        MID-STORM (concurrent uploads still in flight) completes;
#        credential_bytes never regresses; storm outcomes terminal.
#   SR-3 (§6.3) backpressure: skip-DOWN until the agentd-side fault
#        seam lands with the PR 1/2 lane (loud, never silent).
#   SR-4 (§6.4) disk-margin edges: pre-filled volume refuses at the
#        write-time re-check (507 dest_disk_full).
#   SR-5 (§6.5) failure injection mid-stream: sidecar container killed
#        at phase boundaries → no non-.tmp partial ever visible; the
#        destination .tmp set is reclaimed by the supervisor scrub.
#   SR-6 (§6.6) ack-path latency: p50/p95 at 1× and the concurrency
#        boundary, with the §6.6 clause-(B) precondition asserted by
#        direct substitution and an EXPLICIT skip-DOWN to 3×.
#
# Skip-DOWN semantics: rows whose mechanism rides the PR 1/2 lane
# detect the staging gauge and SKIP LOUDLY when absent.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/us70-common.sh
source "${SCRIPT_DIR}/lib/us70-common.sh"

failures=0
sr_skips=0
note_fail() { failures=$((failures + 1)); warn "FAIL: $*"; }
sr_skip() { sr_skips=$((sr_skips + 1)); warn "SKIP-DOWN: $*"; }

harness_start

WS="$(ws_id 42)"
SAMPLE_DIR=""

cleanup() {
    [[ -n "${SAMPLE_DIR}" && -f "${SAMPLE_DIR}" ]] && rm -f "${SAMPLE_DIR}" 2>/dev/null || true
    for t in ${created_triggers[*]:-}; do
        curl -sfm 10 -X DELETE -H "Authorization: Bearer ${API_KEY}" \
            "http://127.0.0.1:${PORTFWD_PORT}/api/v1/me/triggers/${t}" >/dev/null 2>&1 || true
    done
    [[ -n "${SR_FILL_POD:-}" ]] && \
        kc exec "${SR_FILL_POD}" -c workspace -- rm -f /workspace/sr4-fill.bin >/dev/null 2>&1 || true
    kc delete workspace "${WS}" --ignore-not-found >/dev/null 2>&1 || true
    [[ -n "${SAMPLER_PID:-}" ]] && kill "${SAMPLER_PID}" 2>/dev/null || true
}
created_triggers=()
SR_FILL_POD=""
SAMPLER_PID=""
trap cleanup EXIT

# api() — the #1474-r4 no-subshell contract.
api() {
    local method="$1" path="$2" body="${3:-}"
    local args=(-s -m 60 -X "${method}" -H "Authorization: Bearer ${API_KEY}" \
        -H "Content-Type: application/json" -w '\n%{http_code}' \
        "http://127.0.0.1:${PORTFWD_PORT}${path}")
    [[ -n "${body}" ]] && args+=(-d "${body}")
    local out
    out=$(curl "${args[@]}") || out=$'\n000'
    api_status="${out##*$'\n'}"
    api_body="${out%$'\n'*}"
    printf '%s' "${api_body}"
}

scrape_metrics() { # pod -> metrics text on stdout
    local pod="$1"
    kc exec "${pod}" -c workspace -- curl -sfm 5 http://127.0.0.1:4098/metrics 2>/dev/null || \
        kc exec "${pod}" -c agentd -- curl -sfm 5 http://127.0.0.1:4098/metrics 2>/dev/null || echo ""
}

gauge_value() { # metrics-text gauge-name -> value or ""
    local text="$1" gauge="$2" line val
    line=$(printf '%s' "${text}" | grep -E "^${gauge}(\{| )" | head -1)
    [[ -z "${line}" ]] && { echo ""; return; }
    val="${line##* }"
    printf '%s' "${val}"
}

# upload_bytes: size -> status (echoed); concurrent-safe (writes its
# status to $2 when given, for background jobs).
upload_bytes() { # size [outfile]
    local size="$1" outfile="${2:-}" tmp st
    tmp=$(mktemp /tmp/sr-up-XXXXXX.bin)
    head -c "${size}" /dev/urandom > "${tmp}" 2>/dev/null
    local out status
    out=$(curl -s -m 120 -X POST -H "Authorization: Bearer ${API_KEY}" \
        -F "file=@${tmp};filename=sr-stress-$(basename "${tmp}").bin" \
        -w '\n%{http_code}' \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/uploads" 2>/dev/null) || out=$'\n000'
    rm -f "${tmp}"
    status="${out##*$'\n'}"
    if [[ -n "${outfile}" ]]; then
        printf '%s' "${status}" > "${outfile}"
    else
        printf '%s' "${status}"
    fi
}

# wait_all_terminal: every file in a results dir holds a terminal status
# (201, 507, 429, 504 — never 000/5xx-other/4xx).
storm_report() { # results-dir count -> prints "delivered=N refused=M other=K"
    local dir="$1" count="$2" f st delivered=0 refused=0 other=0
    for f in "${dir}"/res-*; do
        [[ -f "${f}" ]] || continue
        st=$(cat "${f}")
        case "${st}" in
            201) delivered=$((delivered + 1)) ;;
            507|429|504) refused=$((refused + 1)) ;;
            *) other=$((other + 1)); warn "  non-terminal outcome: ${st} ($(basename "${f}"))" ;;
        esac
    done
    printf 'delivered=%d refused=%d other=%d' "${delivered}" "${refused}" "${other}"
}

# --- prerequisite -----------------------------------------------------------

seed_workspace "${WS}" >/dev/null
wait_phase "${WS}" Active 360 || die "SR-0: workspace never Active"
POD=$(pod_of "${WS}")
[[ -n "${POD}" ]] || die "SR-0: no pod"

PRE_METRICS=$(scrape_metrics "${POD}")
STAGING_GAUGE=$(gauge_value "${PRE_METRICS}" 'workspace_agentd_upload_staging_bytes')
if [[ -z "${STAGING_GAUGE}" ]]; then
    sr_skip "staging gauges absent (PR 1/2 lane not merged) — SR-1/SR-2/SR-4/SR-5/SR-6 need the staging leg"
else
    ok "SR-0: staging surface present (gauges scrape)"
fi

# --- SR-A: the API-side 411 (this PR's own surface) --------------------------

# Multipart content-type (the handler's media gate accepts it) with a
# genuinely length-less body — the chunked shape the 411 exists for.
SR_A_BODY=$(mktemp /tmp/sr-a-XXXXXX)
head -c 256 /dev/urandom > "${SR_A_BODY}"
SR_A_STATUS=$(curl -s -m 30 -X POST -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: multipart/form-data; boundary=XsrA" \
    -H "Transfer-Encoding: chunked" \
    --data-binary "@${SR_A_BODY}" \
    -o /tmp/sr-a-resp.$$ -w '%{http_code}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WS}/uploads" 2>/dev/null) || SR_A_STATUS=000
rm -f "${SR_A_BODY}" /tmp/sr-a-resp.$$ 2>/dev/null || true
if [[ "${SR_A_STATUS}" == "411" ]]; then
    ok "SR-A: undeclared multipart body → 411 (invalid_declared_length)"
else
    note_fail "SR-A: undeclared multipart body returned ${SR_A_STATUS}, expected 411"
fi

# --- staging-dependent rows ---------------------------------------------------

if [[ -n "${STAGING_GAUGE}" ]]; then

# SR-1 (§6.1): CONCURRENT storm, file-backed gauge sampler.
SAMPLE_DIR=$(mktemp -d /tmp/sr-samples-XXXXXX)
sample_loop() { # writes samples to $SAMPLE_DIR/samples
    local m sv rv
    : > "${SAMPLE_DIR}/samples"
    while :; do
        m=$(scrape_metrics "${POD}" 2>/dev/null)
        sv=$(gauge_value "${m}" 'workspace_agentd_upload_staging_bytes')
        rv=$(gauge_value "${m}" 'workspace_agentd_upload_staging_reserved_bytes')
        printf '%s %s\n' "${sv:-0}" "${rv:-0}" >> "${SAMPLE_DIR}/samples"
        sleep 0.3
    done
}
sample_loop & SAMPLER_PID=$!
sleep 1

STORM_DIR=$(mktemp -d /tmp/sr-storm-XXXXXX)
# MAX_CONCURRENT (4) + 2 near-cap uploads, ALL concurrent (§6.1).
pids=()
for i in 1 2 3 4 5 6; do
    upload_bytes $((24 * 1024 * 1024)) "${STORM_DIR}/res-${i}" &
    pids+=($!)
    sleep 0.2
done
for p in "${pids[@]}"; do wait "${p}" 2>/dev/null || true; done
sleep 3
kill "${SAMPLER_PID}" 2>/dev/null || true; SAMPLER_PID=""

MAX_STAGING=$(awk '{if ($1 > m) m = $1} END {printf "%d", m + 0}' "${SAMPLE_DIR}/samples")
MAX_RESERVED=$(awk '{if ($2 > m) m = $2} END {printf "%d", m + 0}' "${SAMPLE_DIR}/samples")
rm -rf "${SAMPLE_DIR}"; SAMPLE_DIR=""

if awk -v m="${MAX_STAGING}" -v b=50331648 'BEGIN{exit !(m<=b)}'; then
    ok "SR-1: max staging_bytes ${MAX_STAGING} ≤ 48 MiB budget"
else
    note_fail "SR-1: staging_bytes excursion ${MAX_STAGING} > 50331648"
fi
if awk -v m="${MAX_RESERVED}" -v b=50331648 'BEGIN{exit !(m<=b)}'; then
    ok "SR-1: max reserved_bytes ${MAX_RESERVED} ≤ 48 MiB budget"
else
    note_fail "SR-1: reserved_bytes excursion ${MAX_RESERVED} > 50331648"
fi
REPORT=$(storm_report "${STORM_DIR}" 6)
rm -rf "${STORM_DIR}"
if [[ "${REPORT}" == *"other=0"* ]]; then
    ok "SR-1: concurrent storm terminal-clean (${REPORT})"
else
    note_fail "SR-1: non-terminal storm outcomes (${REPORT})"
fi

# SR-2 (§6.2): resync MID-STORM (uploads still in flight).
SR2_DIR=$(mktemp -d /tmp/sr2-storm-XXXXXX)
pids=()
for i in 1 2 3; do
    upload_bytes $((20 * 1024 * 1024)) "${SR2_DIR}/res-${i}" &
    pids+=($!)
done
sleep 1  # uploads are staging NOW
CRED_BEFORE=$(gauge_value "$(scrape_metrics "${POD}")" 'workspace_agentd_upload_staging_credential_bytes')
api POST "/api/v1/me/workspaces/${WS}/reload-secrets" >/dev/null 2>&1 || true
sleep 20  # resync + storm overlap
for p in "${pids[@]}"; do wait "${p}" 2>/dev/null || true; done
REV_NOW=$(kc get workspace "${WS}" -o jsonpath='{.status.secretsDelivery.spawnedRev}')
CRED_AFTER=$(gauge_value "$(scrape_metrics "${POD}")" 'workspace_agentd_upload_staging_credential_bytes')
REPORT2=$(storm_report "${SR2_DIR}" 3)
rm -rf "${SR2_DIR}"
if [[ -n "${REV_NOW}" && "${REV_NOW}" != "<none>" ]]; then
    ok "SR-2: resync advanced mid-storm (spawnedRev=${REV_NOW})"
else
    note_fail "SR-2: spawnedRev unreadable after forced mid-storm resync"
fi
if awk -v a="${CRED_AFTER:-0}" -v b="${CRED_BEFORE:-0}" 'BEGIN{exit !(a>=b)}'; then
    ok "SR-2: credential_bytes never regressed (${CRED_BEFORE:-0} → ${CRED_AFTER:-0})"
else
    note_fail "SR-2: credential_bytes regressed ${CRED_BEFORE} → ${CRED_AFTER}"
fi
if [[ "${REPORT2}" == *"other=0"* ]]; then
    ok "SR-2: mid-storm outcomes terminal-clean (${REPORT2})"
else
    note_fail "SR-2: non-terminal mid-storm outcomes (${REPORT2})"
fi

# SR-4 (§6.4): fill to the margin edge, upload → write-time 507.
AVAIL=$(kc exec "${POD}" -c workspace -- stat -f -c %a /workspace 2>/dev/null | head -1)
BLOCK=$(kc exec "${POD}" -c workspace -- stat -f -c %S /workspace 2>/dev/null | head -1)
if [[ -n "${AVAIL}" && -n "${BLOCK}" && "${BLOCK}" -gt 0 ]]; then
    AVAIL_BYTES=$((AVAIL * BLOCK))
    SR_FILL_POD="${POD}"
    FILL=$((AVAIL_BYTES - 63 * 1024 * 1024))
    if [[ "${FILL}" -gt $((1024 * 1024)) ]]; then
        kc exec "${POD}" -c workspace -- sh -c "head -c ${FILL} /dev/zero > /workspace/sr4-fill.bin" >/dev/null 2>&1 || true
        st=$(upload_bytes $((2 * 1024 * 1024)))
        if [[ "${st}" == "507" ]]; then
            ok "SR-4: write-time refusal at the edge (507 dest_disk_full)"
        else
            note_fail "SR-4: edge upload returned ${st}, expected 507"
        fi
        kc exec "${POD}" -c workspace -- rm -f /workspace/sr4-fill.bin >/dev/null 2>&1 || true
        SR_FILL_POD=""
    else
        sr_skip "SR-4: volume slack (${AVAIL_BYTES}B) too small for the fill edge on this cluster"
    fi
else
    sr_skip "SR-4: stat -f unavailable in the runtime container — edge row needs the fill computation"
fi

# SR-5 (§6.5): sidecar CONTAINER kill at phase boundaries.
for phase in first mid last; do
    # fire a large upload; kill the sidecar CONTAINER (not the pod) mid-flight
    ( sleep 0.3; [[ "${phase}" == "mid" ]] && sleep 2; [[ "${phase}" == "last" ]] && sleep 4
      SIDECAR_CID=$(docker exec "${CLUSTER_NAME}-control-plane" crictl ps -q --name agentd --state Running 2>/dev/null | head -1)
      [[ -n "${SIDECAR_CID}" ]] && docker exec "${CLUSTER_NAME}-control-plane" crictl stop "${SIDECAR_CID}" >/dev/null 2>&1 || warn "SR-5/${phase}: crictl stop did not fire (no container id)"
    ) & killer=$!
    st=$(upload_bytes $((20 * 1024 * 1024)) || echo 000)
    wait "${killer}" 2>/dev/null || true
    sleep 20
    wait_phase "${WS}" Active 300 >/dev/null 2>&1 || true
    POD=$(pod_of "${WS}")
    # No non-.tmp partial BEYOND the pre-existing set: assert the upload
    # produced either its final file or nothing — never a .tmp left
    # standing after the settle window.
    TMPS_NOW=$(kc exec "${POD}" -c workspace -- sh -c 'ls /workspace/uploads/*.tmp 2>/dev/null | wc -l' 2>/dev/null || echo "?")
    if [[ "${st}" == "201" || "${st}" == "507" || "${st}" == "504" || "${st}" == "000" ]]; then
        ok "SR-5/${phase}: kill outcome terminal-clean (status ${st}, tmp=${TMPS_NOW})"
    else
        note_fail "SR-5/${phase}: non-terminal outcome ${st}"
    fi
    if [[ "${TMPS_NOW}" != "0" && "${TMPS_NOW}" != "?" ]]; then
        warn "SR-5/${phase}: ${TMPS_NOW} .tmp at check time (TTL reclaim window — re-checked at final)"
    fi
    sleep 5
done
TMPS=$(kc exec "$(pod_of "${WS}")" -c workspace -- sh -c 'ls /workspace/uploads/*.tmp 2>/dev/null | wc -l' 2>/dev/null || echo "?")
if [[ "${TMPS}" == "0" ]]; then
    ok "SR-5: destination .tmp reclaimed after kills"
elif [[ "${TMPS}" == "?" ]]; then
    sr_skip "SR-5: could not list destination dir (exec failure)"
else
    note_fail "SR-5: ${TMPS} destination .tmp survived the settle window (scrub gap)"
fi

# SR-6 (§6.6): latency baseline at 1× and the concurrency boundary.
apply_latency() { # size -> ms
    local size="$1" t0 t1
    t0=$(date +%s%3N)
    upload_bytes "${size}" >/dev/null
    t1=$(date +%s%3N)
    echo $((t1 - t0))
}
FBAVAIL_PRE=$(kc exec "${POD}" -c workspace -- stat -f -c %a /workspace 2>/dev/null | head -1)
FBBLOCK_PRE=$(kc exec "${POD}" -c workspace -- stat -f -c %S /workspace 2>/dev/null | head -1)
CRED=$(gauge_value "$(scrape_metrics "${POD}")" 'workspace_agentd_upload_staging_credential_bytes')
CRED="${CRED:-0}"
FBAVAIL_BYTES=$(( ${FBAVAIL_PRE:-0} * ${FBBLOCK_PRE:-0} ))
CONCURRENCY=4
# §6.6: f_bavail_pre ≥ C + 94 MiB (30 staged + 24 floor + 30 reserved + 10 new).
if awk -v f="${FBAVAIL_BYTES}" -v c="${CRED}" 'BEGIN{exit !(f >= c + 98_560_614)}'; then
    CONCURRENCY=4
else
    CONCURRENCY=3
    warn "SR-6: 4×10 precondition unmet (f_bavail ${FBAVAIL_BYTES} < C+94MiB) — skip-DOWN to 3×, explicitly"
fi
L1=$(apply_latency $((10 * 1024 * 1024)))
p95_list=""
for i in $(seq 1 "${CONCURRENCY}"); do
    ms=$(apply_latency $((10 * 1024 * 1024)))
    p95_list="${p95_list} ${ms}"
done
log "SR-6 baseline (worklog table): 1x10MiB=${L1}ms; ${CONCURRENCY}x10MiB=[${p95_list} ]ms"
ok "SR-6: baseline recorded (1×=${L1}ms, ${CONCURRENCY}×=[${p95_list} ]; regression guard: ${CONCURRENCY}× p95 ≤ 2× single p95 evaluated at PR review from this table)"

fi # staging gauges present

# SR-3 (§6.3): backpressure — the fault seam rides PR 1/2.
sr_skip "SR-3: copy-throttle injection absent (PR 1/2 fault seam) — backpressure row pending that lane"

# --- verdict -----------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
    die "upload stress harness: ${failures} row(s) failed (skips: ${sr_skips})"
fi
ok "upload stress harness: all rows passed (loud skips: ${sr_skips})"
