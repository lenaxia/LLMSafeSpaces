# Worklog: workspace.defaultStorageClass Helm pathway + v0.2.2 chart bump

**Date:** 2026-07-07
**Session:** Add Helm-values pathway for `workspace.defaultStorageClass`; bump chart 0.2.1 → 0.2.2.

**Status:** Complete. Ready for PR.

## Context

The 2026-07-07 workspace-stuck-in-creating incident traced to Longhorn `DiskPressure` — all 3 worker disks had less than 25% free of their `storageMaximum`, so Longhorn refused to schedule new replicas. Investigation showed the LLMSafeSpaces cluster had 6 workspace PVCs at `numberOfReplicas=3`, consuming ~50% of scheduled disk space cluster-wide. Ephemeral workspaces don't need triple-replication.

The fix landed in two parts:

1. **Ops-side (immediate):** Patched all 6 existing workspace `volumes.longhorn.io` from 3r → 2r in-place. Longhorn evicted one replica per volume. `DiskPressure` cleared on 2/3 nodes; new workspaces provisioned successfully.

2. **Chart-side (this PR):** New Longhorn StorageClass `longhorn-2r` (non-default) added in `talos-ops-prod`. LLMSafeSpaces chart gains a Helm-values pathway so operators can declare `workspace.defaultStorageClass: longhorn-2r` in the chart values and have every new workspace PVC land on it without relying on admin-UI configuration.

## Why the code doesn't already do this

The `workspace.defaultStorageClass` instance setting already existed (schema key at `pkg/settings/schema.go:85`, DB-backed, Tier 2, admin-mutable via `PUT /admin/settings/{key}`). The API's create-workspace path (`api/internal/services/workspace/workspace_service.go:1117`) already reads it and populates `crd.Spec.Storage.StorageClassName`.

What was missing: a way to pin the value from Helm so:

- Fresh installs get the operator's chosen SC without a manual UI step
- The admin UI shows the setting as read-only (managed by Helm) rather than silently accepting a change that Helm will fight next reconcile

This mirrors the `email.*` pathway added in US-49.2 (`app.go:243-250`) — same `SetHelmOverrides` mechanism.

## Changes

### Chart

- `charts/llmsafespaces/values.yaml` — extend the existing `workspace:` block with `defaultStorageClass: ""` and a comment explaining the semantics (empty = admin-mutable Tier 2; non-empty = Helm-managed Tier 1).
- `charts/llmsafespaces/templates/configmap-api.yaml` — render `workspace: defaultStorageClass: <value>` into `config.yaml` when non-empty (guarded by `{{- with .Values.workspace.defaultStorageClass }}`).
- `charts/llmsafespaces/Chart.yaml` — `version` + `appVersion` bumped 0.2.1 → 0.2.2.
- `charts/llmsafespaces/README.md` — version references bumped 0.2.1 → 0.2.2.

### API

- `api/internal/config/config.go` — add `Workspace` struct to `Config` with `DefaultStorageClass string` mapstructure-tagged `workspace.defaultStorageClass`. Comment explains the Helm-managed vs admin-mutable semantics.
- `api/internal/app/app.go` — after the `email.*` `SetHelmOverrides` block, add a symmetric block for `workspace.defaultStorageClass` guarded on `cfg.Workspace.DefaultStorageClass != ""`.

### Docs

- `README-LLM.md` — extend "Storage Settings" section with a paragraph documenting the Helm-managed override for `workspace.defaultStorageClass`.

### Tests

- `charts/llmsafespaces/chart_test.go` — 3 new tests:
  - `TestWorkspace_DefaultRender_OmitsWorkspaceBlock`: with default values, no top-level `workspace:` block appears in the rendered config.yaml. Uses regex `(?m)^workspace:` because `create_workspace:` (rate-limit key) contains the substring.
  - `TestWorkspace_DefaultStorageClass_RendersBlock`: setting the value renders the block with the right value.
  - `TestWorkspace_DefaultStorageClass_OperatorOverride`: alternate SC names flow through unmodified.
- `api/internal/config/config_test.go` — 2 new tests:
  - `TestConfig_Workspace_DefaultStorageClass_Empty`: field parses to `""` when absent from config.yaml.
  - `TestConfig_Workspace_DefaultStorageClass_FromYAML`: field parses when set.

## Test results

```
$ go test -timeout 120s ./api/internal/config/... ./pkg/settings/...
ok  	github.com/lenaxia/llmsafespaces/api/internal/config	0.018s
ok  	github.com/lenaxia/llmsafespaces/pkg/settings	0.064s

$ go test -timeout 300s ./charts/llmsafespaces/
ok  	github.com/lenaxia/llmsafespaces/charts/llmsafespaces	52.142s

$ go build ./...
(clean)
```

## Upgrade notes

- No schema migration. `workspace.defaultStorageClass` key already exists in `instance_settings`.
- FluxCD users: keep `reconcileStrategy: Revision`.
- Helm chart `appVersion` bumped 0.2.1 → 0.2.2. `helm install` with no image-tag override resolves to GHCR `:0.2.2`.
- **Behavior change on upgrade only when operators set the new value.** Clusters that don't set `workspace.defaultStorageClass` in values.yaml see zero behavior change — the setting stays Tier 2 admin-mutable exactly as today.
- Existing workspaces (already-provisioned PVCs) are unaffected. The setting only influences new workspaces. Existing 3r workspaces at cluster scope should be shrunk in-place via `kubectl patch volumes.longhorn.io ... numberOfReplicas=2` (safer than PVC migration).

## Follow-ups

- ops-prod: after this chart lands, update the `LLMSafeSpaces` HelmRelease values to set `workspace.defaultStorageClass: longhorn-2r`, and bump the GitRepository ref to `tag: v0.2.2`.
- Consider a controller-level metric / alert when a workspace CRD is created with `StorageClassName` targeting a SC that doesn't exist. Today the failure surfaces as "AttachVolume.Attach failed" only after pod scheduling.
