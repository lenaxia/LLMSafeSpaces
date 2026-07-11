# Upgrading

This page covers upgrading an LLMSafeSpaces deployment: the semver policy, what to read before upgrading, the `helm upgrade` flow, database migration handling, the KEK rotation window, rolling back, and common upgrade pitfalls (CRD upgrades, GitOps packaging cache, dashboard UIDs). LLMSafeSpaces is v0.3.x — upgrades within v0.x can carry breaking changes, so read the changelog carefully.

## On this page

- [Semver policy](#semver-policy)
- [Before you upgrade](#before-you-upgrade)
- [The upgrade flow](#the-upgrade-flow)
- [Database migrations](#database-migrations)
- [CRD upgrades](#crd-upgrades)
- [The KEK rotation window](#the-kek-rotation-window)
- [Rolling back](#rolling-back)
- [Common pitfalls](#common-pitfalls)

---

## Semver policy

LLMSafeSpaces follows semver with the caveat that **during v0.x, minor versions can carry breaking changes**. Specifically:

| Version change | Breaking changes possible? |
|---|---|
| Patch (`0.3.0` → `0.3.1`) | No (bug fixes only) |
| Minor (`0.3.x` → `0.4.0`) | **Yes** — read the changelog |
| Major (`0.x` → `1.0.0`) | Yes (GA) |

The chart's `Chart.yaml` version and the app version are kept in sync. Image tags follow CI's four-tag scheme (`sha-<commit>`, `ts-<unix>`, `dev`, semver) — see [Image tags](installation.md#image-tags).

---

## Before you upgrade

1. **Read the CHANGELOG.** Every release notes breaking changes, required migrations, and config-value renames. Do not skip this.

2. **Check the chart's `CHART-UPGRADE.md`** for upgrade-time procedures and known operational gotchas that `helm upgrade` does not handle automatically.

3. **Back up Postgres.** Migrations are forward-only in practice (rollback migrations exist but are not battle-tested). A pre-upgrade `pg_dump` is your safety net.

4. **Verify the workspace namespace is quiet.** Active sessions during an API rolling update will see transient errors. Schedule a maintenance window for minor upgrades.

5. **Pin image tags in your values file.** Avoid relying on moving tags (`dev`, `latest`) across upgrades.

6. **Review the threat model delta.** Security-relevant changes are called out in release notes. If a new gap was opened or closed, understand the operator action required.

### Pre-upgrade checklist

- [ ] CHANGELOG read; breaking changes understood.
- [ ] `CHART-UPGRADE.md` reviewed for gotchas.
- [ ] Postgres backed up (`pg_dump`).
- [ ] Image tags pinned in values file.
- [ ] CRD changes identified (run `diff` on `crds/`).
- [ ] KEK rotation requirement checked (release notes).
- [ ] Grafana dashboard UID changes checked (rare).
- [ ] Maintenance window scheduled (for minor versions).
- [ ] Rollback plan ready (Helm revision + Postgres backup).
- [ ] Monitoring alerts silenced during the window (optional).

---

## The upgrade flow

```bash
# 1. Pull the latest chart
git pull origin main

# 2. Review the diff in values.yaml defaults
git diff HEAD~1 -- helm/values.yaml

# 3. Lint and template
make helm-lint
make helm-template

# 4. Dry-run against the live cluster
make helm-install-dry-run

# 5. Back up Postgres
pg_dump -h <pg-host> -U llmsafespaces llmsafespaces > backup-$(date +%F).sql

# 6. Upgrade
helm upgrade llmsafespaces ./helm \
    -n llmsafespaces \
    -f llmsafespaces.yaml

# 7. Watch the migration Job
kubectl -n llmsafespaces get jobs -w

# 8. Wait for rollouts
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-api
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-controller

# 9. Verify health
kubectl -n llmsafespaces port-forward svc/llmsafespaces-api 8080:8080 &
curl http://localhost:8080/readyz   # must return 200
```

### What `helm upgrade` does automatically

- Renders updated templates (Deployments, ConfigMaps, Services, RBAC, NetworkPolicies).
- Runs the pre-upgrade migration Job (new SQL files bundled in the migrations ConfigMap).
- Rolls the API and controller Deployments (RollingUpdate by default).
- Preserves generated credentials (the credentials Secret has `helm.sh/resource-policy: keep`).

### What `helm upgrade` does NOT do

- **Upgrade CRDs** from `crds/` (Helm 3 does not touch them). See [CRD upgrades](#crd-upgrades).
- **Run the `rotate-kek` CLI** if the KEK rotation window applies. See [KEK rotation window](#the-kek-rotation-window).
- **Migrate dashboard UIDs** if they changed (rare, coordinated). See [Common pitfalls](#grafana-dashboard-uids).
- **Re-randomize passwords** unless they were left in a vulnerable state (then it does, and you must sync the Postgres role).

---

## Database migrations

Migrations run as a Helm pre-install/pre-upgrade Job using the `migrate/migrate:v4.17.1` image. The SQL files under `helm/migrations/` are bundled into a ConfigMap mounted at `/migrations`.

```yaml
migrations:
  enabled: true
  backoffLimit: 3
  ttlSecondsAfterFinished: 600
```

The Job is a hook (`pre-upgrade`), so `helm upgrade` waits for it before proceeding. If it fails:

```bash
kubectl -n llmsafespaces logs job/llmsafespaces-migrations
```

Common causes:

- **Database doesn't exist** — enable `dbInit.enabled` (green-field Postgres without a pre-created role).
- **Role lacks permissions** — the migration user needs DDL on the `llmsafespaces` database.
- **Dirty migration state** — if a previous migration partially applied, the `migrate` CLI marks it dirty. Resolve with `migrate force <version>` then re-run.

!!! note "Forward-only in practice"
    Down migrations exist in the chart but are not regularly exercised in production. A `pg_dump` before upgrade is the reliable rollback path.

---

## CRD upgrades

**Helm 3 does not upgrade CRDs from `crds/` on `helm upgrade`.** It installs them on first install but never updates them afterward. To pick up CRD changes:

```bash
kubectl apply -f helm/crds/
helm upgrade llmsafespaces ./helm -n llmsafespaces
```

### Production-safe CRD management

For production safety, manage CRDs out-of-band:

```yaml
crds:
  install: false   # do not install CRDs from the chart
```

Then apply CRDs via your own GitOps pipeline (`kubectl apply -f crds/`) before upgrading the chart. This decouples CRD evolution (which can have storage-version migrations) from chart upgrades.

### Verify CRD versions

```bash
kubectl get crd workspaces.llmsafespaces.dev -o jsonpath='{.spec.versions[*].name}'
kubectl get crd runtimeenvironments.llmsafespaces.dev -o jsonpath='{.spec.versions[*].name}'
kubectl get crd inferencerelays.llmsafespaces.dev -o jsonpath='{.spec.versions[*].name}'
```

The controller supports the latest served version; older versions are served with a conversion webhook if present.

---

## The KEK rotation window

If a release changes how the master KEK is derived or wrapped, you may need to rotate the KEK so existing ciphertext remains decryptable under the new scheme. The `rotate-kek` CLI (`cmd/rotate-kek/main.go`) handles this:

```bash
# Dry-run first
kubectl -n llmsafespaces exec deploy/llmsafespaces-api -- \
    /usr/local/bin/rotate-kek \
    --old-master-key-file /var/run/secrets/llmsafespaces/master-secret \
    --new-master-key-file /path/to/new-kek \
    --dry-run

# Apply
kubectl -n llmsafespaces exec deploy/llmsafespaces-api -- \
    /usr/local/bin/rotate-kek \
    --old-master-key-file /var/run/secrets/llmsafespaces/master-secret \
    --new-master-key-file /path/to/new-kek
```

Features: per-purpose key derivation, Postgres + Redis connections, `RotationCoordinator`, dry-run, resume-from, multi-table support. See the [Runbook](runbook.md#rotating-the-master-kek) for the full procedure.

**When rotation is needed:** only when the release notes say so. The KEK rotation window (US-50.4) is supported by the multi-key `StaticKeyProvider`, so during the window both old and new keys can decrypt — old ciphertext is re-wrapped lazily. Most upgrades do not require KEK rotation.

---

## Rolling back

```bash
# 1. Roll back the Helm release
helm rollback llmsafespaces <REVISION> -n llmsafespaces

# 2. Restore Postgres from backup (if migrations are irreversible)
psql -h <pg-host> -U llmsafespaces llmsafespaces < backup-$(date +%F).sql

# 3. Roll back CRDs (if they changed)
kubectl apply -f helm/crds/   # at the old chart revision
```

Find the revision to roll back to:

```bash
helm history llmsafespaces -n llmsafespaces
```

!!! warning "Rollback is not always clean"
    `helm rollback` reverts the chart templates but does **not** revert database migrations or CRD storage versions. If a migration was destructive (column drop), rollback requires restoring Postgres from the pre-upgrade backup. This is why the backup step is non-negotiable.

### Rollback with the self-hosted relay fleet

If the fleet was enabled and the controller provisioned VMs, rollback to a chart version that doesn't know about `InferenceRelay` will leave orphan VMs. Destroy them via the cloud console or the admin API before rolling back.

### The dbInit path across upgrades

If you enabled `dbInit.enabled` (green-field Postgres bootstrap) on install, it runs as a pre-install/pre-upgrade hook. On upgrade, it re-runs the idempotent `CREATE ROLE` / `CREATE DATABASE` statements — safe because they're `IF NOT EXISTS`. Ensure the `superuserSecret` still exists and is valid before upgrading; a missing secret fails the chart render (the chart refuses to point the Job at an unnamed Secret).

---

## Common pitfalls

### GitOps packaging cache (FluxCD)

!!! danger "Read this if you deploy via FluxCD GitRepository"
    If you consume this chart from a Git source (FluxCD `GitRepository` or Argo CD Application pointing at this repo), you **must** set `reconcileStrategy: Revision` on your Flux `HelmRelease`. Otherwise the chart is packaged exactly once and **never re-packaged**, and every `helm upgrade` after the first will silently render against a stale snapshot.

    ```yaml
    apiVersion: helm.toolkit.fluxcd.io/v2
    kind: HelmRelease
    spec:
      chart:
        spec:
          sourceRef:
            kind: GitRepository
            reconcileStrategy: Revision   # required
    ```

    **Symptom:** `kubectl get configmap <release>-migrations -o jsonpath='{.data}'` shows only the original migration files after you've added new ones; new chart templates don't appear. This caused the 2026-06-29 production incident.

    Argo CD users: the equivalent is ensuring your Application tracks `targetRevision`
    of a branch/tag (not a chart version) and uses `helm` with `passCredentials` off;
    Argo re-renders the chart from the synced Git revision on every sync, so it does
    not hit the ChartVersion cache — but verify your `syncPolicy` re-renders rather
    than re-applying a cached manifest set.

    **Long-term alternative.** Publishing the chart to an OCI registry (ghcr.io)
    on each commit would make `reconcileStrategy` irrelevant and is the most robust
    fix. That is tracked as a follow-up to issue #456.

### CRD upgrades skipped

Helm 3 does not upgrade CRDs. Always `kubectl apply -f crds/` before `helm upgrade`. See [CRD upgrades](#crd-upgrades).

### Grafana dashboard UIDs

If a chart upgrade renames a dashboard UID, bookmarks break and Grafana's sidecar provisioner trips an optimistic-concurrency check ("found 2 with same internal hash, desired 1"). The chart test `TestMonitoring_DashboardUIDsAreStable` prevents accidental changes, but if you forked the dashboards, follow the migration procedure in `helm/CHART-UPGRADE.md`.

### Controller webhook unavailable blocks admission

`webhooks.failurePolicy: "Fail"` (the default) means if the controller webhook pod is down during upgrade, kube-apiserver rejects all CREATE/UPDATE on `Workspace` and `RuntimeEnvironment`. The migration Job and Deployment rollouts are not affected (they don't create those resources), but users will see errors. This is the secure default — controller downtime blocks workspace mutations.

### Moving image tags in production

If you pinned to `dev` or `latest` and the tag moved, `helm upgrade` pulls the new image but kubelet caching of moving tags is unreliable (Talos/containerd). Pin to immutable `sha-<commit>` or semver tags.

### GHCR retention pruning old versions

GitHub's native package-version retention prunes old versions. `sha-` and `ts-` tags label the same manifest version and are pruned together — pinning to `sha-` does not prevent recurrence on its own. Configure retention to keep the latest semver-tagged version, or cut releases frequently. See issue #454.

### Namespace pod-security labels

If you set `namespace.podSecurityEnforce: ""` to opt out of PSA in one release, a later upgrade that re-enables it may reject existing pods that don't comply. The chart's pods always comply with `restricted`, so this only bites manually-applied pods.

---

## Post-upgrade verification

After every upgrade, verify the platform is healthy before closing the maintenance window:

```bash
# 1. Health endpoints
kubectl -n llmsafespaces port-forward svc/llmsafespaces-api 8080:8080 &
curl -sf http://localhost:8080/readyz && echo "API ready"

# 2. Rollouts complete
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-api
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-controller

# 3. Webhook functional (create a throwaway workspace)
TOKEN=$(...)  # auth token
WS=$(curl -sX POST "$API/api/v1/workspaces" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"name":"upgrade-smoke","runtime":"base","storageSize":"1Gi"}' | jq -r .id)
curl -sf -X POST "$API/api/v1/workspaces/$WS/activate" -H "Authorization: Bearer $TOKEN"
# ... wait for Active, then delete

# 4. CRDs serving expected versions
kubectl get crd workspaces.llmsafespaces.dev -o jsonpath='{.spec.versions[*].name}'

# 5. No stuck workspaces
kubectl get workspaces -A -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.status.phase}{"\n"}{end}' | grep -v Active | grep -v Suspended
```

If any check fails, consult [Troubleshooting](troubleshooting.md) or roll back.

---

## Related

- [Installation](installation.md) — initial deploy, image tags.
- [Configuration](configuration.md) — values that may have changed.
- [Runbook](runbook.md) — KEK rotation procedure.
- [CHART-UPGRADE.md](https://github.com/lenaxia/LLMSafeSpaces/blob/main/helm/CHART-UPGRADE.md) — upgrade-time gotchas in the chart repo.
