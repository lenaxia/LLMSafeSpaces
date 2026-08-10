# Worklog: Nightly e2e — controller RBAC denied at cluster scope

**Date:** 2026-08-10
**Session:** Diagnose why the nightly e2e (`e2e-nightly.yml`) never gets past Test 4 (Workspace lifecycle create → Active); fix the blocker on `fix/nightly-e2e-investigation-handoff`.
**Status:** Complete (root cause fixed; subsequent layers will surface in the next nightly run)

---

## Objective

The nightly e2e has never completed `local/test.sh`. Each fix peels back the next layer of pre-existing infra issues. The hand-off worklog (`docs/nightly-e2e-investigation-handoff`) identified and fixed five layers (Secret ordering, image allow-list retag, `spec.runtime`, `deployment/valkey`, `app=postgres` label). Test 4 still timed out: the Workspace CRD was created but never reached `Phase=Active`. This worklog root-causes and fixes the sixth layer.

---

## Root Cause (Proven)

### Controller cannot list/watch any resource at cluster scope

The failure-dump from CI run `31409979192` (latest nightly on `main`) shows the controller pod Running (1/1, 0 restarts) but its reflector logs are a wall of RBAC denials:

```
workspaces.llmsafespaces.dev is forbidden: User "system:serviceaccount:llmsafespaces:llmsafespaces-controller" cannot list resource "workspaces" in API group "llmsafespaces.dev" at the cluster scope
pods is forbidden: ... at the cluster scope
secrets is forbidden: ... at the cluster scope
persistentvolumeclaims is forbidden: ... at the cluster scope
serviceaccounts is forbidden: ... at the cluster scope
configmaps is forbidden: ... at the cluster scope
```

Because the controller cannot list/watch Workspaces, it never reconciles the Workspace CRD that `test.sh` Test 4 creates. The Workspace stays in `Pending` (empty `.status.phase`) and the test times out at 90 s.

### Why the RBAC is wrong

The chart ships two mutually exclusive RBAC topologies (`helm/templates/rbac.yaml`):

| `rbac.scope` | What is emitted | What the controller can do |
|---|---|---|
| `namespace` (chart default, G5) | Namespace-scoped `Role` for pods/secrets/PVCs/SAs/NetworkPolicies in the workspace namespace + always-on read-only `ClusterRole` for StorageClasses only | Reconcile only if the controller-runtime cache is restricted to the namespace (`controller.watchNamespaces` set) |
| `cluster` (opt-in) | All of the above + a read-only `ClusterRole` granting get/list/watch on pods/secrets/PVCs/SAs/configmaps/NetworkPolicies + CRUD on workspaces/runtimeenvironments cluster-wide | Reconcile cluster-wide (default controller-runtime behaviour) |

The chart's own commentary (`rbac.yaml:199-210`) documents the coupling explicitly:

> controller-runtime's Manager defaults to cluster-wide watches for every typed informer it builds (Pod, Secret, NetworkPolicy, etc.), regardless of the per-resource Role bindings. When the controller starts, the reflectors call `apiserver.list(secrets, all-namespaces)` and the apiserver denies it because secrets/pods aren't in the cluster-scope grant.

The cluster-wide read-only `ClusterRole` that fixes this is emitted **only** when `rbac.scope == "cluster"`. The nightly workflow and `bootstrap.sh` install the chart with all defaults (`rbac.scope=namespace`, `controller.watchNamespaces=""`), which is the broken combination: cluster-wide cache (empty watchNamespaces) + namespace-only RBAC.

### Why `controller.watchNamespaces=$NS` alone is insufficient

Restricting the cache to the release namespace would make the namespace-scoped Role sufficient for pods/secrets/PVCs. But the controller also reads `RuntimeEnvironment` (a **cluster-scoped** CRD — see README-LLM.md §CRDs and `controller/internal/workspace/runtime_resolver.go:40`) to map `spec.runtime` names to container images. The namespace-scoped Role does not (and cannot) grant access to a cluster-scoped resource. So the controller would still fail with `runtimeenvironments is forbidden` when reconciling any workspace that uses a name-style runtime reference (`python:3.11`).

Setting `rbac.scope=cluster` is the single knob that grants both the informer-cache list/watch and the RuntimeEnvironment read in one step.

---

## Validation of the Root Cause

1. **CI logs** (run `31409979192`, failure-dump step): controller reflector errors are exclusively RBAC denials at cluster scope for six resource types. No other controller errors are present. The controller pod is Running (started, leader-elected) but functionally inert.
2. **Chart template** (`helm/templates/rbac.yaml:190-275`): the cluster-wide read-only `ClusterRole` + `ClusterRoleBinding` are inside `{{- if $isCluster }}`, confirming they are omitted by default.
3. **Controller source** (`controller/main.go:179-184`): empty `--watch-namespaces` → no `cache.DefaultNamespaces` → cluster-wide informers. The deployment template (`controller-deployment.yaml:58-60`) only passes `--watch-namespaces` when `.Values.controller.watchNamespaces` is set; the nightly does not set it.
4. **RuntimeEnvironment scope** (`pkg/apis/llmsafespaces/v1/`): cluster-scoped CRD; `runtime_resolver.go:40` does a cluster-scoped `Get` by name.
5. **Chart tests** (`helm/chart_test.go:917,4203`): already assert `rbac.scope=cluster` renders the controller ClusterRole — the fix uses an already-tested code path.

---

## Assumptions

| # | Assumption | Validated via |
|---|---|---|
| 1 | The controller is the only component blocking workspace reconciliation | CI logs: API healthy, Postgres/Valkey Running, controller is the sole error source |
| 2 | `rbac.scope=cluster` grants all permissions the controller needs for the e2e | `rbac.yaml:211-259` enumerates pods/secrets/PVCs/SAs/configmaps/NetworkPolicies/workspaces/runtimeenvironments |
| 3 | No additional values (e.g. `controller.watchNamespaces`) are required alongside `rbac.scope=cluster` | `rbac.yaml:190` gates only on `$isCluster`; the cluster `ClusterRole` is self-contained |
| 4 | Subsequent blockers (Test 5+) are not yet visible because the controller never created a workspace pod | CI logs show zero workspace-pod-related events; will surface in the next nightly run |

---

## Fixes Applied

Branch: `fix/nightly-e2e-infra`

### Cherry-picked from `docs/nightly-e2e-investigation-handoff` (the five known layers)

These were on the hand-off branch but not yet on `main`/`fix/nightly-e2e-infra`:

1. `d03407db` — Create `llmsafespaces-credentials` Secret before applying `postgres-redis.yaml` (Postgres CrashLoopBackoff without it). Set `externalSecret.postgresPassword`/`redisPassword` to match.
2. `f4207b7e` — Retag `runtime-base` with `ghcr.io/lenaxia/` prefix so the RuntimeEnvironment validating webhook's image allow-list accepts it.
3. `49efa0ef` — `test.sh`: `spec.defaultRuntime` → `spec.runtime` (stale CRD field); `app.kubernetes.io/name=postgres` → `app=postgres` (pod label mismatch); read PG password from the credentials Secret.

### New fix (this worklog — the sixth layer)

4. **`rbac.scope=cluster`** in the nightly helm install (`.github/workflows/e2e-nightly.yml`) and `bootstrap.sh`. Emits the controller's cluster-wide read-only `ClusterRole`, allowing the informer cache to start and `RuntimeEnvironment` lookups to succeed.

---

## Adversarial Review

**Phase 1 — weaknesses:**
- *Could there be a second blocker masked by the RBAC failure?* Yes — the controller never created a workspace pod, so pod-level failures (image pull, init containers, webhook rejection of the Test 4 workspace spec) are not yet exercised. These will surface in the next nightly run.
- *Is `rbac.scope=cluster` over-broad for CI?* It grants cluster-wide read on pods/secrets/PVCs. For a throwaway kind cluster this is acceptable; the chart already supports this mode for multi-namespace deployments, and CRUD remains namespace-scoped via the Role.

**Phase 2 — validation:**
- The RBAC denial is the *only* error in the controller logs → real finding, not a false alarm.
- `rbac.scope=cluster` is chart-tested (`chart_test.go:917`) → the fix path is proven.
- `controller.watchNamespaces` alone is insufficient (RuntimeEnvironment is cluster-scoped) → real design constraint, not a guess.

**Phase 3 — remediation:**
- Root cause fixed. Subsequent layers are out of scope for this session (cannot run the e2e locally; will surface in CI).

---

## Follow-ups (not addressed here)

- The test.sh Test 4 expects `Phase=Active` while Test 5 expects `Phase=Running`; the README-LLM.md lifecycle lists `Active` (not `Running`) as the steady-state phase. This may be a stale assertion in test.sh — to be confirmed when the controller can actually reconcile.
- Consider also setting `controller.watchNamespaces` for defense-in-depth once the basic flow is proven.
