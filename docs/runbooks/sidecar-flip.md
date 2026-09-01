# Runbook: agentd sidecar flip (design 0051, migration step 3)

**Owner:** platform operator
**Preconditions (all mandatory, in order):**

1. Release carrying steps 1–2 (`init-fs`, platform-init containers, sidecar boot phase, supervisor Command bypass + self-verify) + step 4 (factory `MinBaseVersion` floor) is deployed: controller, API, and the digest-pinned agentd delivery image move as ONE coordinate (`controller.agentdDelivery.image` — the Renovate-managed single coordinate).
2. The L3 kind suite (K1–K13) has passed once against the released artifacts, including:
   - **K10** resumed-legacy-PVC boot (the force-upgrade path),
   - **K11** sidecar-restart idempotency guard,
   - **K13** degraded-base boot (the 2026-08-25 incident-class regression).
3. Cluster ≥ 1.35 (native sidecars) — already the chart floor.
4. **S5 overlay validation green** (`local/s5-overlay-validation.yml`) — design 0053: the stripped base + both mandatory overlay pins are now the ONLY mode; the flip rides on S5.2 (launch→ready), S5.5 (resume cost), and S5.6 (gVisor runsc leg — design 0051's open item).
5. **US-70.1 deployed** (spawn-time env pull, #1164/design 0057) — the pre-70.1 sidecar boot deterministically lost env-class secrets (`pushInitialSpawnEnv` dialed the control socket before the workspace container existed); the pull closed that class.

**Flip:** set `controller.agentdSidecar.enabled: true` in the release values. New and recreated pods get the sidecar spec; existing running pods keep their old spec until restarted (D6.1 convergence).

**Rolling upgrade (canary → fleet):**

1. **Canary one Active workspace:** `kubectl -n llmsafespaces delete pod <pod>` (NORMAL delete only — SIGTERM; never `--grace-period=0`, agentd's session-aware shutdown + SQLite WAL need the grace window). Verify:
   - new pod spec: `platform-init` first init, `agentd` last init with `restartPolicy: Always`, main `command[0]=/agentd/usr/local/bin/workspace-agentd`;
   - `BootReady=True` condition on the Workspace;
   - sidecar Ready, opencode session smoke (chat round-trip);
   - env-secrets/SSH key still land (cross-uid 0640 profile — K3's `secrets-env` mode).
2. **Batch:** delete pods of Active workspaces in batches (≤25% per wave), same verification per wave. Watch the stop-condition metrics — **any non-zero value STOPS the rollout** (page; it is a platform regression, not load):
   - `llmsafespaces_workspace_platform_boot_failures_total`
   - `llmsafespaces_workspace_agentd_verify_failures_total`
   - `llmsafespaces_workspace_opencode_verify_failures_total`

   **CounterVec semantics (verified live, #1211 F4): these metrics are ABSENT from `/metrics` while failures are zero** — a Prometheus CounterVec emits nothing until its first increment. Absence is the healthy state, not a missing/rename. The positive health signals are `llmsafespaces_agentd_gate_duration_seconds{gate=…}` (count ≥ 1 = every gate passed) and `llmsafespaces_stalled_entries 0`.

   **Scrape method:** port-forward the controller's metrics port (`kubectl -n llmsafespaces port-forward deploy/llmsafespaces-controller 8080:8080`) — the controller image ships no wget/curl, so `kubectl exec … -- wget` is a false negative.
3. **Suspended workspaces:** do nothing. They regenerate the pod spec at resume — free upgrades, no action, no risk window. **Spot-resume caveat (#1211 F3):** a workspace idle longer than the auto-suspend timeout resumes, verifies, then re-suspends in ~a minute (Creating → OpencodeVerified → "idle timeout exceeded") — functionally correct, but refresh `status.lastActivityAt` first if you want a resumed workspace to stay up (the F2 drain used exactly this).
4. **Accepting force-upgrade semantics:** an in-flight session on a deleted pod is interrupted (~22s resume cost); PVCs retain all state; opencode.db replays via WAL.

**Rollback:** flip the value back off. Recreated pods converge to the single-container spec (`--supervise`); Secrets carry extra keys that legacy readers ignore; file relocations re-converge via reset()+re-materialize.

**Step 5 — DONE (design 0053, #1156):** the baked agentd + entrypoints + legacy bash init path were deleted from `runtimes/base/Dockerfile` and `pod_builder.go` (the `overlayOn` branch); `MinBaseVersion` was deleted outright (no baked contract remains to floor). The rollback artifact is the LAST pre-strip base tag (kept pullable; content-versioned rows start at `bookworm@2026.08.0`).
