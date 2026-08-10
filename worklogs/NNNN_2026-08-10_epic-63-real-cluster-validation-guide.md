# Epic 63 V2 Session Queue — Real Cluster Validation Guide

**Date:** 2026-08-10
**Purpose:** Manual validation instructions for the V2 session-queue feature against a real LLMSafeSpaces deployment before flipping the flag in production. This complements the automated nightly e2e (`local/us-63-v2-behavior-e2e.sh`) and is required before US-63.7 (legacy deletion).

---

## Prerequisites

1. A running Kubernetes cluster with LLMSafeSpaces installed (kind or real cluster)
2. `kubectl` pointed at the cluster
3. A workspace at Phase=Active with LLM credentials staged
4. `curl` + `python3` on PATH
5. The workspace name (default: `e2e-workspace`) and its namespace (default: `llmsafespaces`)

If using the local kind dev environment:
```bash
./local/bootstrap.sh
./local/test.sh   # creates workspace "e2e-workspace" + seeds API key
```

---

## Step 0: Establish port-forward + get API key

```bash
CTX="${CTX:-kind-llmsafespaces}"
NS="${NS:-llmsafespaces}"
WORKSPACE_NAME="${WORKSPACE_NAME:-e2e-workspace}"
PORTFWD_PORT="${PORTFWD_PORT:-18080}"

kubectl --context "$CTX" -n "$NS" port-forward svc/llmsafespaces-api ${PORTFWD_PORT}:8080 &
sleep 2

# The test.sh script seeds this API key. If you didn't run test.sh,
# create one via the admin UI or database insert.
API_KEY="lsp_e2etestkey1234567890abcdef"
```

Verify the API is reachable:
```bash
curl -sf "http://127.0.0.1:${PORTFWD_PORT}/livez" && echo " OK"
```

---

## Step 1: Enable the V2 flag

```bash
kubectl --context "$CTX" -n "$NS" set env deployment/llmsafespaces-api \
    LLMSAFESPACES_V2_SESSION_QUEUE=true
kubectl --context "$CTX" -n "$NS" rollout status deployment/llmsafespaces-api --timeout=120s
```

The API restarts with the V2 flag on. All enqueue/abort calls now use the V2 path.

---

## Step 2: Verify V2 endpoints are reachable (US-63.2)

Create a session and verify the V2 prompt + interrupt endpoints work:

```bash
# Create a session
SID=$(curl -sfm 15 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))")
echo "Session: $SID"

# Enqueue a message via POST /queue (V2 path)
curl -sfm 15 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"text":"reply with: hello"}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID}/queue"
echo ""

# Wait for it to run, then check history
sleep 10
curl -sfm 15 \
    -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID}/message" \
    | python3 -c "import json,sys;d=json.load(sys.stdin);print([p.get('text','') for m in d.get('data',d) if isinstance(m,dict) for p in m.get('parts',[]) if p.get('type')=='text'])"
```

**Expected:** The message appears in history after a few seconds.

---

## Step 3: US-63.3 — Enqueue while busy → runs after

```bash
SID2=$(curl -sfm 15 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))")

# Start a long turn (background)
LLM_MODEL="${LLM_MODEL:-default}"
BODY=$(python3 -c "import json;print(json.dumps({'model':{'providerID':'litellm','modelID':'${LLM_MODEL}'},'parts':[{'type':'text','text':'write a 200-word essay about the ocean'}]}))")
curl -sfm 180 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "$BODY" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID2}/message" &
sleep 3

# Enqueue while busy — must NOT get 409
MARKER="ACK_BUSY_$(date +%s)"
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"text\":\"reply with exactly: ${MARKER}\"}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID2}/queue")

if [ "$CODE" = "202" ]; then
    echo "PASS: enqueue while busy returned 202 (not 409)"
else
    echo "FAIL: enqueue while busy returned $CODE (expected 202)"
fi

# Wait for the marker to appear in history
wait
sleep 5
for i in $(seq 1 20); do
    HIST=$(curl -sfm 15 \
        -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID2}/message" 2>/dev/null || echo '{}')
    if echo "$HIST" | grep -q "$MARKER"; then
        echo "PASS: queued message ran after the long turn"
        break
    fi
    sleep 3
done
```

**Expected:** 202 (not 409) on the busy enqueue, and the marker appears in history after the long turn completes.

---

## Step 4: US-63.4 — Abort mid-turn → queued messages survive

```bash
SID3=$(curl -sfm 15 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))")

# Start a long turn
BODY=$(python3 -c "import json;print(json.dumps({'model':{'providerID':'litellm','modelID':'${LLM_MODEL}'},'parts':[{'type':'text','text':'write a 200-word essay about mountains'}]}))")
curl -sfm 180 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "$BODY" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID3}/message" &
sleep 3

# Queue two messages
MARKER_A="ABORT_A_$(date +%s)"
MARKER_B="ABORT_B_$(date +%s)"
curl -s -o /dev/null -w "A=%{http_code} " -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"text\":\"reply with exactly: ${MARKER_A}\"}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID3}/queue"
curl -s -o /dev/null -w "B=%{http_code}\n" -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"text\":\"reply with exactly: ${MARKER_B}\"}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID3}/queue"

# Abort mid-turn — must get 204
sleep 2
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID3}/abort")
echo "Abort: $CODE (expected 204)"

# Wait for both markers to appear — they must survive the abort
wait
for i in $(seq 1 30); do
    HIST=$(curl -sfm 15 \
        -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID3}/message" 2>/dev/null || echo '{}')
    HAS_A=$(echo "$HIST" | grep -c "$MARKER_A" || true)
    HAS_B=$(echo "$HIST" | grep -c "$MARKER_B" || true)
    if [ "$HAS_A" -gt 0 ] && [ "$HAS_B" -gt 0 ]; then
        echo "PASS: both queued messages survived abort and ran"
        break
    fi
    sleep 3
done
```

**Expected:** 204 on abort, and both markers appear in history.

---

## Step 5: US-63.9 — Kill opencode → restart → stranded messages drain

**This is the load-bearing test.** The spike confirmed input strands without a wake trigger.

```bash
SID4=$(curl -sfm 15 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))")

# Start a long turn + queue a marker
BODY=$(python3 -c "import json;print(json.dumps({'model':{'providerID':'litellm','modelID':'${LLM_MODEL}'},'parts':[{'type':'text','text':'write a 300-word essay about the moon'}]}))")
curl -sfm 180 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "$BODY" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID4}/message" &
sleep 3

MARKER="OOM_DRAIN_$(date +%s)"
curl -s -o /dev/null -w "enqueue=%{http_code}\n" -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"text\":\"reply with exactly: ${MARKER}\"}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID4}/queue"

# Kill opencode inside the pod
POD=$(kubectl --context "$CTX" -n "$NS" get pod \
    -l "llmsafespaces.dev/workspace=${WORKSPACE_NAME}" \
    -o jsonpath='{.items[0].metadata.name}')
echo "Killing opencode in pod $POD..."
kubectl --context "$CTX" -n "$NS" exec "$POD" -c main -- \
    sh -c 'pkill -9 -f "opencode serve" || pkill -9 -f opencode || true'

# Wait for opencode to restart (~10s)
echo "Waiting for opencode to restart..."
for i in $(seq 1 15); do
    PHASE=$(kubectl --context "$CTX" -n "$NS" get workspace "$WORKSPACE_NAME" \
        -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    if [ "$PHASE" = "Active" ]; then
        echo "Workspace Active after restart"
        sleep 5  # give agentd time to restart opencode + reconnect SSE
        break
    fi
    sleep 2
done

# Trigger an SSE reconnect (re-list sessions causes the proxy tracker to reconnect)
curl -sfm 5 \
    -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" >/dev/null

# Wait for the marker to drain (the wake should fire on reconnect)
echo "Waiting for stranded marker to drain (up to 120s)..."
for i in $(seq 1 40); do
    HIST=$(curl -sfm 15 \
        -H "Authorization: Bearer ${API_KEY}" \
        "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID4}/message" 2>/dev/null || echo '{}')
    if echo "$HIST" | grep -q "$MARKER"; then
        echo "PASS: stranded message drained after restart"
        break
    fi
    sleep 3
done
```

**Expected:** The marker appears in history within 120s of the restart — the proxy's `wakeStrandedV2Sessions` fires on SSE reconnect and triggers the drain.

**⚠️ If this FAILS:** The `\n` wake prompt may not work. Check:
1. API logs for "V2 stranded-input recovery: waking idle session"
2. API logs for "V2 stranded-input recovery: wake prompt failed"
3. Whether opencode accepted the `{prompt:{text:"\n"}}` body (check pod logs for 400 errors)

---

## Step 6: US-63.10 — Fresh-load queue visibility

```bash
SID5=$(curl -sfm 15 -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions" \
    | python3 -c "import json,sys;print(json.load(sys.stdin).get('id',''))")

# Queue a message
curl -s -o /dev/null -w "%{http_code}\n" -X POST \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d '{"text":"reply with: fresh-load-test"}' \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID5}/queue"

# Immediately GET /queue — the shadow should have the pill
QUEUE=$(curl -sfm 5 \
    -H "Authorization: Bearer ${API_KEY}" \
    "http://127.0.0.1:${PORTFWD_PORT}/api/v1/workspaces/${WORKSPACE_NAME}/sessions/${SID5}/queue")
echo "Queue: $QUEUE"

COUNT=$(echo "$QUEUE" | python3 -c "import json,sys;d=json.load(sys.stdin);print(len(d.get('messages',[])))")
if [ "$COUNT" -ge "1" ]; then
    echo "PASS: fresh-load shows queued pill"
else
    echo "FAIL: fresh-load shows no pills (shadow not populated)"
fi
```

**Expected:** `GET /queue` returns at least one message from the shadow marker.

---

## Step 7: Disable V2 flag (cleanup)

```bash
kubectl --context "$CTX" -n "$NS" set env deployment/llmsafespaces-api \
    LLMSAFESPACES_V2_SESSION_QUEUE=false
kubectl --context "$CTX" -n "$NS" rollout status deployment/llmsafespaces-api --timeout=120s
```

---

## Automated version

The entire Step 1-6 sequence is automated in:

```bash
CTX="kind-llmsafespaces" NS="llmsafespaces" WORKSPACE_NAME="e2e-workspace" \
    PORTFWD_PORT=18080 LLM_MODEL="default" \
    bash local/us-63-v2-behavior-e2e.sh
```

This script enables the flag, runs all assertions, and disables the flag at the end. It exits non-zero on any failure.

---

## Known risks to watch for

1. **`\n` wake prompt (US-63.9):** If opencode trims whitespace and rejects `\n` as empty, the wake fails silently. Mitigation: try `" "` (single space) as an alternative.

2. **SSE reconnect behavior:** The proxy's `reconcileSessionState` fires on SSE reconnect. If the reconnect path doesn't reach `wakeStrandedV2Sessions`, messages strand. Check API logs.

3. **Multi-replica failover:** `v2PendingSessions` is in-memory per-replica. If the API replica handling the workspace's SSE stream dies, tracking is lost. Accepted limitation for now.

4. **Shadow marker staleness (US-63.10):** The shadow has a 10-minute TTL. A long turn + SSE disconnect could leave phantom pills. Self-heals on TTL expiry.

---

## Sign-off

Before flipping the flag in production (or deleting the legacy queue in US-63.7), all six steps above must PASS on a real cluster. Record the results in a worklog with the date, cluster name, and any failures + root causes.
