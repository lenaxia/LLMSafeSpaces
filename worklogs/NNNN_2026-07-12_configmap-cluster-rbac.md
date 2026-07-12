# Worklog: #469 — ConfigMap ClusterRole grant for free-models refresher

**Date:** 2026-07-12
**Session:** When `rbac.scope=cluster` AND `freeModelsRefresher.enabled=true` (both defaults), the ClusterRole was missing `configmaps` because the grant was gated only on `inferenceRelay.enabled`. The manager's cache created a cluster-wide ConfigMap informer on first Get and the reflector failed with "configmaps is forbidden ... at the cluster scope."
**Status:** Complete

---

## Objective

Add the `configmaps` grant to the ClusterRole when `freeModelsRefresher.enabled=true`, not just when `inferenceRelay.enabled=true`.

---

## Work Completed

### `helm/templates/rbac.yaml`

Changed the ClusterRole's configmaps condition from:
```
{{- if .Values.controller.inferenceRelay.enabled }}
```
to:
```
{{- if or .Values.controller.inferenceRelay.enabled .Values.controller.freeModelsRefresher.enabled }}
```

One-line change. The verb set (CRUD) is the same in both cases because the free-models refresher also needs create/update/patch to sync its ConfigMap.

### `helm/chart_test.go`

Two new tests:
- `TestClusterRole_ConfigMapsGrantedWhenFreeModelsEnabled` — positive: configmaps present in ClusterRole when freeModelsRefresher.enabled=true + rbac.scope=cluster, even when inferenceRelay.enabled=false.
- `TestClusterRole_ConfigMapsAbsentWhenBothDisabled` — negative: configmaps absent when both are disabled. Prevents accidental over-granting.

---

## Key Decisions

1. **Expand the OR condition, not add a separate block.** The inferenceRelay and freeModelsRefresher both need the same verbs on configmaps. A single `if or` block is simpler than two separate blocks with identical content.

2. **CRUD verbs, not just read-only.** The free-models refresher calls `SyncConfigMap` which does Get + Create-or-Update. The leader-election Role already grants CRUD for the narrower case; the ClusterRole needs CRUD too because the manager's cached client creates an informer (needs list/watch) and the SyncConfigMap path writes (needs create/update/patch).

---

## Tests Run

```
go test -timeout 50s -race -count=1 -run "TestClusterRole_ConfigMaps" -v ./helm/...
  2/2 PASS
```

---

## Files Modified

- `helm/templates/rbac.yaml` — expanded configmaps ClusterRole condition to include freeModelsRefresher.enabled.
- `helm/chart_test.go` — 2 new tests + findClusterRoleByNameSubstr helper.
