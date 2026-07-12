# Worklog: #469 — ConfigMap ClusterRole grant for free-models refresher

**Date:** 2026-07-12
**Session:** When `rbac.scope=cluster` AND `freeModelsRefresher.enabled=true` (both defaults), the ClusterRole was missing `configmaps` because the grant was gated only on `inferenceRelay.enabled`. The manager's cache created a cluster-wide ConfigMap informer on first Get and the reflector failed with "configmaps is forbidden ... at the cluster scope."
**Status:** Complete

---

## Objective

Add the `configmaps` grant to the ClusterRole when `freeModelsRefresher.enabled=true`, scoped to read-only verbs. Writes go through the namespace-scoped Role.

---

## Work Completed

### `helm/templates/rbac.yaml`

Changed the ClusterRole's configmaps block to mirror the namespace-scoped Role's `if/else if` pattern:

- `inferenceRelay.enabled=true`: full CRUD including delete (the reconciler destroys + reprovisions relay VMs).
- `freeModelsRefresher.enabled=true` (else if): **read-only** — `get/list/watch` only. The manager's DelegatingClient reads from the cache (needs list/watch at cluster scope for the informer) and writes to the live API (needs create/update at namespace scope, already granted by the Role). Granting cluster-scope CRUD would allow the controller SA to modify ANY ConfigMap in ANY namespace, re-introducing the CoreDNS-hijack surface Epic 17 removed.

### `helm/chart_test.go`

Two new tests:
- `TestClusterRole_ConfigMapsGrantedWhenFreeModelsEnabled` — asserts configmaps are present in the ClusterRole with exactly `{get, list, watch}` (read-only). Uses `assert.ElementsMatch` for the exact verb set.
- `TestClusterRole_ConfigMapsAbsentWhenBothDisabled` — negative: configmaps absent when both refresher and relay are disabled.

---

## Key Decisions

1. **Read-only at cluster scope.** The ClusterRole's purpose is to allow the informer cache to list/watch ConfigMaps cluster-wide. The cache only READS. Writes go through the DelegatingClient's writer, which uses the live API — and for namespace-scoped resources, the writer checks the Role (namespace-scoped). Granting `create/update/patch` at cluster scope is unnecessary and would allow modifying any ConfigMap in any namespace.

2. **Mirror the namespace Role's `if/else if`, not a single `if or`.** The inferenceRelay path needs full CRUD (including delete for VM reprovisioning); the free-models refresher path needs read-only only. A single `if or` would grant the broader verb set in both cases — wrong for least privilege.

3. **Existing `TestF131` satisfied without modification.** TestF131 already checks all controller Roles AND ClusterRoles for the absence of `delete` on configmaps when `freeModelsRefresher` is the only enabled feature. The read-only verb set (`get/list/watch`) naturally satisfies this.

---

## Tests Run

```
go test -timeout 50s -race -count=1 -run "TestClusterRole_ConfigMaps|TestF131" -v ./helm/...
  4/4 PASS (2 new + TestF131's 2 subtests)
```

---

## Files Modified

- `helm/templates/rbac.yaml` — expanded configmaps ClusterRole condition with if/else if pattern; read-only verbs for free-models refresher.
- `helm/chart_test.go` — 2 new tests + findClusterRoleByNameSubstr helper.
