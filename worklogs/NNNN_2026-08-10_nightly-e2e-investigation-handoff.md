# Nightly E2E Investigation — Hand-Off Prompt

**Date:** 2026-08-10
**Branch:** `docs/nightly-e2e-investigation-handoff`
**Purpose:** The nightly e2e (`e2e-nightly.yml`) has never successfully completed `local/test.sh` end-to-end. Each fix peels back the next layer of pre-existing infra issues. This worklog is a prompt for an in-cluster agent to investigate and fix the remaining blockers so the V2 behavioral e2e can run.

---

## Background

The nightly e2e workflow (`.github/workflows/e2e-nightly.yml`) creates a kind cluster, builds images, installs LLMSafeSpaces, runs `local/test.sh`, then runs the V2 behavioral e2e (`local/us-63-v2-behavior-e2e.sh`). The V2 behavioral e2e (US-63.3/63.4/63.9) has NEVER run in CI because `test.sh` fails before it.

### Fixes already applied (merged on main)

These were all pre-existing issues masked by earlier failures:

1. **Postgres Secret ordering** (PR #712): `local/postgres-redis.yaml` was applied before the helm chart created the `llmsafespaces-credentials` Secret. Postgres CrashLoopBackoff'd. Fixed by creating the Secret before the manifest.

2. **RuntimeEnvironment image allow-list**: The validating webhook rejects non-`ghcr.io/lenaxia/` images. Fixed by retagging `runtime-base` with the `ghcr.io/lenaxia/` prefix before loading into kind.

3. **`spec.defaultRuntime` → `spec.runtime`** in test.sh: stale CRD field name.

4. **`deployment/redis-master` → `deployment/valkey`** in bootstrap.sh: Valkey deployment is named `valkey`, not `redis-master`.

5. **`app.kubernetes.io/name=postgres` → `app=postgres`** in test.sh: pod label mismatch.

### Current failure point (as of latest run `31409979192`)

Test 4/9 in `local/test.sh`: **Workspace lifecycle (create → Active) times out at 90s.** The Workspace CRD is created successfully (Test 4 starts), but never reaches `Phase=Active`. The test dies with "Workspace timeout".

### What was NOT captured

The nightly's failure dump (`kubectl get pods -o wide` + API/controller logs) ran but I cannot read the pod-level output from the CI run without cluster access. The workspace likely fails to become Active because the pod doesn't start — but I can't see whether it's an ImagePullBackoff, a CrashLoopBackoff, a PVC binding failure, or a controller reconciliation error.

---

## Investigation prompt for in-cluster agent

```
You are debugging why the LLMSafeSpaces nightly e2e never completes
local/test.sh. The repo is github.com/lenaxia/LLMSafeSpaces on branch
main. The nightly workflow is .github/workflows/e2e-nightly.yml.

## The problem

Test 4/9 in local/test.sh creates a Workspace CRD and waits up to 90s
for Phase=Active. It times out. The workspace never becomes Active.

## What to do

1. Create a kind cluster and install LLMSafeSpaces using the SAME steps
   as the nightly:

     kind create cluster --config local/kind-cluster.yaml --name llmsafespaces-ci
     docker build --network host -f api/Dockerfile -t llmsafespaces/api:ci .
     docker build --network host -f controller/Dockerfile -t llmsafespaces/controller:ci .
     docker build --network host -f runtimes/base/Dockerfile -t llmsafespaces/runtime-base:ci .
     docker tag llmsafespaces/runtime-base:ci ghcr.io/lenaxia/llmsafespaces/runtime-base:ci
     kind load docker-image llmsafespaces/api:ci --name llmsafespaces-ci
     kind load docker-image llmsafespaces/controller:ci --name llmsafespaces-ci
     kind load docker-image ghcr.io/lenaxia/llmsafespaces/runtime-base:ci --name llmsafespaces-ci
     kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.0/cert-manager.yaml
     kubectl -n cert-manager rollout status deployment/cert-manager --timeout=180s
     kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=180s

     kubectl create namespace llmsafespaces
     kubectl -n llmsafespaces create secret generic llmsafespaces-credentials \
         --from-literal=postgres-password=e2e-pg-pw-2026 \
         --from-literal=redis-password=e2e-redis-pw-2026
     kubectl apply -f local/postgres-redis.yaml
     kubectl -n llmsafespaces rollout status deployment/postgres --timeout=180s
     kubectl -n llmsafespaces rollout status deployment/valkey --timeout=180s

     helm upgrade --install llmsafespaces helm \
         -n llmsafespaces \
         --set api.image.repository=llmsafespaces/api \
         --set api.image.tag=ci \
         --set api.image.pullPolicy=IfNotPresent \
         --set controller.image.repository=llmsafespaces/controller \
         --set controller.image.tag=ci \
         --set controller.image.pullPolicy=IfNotPresent \
         --set mcp.enabled=false \
         --set postgresql.host=postgres \
         --set postgresql.port=5432 \
         --set postgresql.user=llmsafespaces \
         --set postgresql.database=llmsafespaces \
         --set redis.host=redis-master \
         --set redis.port=6379 \
         --set externalSecret.create=true \
         --set externalSecret.postgresPassword=e2e-pg-pw-2026 \
         --set externalSecret.redisPassword=e2e-redis-pw-2026 \
         --set api.config.logging.development=true \
         --wait --timeout 5m

     kubectl -n llmsafespaces rollout status deployment/llmsafespaces-api --timeout=180s
     kubectl -n llmsafespaces rollout status deployment/llmsafespaces-controller --timeout=180s

2. Create a RuntimeEnvironment (the nightly does this in test.sh Test 3):

     cat <<EOF | kubectl apply -f -
     apiVersion: llmsafespaces.dev/v1
     kind: RuntimeEnvironment
     metadata:
       name: python-3.11
     spec:
       image: ghcr.io/lenaxia/llmsafespaces/runtime-base:ci
       description: "Python 3.11 runtime"
     EOF

3. Create a Workspace (test.sh Test 4):

     cat <<EOF | kubectl apply -f -
     apiVersion: llmsafespaces.dev/v1
     kind: Workspace
     metadata:
       name: e2e-workspace
       labels:
         user-id: e2e-user
     spec:
       owner:
         userID: e2e-user
       runtime: python:3.11
       storage:
         size: 1Gi
         accessMode: ReadWriteOnce
     EOF

4. Watch the workspace phase:

     kubectl get workspace e2e-workspace -o yaml -w

   AND watch for pod events:

     kubectl get events -n llmsafespaces --sort-by='.lastTimestamp' -w

5. If the workspace stays Pending/Creating for >30s, check:

   a. Does the pod exist?
        kubectl -n llmsafespaces get pods
   b. What does the pod status say?
        kubectl -n llmsafespaces describe pod -l llmsafespaces.dev/workspace=e2e-workspace
   c. Are there events explaining why the pod isn't scheduling?
        kubectl -n llmsafespaces get events --field-selector reason!=Scheduled
   d. Controller logs:
        kubectl -n llmsafespaces logs deployment/llmsafespaces-controller --tail=50
   e. Is the PVC binding?
        kubectl -n llmsafespaces get pvc
   f. Is the pod image pullable? (ImagePullBackOff would show in describe)
   g. Is the runtime image present in the kind node?
        docker exec llmsafespaces-ci-control-plane crictl images | grep runtime-base

6. Fix whatever is blocking the workspace from reaching Active. Common
   suspects:
   - ImagePullBackOff: the runtime-base image wasn't loaded into kind
     correctly, or the tag/ref doesn't match the RuntimeEnvironment spec
   - PVC stuck Pending: the kind default StorageClass might not support
     the access mode or the PVC might be waiting for a provisioner
   - Controller error: the controller might be failing to reconcile
     (check logs for stack traces or reconciliation errors)
   - Pod stuck in ContainerCreating: init containers failing, volume
     mount issues, or secrets missing
   - Validating webhook rejection: the workspace spec might have fields
     the webhook rejects (check controller logs for admission denials)

7. Once the workspace reaches Active, continue running test.sh and fix
   any subsequent failures (Tests 5-9). Each fix should be committed on
   a branch named fix/nightly-e2e-infra.

8. After test.sh passes, run the V2 behavioral e2e:

     CTX=kind-llmsafespaces-ci NS=llmsafespaces WORKSPACE_NAME=e2e-workspace \
         PORTFWD_PORT=18080 \
         bash local/us-63-v2-behavior-e2e.sh

   This enables the V2 flag, runs the three behavioral assertions
   (US-63.3 enqueue-while-busy, US-63.4 non-destructive-abort,
   US-63.9 OOM-restart-drain), and disables the flag. It requires LLM
   credentials (LLM_BASE_URL, LLM_API_KEY, LLM_MODEL) to produce real
   turns. If no LLM credentials are available, the script will still
   run but the behavioral assertions that require real turn activity
   will timeout.

9. Document every issue found and fixed in a worklog. Commit with
   conventional commit format. Follow the repo's conventions (AGPL
   headers, NNNN_ worklog sentinels, gofmt, go vet). Read README-LLM.md
   for the hard rules.

## Expected output

A short summary (under 300 words) containing:
1. What blocked the workspace from reaching Active (root cause)
2. Each fix applied (commit SHAs + branch name)
3. Whether test.sh passes end-to-end
4. Whether the V2 behavioral e2e passes
5. Whether the nightly is now ready to run successfully on schedule

## Important notes

- The nightly runs WITHOUT LLM credentials (secrets.LLM_API_KEY is
  empty in the scheduled trigger). Test 6 (prompt round-trip) and
  Test 8 (suspend/resume) are skipped when LLM creds are absent.
  The workspace still needs to reach Active — that doesn't require
  an LLM. If you're testing manually and have LLM creds, set them;
  if not, the workspace reaching Active is the gate.

- The kind cluster uses the default StorageClass. If PVCs don't bind,
  check that a default StorageClass exists:
    kubectl get storageclass
  kind ships with a standard provisioner but it may need to be set as
  default.

- The controller deploys validating webhooks for Workspace and
  RuntimeEnvironment CRDs. These require cert-manager (installed in
  step 1). If the webhooks aren't ready, CRD creation will fail with
  connection errors.

- DO NOT modify any Epic 63 V2 code. The V2 code is validated and
  merged. You are fixing nightly e2e infrastructure only.
```
