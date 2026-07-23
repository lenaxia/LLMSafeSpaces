# Operator Runbook

This page is a collection of step-by-step procedures for routine and emergency operations: rotating the JWT secret, rotating the master KEK, responding to a compromised API pod, recovering from a corrupted PVC, scaling the API horizontally, and running database maintenance windows. Each procedure lists prerequisites, the exact commands, and the verification steps.

## On this page

- [Rotating the JWT secret](#rotating-the-jwt-secret)
- [Rotating the master KEK](#rotating-the-master-kek)
- [Responding to a compromised API pod](#responding-to-a-compromised-api-pod)
- [Recovering from a corrupted PVC](#recovering-from-a-corrupted-pvc)
- [Scaling the API horizontally](#scaling-the-api-horizontally)
- [Database maintenance windows](#database-maintenance-windows)
- [Rotating a relay VM (self-hosted fleet)](#rotating-a-relay-vm-self-hosted-fleet)

---

## Rotating the JWT secret

The JWT signing secret has no in-process rotation primitive. Rotation requires changing the secret and restarting the API pods, which **invalidates all active sessions** (every user must re-authenticate).

### When to rotate

- Suspected or confirmed JWT signing key compromise.
- Periodic rotation per your security policy (the platform does not enforce a schedule — gap A8).
- Staff turnover with access to the credentials Secret.

### Prerequisites

- A maintenance window (users will be logged out).
- The new secret value (use a strong random: `openssl rand -base64 48`).

### Procedure

```bash
NEW_SECRET=$(openssl rand -base64 48)

# 1. Update the credentials Secret
kubectl -n llmsafespaces patch secret llmsafespaces-credentials \
    --type merge -p "{\"data\":{\"jwt-secret\":\"$(echo -n "$NEW_SECRET" | base64)\"}}"

# 2. Restart the API pods to pick up the new secret
kubectl -n llmsafespaces rollout restart deployment/llmsafespaces-api

# 3. Wait for rollout
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-api

# 4. Verify health
kubectl -n llmsafespaces port-forward svc/llmsafespaces-api 8080:8080 &
curl http://localhost:8080/readyz   # 200
```

### If rotating via Helm

```bash
helm upgrade llmsafespaces ./helm \
    -n llmsafespaces \
    --set externalSecret.jwtSecret="$NEW_SECRET" \
    --set externalSecret.create=true
```

!!! note "The credentials Secret has resource-policy: keep"
    When `externalSecret.create=true`, the chart manages the Secret but annotates it `helm.sh/resource-policy: keep` so it survives `helm uninstall`. To force a new value, patch the Secret directly (above) or set the value explicitly in `helm upgrade`.

### Verification

- All existing JWTs are now invalid (users get 401 and must re-login).
- New logins issue JWTs signed with the new secret.
- The `token:<hash>` revocation keys in Redis for old tokens become orphaned but harmless (they expire).

### Post-rotation

- Notify users of the forced re-login.
- If the rotation was due to compromise, also rotate the master KEK (a compromised API pod may have had access to both).

---

## Rotating the master KEK

The master KEK is the root of trust for at-rest encryption. The `rotate-kek` CLI (`cmd/rotate-kek/main.go`) supports zero-downtime rotation: it loads old + new keys, re-wraps encrypted rows in Postgres, and flushes the Redis DEK cache. The multi-key `StaticKeyProvider` (US-50.4) allows both keys to decrypt during the window.

### When to rotate

- Suspected or confirmed KEK compromise (e.g., API pod RCE).
- Periodic rotation per your security policy.
- After staff turnover with master-secret access.
- When the release notes indicate a KEK derivation change.

### Prerequisites

- The `rotate-kek` binary (available in the API image, or build from `cmd/rotate-kek/`).
- The current (old) master KEK (the projected file mount).
- A new master KEK (`openssl rand -base64 48` — must be ≥32 bytes).
- Postgres connectivity from where you run the CLI.
- A maintenance window is **not required** (zero-downtime rotation), but do it during low traffic.

### Procedure

```bash
# 1. Generate the new KEK
NEW_KEK=$(openssl rand -base64 48)
echo -n "$NEW_KEK" > /tmp/new-master-secret
chmod 0400 /tmp/new-master-secret

# 2. Dry-run from within the API pod
kubectl -n llmsafespaces exec deploy/llmsafespaces-api -- \
    /usr/local/bin/rotate-kek \
    --old-master-key-file /var/run/secrets/llmsafespaces/master-secret \
    --new-master-key-file /tmp/new-master-secret \
    --dry-run

# 3. Copy the new KEK into the pod and apply
kubectl -n llmsafespaces cp /tmp/new-master-secret \
    deploy/llmsafespaces-api:/tmp/new-master-secret

kubectl -n llmsafespaces exec deploy/llmsafespaces-api -- \
    /usr/local/bin/rotate-kek \
    --old-master-key-file /var/run/secrets/llmsafespaces/master-secret \
    --new-master-key-file /tmp/new-master-secret

# 4. Update the credentials Secret with the new KEK
kubectl -n llmsafespaces patch secret llmsafespaces-credentials \
    --type merge -p "{\"data\":{\"master-secret\":\"$(echo -n "$NEW_KEK" | base64)\"}}"

# 5. Restart API pods to load the new KEK as primary
kubectl -n llmsafespaces rollout restart deployment/llmsafespaces-api
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-api

# 6. Clean up
rm /tmp/new-master-secret
kubectl -n llmsafespaces exec deploy/llmsafespaces-api -- rm /tmp/new-master-secret
```

### Resume-from

If the rotation is interrupted (network blip, CLI crash), use `--resume-from` to continue from the last completed table:

```bash
kubectl -n llmsafespaces exec deploy/llmsafespaces-api -- \
    /usr/local/bin/rotate-kek \
    --old-master-key-file /var/run/secrets/llmsafespaces/master-secret \
    --new-master-key-file /tmp/new-master-secret \
    --resume-from <table-name>
```

### Verification

- API pods start cleanly with the new KEK (no decrypt errors in logs).
- Existing encrypted credentials (API keys, org SSO secrets, provider credentials) still decrypt correctly.
- The `key_version` columns on `api_keys` + `org_sso_configs` reflect the new version for newly written rows.
- Old ciphertext rows re-wrapped during rotation decrypt under the new key.

!!! note "The rotation window"
    During rotation, both old and new keys can decrypt (multi-key `StaticKeyProvider`). After all rows are re-wrapped and API pods restarted with the new key, the old key is no longer needed. Securely destroy the old key material.

### Post-rotation

- Verify Redis DEK cache was flushed (the CLI handles this).
- If the rotation was due to compromise, also rotate the JWT secret and review `secret_audit_log` for suspicious decrypt activity (G50 fixed — AuditedProvider is wired).

---

## Responding to a compromised API pod

A confirmed RCE in an API pod means the attacker may have accessed: the master KEK (in process memory), active session DEKs (Redis cache), and the ability to impersonate users (JWT signing).

### Immediate containment

```bash
# 1. Isolate the suspect pod (cordon its node to prevent new pods scheduling there)
NODE=$(kubectl -n llmsafespaces get pod <suspect-pod> -o jsonpath='{.spec.nodeName}')
kubectl cordon "$NODE"

# 2. Delete the suspect pod (the Deployment replaces it)
kubectl -n llmsafespaces delete pod <suspect-pod>

# 3. Force-rotate the JWT secret (invalidates all sessions — see procedure above)
# 4. Force-rotate the master KEK (re-wraps all credentials — see procedure above)
```

### Investigation

```bash
# Pod logs (may be gone after deletion — retrieve from your log aggregator)
kubectl -n llmsafespaces logs <suspect-pod> --previous

# Node investigation (if the escape reached the node)
kubectl debug node/"$NODE" -it --image=busybox

# Check for unexpected CRD/Secret changes during the window
kubectl get events -n llmsafespaces --since=2h | grep -i "secret\|workspace"
```

### Full remediation checklist

- [ ] Suspect pod deleted and replaced.
- [ ] Node cordoned and drained (if node-level compromise suspected).
- [ ] JWT secret rotated (all sessions invalidated).
- [ ] Master KEK rotated (all credentials re-wrapped).
- [ ] Redis password rotated and Redis restarted (DEK cache flushed).
- [ ] Postgres password rotated (belt-and-suspenders).
- [ ] `secret_audit_log` reviewed for data exfiltration (G50 fixed — AuditedProvider wired).
- [ ] Workspace passwords (per-workspace K8s Secrets) reviewed for tampering.
- [ ] NetworkPolicy reviewed for any rules the attacker may have added/relaxed.
- [ ] Images verified with cosign (see [Verifying image signatures](#verifying-image-signatures) below).

!!! info "Decrypt audit is wired (G50 fixed)"
    The `AuditedProvider` is wired into production decrypt paths (`app.go:408,409,624`). Every Decrypt call on provider-credentials, org-credentials, and api-keys logs to `secret_audit_log`. Exfiltration via legitimate API decrypt calls is now **detectable** in the audit log.

---

## Verifying image signatures

All release images are signed with cosign keyless (OIDC). After a security incident or before deploying to a production cluster, verify the images haven't been tampered with:

```bash
for img in api controller base frontend relay-router relay-proxy; do
  echo "Verifying ${img}..."
  cosign verify "ghcr.io/lenaxia/llmsafespaces/${img}:0.5.0" \
    --certificate-identity-regexp "https://github.com/lenaxia/LLMSafeSpaces" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com"
done
```

If any verification fails, the image was not built by the project's release workflow — do not deploy it. Pull a known-good version from the [GitHub Releases page](https://github.com/lenaxia/LLMSafeSpaces/releases) instead.

---

## Recovering from a corrupted PVC

A workspace PVC is corrupted (filesystem errors, the agent can't read its database, opencode crashes on boot).

### Prerequisites

- The corruption is confirmed (not a transient mount issue).
- The workspace owner has been notified (data loss is possible).
- A backup of the PVC if one exists (Longhorn snapshots, cloud CSI snapshots).

### Procedure: restore from snapshot

```bash
# 1. Suspend the workspace (deletes pod, retains PVC)
curl -X POST "$API/api/v1/workspaces/$WS/suspend" -H "Authorization: Bearer $TOKEN"

# 2. Restore the PVC from snapshot (Longhorn example)
# Via the Longhorn UI or:
kubectl apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: restore-$WS
spec:
  volumeSnapshotClassName: longhorn
  source:
    volumeSnapshotContentName: <snapshot-content>
EOF

# 3. Activate the workspace
curl -X POST "$API/api/v1/workspaces/$WS/activate" -H "Authorization: Bearer $TOKEN"
```

### Procedure: reset to empty (no snapshot)

If no snapshot exists, reset the PVC to empty (data loss):

```bash
# 1. Suspend the workspace
curl -X POST "$API/api/v1/workspaces/$WS/suspend" -H "Authorization: Bearer $TOKEN"

# 2. Delete the PVC
kubectl delete pvc <pvc-name> -n llmsafespaces

# 3. Patch the Workspace CR to reset (or delete + recreate)
# The controller recreates the PVC on next activate.

# 4. Activate
curl -X POST "$API/api/v1/workspaces/$WS/activate" -H "Authorization: Bearer $TOKEN"
```

### Verification

- Workspace reaches `Active`.
- opencode boots cleanly (no `opencode.db` corruption errors in logs).
- Session history is present (from snapshot) or empty (reset).

### Prevention

- Enable StorageClass snapshots (Longhorn, cloud CSI).
- Schedule periodic snapshots for critical workspaces.
- Monitor PVC usage (`workspace_disk_usage` alert at >90%).

---

## Scaling the API horizontally

The API is stateless and horizontally scalable — no sticky sessions required. Scale by increasing replicas.

### When to scale

- API p99 latency rising under load.
- API CPU/memory limits being hit.
- Adding capacity before a known traffic spike.

### Procedure

```bash
# Via Helm
helm upgrade llmsafespaces ./helm \
    -n llmsafespaces \
    --set api.replicaCount=4 \
    -f llmsafespaces.yaml

# Or via kubectl (immediate, not persisted across helm upgrade)
kubectl -n llmsafespaces scale deployment/llmsafespaces-api --replicas=4
```

### Verification

```bash
kubectl -n llmsafespaces rollout status deployment/llmsafespaces-api
kubectl -n llmsafespaces get pods -l app.kubernetes.io/component=api
```

### Scaling considerations

| Concern | Guidance |
|---|---|
| **Database connections** | Each replica opens up to `postgresql.maxOpenConns` (default 25) connections. 4 replicas = up to 100 connections. Ensure Postgres `max_connections` accommodates this. |
| **Redis connections** | Each replica uses a pool (`redis.poolSize`, default 20). Ensure Redis `maxclients` accommodates. |
| **Leader election** | The API uses leader election (`api.config.kubernetes.leaderElection.enabled`) for distributed coordination. Multiple replicas are safe. |
| **SSE connections** | Connections are distributed across replicas by the Service. The `sseConnCounts` map is per-replica; stale entries are pruned on every call (G42 fixed). |
| **In-memory caches** | The model cache is per-replica (up to 5s staleness across replicas). Not a correctness issue; cosmetic for `relayInjected`. |

### Scaling the controller

The controller defaults to 1 replica with leader election. Scaling it beyond 1 is supported (leader election ensures only one reconciler is active), but provides no throughput benefit — only HA. For HA:

```yaml
controller:
  replicaCount: 2
  leaderElection:
    enabled: true
```

---

## Database maintenance windows

For Postgres maintenance (vacuum, index rebuild, version upgrade, failover).

### Soft maintenance (no downtime, degraded performance)

1. Reduce API replicas to lower connection count:

   ```bash
   kubectl -n llmsafespaces scale deployment/llmsafespaces-api --replicas=1
   ```

2. Perform maintenance (the API continues to serve, slower).

3. Scale back up.

### Hard maintenance (API down)

1. Announce the window to users.

2. Scale the API to 0 (or suspend all active workspaces first if you want pods gone too):

   ```bash
   kubectl -n llmsafespaces scale deployment/llmsafespaces-api --replicas=0
   ```

3. Perform maintenance.

4. Scale back up and verify `/readyz`.

### Connection draining

The API has a graceful shutdown timeout (`api.config.server.shutdownTimeout`, default 30s). During a rolling update, SIGTERM triggers graceful shutdown — in-flight requests complete, then the pod exits. Long-running SSE streams may be cut.

### Postgres failover (CloudNativePG)

If you use CloudNativePG, failover is automatic. The API will see transient connection errors during the switchover (~10–30s). `/readyz` returns 503 during this window, removing the pod from the Service. No manual action needed unless the failover fails.

---

## Rotating a relay VM (self-hosted fleet)

The self-hosted InferenceRelay fleet (Epic 42) uses per-VM tokens. Each VM's token authenticates router→VM traffic; rotation = destroy + reprovision the VM, which generates a fresh token. (The Cloudflare Worker relay that previously required a separate cluster-side secret was removed in Epic 60 — Zen blocks CF Worker IPs.)

Rotate when a VM returns sustained 429s, becomes unhealthy, or its token is suspected compromised.

### Procedure

```bash
# 1. Check fleet status
curl "$API/api/v1/admin/relay/status" -H "Authorization: Bearer $ADMIN_TOKEN"

# 2. Rotate the specific VM
curl -X POST "$API/api/v1/admin/relay/rotate/$VM_ID" \
    -H "Authorization: Bearer $ADMIN_TOKEN"

# 3. Monitor provisioning
curl "$API/api/v1/admin/relay/status" -H "Authorization: Bearer $ADMIN_TOKEN"
```

The router distributes traffic to healthy VMs during the rotation. If all VMs are down, the router falls back to direct upstream (rate-limited).

### Pause/resume reconciliation

For investigation without the controller interfering:

```bash
curl -X POST "$API/api/v1/admin/relay/pause" -H "Authorization: Bearer $ADMIN_TOKEN"
# ... investigate ...
curl -X POST "$API/api/v1/admin/relay/resume" -H "Authorization: Bearer $ADMIN_TOKEN"
```

---

## Related

- [Security Hardening](security.md) — the threat model these procedures address.
- [Upgrading](upgrading.md) — upgrade-time procedures.
- [Troubleshooting](troubleshooting.md) — symptom-based diagnosis.
- [Inference Relay](inference-relay.md) — the relay fleet architecture.
