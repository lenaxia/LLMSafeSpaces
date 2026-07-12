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
#   - `kubectl` configured with cluster-admin (needed to apply the admin-gated
#     runtimeClass override annotation on runc-leg workspaces — the API does
#     not expose spec.runtimeClass, by design).
#   - The LLMSafeSpaces API reachable at $API_URL with a valid $API_TOKEN
#     that can create workspaces and send messages.
#   - `jq` and `curl` on the machine running this script.
#
# What this script measures (three phases of a real LLM-coding session):
#   1. POD_BOOT     — workspace Pending → Active. Dominated by PVC attach +
#                     opencode boot; the part most likely to surface gVisor's
#                     syscall-interception cost on init paths.
#   2. COLD_PROMPT  — first user message → first assistant content event via
#                     SSE. Proxy + opencode + first LLM round-trip. LLM API
#                     latency dominates but the proxy/pod side gets measured too.
#   3. FILE_IO      — write 5 MiB random to /workspace, read back, sha256.
#                     Pure pod-local syscall cost — gVisor's worst case per
#                     gvisor.dev docs (5–30% on syscall-heavy workloads).
#
# Workspace creation:
#   Both legs create the Workspace CR directly via `kubectl apply` rather than
#   the REST API. Reason: the runc leg requires `spec.runtimeClass: "runc"` +
#   the admin annotation `llmsafespaces.dev/allow-runtime-class-override=true`,
#   and the API's CreateWorkspaceRequest does not expose either field (the
#   opt-out is admin-gated by design — see controller/internal/webhooks/
#   workspace_webhook.go). Using kubectl for both legs keeps the creation
#   path identical modulo the runtimeClass field, which is the variable under
#   test. The API is used only for session/message operations.
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
WORKSPACE_RUNTIME="${WORKSPACE_RUNTIME:-base}"  # RuntimeEnvironment name
WORKSPACE_USERID="${WORKSPACE_USERID:?WORKSPACE_USERID must be set (the API user the benchmark runs as)}"
WORKSPACE_ORGID="${WORKSPACE_ORGID:-}"  # optional; leave empty for personal workspaces
LLM_MODEL="${LLM_MODEL:-opencode/free}" # model for the cold-prompt phase
ALLOW_OVERRIDE_ANNOTATION="llmsafespaces.dev/allow-runtime-class-override"

# --- pretty logging ---
if [[ -t 1 ]]; then
    BOLD=$'\033[1m'; RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; CYAN=$'\033[36m'; RESET=$'\033[0m'
else
    BOLD=''; RED=''; GREEN=''; YELLOW=''; CYAN=''; RESET=''
fi
log()  { printf '%s==>%s %s\n' "${CYAN}${BOLD}" "${RESET}" "$*" >&2; }
ok()   { printf '%s ok %s %s\n' "${GREEN}" "${RESET}" "$*" >&2; }
fail() { printf '%s FAIL %s %s\n' "${RED}" "${RESET}" "$*" >&2; }

# --- preflight ---
for dep in kubectl jq curl; do
    command -v "$dep" >/dev/null 2>&1 || { fail "missing dependency: $dep"; exit 2; }
done

# --- helpers ---
ns_now() { date +%s.%N; }

# Apply a Workspace CR for the given runtimeClass. Echoes the workspace name
# (a generated unique name). The benchmark uses kubectl apply rather than
# the API because spec.runtimeClass + the admin override annotation are not
# exposed via CreateWorkspaceRequest — they are admin-gated by design.
create_workspace() {
    local runtime_class="$1"   # "gvisor" or "runc"
    local name="gvisor-bench-${runtime_class}-$(date +%s)-${RANDOM}"

    # spec.runtimeClass is omitted (nil) for the gVisor leg so the controller's
    # default applies (gvisor when gvisor.enabled=true). It is explicitly "runc"
    # for the runc leg, with the admin annotation that the validating webhook
    # requires (controller/internal/webhooks/workspace_webhook.go).
    local runtime_class_line=""
    local annotations_block=$'    llmsafespaces.dev/bench: "gvisor-benchmark"'
    if [[ "$runtime_class" == "runc" ]]; then
        runtime_class_line=$'\n  runtimeClass: "runc"'
        annotations_block=$'    llmsafespaces.dev/allow-runtime-class-override: "true"\n    llmsafespaces.dev/bench: "gvisor-benchmark"'
    fi

    # The bench marker is also a label so kubectl -l selectors work for
    # cleanup (annotations are not selectable). The runtime override lives
    # only in annotations (where the webhook reads it).
    local orgid_line=""
    if [[ -n "$WORKSPACE_ORGID" ]]; then
        orgid_line=$'\n    orgID: "'"$WORKSPACE_ORGID"'"'
    fi

    # heredoc with variable expansion. The runtimeClass line is conditionally
    # inserted; omitting it entirely (vs setting empty string) matters because
    # the webhook treats empty-string as an explicit clear, not as unset.
    local manifest
    manifest=$(cat <<EOF
apiVersion: llmsafespaces.dev/v1
kind: Workspace
metadata:
  name: $name
  namespace: $NAMESPACE
  labels:
    llmsafespaces.dev/bench: gvisor-benchmark
  annotations:
$annotations_block
spec:
  owner:
    userID: "$WORKSPACE_USERID"$orgid_line
  runtime: "$WORKSPACE_RUNTIME"
  storage:
    size: 5Gi$runtime_class_line
EOF
)
    printf '%s\n' "$manifest" | kubectl apply -f - 2>&1 | sed 's/^/    /' >&2 || {
        fail "kubectl apply failed for workspace $name"
        return 1
    }
    echo "$name"
}

delete_workspace() {
    local ws="$1"
    kubectl delete workspace "$ws" -n "$NAMESPACE" --ignore-not-found >/dev/null 2>&1 || true
}

# Wait for a workspace to reach Active. Echoes wall-clock seconds from create.
wait_active() {
    local ws="$1" start="$2"
    local deadline=$(( $(date +%s) + 300 )) # 5 min cap
    while :; do
        local phase
        phase=$(kubectl get workspace "$ws" -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
        if [[ "$phase" == "Active" ]]; then
            awk -v a="$start" -v b="$(ns_now)" 'BEGIN{printf "%.3f", b-a}'
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

# Assert that the pod actually landed on the expected runtimeClass.
# Catches the regression where the API silently dropped the opt-out (the
# original v1 of this script had this bug — both legs ran under gVisor).
assert_runtime_class() {
    local ws="$1" expected="$2"
    local pod
    pod=$(kubectl get pods -n "$NAMESPACE" -l "llmsafespaces.dev/workspace=$ws" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [[ -z "$pod" ]]; then
        fail "no pod found for workspace $ws; cannot verify runtimeClass"
        return 1
    fi
    local actual
    actual=$(kubectl get pod "$pod" -n "$NAMESPACE" -o jsonpath='{.spec.runtimeClassName}' 2>/dev/null || true)
    # Empty runtimeClassName means kubelet default (typically runc on nodes
    # without runsc; gVisor if the cluster defaults to it via RuntimeClass).
    # For the gVisor leg, expected may be empty OR "gvisor" — both are valid
    # depending on how the cluster's RuntimeClass is wired. For the runc leg,
    # expected is "runc" and must match exactly.
    if [[ "$expected" == "runc" && "$actual" != "runc" ]]; then
        fail "runtimeClass assertion failed: expected '$expected' for $ws, got '$actual'"
        return 1
    fi
    ok "workspace $ws pod runtimeClassName='$actual' (expected=$expected)"
}

# Send a cold prompt and time first assistant content event. Echoes wall-clock seconds.
time_cold_prompt() {
    local ws="$1"
    local start
    start=$(ns_now)

    # Ensure an active session exists. The route is POST /sessions/new (not
    # POST /sessions — that route does not exist). The response field is
    # sessionId (not id — EnsureSessionResponse in pkg/types/session.go).
    local session_id
    session_id=$(curl -sS -X POST "$API_URL/api/v1/workspaces/$ws/sessions/new" \
        -H "Authorization: Bearer $API_TOKEN" \
        -H 'Content-Type: application/json' \
        -d '{}' | jq -r '.sessionId // empty')
    if [[ -z "$session_id" ]]; then
        fail "session create failed (POST /sessions/new returned no sessionId)"
        return 1
    fi

    # Time to first assistant content event via SSE. The SSE stream is
    # `data: <json>\n\n` (no `event:` lines — the type lives inside the JSON
    # payload as "type":"message.part.updated"). We exit on the first such
    # event to measure time-to-first-token. The timestamp is captured
    # OUTSIDE awk (after awk exits) using `date +%s.%N` — `systime()` inside
    # awk is integer-second resolution and loses sub-second precision.
    #
    # The END block uses a `found` flag to make awk exit non-zero when the
    # stream completes without a match (opencode error event, model
    # unavailable, auth failure, etc.). Without this guard, awk's default
    # exit-0 on EOF would let `&& ns_now` run anyway, producing a silently
    # wrong time-to-stream-end measurement instead of a clean failure.
    local event_at
    event_at=$(curl -sS -N --no-buffer -X POST "$API_URL/api/v1/workspaces/$ws/sessions/$session_id/message" \
        -H "Authorization: Bearer $API_TOKEN" \
        -H 'Content-Type: application/json' \
        --max-time 60 \
        -d "{\"model\":{\"providerID\":\"opencode\",\"modelID\":\"$LLM_MODEL\"},\"parts\":[{\"type\":\"text\",\"text\":\"Reply with exactly: OK\"}]}" \
        | awk '
            /^data:/ {
                line = $0
                sub(/^data:[ ]?/, "", line)
                if (line ~ /"type":"message\.part\.updated"/) { found = 1; exit 0 }
            }
            END { if (!found) exit 1 }
        ' && ns_now)
    if [[ -z "$event_at" ]]; then
        fail "no assistant part received (SSE stream ended or timed out without a message.part.updated event)"
        return 1
    fi
    awk -v a="$start" -v b="$event_at" 'BEGIN{printf "%.3f", b-a}'
}

# Run a pod-local file I/O round-trip and time it. Echoes wall-clock seconds.
# Uses kubectl exec on the pod directly (by pod name, looked up via the
# workspace label) — the Workspace CRD has no exec subresource.
time_file_io() {
    local ws="$1"
    local snippet='dd if=/dev/urandom of=/workspace/.bench bs=1M count=5 status=none && sha256sum /workspace/.bench >/dev/null && rm /workspace/.bench'
    local pod
    pod=$(kubectl get pods -n "$NAMESPACE" -l "llmsafespaces.dev/workspace=$ws" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [[ -z "$pod" ]]; then
        fail "no pod found for workspace $ws (file_io)"
        return 1
    fi
    local start
    start=$(ns_now)
    # The workspace pod has exactly one running container named "workspace"
    # (controller/internal/workspace/pod_builder.go:63). Omit -c so kubectl
    # uses the pod's first container by convention — robust to any future
    # container rename and avoids the previous bug (-c main, where "main"
    # was the variable name in pod_builder.go but never the actual
    # container name; review finding N5). Don't suppress stderr: on failure
    # the operator needs to see the kubectl error (review finding N6).
    if ! kubectl exec -n "$NAMESPACE" "$pod" -- sh -lc "$snippet" >/dev/null; then
        fail "file I/O snippet failed on pod $pod (see kubectl error above)"
        return 1
    fi
    awk -v a="$start" -v b="$(ns_now)" 'BEGIN{printf "%.3f", b-a}'
}

# --- header ---
printf 'runtime\tphase\titeration\tseconds\n'

# --- main loop: for each runtime, for each iteration, measure all 3 phases ---
for runtime in runc gvisor; do
    log "runtime=$runtime, $ITERATIONS iterations x 3 phases"
    for ((i=1; i<=ITERATIONS; i++)); do
        local_ws=""

        # trap ensures cleanup even on Ctrl-C or error between phases. The
        # `local_ws` variable is re-evaluated at trap-fire time, so it cleans
        # up the right workspace regardless of which phase failed.
        trap '[[ -n "$local_ws" ]] && delete_workspace "$local_ws"' EXIT

        local_ws=$(create_workspace "$runtime") || continue
        local create_start
        create_start=$(ns_now)

        # Phase 1: pod boot. On failure, delete and continue (don't leak).
        if ! boot_sec=$(wait_active "$local_ws" "$create_start"); then
            delete_workspace "$local_ws"
            continue
        fi
        printf '%s\t%s\t%d\t%s\n' "$runtime" "pod_boot" "$i" "$boot_sec"

        # Verify the pod actually landed on the expected runtime before
        # measuring phases 2-3 — a silent opt-out drop would otherwise make
        # both legs run under the same runtime and the comparison meaningless.
        if ! assert_runtime_class "$local_ws" "$runtime"; then
            delete_workspace "$local_ws"
            continue
        fi

        # Phase 2: cold prompt.
        if ! cold_sec=$(time_cold_prompt "$local_ws"); then
            delete_workspace "$local_ws"
            continue
        fi
        printf '%s\t%s\t%d\t%s\n' "$runtime" "cold_prompt" "$i" "$cold_sec"

        # Phase 3: file I/O.
        if ! io_sec=$(time_file_io "$local_ws"); then
            delete_workspace "$local_ws"
            continue
        fi
        printf '%s\t%s\t%d\t%s\n' "$runtime" "file_io" "$i" "$io_sec"

        delete_workspace "$local_ws"
        local_ws=""
        ok "runtime=$runtime iter=$i boot=${boot_sec}s cold=${cold_sec}s io=${io_sec}s"
    done
done

trap - EXIT
log "done — pipe output through the stats recipes in docs/operator/gvisor-benchmark.md"
