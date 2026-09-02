# Runbook: secret delivery v2 (design 0057, Epic 70)

**Owner:** platform operator
**Scope:** pull-based, terminal-verified credential delivery — post-US-70.5 (demolition) regime. The API never pushes credential bodies; bind/unbind/rotate bumps the stored revision, notifies the pod (bodyless), and the pod re-pulls through the conditional bootstrap path with a fresh SA token. The periodic reconcile loop is the correctness path: divergence heals without human action.

## Signals

| Surface | Field/metric | Meaning |
|---|---|---|
| CRD `status.secretsDelivery` | `spawnedRev`, `filesRev` | Terminal revision the supervised agent process / uid-1000 file writer actually applied (`seq:manifestHash:contentHash`). Scraped by the controller from agentd `/v1/healthz`; cleared to nil when the pod is unreachable; omitted by pre-70.2 runtimes (mixed-fleet W15). |
| CRD `status.secretsDelivery` | `degradedReason` | Empty = converged. `spawn_env_*` / `spawn_files_*` families = env/file class degrade; `pull_failed`, `dek_unwrap_failed` = bootstrap-path failures. |
| agentd `/v1/healthz` | `delivery: "v2"` | Fleet-version marker (old runtimes omit the field — the fleet gauge for W15 retirement). |
| Metric | `llmsafespaces_secrets_delivery_converged{workspace_id}` | 1/0 per Active workspace — the convergence gauge. |
| Metric | `llmsafespaces_secrets_delivery_divergent_total{reason}` | Divergence counter by reason: `legacy_format`, `missing_rev`, `stale_seq`, `notify_failed`. |
| Metric | `llmsafespaces_secrets_notify_total{outcome}` | Per-notify outcome (the latency-shrinking path, never the correctness path). |
| Metric | `llmsafespaces_secrets_reconcile_last_pass_success_timestamp` | Loop liveness — no successful pass in 3× the period trips `SecretsReconcileStalled`. |

## Diagnosis: a divergent workspace

1. Compare the stored revision against the pod's terminal report:
   `kubectl get workspace <id> -o jsonpath='{.status.secretsDelivery}'` —
   `spawnedRev`/`filesRev` below the stored `manifest_rev` (`seq:manifestHash`, from `workspace_secret_revisions`) means the pod is serving stale plaintext.
2. Reason drives the action:

| Reason / symptom | Meaning | Action |
|---|---|---|
| `legacy_format` | Pre-US-70.2 runtime (bare-array world) | Upgrade the workspace runtime / delivery images; converges with the fleet (never pages — excluded from the SLO) |
| `missing_rev` / `stale_seq` | Pod reported no anchor, or the apply-guard held a stale/out-of-order pull (by design — never downgrades) | Force-reconcile (below); if it persists, check the pod's pull path (SA token, API reachability) |
| `notify_failed` | Pod unreachable when notified (suspend window, restart) | No action if Active — the loop retries each period; suspended pods converge at resume boot-pull |
| `degradedReason: pull_failed` / `pull_unauthorized` | Bootstrap pull failing on the pod (auth vs transport distinction) | `kubectl logs` the agentd container; check `/internal/v1/pod-bootstrap` reachability and SA-token rotation (AC-14: fresh read per pull) |
| `degradedReason: dek_unwrap_failed` | User DEK unwrap failed server-side | Audit row names the workspace + error; check the keyrewrap section below — the row is a heal candidate |
| `spawn_env_unavailable` family | Spawn-time env pull degraded (bounded wait exhausted, last-good in use) | Sidecar user-mux reachability; see design 0057 R2 |

## Force-reconcile (no restart needed)

- **API manual trigger:** `POST /api/v1/workspaces/{id}/reload-secrets` — notifies the pod's resync endpoint; the pod re-pulls (fresh SA token, apply-guard, revision anchor). Response reports what the pod did.
- **From inside the workspace (no human):** the agent-facing MCP tool `secrets_resync` drives the same re-pull loopback and returns the applied revision.
- **Do nothing:** the reconcile loop converges any divergence within one period (default 60s; `LLMSAFESPACES_SECRETS_RECONCILE_INTERVAL`).

## Alert triage

| Alert | Severity | Action |
|---|---|---|
| `LLMSafeSpacesSecretsDeliveryDivergent` | critical | Per-workspace >15m stale — this runbook's diagnosis flow, then force-reconcile |
| `LLMSafeSpacesSecretsConvergenceSLOBurn` | warning | Fleet-wide <99% over 5m — suspect the loop/resync/bootstrap path, correlate `notify_total` outcomes |
| `LLMSafeSpacesSecretsReconcileStalled` | critical | No successful loop pass in 3× the period (or ever) — check the API's K8s client and loop error logs; nothing self-heals while dead |
| `LLMSafeSpacesKeyRewrapVerifyFailures` | warning | Re-wraps refused verification — investigate master-KEK health; rows left untouched by design |
| `LLMSafeSpacesKeyRewrapHalted` | critical | Blast-radius guard engaged (≥3 verify failures) — investigate the KEK/provider before it resumes |
| `LLMSafeSpacesKeyRewrapUnwrappable` | info | Per-user strand signal (owner offline / no secret to verify) — detail in `secret_audit_log` action=`key_rewrap_unwrappable` |

## Key re-wrap (US-70.4) specifics

`user_keys` rows that fail attribute-provider decrypt are re-healed login-independently: recovery source is the Redis session-cache DEK (`GetCachedDEKForUser` — no unwrap fallback), verified against one user secret, committed via CAS after verify-after-write. A user seen `unwrappable_no_source` either logs in once (repopulates the cache → next pass heals) or has no secrets to verify against (never healed unverified, by design).

## Rollback

The demolition is code-only — no data migration to reverse (batches are built on read; the pod-side batch file is per-pod ephemeral). Roll back the release (`helm rollback` to the pre-70.5 revision — API, controller, and agentd/opencode delivery images move together). Notes:

- The pruned CRD (no `userCredsPresent`) is benign for an older controller — it reads an absent field as empty; re-apply the old CRD only if tooling needs the field back.
- A post-rollback fleet briefly reports revisions the old loop does not understand — the old regime's push path re-establishes itself on next bind/reload; divergence self-resolves.
- The chaos/partition convergence legs of the US-70 delivery pool (`local/` pool scripts) exercise the equivalent heal paths machine-verified; `helm rollback` + one force-reconcile per canary workspace is the manual drill.
