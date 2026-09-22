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
    # Completeness (r3 finding 5): a storm that lost result files
    # yields 0+0+0 and passes any substring check. The count must
    # match — the caller checks it.
    printf ' total=%d' "$((delivered + refused + other))"
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

# WELL-FRAMED multipart with a length-less body: the 411 gate sits
# after the file-part locator, so the body must parse as multipart
# (boundary + file-part headers) before the gate evaluates (r2
# finding 1: random bytes 400 at the locator; JSON 415 at the media
# gate — this is the shape the handler actually 411s).
SR_A_BODY=$(mktemp /tmp/sr-a-XXXXXX)
{
    printf -- '--XsrA\r\n'
    printf 'Content-Disposition: form-data; name="file"; filename="a.txt"\r\n'
    printf 'Content-Type: application/octet-stream\r\n\r\n'
    head -c 256 /dev/urandom
    printf '\r\n--XsrA--\r\n'
} > "${SR_A_BODY}"
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
if [[ "${REPORT}" == *"other=0"* && "${REPORT}" == *"total=6"* ]]; then
    ok "SR-1: concurrent storm complete + terminal-clean (${REPORT})"
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
REV_BEFORE=$(kc get workspace "${WS}" -o jsonpath='{.status.secretsDelivery.spawnedRev}')
api POST "/api/v1/workspaces/${WS}/reload-secrets" >/dev/null 2>&1 || true
# The resync ROUTE must have fired (200 — r3 finding 2: a 404 here is
# indistinguishable from a legitimate rev-hold without this check).
if [[ "${api_status}" == "200" ]]; then
    :
else
    note_fail "SR-2: reload-secrets returned ${api_status}, expected 200 (route regression)"
fi
sleep 20  # resync + storm overlap
for p in "${pids[@]}"; do wait "${p}" 2>/dev/null || true; done
REV_NOW=$(kc get workspace "${WS}" -o jsonpath='{.status.secretsDelivery.spawnedRev}')
CRED_AFTER=$(gauge_value "$(scrape_metrics "${POD}")" 'workspace_agentd_upload_staging_credential_bytes')
REPORT2=$(storm_report "${SR2_DIR}" 3)
rm -rf "${SR2_DIR}"
if [[ -n "${REV_NOW}" && "${REV_NOW}" != "<none>" && "${REV_NOW}" != "${REV_BEFORE}" ]]; then
    ok "SR-2: resync advanced mid-storm (spawnedRev ${REV_BEFORE} → ${REV_NOW})"
elif [[ "${REV_NOW}" == "${REV_BEFORE}" ]]; then
    # No credential CHANGE between boot and the forced resync → the rev
    # legitimately holds. The row's real assertion is the resync ROUTE
    # fired (200/204, not 404 — r2 finding 4) + credential_bytes intact.
    ok "SR-2: rev held ${REV_NOW} (no credential change since boot — resync no-op)"
else
    note_fail "SR-2: spawnedRev unreadable after forced mid-storm resync"
fi
if awk -v a="${CRED_AFTER:-0}" -v b="${CRED_BEFORE:-0}" 'BEGIN{exit !(a>=b)}'; then
    ok "SR-2: credential_bytes never regressed (${CRED_BEFORE:-0} → ${CRED_AFTER:-0})"
else
    note_fail "SR-2: credential_bytes regressed ${CRED_BEFORE} → ${CRED_AFTER}"
fi
if [[ "${REPORT2}" == *"other=0"* && "${REPORT2}" == *"total=3"* ]]; then
    ok "SR-2: mid-storm outcomes complete + terminal-clean (${REPORT2})"
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
# §6.5's PRIMARY invariant: no non-.tmp artifact beyond the pre-existing
# set. Snapshot the uploads dir before the kills; after the settle, any
# NEW non-.tmp file must correspond to a 201-delivered upload.
PRE_KILL_LISTING=$(kc exec "${POD}" -c workspace -- sh -c 'ls /workspace/uploads/ 2>/dev/null | grep -v "\.tmp$" | sort' 2>/dev/null || echo "")
for phase in first mid last; do
    # fire a large upload; kill the sidecar CONTAINER (not the pod) mid-flight
    ( sleep 0.3; [[ "${phase}" == "mid" ]] && sleep 2; [[ "${phase}" == "last" ]] && sleep 4
      SIDECAR_CID=$(docker exec "${CLUSTER_NAME}-control-plane" crictl ps -q --name agentd --state Running 2>/dev/null | head -1)
      [[ -n "${SIDECAR_CID}" ]] && docker exec "${CLUSTER_NAME}-control-plane" crictl stop "${SIDECAR_CID}" >/dev/null 2>&1 || warn "SR-5/${phase}: crictl stop did not fire (no container id)"
    ) & killer=$!
    st=$(upload_bytes $((20 * 1024 * 1024)) || echo 000)
    SR5_PHASE_STATUSES="${SR5_PHASE_STATUSES:-} ${st}"
    wait "${killer}" 2>/dev/null || true
    sleep 20
    wait_phase "${WS}" Active 300 >/dev/null 2>&1 || true
    POD=$(pod_of "${WS}")
    # No non-.tmp partial BEYOND the pre-existing set: assert the upload
    # produced either its final file or nothing — never a .tmp left
    # standing after the settle window.
    TMPS_NOW=$(kc exec "${POD}" -c workspace -- sh -c 'ls /workspace/uploads/*.tmp 2>/dev/null | wc -l' 2>/dev/null || echo "?")
    # 502 is the API's mapped transport outcome for a killed agentd
    # (design §6.5 allows 507/504/transport; r2 finding 5).
    if [[ "${st}" == "201" || "${st}" == "507" || "${st}" == "504" || "${st}" == "502" || "${st}" == "000" ]]; then
        ok "SR-5/${phase}: kill outcome terminal-clean (status ${st}, tmp=${TMPS_NOW})"
    else
        note_fail "SR-5/${phase}: non-terminal outcome ${st}"
    fi
    # §6.5's PRIMARY invariant: no non-.tmp partial. A non-.tmp file
    # with our upload's name that was NOT 201-delivered is a partial.
    sleep 5
done
# §6.5's PRIMARY invariant check: the post-settle non-.tmp set minus
# the pre-kill set must be empty OR every addition is a 201's uuid file
# (a kill-fragmented rename would produce a non-.tmp partial).
POST_KILL_EXIT=0
POST_KILL_LISTING=$(kc exec "$(pod_of "${WS}")" -c workspace -- sh -c 'ls /workspace/uploads/ 2>/dev/null | grep -v "\.tmp$" | sort' 2>/dev/null) || POST_KILL_EXIT=1
# Zero-match arithmetic: grep -c . prints 0 AND exits 1 under pipefail —
# the || echo 0 appended a SECOND 0 (r4 finding: "0\n0" → arithmetic
# syntax error → false-fail on the SUCCESS case). awk never trips.
NEW_NON_TMP=$(comm -13 <(printf '%s\n' "${PRE_KILL_LISTING}") <(printf '%s\n' "${POST_KILL_LISTING}") | awk 'NF{n++} END{printf "%d", n+0}')
# All SR-5 outcomes are transport/terminal (no 201s during kill phases
# unless the copy completed before the kill — a completed 201 is a
# final file, not a partial; we cannot distinguish per-file here, but
# a FAILED rename leaving a non-.tmp fragment IS what this catches:
# fragments have random-uuid names we never asked for).
# The honest bound (r4): each new non-.tmp file must be a 201-delivered
# upload — count the 201 phases; more new files than that is fragments.
SR5_DELIVERED=0
for st in ${SR5_PHASE_STATUSES:-}; do
    [[ "${st}" == "201" ]] && SR5_DELIVERED=$((SR5_DELIVERED + 1))
done
if [[ "${POST_KILL_EXIT}" -ne 0 ]]; then
    sr_skip "SR-5: post-settle listing exec failed (§6.5 primary invariant unverifiable this run)"
elif [[ "${NEW_NON_TMP}" -le "${SR5_DELIVERED}" ]]; then
    ok "SR-5: no non-.tmp partials beyond ${SR5_DELIVERED} completed uploads (${NEW_NON_TMP} new)"
else
    note_fail "SR-5: ${NEW_NON_TMP} non-.tmp artifacts > ${SR5_DELIVERED} delivered (rename fragmentation)"
fi
# .tmp reclaim: the TTL ticker's default is 15 min (§4.3) — the honest
# assertion at this point is bounded, not zero (r2 finding 5b).
TMPS=$(kc exec "$(pod_of "${WS}")" -c workspace -- sh -c 'ls /workspace/uploads/*.tmp 2>/dev/null | wc -l' 2>/dev/null || echo "?")
if [[ "${TMPS}" == "?" ]]; then
    sr_skip "SR-5: could not list destination dir (exec failure)"
elif [[ "${TMPS}" -le 3 ]]; then
    ok "SR-5: destination .tmp bounded (${TMPS}; TTL default 15min reclaims — §4.3's window)"
else
    note_fail "SR-5: ${TMPS} destination .tmp accumulated (unbounded)"
fi

# SR-6 (§6.6): latency baseline at 1× and the concurrency boundary.
apply_latency() { # size -> ms
    local size="$1" t0 t1
    t0=$(date +%s%3N)
    upload_bytes "${size}" >/dev/null
    t1=$(date +%s%3N)
    echo $((t1 - t0))
}
# §6.6 conditions on the STAGING TMPFS's f_bavail (the 96 MiB volume
# the clause-B admission reads), NOT the PVC. r2 finding 3: /workspace
# is GB-scale — vacuous. The constant is 94 MiB = 98566144 bytes (no
# underscore separator: gawk lexes 98_560_614 as "98" + unset var —
# r2 finding 2).
TMPFS_AVAIL=$(kc exec "${POD}" -c workspace -- stat -f -c %a /sandbox-runtime 2>/dev/null | head -1)
TMPFS_BLOCK=$(kc exec "${POD}" -c workspace -- stat -f -c %S /sandbox-runtime 2>/dev/null | head -1)
CRED=$(gauge_value "$(scrape_metrics "${POD}")" 'workspace_agentd_upload_staging_credential_bytes')
CRED="${CRED:-0}"
TMPFS_AVAIL_BYTES=$(( ${TMPFS_AVAIL:-0} * ${TMPFS_BLOCK:-0} ))
CONCURRENCY=4
# §6.6: f_bavail_pre ≥ C + 94 MiB (30 staged + 24 floor + 30 reserved + 10 new).
if awk -v f="${TMPFS_AVAIL_BYTES}" -v c="${CRED}" 'BEGIN{exit !(f >= c + 98566144)}'; then
    CONCURRENCY=4
else
    CONCURRENCY=3
    warn "SR-6: 4×10 precondition unmet (tmpfs f_bavail ${TMPFS_AVAIL_BYTES} < C+94MiB) — skip-DOWN to 3×, explicitly"
fi
# Single-upload baseline (1×) — status-checked: a failed single makes
# every downstream number garbage (r5 finding: apply_latency discarded
# the status).
L1_T0=$(date +%s%3N)
L1_STATUS=$(upload_bytes $((10 * 1024 * 1024)))
L1_T1=$(date +%s%3N)
L1=$((L1_T1 - L1_T0))
if [[ "${L1_STATUS}" == "507" ]]; then
    # The in-run skip-DOWN gate (run 35679282297's adjudication): a 507
    # (dest_disk_full — the DESIGNED clean-fail of the half-stack) means
    # the upload path's DELIVERY leg is absent (the supervisor apply,
    # #1518/#1524). Keyed on 507 SPECIFICALLY so a genuine 500/000/429
    # regression still reaches the failure path. SR-1–SR-5's clean-fail
    # assertions already ran; the LATENCY rows are meaningless without
    # delivery. Loud skip, never a silent step-level if. A non-507
    # non-201 hits the baseline-failure note_fail BELOW this gate.
    sr_skip "SR-6: baseline upload ${L1_STATUS} (delivery leg #1518/#1524 absent) — latency + boundary rows skip-DOWN"
    log "upload stress harness: SR-1–SR-5 complete; SR-6 skipped (skips: ${sr_skips})"
    if [[ "${failures}" -ne 0 ]]; then
        die "upload stress harness: ${failures} row(s) failed (skips: ${sr_skips})"
    fi
    exit 0
fi
if [[ "${L1_STATUS}" != "201" ]]; then
    # A non-507 non-201 baseline (500/429/000/502/504) is NOT the
    # half-stack's designed clean-fail — it's a genuine upload-path
    # failure and the latency numbers are meaningless for the wrong
    # reason. This assertion MUST stay BELOW the 507 gate.
    note_fail "SR-6: single-upload baseline failed (${L1_STATUS}) — latency numbers meaningless"
fi
# The concurrency boundary — CONCURRENT uploads with PER-UPLOAD timing
# (r5 finding: wall-clock-only was conditionally vacuous for fast
# uploads; the design's §6.6 guard is per-upload p95 ≤ 2× single p95).
SR6_DIR=$(mktemp -d /tmp/sr6-storm-XXXXXX)
SR6_PIDS=()
for i in $(seq 1 "${CONCURRENCY}"); do
    (
        _t0=$(date +%s%3N)
        upload_bytes $((10 * 1024 * 1024)) "${SR6_DIR}/res-${i}" >/dev/null
        _t1=$(date +%s%3N)
        echo $((_t1 - _t0)) > "${SR6_DIR}/ms-${i}"
    ) &
    SR6_PIDS+=($!)
done
# Per-pid wait (run 35679282297: a BARE `wait` also waits the immortal
# kc port-forward child that harness_start spawned — the actual hang
# mechanism, 36 minutes of silence until cancellation).
for p in "${SR6_PIDS[@]}"; do wait "${p}" 2>/dev/null || true; done
REPORT6=$(storm_report "${SR6_DIR}" "${CONCURRENCY}")
# §6.6 specifies p95; at N≤4 samples the p95 IS the max. The guard's
# exact-invariance property: max ≤ 2×single ⟺ no job waited > single
# after its own upload — precisely the serialization invariant. (The
# r5 median was strictly BELOW the boundary at N=3 serial, never AT
# it — the reason it could not fail.)
SR6_P95=$(cat "${SR6_DIR}"/ms-* 2>/dev/null | sort -n | awk 'END{if (NR==0){print 0} else {print $1}}')
rm -rf "${SR6_DIR}"
# The 5th-concurrent-429 row (§6.6's boundary characterization): fire
# MAX+1 CONCURRENT uploads; the 5th must 429 (the count cap). Gated on
# the SAME §6.6 precondition (r5 finding: SR-5's orphans can make this
# deterministically 507 instead of 429 — red-by-environment).
if [[ "${CONCURRENCY}" -eq 4 ]]; then
SR6B_DIR=$(mktemp -d /tmp/sr6b-storm-XXXXXX)
SR6B_PIDS=()
for i in 1 2 3 4 5; do
    upload_bytes $((10 * 1024 * 1024)) "${SR6B_DIR}/res-${i}" &
    SR6B_PIDS+=($!)
done
for p in "${SR6B_PIDS[@]}"; do wait "${p}" 2>/dev/null || true; done
REPORT6B=$(storm_report "${SR6B_DIR}" 5)
# Parse the refused COUNT (r3 finding 1: *"refused="* matches every
# report — refused=0 included). The count-cap regression means
# refused=0 here; the boundary demands at least one.
# Completeness + a LITERAL 429 (r4: refused lumps 507|429|504 — a 507
# satisfies the count without the boundary being the count cap).
SR6B_HAS_429=0
for f in "${SR6B_DIR}"/res-*; do
    [[ "$(cat "${f}" 2>/dev/null)" == "429" ]] && SR6B_HAS_429=1
done
if [[ "${REPORT6B}" == *"total=5"* && "${SR6B_HAS_429}" -eq 1 ]]; then
    ok "SR-6: 5th-concurrent 429 boundary observed (literal 429 present; ${REPORT6B})"
else
    note_fail "SR-6: 5-concurrent storm lacks a literal 429 or incomplete (${REPORT6B}, has429=${SR6B_HAS_429})"
fi
rm -rf "${SR6B_DIR}"
else
    sr_skip "SR-6B: 5th-429 boundary needs the 4× precondition (skip-DOWN was active)"
fi

# The latency storm's outcomes must be asserted (r4 finding ◐ r3.5):
# other=0 AND total=CONCURRENCY — an all-000 storm has a tiny wall and
# would pass any guard while measuring nothing.
if [[ "${REPORT6}" != *"other=0"* || "${REPORT6}" != *"total=${CONCURRENCY}"* ]]; then
    note_fail "SR-6: latency storm not clean+complete (${REPORT6})"
fi
log "SR-6 baseline (worklog table): 1x10MiB=${L1}ms (status ${L1_STATUS}); ${CONCURRENCY}x10MiB-concurrent per-upload max(p95@N≤4)=${SR6_P95}ms; report=${REPORT6}"
# Design §6.6's regression guard: per-upload p95(=max at N≤4) ≤ 2×
# the single-upload p95. The guard's exact-invariance property: the
# max exceeds 2×single precisely when some job waited longer than its
# own upload — the serialization invariant, regardless of regime.
SR6_GUARD=$((2 * L1))
if [[ "${SR6_P95}" -le "${SR6_GUARD}" ]]; then
    ok "SR-6: regression guard (per-upload max(p95@N≤4) ${SR6_P95}ms ≤ 2×single ${L1}ms = ${SR6_GUARD}ms)"
else
    note_fail "SR-6: per-upload max(p95@N≤4) ${SR6_P95}ms > 2×single (${L1}ms → guard ${SR6_GUARD}ms) — serialization?"
fi

fi # staging gauges present

# SR-3 (§6.3): backpressure — the fault seam rides PR 1/2.
sr_skip "SR-3: copy-throttle injection absent (PR 1/2 fault seam) — backpressure row pending that lane"

# --- verdict -----------------------------------------------------------------

if [[ "${failures}" -ne 0 ]]; then
    die "upload stress harness: ${failures} row(s) failed (skips: ${sr_skips})"
fi
ok "upload stress harness: all rows passed (loud skips: ${sr_skips})"
