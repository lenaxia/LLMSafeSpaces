# Worklog: workspace.defaultStorageClass Helm pathway + v0.2.2 chart bump

**Date:** 2026-07-07
**Session:** Add Helm-values pathway for `workspace.defaultStorageClass`; bump chart 0.2.1 → 0.2.2.
**Status:** Complete

---

## Objective

Give operators a way to declare `workspace.defaultStorageClass` in the Helm chart so every new workspace PVC lands on the operator's chosen StorageClass without requiring post-install admin-UI configuration. Prompted by the 2026-07-07 workspace-stuck-in-creating incident, where all workspace PVCs at `numberOfReplicas: 3` consumed ~50% of scheduled Longhorn disk cluster-wide and pushed all 3 worker disks below the 25% MinimalAvailable threshold, causing new workspaces to hang forever in `Init:0/2` with `AttachVolume.Attach failed`.

---

## Work Completed

### Cluster-side (immediate unblock)

In-place shrink of all 6 existing workspace `volumes.longhorn.io` from `numberOfReplicas=3` → `2`. Longhorn evicted one replica per volume in the background. Two of three nodes cleared `DiskPressure` within 45s; new workspace creation succeeded end-to-end (user-verified).

### talos-ops-prod PR #1976

New non-default `StorageClass` `longhorn-2r` at `kubernetes/apps/storage/longhorn/app/storageclass-2r.yaml`, mirroring the chart-managed `longhorn` SC parameters but with `numberOfReplicas: "2"`. Registered in the kustomization. Not set as default.

### LLMSafeSpaces PR #509 — the code path already existed

Investigation showed the `workspace.defaultStorageClass` instance setting was already fully wired (schema.go:85 → registry.go:37 → instance_service.go:321 `DefaultStorageClass(ctx)` → workspace_service.go:1117 `crd.Spec.Storage.StorageClassName`). What was missing was a Helm-values pathway to pre-set it at install time. The `email.*` pathway added in US-49.2 (`app.go:243-250` uses `SetHelmOverrides`) is exactly the pattern needed.

- `charts/llmsafespaces/values.yaml`: extend the existing `workspace:` block with `defaultStorageClass: ""` plus a comment explaining Tier 1 (Helm-managed) vs Tier 2 (admin-mutable) semantics.
- `charts/llmsafespaces/templates/configmap-api.yaml`: `{{- with .Values.workspace.defaultStorageClass }}` guarded render of `workspace: defaultStorageClass: <value>` into `config.yaml`.
- `api/internal/config/config.go`: add `Workspace` struct to `Config` with `DefaultStorageClass string` (mapstructure tag `workspace.defaultStorageClass`), full doc comment.
- `api/internal/app/app.go`: after the `email.*` `SetHelmOverrides` block, structural clone gated on `cfg.Workspace.DefaultStorageClass != ""` pinning the key.
- `charts/llmsafespaces/Chart.yaml` + `charts/llmsafespaces/README.md`: bump 0.2.1 → 0.2.2.
- `README-LLM.md`: extend "Storage Settings" section documenting the Helm-managed override.

### Tests written (TDD)

- `charts/llmsafespaces/chart_test.go` — 3 new tests:
  - `TestWorkspace_DefaultRender_OmitsWorkspaceBlock` — with default values, no top-level `workspace:` block renders. Uses regex `(?m)^workspace:` because the string `create_workspace:` (rate-limit key) contains `workspace:` as a substring; naive `NotContains` false-positives.
  - `TestWorkspace_DefaultStorageClass_RendersBlock` — setting the value renders the block with the right value.
  - `TestWorkspace_DefaultStorageClass_OperatorOverride` — alternate SC names flow through unmodified.
- `api/internal/config/config_test.go` — 2 new tests:
  - `TestConfig_Workspace_DefaultStorageClass_Empty` — parses to `""` when absent.
  - `TestConfig_Workspace_DefaultStorageClass_FromYAML` — parses when set.
- `pkg/settings/helm_precedence_workspace_test.go` (added in review-iteration commit): mirrors the `email.*` glue coverage by exercising `SetHelmOverrides({"workspace.defaultStorageClass": ...})` end-to-end — asserts the key is Tier-1 read-only in the returned schema and rejects Set with `ErrReadOnly`. Addresses the automated reviewer's "one untested link" finding at `app.go:258-262`.

---

## Key Decisions

- **Chose Helm-values pathway over changing the schema default.** Schema default `""` (cluster default SC) preserved so upgrades of clusters that have not opted in see zero behavior change.
- **Chose `SetHelmOverrides` (Tier 1 read-only) over just injecting a DB seed.** Seeds only insert if missing; if an admin changes it via UI, the operator's declared choice is silently lost. `SetHelmOverrides` makes the admin UI show it disabled with a "Managed by Helm" badge and returns 409 on PUTs — the operator's declared choice cannot drift.
- **Chose in-place `kubectl patch volumes.longhorn.io ... numberOfReplicas=2` over migrating PVCs to a new SC** for existing workspaces. PVC `storageClassName` is immutable after binding, and even if patched Longhorn would not retroactively shrink replicas — Longhorn tracks replicas on the `Volume` CR, not the PVC. In-place shrink is the only zero-data-loss zero-migration option.
- **Chose per-workspace `longhorn-2r` scope over cluster-wide default change.** Databases, media, home apps all keep 3r durability. Only workspaces (ephemeral, user-recoverable) drop to 2r.
- **Did NOT set `longhorn-2r` as the cluster default StorageClass.** Explicit opt-in via `spec.storageClassName` on PVCs, or per-app via Helm values. Prevents accidental durability regressions on non-workspace apps.

---

## Blockers

None.

---

## Tests Run

```
$ go test -timeout 120s ./api/internal/config/... ./pkg/settings/...
ok  	github.com/lenaxia/llmsafespaces/api/internal/config	0.018s
ok  	github.com/lenaxia/llmsafespaces/pkg/settings	0.064s

$ go test -timeout 300s ./charts/llmsafespaces/
ok  	github.com/lenaxia/llmsafespaces/charts/llmsafespaces	52.142s

$ go build ./...
(clean)
```

PR CI: Lint / Frontend / Trivy / govulncheck / Gitleaks / pkg/secrets integration all green. Full test suite + race detector still running at time of writing; will iterate if either fails.

Cluster verification (post-shrink): all 6 workspaces `Active` or `Suspended`, `DiskPressure=False` on worker-00 + worker-01; new workspace created successfully by user.

---

## Next Steps

1. Wait for automated reviewer to re-approve PR #509 after the app.go test + worklog format fix.
2. Merge #1976 (ready — clean, all checks green, automated APPROVE posted as comment).
3. Merge #509 (waiting on re-review + full test suite green).
4. Cut `v0.2.2` release tag on LLMSafeSpaces (post-merge bot handles the ghcr push).
5. In `talos-ops-prod`, open a follow-up PR that:
   - Updates `kubernetes/flux/repositories/git/llmsafespaces.yaml` GitRepository ref `tag: v0.2.1` → `tag: v0.2.2`.
   - Sets `workspace.defaultStorageClass: longhorn-2r` in the LLMSafeSpaces HelmRelease values.

---

## Files Modified

**talos-ops-prod (branch `feat/longhorn-2r-sc`, PR #1976):**
- `kubernetes/apps/storage/longhorn/app/storageclass-2r.yaml` (new)
- `kubernetes/apps/storage/longhorn/app/kustomization.yaml`

**LLMSafeSpace (branch `feat/workspace-default-storage-class`, PR #509):**
- `README-LLM.md`
- `api/internal/app/app.go`
- `api/internal/config/config.go`
- `api/internal/config/config_test.go`
- `charts/llmsafespaces/Chart.yaml`
- `charts/llmsafespaces/README.md`
- `charts/llmsafespaces/chart_test.go`
- `charts/llmsafespaces/templates/configmap-api.yaml`
- `charts/llmsafespaces/values.yaml`
- `pkg/settings/helm_precedence_workspace_test.go` (new, added in review-iteration)
- `worklogs/NNNN_2026-07-07_workspace-defaultstorageclass-helm-pathway.md` (new)

**Cluster changes (out-of-band, `kubectl patch`; no repo diff):**
- `longhorn-system` namespace: 6 `volumes.longhorn.io` objects patched from `spec.numberOfReplicas: 3` → `2`.
