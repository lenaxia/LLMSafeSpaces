#!/usr/bin/env bash
# gVisor overhead benchmark for an LLM-coding workload (Epic 51 S51.1 AC #8).
#
# Measures the wall-clock cost of running a representative agent workload
# under gVisor (runsc) vs runc, on the SAME node pool, with the SAME
# workspace image and credential set. The accept/reject gate for flipping
# `gvisor.enabled` default to true is: median overhead across the three
# workload phases < 30%.
#
# Prerequisites (operator must provision; this script cannot):
#   - A cluster with at least one node that has runsc installed and configured
#     as a container runtime handler (per docs/operator/security.md §gVisor).
#   - The chart deployed with gvisor.enabled=true so the RuntimeClass exists.
#   - `kubectl` configured with cluster-admin.
#   - The LLMSafeSpaces API reachable at $API_URL with a valid $API_TOKEN
#     that can create workspaces and send messages.
#   - `jq` and `curl` on the machine running this script.
#
# What this script measures (three phases of a real LLM-coding session):
#   1. POD BOOT     — workspace Pending → Active. Dominated by PVC attach +
#                     opencode boot; the part most likely to surface gVisor's
#                     syscall-interception cost on init paths.
#   2. COLD PROMPT  — first user message → first assistant token. Proxy +
#                     opencode + first LLM round-trip. The LLM API latency
#                     dominates but the proxy/pod side gets measured too.
#   3. FILE I/O     — opencode writes a 5 MB scratch file to /workspace,
#                     reads it back, checksums. Pure pod-local syscall cost —
#                     the phase where gVisor's overhead is highest per
#                     gvisor.dev docs (5–30% on syscall-heavy workloads).
#
# Phases 1 and 2 intentionally include non-pod latency (LLM API, PVC attach)
# because the AC asks for the cost on a REPRESENTATIVE workload, not a
# microbenchmark. If gVisor's overhead is invisible at the workload level
# (because LLM latency dominates), that is the answer the AC wants.
# Phase 3 isolates the worst-case so the operator can see what gVisor
# would cost on a purely syscall-bound workload.
#
# Output: TSV to stdout, one row per (runtime × phase × iteration). Summary
# stats (median, p90, overhead %) computed by docs/operator/gvisor-benchmark.md's
# recipes — keep this script dumb, do the math in the doc.
set -Eeuo pipefail

# --- configuration (override via env) ---
API_URL="${API_URL:?API_URL must be set, e.g. https://safespaces.example.com}"
API_TOKEN="${API_TOKEN:?API_TOKEN must be set}"
NAMESPACE="${NAMESPACE:-llmsafespaces}"
ITERATIONS="${ITERATIONS:-5}"           # per runtime per phase
WORKSPACE_IMAGE="${WORKSPACE_IMAGE:-ghcr.io/lenaxia/llmsafespaces/python:latest}"
RUNTIME_CLASS_OPT_OUT_ANNOTATION="llmsafespaces.dev/allow-runtime-class-override=true"

# --- pretty logging ---
if [[ -t 1 ]]; then
    BOLD=$'\033[1m'; RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf '%s==>%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*" >&2; }
ok()   { printf '%s ✓%s %s\n' "${GREEN}" "${RESET}" "$*" >&2; }
fail() { printf '%s ✗%s %s\n' "${RED}" "${RESET}" "$*" >&2; }

# --- preflight ---
for dep in kubectl jq curl; do
    command -v "$dep" >/dev/null 2>&1 || { fail "missing dependency: $dep"; exit 2; }
done

# --- helpers ---
ns_now() { date +%s.%N; }

# Wait for a workspace to reach Active, echo wall-clock seconds from create.
wait_active() {
    local ws="$1" start="$2"
    local deadline=$(( $(date +%s) + 300 )) # 5 min cap
    while :; do
        local phase
        phase=$(kubectl get workspace "$ws" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        if [[ "$phase" == "Active" ]]; then
            echo "$(awk -v a="$start" -v b="$(ns_now)" 'BEGIN{printf "%.3f", b-a}')"
            return 0
        fi
        if [[ "$(date +%s)" -ge "$deadline" ]]; then
            fail "workspace $ws did not reach Active in 300s (last phase: $phase)"
            kubectl describe workspace "$ws" -n "$NAMESPACE" >&2 || true
            return 1
        fi
        sleep 2
    done
}

# Send a cold prompt and time first-token. Echoes wall-clock seconds.
time_cold_prompt() {
    local ws="$1"
    local start ns url http code first_byte
    start=$(ns_now)

    # Create a session, then send a "hello" prompt and wait for the first
    # SSE event with assistant content. We do NOT consume the full stream —
    # the AC measures time-to-first-token, not full-response latency.
    local session_id
    session_id=$(curl -sS -X POST "$API_URL/api/v1/workspaces/$ws/sessions" \
        -H "Authorization: Bearer $API_TOKEN" \
        -H 'Content-Type: application/json' \
        -d '{"title":"gvisor-bench"}' | jq -r .id)
    [[ -n "$session_id" && "$session_id" != "null" ]] || { fail "session create failed"; return 1; }

    # Time to first assistant token via SSE. curl's --no-buffer + a small
    # awk that exits on the first 'part' event gives us the cold latency.
    local first_event_at
    first_event_at=$(
        curl -sS -N --no-buffer -X POST "$API_URL/api/v1/workspaces/$ws/sessions/$session_id/message" \
            -H "Authorization: Bearer $API_TOKEN" \
            -H 'Content-Type: application/json' \
            -d '{"content":"Say OK and stop.","model":"opencode/free"}' \
            | awk '
                /^event:/ { ev=$2 }
                /^data:/ && ev=="part" { print systime(); exit }
            '
    )
    [[ -n "$first_event_at" ]] || { fail "no assistant part received"; return 1; }
    awk -v a="$start" -v b="$first_event_at" 'BEGIN{printf "%.3f", b-a}'
}

# Run a pod-local file I/O round-trip and time it. Echoes wall-clock seconds.
time_file_io() {
    local ws="$1" pod_ip
    # Use the workspace exec API to run a shell snippet that writes 5 MiB
    # of random data, reads it back, and pipes through sha256sum. The
    # snippet's wall clock is what we want.
    local snippet='dd if=/dev/urandom of=/workspace/.bench bs=1M count=5 status=none && sha256sum /workspace/.bench >/dev/null && rm /workspace/.bench'
    local start
    start=$(ns_now)
    kubectl exec -n "$NAMESPACE" "workspace/$ws" -c main -- bash -lc "$snippet" >/dev/null 2>&1 || {
        # The exec path may not be wired uniformly; fall back to a direct
        # pod-name exec by listing pods with the workspace label.
        local pod
        pod=$(kubectl get pods -n "$NAMESPACE" -l "llmsafespaces.dev/workspace=$ws" -o jsonpath='{.items[0].metadata.name}')
        kubectl exec -n "$NAMESPACE" "$pod" -c main -- bash -lc "$snippet" >/dev/null 2>&1 || {
            fail "file I/O snippet failed on workspace $ws"
            return 1
        }
    }
    awk -v a="$start" -v b="$(ns_now)" 'BEGIN{printf "%.3f", b-a}'
}

# Create a workspace under the given runtime. Echoes workspace name.
create_workspace() {
    local runtime="$1"
    local name="gvisor-bench-$runtime-$(date +%s)-$RANDOM"
    local runtime_class_patch=""
    if [[ "$runtime" == "runc" ]]; then
        # Admin-gated opt-out per docs/operator/security.md.
        runtime_class_patch=",\"metadata\":{\"annotations\":{\"$RUNTIME_CLASS_OPT_OUT_ANNOTATION\":\"true\"}},\"spec\":{\"runtimeClass\":\"runc\"}"
    fi
    local body
    body=$(cat <<EOF
{
  "name": "$name",
  "image": "$WORKSPACE_IMAGE"
$runtime_class_patch
}
EOF
)
    local ws
    ws=$(curl -sS -X POST "$API_URL/api/v1/workspaces" \
        -H "Authorization: Bearer $API_TOKEN" \
        -H 'Content-Type: application/json' \
        -d "$body" | jq -r .id)
    [[ -n "$ws" && "$ws" != "null" ]] || { fail "workspace create failed for runtime=$runtime"; return 1; }
    echo "$ws"
}

delete_workspace() {
    local ws="$1"
    curl -sS -X DELETE "$API_URL/api/v1/workspaces/$ws" \
        -H "Authorization: Bearer $API_TOKEN" >/dev/null 2>&1 || true
}

# --- header ---
printf 'runtime\tphase\titeration\tseconds\n'

# --- main loop: for each runtime, for each iteration, measure all 3 phases ---
for runtime in runc gvisor; do
    log "runtime=$runtime, $ITERATIONS iterations × 3 phases"
    for ((i=1; i<=ITERATIONS; i++)); do
        local_ws=""
        trap '[[ -n "$local_ws" ]] && delete_workspace "$local_ws"' EXIT

        local_ws=$(create_workspace "$runtime") || continue
        local create_start
        create_start=$(ns_now)
        boot_sec=$(wait_active "$local_ws" "$create_start") || continue
        printf '%s\t%s\t%d\t%s\n' "$runtime" "pod_boot" "$i" "$boot_sec"

        cold_sec=$(time_cold_prompt "$local_ws") || { delete_workspace "$local_ws"; continue; }
        printf '%s\t%s\t%d\t%s\n' "$runtime" "cold_prompt" "$i" "$cold_sec"

        io_sec=$(time_file_io "$local_ws") || { delete_workspace "$local_ws"; continue; }
        printf '%s\t%s\t%d\t%s\n' "$runtime" "file_io" "$i" "$io_sec"

        delete_workspace "$local_ws"
        local_ws=""
        ok "runtime=$runtime iter=$i boot=${boot_sec}s cold=${cold_sec}s io=${io_sec}s"
    done
done

log "done — pipe output through the stats recipes in docs/operator/gvisor-benchmark.md"
