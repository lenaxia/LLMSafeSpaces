# Runbook: agentd sidecar flip (design 0051, migration step 3)

**Owner:** platform operator
**Preconditions (all mandatory, in order):**

1. Release carrying steps 1–2 (`init-fs`, platform-init containers, sidecar boot phase, supervisor Command bypass + self-verify) + step 4 (factory `MinBaseVersion` floor) is deployed: controller, API, and the digest-pinned agentd delivery image move as ONE coordinate (`controller.agentdDelivery.image` — the Renovate-managed single coordinate).
2. The L3 kind suite (K1–K13) has passed once against the released artifacts, including:
   - **K10** resumed-legacy-PVC boot (the force-upgrade path),
   - **K11** sidecar-restart idempotency guard,
   - **K13** degraded-base boot (the 2026-08-25 incident-class regression).
3. Cluster ≥ 1.35 (native sidecars) — already the chart floor.

**Flip:** set `controller.agentdSidecar.enabled: true` in the release values. New and recreated pods get the sidecar spec; existing running pods keep their old spec until restarted (D6.1 convergence).

**Rolling upgrade (canary → fleet):**

1. **Canary one Active workspace:** `kubectl -n llmsafespaces delete pod <pod>` (NORMAL delete only — SIGTERM; never `--grace-period=0`, agentd's session-aware shutdown + SQLite WAL need the grace window). Verify:
   - new pod spec: `platform-init` first init, `agentd` last init with `restartPolicy: Always`, main `command[0]=/agentd/usr/local/bin/workspace-agentd`;
   - `BootReady=True` condition on the Workspace;
   - sidecar Ready, opencode session smoke (chat round-trip);
   - env-secrets/SSH key still land (cross-uid 0640 profile — K3's `secrets-env` mode).
2. **Batch:** delete pods of Active workspaces in batches (≤25% per wave), same verification per wave. Watch `llmsafespaces_workspace_platform_boot_failures_total` — any non-zero value STOPS the rollout (page; it is a platform regression, not load).
3. **Suspended workspaces:** do nothing. They regenerate the pod spec at resume — free upgrades, no action, no risk window.
4. **Accepting force-upgrade semantics:** an in-flight session on a deleted pod is interrupted (~22s resume cost); PVCs retain all state; opencode.db replays via WAL.

**Rollback:** flip the value back off. Per D6.1: recreated pods converge to the legacy single-container spec; Secrets carry extra keys that legacy readers ignore; file relocations re-converge via reset()+re-materialize. The legacy bash path and baked binary remain in the base image until migration step 5 (post-soak deletion) — that is exactly why they are retained.

**Step 5 (separate, later):** after ≥1 soak cycle with zero platform-boot failures, delete the baked agentd + entrypoints + legacy bash init path from `runtimes/base/Dockerfile` and `pod_builder.go` (the `overlayOn=false` branch). Keep the prior base tag pullable as the rollback artifact. The `MinBaseVersion` floor then also ratchets to the release that makes baked-agentd absence universal.
