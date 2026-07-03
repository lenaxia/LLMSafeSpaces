# Worklog: user-DEK secrets don't manifest on pod recreation — design + implementation

**Date:** 2026-07-02
**Session:** Diagnose why user's env-secrets, SSH keys, and user-owned LLM providers disappear from workspaces on pod recreation, design the fix, and implement it.

**Status:** Design complete, implementation complete on branch `fix/pod-recreation-auto-push-secrets`. Upstream issue #493 filed. PR ready to open once bot/CI validate.

---

## User-reported symptom

> "Secrets arent getting manifested into workspaces, and if I go through session -> settings -> secrets disable then enable, they will remanifest, but then eventually disappear again"

Test workspace: `chat.safespaces.dev/chat/a127833a-d68c-4732-ba45-6dafd8081bfd/ses_0eb6352b5ffe9xiApZ5P7KeVLo`

---

## Diagnosis (verified against running cluster)

### Ground truth on the pod

Workspace `a127833a` (Active, 4 minutes old at diagnosis time, pod `a127833a-...-f5a62fdb`):

- `credential-setup` init container logged: `bootstrap: wrote admin prompt (241 bytes)` and `bootstrap: success, 732 bytes secrets`.
- Main container's early stdout: `materialize: 2 materialized, 0 skipped, 0 failed`.
- On-disk state at diagnosis:
  - `/sandbox-runtime/secrets-env` — **absent**.
  - `/sandbox-runtime/last-reload-secrets.json` — **absent**.
  - `/sandbox-runtime/rt/secrets/` — empty directory.
- `/sandbox-cfg/secrets.json` — 732 bytes, 2 entries: `{type: llm-provider, name: thekaocloud}` + `{type: llm-provider, name: opencode-free-tier}`.
- `/sandbox-runtime/agent-config.json` — has 3 providers (`opencode-free-tier`, `opencode-relay`, `thekaocloud`).
- Workspace CRD conditions: `AgentHealthy=True`, `ProviderReady=True (connected=[opencode-relay thekaocloud])`. **No `CredentialsAvailable` condition.**
- API workspace-status response: `credentialState.available=false, reason=NotChecked` and `providersConfigured=0`.

### API observation

- The `POST /internal/v1/pod-bootstrap` response body for this workspace (from API logs): `{"secrets": [2 llm-provider entries], "workspaceConfig": {"defaultModel": "glm-5.2"}}`.
- No user-owned entries (env-secrets, SSH keys) delivered.
- No `/api/v1/workspaces/a127833a/secrets` or `/bindings` writes in the last hour — the user hasn't done the "disable/enable" dance yet on this session.

### What phase-1 delivers vs. what's missing

`InjectSessionlessSecrets` at `pkg/secrets/injection.go:123` deliberately delivers only server-KEK-decryptable content:

- `loadServerKEKCredentials` — admin/org LLM providers (server-KEK).
- `loadNonLLMSecrets` with `sessionID=""` — every user-owned entry hits the `session_id == ""` case in the DEK loop and is skipped with an audit (`secret_skipped_no_session`).

Comment at `pkg/secrets/injection.go:353-361` documents this: user-DEK material is intentionally deferred to a "phase 2" that requires a session with a real DEK.

**But there is no automatic phase-2 trigger.** All existing callers of `InjectSecrets` → `pushSecretsToAgent` are user-initiated:

- `SetBindings` (user toggles binding in Workspace Settings).
- `ReloadSecrets` (explicit `POST /workspaces/:id/reload-secrets` from a client).
- User provider-credentials handlers (create/update/delete).

Controller's reconciler comment at `controller/internal/workspace/reconciler.go:67` even says: *"live `/v1/reload-secrets` push handles delivery"* — but nothing in the API actively fires that after pod boot.

### Reload cache

`/sandbox-runtime/last-reload-secrets.json` is on tmpfs (`agentd.ReloadSecretsCachePath = "/sandbox-runtime/last-reload-secrets.json"`). Per `README-LLM.md:476`: *"persisted by reloadSecretsHandler; replayed by boot-time materialize to restore user-DEK creds after a container restart (#443); tmpfs, wiped on pod death."*

- **Container restart within a pod**: cgroup survives → tmpfs survives → cache replayed by boot materialize → user-DEK secrets restored automatically. Case handled by PR #443.
- **Pod recreation** (kubectl delete pod, node failure, spot reclaim, controller-driven refresh): pod cgroup destroyed → tmpfs wiped → new pod has no cache → phase-1 only. **This is the "eventually disappear" case the user reported.**

### The "disable/enable" workaround explained

User's manual toggle in Workspace Settings drawer fires `PUT /workspaces/:id/bindings` → `SetBindings` → `pushSecretsToAgent` → `InjectSecrets` (with the user's live JWT/session → DEK available) → `POST /v1/reload-secrets` on agentd → materialize with full batch → cache written. Secrets manifest. On next pod recreation, cache is wiped and the cycle repeats.

---

## Design review

### Reject: server-side reconciler with fabricated user identity

I initially considered "controller notifies API → API looks up owner's most-recent session in `jwt_sessions` → API uses that DEK to push." Rejected:

- Requires `matchedSigningKey` (JWT signing key) which the controller-internal caller does not have. Per `KeyService.GetDEK` doc at `pkg/secrets/key_service.go:422-427`: *"matchedSigningKey is the JWT signing key that validated the caller's token. Pass nil for non-JWT auth (API keys, controller-internal callers); those paths cannot rehydrate..."*
- Would smuggle user-authenticated intent into a controller-driven flow that shouldn't have that authority.
- The DEK-outside-user-request-context problem is a real security property, not a bug to work around.

### Reject: frontend-only (ChatPage mount effect)

Works for browser users; misses SDK consumers. Considered a narrow fix, then corrected below.

### Reject: PVC-side secret persistence

Solves "eventually disappear" but violates US-35.7 (`/sandbox-runtime` must be tmpfs; plaintext creds must not persist on PVC across pod death).

### Reject: agentd writes CRD status directly

Requires workspace-pod SA to have `workspaces/status` patch RBAC. Big authority increase. Currently workspace pods have zero cluster API access; that's a deliberate security boundary.

### Reject: controller mirrors agentd `/readyz` to CRD condition

Adds a poll loop AND requires extending `/readyz` to expose per-secret-type coverage (currently only `providers_connected`). Also requires deciding what `CredentialsAvailable=True` actually means (any creds? all bound creds? user-DEK creds specifically?) — the condition would be overloaded.

### Reject: agentd exposes materialized-secrets fingerprint on `/readyz`, API compares to bindings and pushes on drift

Considered as a state-centric reconciler. Structurally elegant but reaches too far: I was inventing a new signal path when an existing DB-backed reconciliation primitive (`workspace_agent_state.pending_refresh`) already covers this exact scenario for the binding-change case. The correct move is to trigger the existing primitive on a new event (pod-UID change), not build a parallel one.

### Reject: purely explicit user-consent banner UX

Just fixing the trigger without auto-push means every pod recreation surfaces a banner the user must click. Better than the current state (no banner at all — user has to hunt through settings), but still worse than automatic when the request-serving context already has the DEK.

### Chosen: piggyback on existing `workspace_agent_state` primitive + auto-push on pod-UID transition

**The critical realization**: the API server *already runs* on every workspace-status GET. That happens on every frontend poll (~every 2s while a chat page is open). It has DB access, has the user's JWT context (matched signing key), and has the pod-UID visible via `crd.status.podUID`. That's the natural place to detect pod-UID transitions.

The primitive to wire it into (`workspace_agent_state`) already exists, has the right shape (`pending_refresh boolean` + `last_credential_changed_at timestamp`), is already surfaced to the frontend (`agentNeedsRefresh` on workspace-list responses), and already has a UX for the fallback case (`AgentReloadBanner` component). All the plumbing is in place; the missing piece is the pod-UID-transition detector.

---

## User-approved design decisions

Two explicit decisions during the design pass:

1. **State-centric reconciliation, not action-centric history.** User's exact words: *"our action doesn't matter what we care about is IF the workspace should have user secrets, DO THEY EXIST? And if not, reconcile while handling error cases."* This shifted the framing from "did we push" to "does state agree."

2. **Auto-reload with banner fallback.** User picked automatic reload on pod-UID transition (fire-and-forget goroutine using the current request's DEK from Redis). Banner surfaces only if auto-push fails — as the manual-consent escape hatch.

---

## Failure-mode walkthrough (all verified against the chosen design)

| # | Scenario | Handled by |
|---|---|---|
| 1 | Container OOM-kill within pod | PR #443 already handles: tmpfs survives cgroup, cache replayed on boot materialize. **No new code needed.** |
| 2 | Pod recreation (`kubectl delete pod`) | Pod-UID transition detected on next status read → auto-push fires with current user's DEK. |
| 3 | Node failure / spot reclaim | Same as #2. New pod, new UID, detected on next status read. |
| 4 | agentd OOM (same container as opencode) | Same container = case #1. |
| 5 | Bindings change while push in flight | `reloadMu` in agentd serializes; second push wins on disk. Convergent. |
| 6 | DEK expired mid-push | Push already had plaintext in memory; completes. Next push may re-fetch. |
| 7 | Stale digest returned by agentd | N/A — we're not using a digest; the pod-UID transition is the trigger. |
| 8 | agentd unreachable | Fire-and-forget goroutine logs error, `pending_refresh` stays TRUE, banner appears as fallback. Next request retries. |
| 9 | Nobody's authenticated for a long time | Workspace not being used → nobody needs secrets right now. Correct behavior. First request after user returns fixes it. |
| 10 | API replica race (two goroutines fire simultaneously) | `reloadMu` serializes on agentd side; both succeed idempotently. Wasted work, not incorrect. |
| 11 | User removes a binding | `SetBindings` already fires `pushSecretsToAgent` with the new (shorter) list; agentd's `reset()` nukes the old secret. Correct. |
| 12 | Same slug, different content | `MarkCredentialChanged` already fires on user-provider-credential update; existing UX picks it up. |
| 13 | Pod-UID never changes but tmpfs is wiped some other way | Not a real scenario I can construct. The only path to tmpfs-wipe-without-pod-recreation is if someone execs into the pod and manually rm's the files. Out of scope. |

---

## Chosen design (detailed)

### Data model change

**Migration `NNN_add_last_seen_pod_uid.up.sql`:**
```sql
ALTER TABLE workspace_agent_state
    ADD COLUMN last_seen_pod_uid text;
```

Down migration drops the column. Idempotent additions.

Rationale for `text` (not `uuid`): pod UID in k8s is a UUID, but storing as text avoids forcing a parse on read and lets a future implementation use a different opaque identity if needed (e.g. `pod.status.startTime` as fallback when UID is missing during a race).

### Detection point

`workspace_service.GetWorkspaceStatus` (already reads the CRD on every status GET). After fetching the CRD, compare:

- `crd.status.podUID` (or `crd.status.podName + creationTimestamp` if UID isn't in the CRD status — needs verification).
- `workspace_agent_state.last_seen_pod_uid` for this workspace.

Three cases:

1. **Both empty / same**: no-op.
2. **Row absent** (first observation): insert row with current UID, `pending_refresh = FALSE` (assume phase 1 was sufficient until proven otherwise).
3. **UIDs differ**: pod was recreated since last observation. Trigger the auto-push path.

Detection lives in `GetWorkspaceStatus` (not middleware, not a new endpoint) because that's the code path that ALREADY reads the pod-UID for every frontend poll — no new reads, no new query load.

Verification needed at implementation time: does `WorkspaceStatus` (the CRD status field) currently include podUID? If not, either add it (controller-side) or use `podName + startTime` as a proxy.

### Auto-push mechanism

On pod-UID-transition detection:

```
tx = db.Begin()
UPDATE workspace_agent_state
    SET last_seen_pod_uid = <new>,
        last_credential_changed_at = NOW(),
        pending_refresh = TRUE,
        updated_at = NOW()
    WHERE workspace_id = $1
tx.Commit()

// Fire-and-forget the push
go func() {
    ctx = context.WithoutCancel(originalCtx) // survive request completion
    secretsJSON, err := secretsSvc.InjectSecrets(ctx, userID, sessionID, matchedSigningKey, workspaceID)
    if err != nil {
        logger.Warn("auto-push after pod recreation: inject failed",
            "workspaceID", workspaceID, "error", err.Error())
        return
    }
    result, err := doReload(ctx, userID, workspaceID, secretsJSON)
    if err != nil {
        logger.Warn("auto-push after pod recreation: reload failed",
            "workspaceID", workspaceID, "error", err.Error())
        metricsService.RecordAutoPushFailure(workspaceID)
        return
    }
    // Clear pending_refresh only on success
    _ = db.MarkAgentReloaded(ctx, tx, workspaceID, priorChangedAt)
    metricsService.RecordAutoPushSuccess(workspaceID)
}()
```

**Structured logging** — every auto-push attempt emits a log line at INFO on success or WARN on failure with `workspaceID`, `oldPodUID`, `newPodUID`, elapsed. Every future incident of "secrets missing on pod X" is grep-able in one hop.

**Metric** — new counter `api_secret_auto_push_total{outcome}` where `outcome ∈ {"success", "inject_failed", "reload_failed", "no_pod"}`. Existing metrics pattern (`services/metrics/`).

### UX (mostly no change)

- `AgentReloadBanner` continues to appear when `agentNeedsRefresh=true`.
- On successful auto-push, `MarkAgentReloaded` clears the flag → next workspace-status poll sees `agentNeedsRefresh=false` → banner disappears within a poll cycle.
- On failed auto-push, flag stays TRUE → banner stays visible → user can click "Reload agent" as the manual escape hatch.
- **Result:** for the common case (auto-push succeeds), the user sees no ceremony — secrets manifest transparently. For the uncommon failure case, they see the exact same banner they see today.

### What this does NOT change

- No new API endpoint.
- No new controller code.
- No new authority delegation (API server still authenticates as the current user).
- No new agentd endpoint.
- No new CRD field or condition.
- No PVC-side secret persistence.
- No frontend-specific code path (SDK, MCP, browser all benefit from the same server-side auto-push).

---

## Consumer inventory (verified for auto-push compatibility)

| Caller | Auth | DEK access | Auto-push fires? |
|---|---|---|---|
| Browser JWT session | Session cookie | Redis `dek:<jti>` + jwt_sessions rehydrate | ✅ Yes |
| SDK with API-key + `DecryptAccess` | API-key header | Redis `dek:apikey:<hash>` cached at auth time (`api/internal/services/auth/auth.go:731`) | ✅ Yes |
| MCP proxying user requests | Passes through JWT or API-key | Same as its authenticated principal | ✅ Yes |
| SDK with API-key without `DecryptAccess` | API-key header | No DEK cached | ❌ No — `InjectSecrets` degrades to skip-with-audit for user-DEK entries |
| Controller | ServiceAccount token | Neither | ❌ No — controller doesn't hit workspace-status GET |

All consumers that could deliver user-DEK secrets are covered by the same code path.

---

## Adversarial checks against the chosen design

### Case: "same slug, different content"

Bindings unchanged (same `{type, name}`), but the credential's stored value was updated. Pod-UID unchanged. Auto-push does not fire.

**Already handled by existing code**: `MarkCredentialChanged` is called from `user_provider_credentials.go` on create/update/delete of a user provider credential. Independent of pod recreation.

### Case: user has NO user-owned bindings

Pod-UID transitions, auto-push fires, but `InjectSecrets` returns only the server-KEK content that was already delivered at phase-1 boot. `pushSecretsToAgent` → agentd re-materializes the same 2 entries. Wasted work but not incorrect.

**Optimization opportunity**: gate the auto-push on "workspace has at least one user-owned binding." Requires a DB query. Skip for now — the wasted-work cost is one materialize per pod recreation (rare), and the query cost would fire on every status GET. Net-negative. **Defer this optimization until we see it in production hot-loops.**

### Case: two consecutive pod recreations quickly (e.g. crashloop)

First transition: UPDATE `last_seen_pod_uid` to UID_A, fire push. Push in-flight against pod at UID_A.

Second transition (UID_A → UID_B, before push completes): UPDATE `last_seen_pod_uid` to UID_B, fire push against UID_B.

First push may hit an already-gone pod (503, connection error, `errNoRunningPod`). Second push targets the current pod. Convergent.

Log lines will show two auto-push attempts in quick succession — noisy but correct. Rate-limit if this becomes a real problem.

### Case: pod-UID transition observed by API replica A, but replica B gets the status read next tick

Replica B sees `workspace_agent_state.last_seen_pod_uid` already equals current UID (replica A updated it). Skips the auto-push. Correct.

Race window: two replicas observe the transition simultaneously and both fire the push. Handled by agentd's `reloadMu` — both succeed idempotently.

### Case: user was authenticated 30 seconds ago but their JWT just expired

Their DEK in Redis has TTL matching JWT TTL. Redis get returns nil. Rehydrate path needs `matchedSigningKey` — but this request already validated their JWT (else auth middleware would have rejected) so `matchedSigningKey` is present in context.

If rehydrate succeeds: push proceeds. If rehydrate fails (jwt_sessions row expired too): `InjectSecrets` degrades to skip-with-audit for user-DEK entries. Auto-push completes with phase-1 content only. Same effective outcome as "nobody's ever logged in" but logged with the specific reason.

### Case: user's cluster has no user-DEK data at all (single-user homelab)

Every pod recreation triggers an auto-push that materializes the same phase-1 entries. Wasted work. See "no user-owned bindings" case above — deferred optimization.

---

## Implementation plan (next session)

**File the upstream issue first**, then TDD:

1. **File issue** with the diagnosis (pod-recreation → tmpfs wipe → no auto phase-2 trigger) and the design (last_seen_pod_uid + workspace-status detection + fire-and-forget push).

2. **Migration**: `NNN_add_last_seen_pod_uid.{up,down}.sql`.

3. **Update `workspace_agent_state` schema access:**
   - Add `LastSeenPodUID *string` to whatever struct maps this table (probably in `pkg/secrets` or `api/internal/services/database`).
   - `SetLastSeenPodUID(ctx, workspaceID, uid)` — update-only.

4. **Test the detection logic in isolation** (unit test):
   - Given: workspace row with `last_seen_pod_uid=UID_A`.
   - Input: CRD returns `podUID=UID_B`.
   - Assert: detector returns "transition needed".
   - Assert: after `SetLastSeenPodUID`, row has UID_B.

5. **Test the auto-push wiring** (integration test):
   - Given: mock agentd that accepts `/v1/reload-secrets`.
   - Given: workspace with a user-owned env-secret binding + a fake DEK in mock Redis.
   - Trigger: `GetWorkspaceStatus` on a request with valid user context, CRD returns a new pod UID.
   - Assert: `pushSecretsToAgent` was called; mock agentd received the reload; `pending_refresh` is TRUE then cleared.

6. **Adversarial tests:**
   - `InjectSecrets` returns error → `pending_refresh` stays TRUE, metric fires with `inject_failed`, log line present.
   - `doReload` returns 5xx → same but `reload_failed`.
   - Pod not yet running (no IP) → `no_pod` outcome, log info-level (not warn — this is transient).
   - Second concurrent transition mid-push → both eventually converge, no lost updates in DB.

7. **PR** with:
   - Root-cause section quoting the "eventually disappear" symptom.
   - Design tradeoffs summarized (why this over the alternatives above).
   - Test coverage description.
   - Deployment note: existing workspaces work as-is; the first status poll after deploy inserts the initial `last_seen_pod_uid` row and skips auto-push (per case 2 in the detection logic).

8. **Deploy to `home-kubernetes`**, then verify against `a127833a` (the workspace in the user's original report): trigger pod recreation via `restartGeneration++`, watch the API logs for the auto-push line, verify `/sandbox-runtime/rt/secrets/` populates without user intervention.

---

## Key files (bookmarks for next session)

- `pkg/secrets/injection.go:123` — `InjectSessionlessSecrets` (phase 1, cold boot path).
- `pkg/secrets/injection.go:93` — `InjectSecrets` (phase 2, user-DEK path).
- `pkg/secrets/injection.go:353-382` — comment block on phase-1/phase-2 semantics.
- `pkg/secrets/key_service.go:407-491` — `GetDEK` and `rehydrateDEKFromJWTSession`.
- `pkg/secrets/redis_cache.go` — Redis key layout for DEK (`dek:<sessionID>`).
- `api/internal/services/auth/auth.go:731` — API-key auth caches DEK under `apikey:<hash>`.
- `api/internal/handlers/secrets.go:306` — `SetBindings`, the existing auto-push path.
- `api/internal/handlers/secrets.go:429` — `pushSecretsToAgent`, reused verbatim by the new path.
- `api/internal/services/database/database.go:1157` — `MarkCredentialChanged`.
- `api/internal/services/database/database.go:1194` — `MarkAgentReloaded`.
- `api/internal/handlers/pod_bootstrap.go:203` — where cold-boot secrets are injected.
- `api/internal/handlers/agent_reload.go:168` — the existing `POST /agent/reload` handler (dispose+restart, does NOT auto-push user-DEK).
- `api/migrations/000001_initial_schema.up.sql` — where `workspace_agent_state` is declared.
- `cmd/workspace-agentd/secrets.go:346` — `runMaterializeCommand`, the boot-time materialize.
- `cmd/workspace-agentd/secrets.go:640` — `reloadSecretsHandler`, the live-push endpoint.
- `frontend/src/components/workspace/AgentReloadBanner.tsx` — the existing banner UX (fallback).
- `frontend/src/pages/ChatPage.tsx:903` — where the banner is conditionally rendered.
- `controller/internal/workspace/reconciler.go:67` — the comment declaring that live push handles delivery.

---

## Open questions to resolve at implementation time

1. **Does `crd.status.podUID` exist?** Need to verify against `pkg/apis/llmsafespaces/v1/workspace_types.go`. If not, either add it (small controller change) or use `podName + startTime` as the identity tuple.

2. **Where does `matchedSigningKey` live in the request context?** The existing `SetBindings` handler pulls it via `extractMatchedSigningKey(c)`. Confirm that's callable from `workspace_service.GetWorkspaceStatus`.

3. **Should we gate on "workspace has at least one user-owned binding" to skip auto-push for single-user-no-user-DEK setups?** Deferred; measure first.

4. **Rate-limiting for pathological pod-crashloop scenarios**: is the current logging noisy enough to be a problem? Probably not in v1; defer.

5. **Frontend regression risk**: none anticipated. The `AgentReloadBanner` already exists and its trigger (`agentNeedsRefresh`) is set by exactly the same DB primitive we're wiring to a new event. The frontend sees the flag flip TRUE briefly and back to FALSE on successful auto-push — that's within the poll interval and unlikely to render the banner at all in the common case.

---

## Definition of done

- Migration merged and applied.
- Auto-push fires on pod-UID transition in production.
- Test workspace `a127833a` (or any workspace) can be `restartGeneration++`'d and secrets manifest without user intervention within one workspace-status poll cycle (~2s).
- Metric `api_secret_auto_push_total{outcome}` visible in Prometheus, non-zero after a pod refresh.
- Log line `"auto-push after pod recreation: success"` (or the corresponding failure line) present for every pod recreation.
- Existing `AgentReloadBanner` UX unchanged for the failure fallback case.
- Worklog updated with post-implementation findings (deferred optimizations that ended up mattering, any adversarial cases we missed, actual metric values in production).

---

## Implementation summary (2026-07-02 session)

### Deviations from the design pass and why

1. **Identity tuple: PodName + StartTime, not PodUID.** The CRD status has no `podUID` field; controller writes only `PodName` + `StartTime` in `phase_creating.go:120,166,171`. Adding `PodUID` would require a controller change + CRD regen + coordinating deploy order. The user chose PodName + StartTime — it's already present, and both fields change on every pod recreation. Two `text` columns in DB rather than a synthetic tuple string (SQL-natural, indexable).

2. **`agentpush` extracted as a proper service, not left on SecretsHandler.** The design said "reuse `pushSecretsToAgent`"; the pragmatic reading of that was "call the existing helper." But the existing helper takes `*gin.Context`, and `workspace.Service.GetWorkspaceStatus` only has `context.Context`. Rather than plumb Gin down into the service layer or add a second implementation of the reload flow, extracted `pkg api/internal/services/agentpush` as the shared implementation:
   - Depends on: `SecretInjector` interface (satisfied by `*secrets.SecretService`), `PodIPResolver`, `ModelCache`, `Logger`.
   - Uses `context.WithValue`-carried sessionID + matchedSigningKey (helpers `agentpush.WithAuth` / `agentpush.AuthFromContext`).
   - Emits outcome metric (`success|inject_failed|reload_failed|no_pod`) via optional hook.
   - `SecretsHandler.pushSecretsToAgent` and `doReload` now delegate to it; behavior identical for all existing callers.
   - `workspace.Service.SetSecretPusher` accepts a narrow `SecretPusher` interface (single method: `Push(ctx, userID, workspaceID) error`); `app.wsAgentPusherAdapter` bridges the concrete `*agentpush.Service` to that interface. Dependency-inversion at both consumer sites.

3. **Auth via context (agentpush.WithAuth) rather than function args.** Non-handler callers (workspace.Service) only have `context.Context`. Rather than have two `Push` signatures, both handler and non-handler callers now populate ctx before calling. Router's `GET /status` closure sets sessionID + matchedSigningKey on ctx before calling `wsSvc.GetWorkspaceStatus`. The handler-driven callers (SetBindings, ReloadSecrets) also set ctx before their internal Push calls.

4. **Metric hook is on agentpush, not on workspace.Service.** Every push goes through agentpush, and agentpush emits the outcome metric. The workspace service was originally going to have its own hook but that would double-count. Removed; the single emission point in agentpush is the invariant now.

5. **Non-Active phase = skip detection entirely.** Suspended/Creating phases legitimately have empty PodName. If we recorded that as the "current" identity, the next Active-transition would look like an initial-observation and never push. Fixed by only entering the state machine when `crd.Status.Phase == Active` AND `PodName != ""` AND `StartTime != nil`. Test `TestPodIdentity_NonActivePhaseSkipsDetection` locks this in.

### TDD progression

- **Migration `000005_workspace_agent_state_pod_identity.{up,down}.sql`** — 2 columns, both nullable; up is idempotent (IF NOT EXISTS). Auto-mirrored to `charts/llmsafespaces/migrations/` via `make chart-sync-migrations` (validated by `TestLive_ChartMigrations_NoDriftFromCanonical`).
- **`GetLastSeenPodIdentity` / `UpsertLastSeenPodIdentity` / `MarkPodIdentityTransition`** on `*database.Service` — 4 sqlmock unit tests including the empty-row-returns-zero-values contract. `ExpectationsWereMet` assertion catches the neutered-implementation adversarial validation.
- **`agentpush.Service`** — 8 unit tests covering happy path, auth-from-context round-trip, all four outcome classifications, and success-only cache eviction. Adversarial-validated with neutered `AuthFromContext` and neutered model-cache eviction.
- **`workspace.Service.maybeAutoPushOnPodTransition`** — 9 tests covering the three-case state machine (unchanged / initial-observation / transition), non-Active-phase skip, no-configured-deps safe-null, push-failure-preserves-pending-refresh, and two context-cancellation adversarials. The stronger of the two ctx-cancellation tests (`_LogAssertion`) failed when I neutered `context.WithoutCancel` in the goroutine spawn, proving the assertion is load-bearing.
- **Handler migration** — `pushSecretsToAgent` and `doReload` now delegate to a lazily-constructed `agentpush.Service` from the handler's own deps; all 200+ existing SecretsHandler-adjacent tests still pass without modification (the setter-DI construction pattern is preserved via `SetPodIPResolver` + `SetModelCache` + `SetLogger`; new consumers use `SetAgentPusher(*agentpush.Service)` directly).

### Adversarial validations performed

Each of the following neutering experiments produced a failing test for the RIGHT reason before I restored the correct implementation:

1. `MarkPodIdentityTransition` becoming a no-op → `TestPodIdentity_MarkTransitionSetsPendingRefresh` fails (sqlmock unmet expectation).
2. `agentpush.AuthFromContext` returning zero values → `TestPush_PassesAuthFromContext` fails (injector never saw the sessionID/key).
3. `agentpush` skipping model-cache eviction on success → `TestPush_SuccessEvictsModelCache` fails.
4. `maybeAutoPushOnPodTransition` becoming an early-return no-op → 3 workspace-service positive-behavior tests fail.
5. Wrong method call (`UpsertLastSeenPodIdentity` in place of `MarkPodIdentityTransition`) → `TestPodIdentity_TransitionTriggersAutoPush` fails on `transitionCalls == 1` assertion.
6. Removing `context.WithoutCancel` → `TestPodIdentity_PushSurvivesRequestContextCancellation_LogAssertion` fails on the "no context-canceled warning" assertion.

### Metric + logging

- `api_secret_auto_push_total{outcome}` — package-level `promauto.NewCounterVec` in `api/internal/services/metrics/metrics.go`, exposed via `RecordSecretAutoPush(outcome)` and `SecretAutoPushCounter()` for test assertions. Same pattern as `upstream5xxTotal` from PR #489.
- Structured log lines from `workspace.Service.runAutoPush`: `"auto-push after pod recreation: success"` (INFO) or `"auto-push after pod recreation: failed"` (WARN). Fields: `workspaceID, oldPodName, newPodName, newPodStartTime, elapsedMs`, plus `error` on failure. Every future incident of "secrets missing after pod recreation" is grep-able in one hop.

### Deployment note

Existing workspaces have no row in `workspace_agent_state` with a `last_seen_pod_name`. On the first status poll after deploy, the API records the current identity via `UpsertLastSeenPodIdentity` (initial-observation branch) — no push fires. Zero-cost, zero-risk migration path. The next pod recreation on any of those workspaces enters the transition-fires-push branch.

### Files modified / added

- `api/migrations/000005_workspace_agent_state_pod_identity.{up,down}.sql` (new)
- `charts/llmsafespaces/migrations/000005_workspace_agent_state_pod_identity.{up,down}.sql` (new; mirror)
- `api/internal/services/database/database.go` (3 new methods)
- `api/internal/services/database/pod_identity_test.go` (new)
- `api/internal/services/agentpush/agentpush.go` (new package)
- `api/internal/services/agentpush/agentpush_test.go` (new)
- `api/internal/services/workspace/workspace_service.go` (interfaces + setters + detection logic)
- `api/internal/services/workspace/workspace_pod_identity_test.go` (new)
- `api/internal/services/metrics/metrics.go` (new counter + helper)
- `api/internal/handlers/secrets.go` (migrate `pushSecretsToAgent`/`doReload` to delegate to agentpush)
- `api/internal/server/router.go` (attach agentpush.WithAuth ctx on GET /status)
- `api/internal/app/app.go` (build shared agentpush.Service, wire into SecretsHandler and workspace.Service)
- `api/internal/app/secrets_adapters.go` (wsAgentPusherAdapter + recordAutoPushOutcome)

### Follow-ups deferred

- **Gate auto-push on "workspace has at least one user-owned binding"** to skip the wasted materialize for single-user-no-user-DEK setups. Deferred pending production data — the check itself would fire on every status poll, and the net cost is not obvious a priori.
- **Rate-limit auto-push for pod crashloops.** Two rapid consecutive transitions produce two auto-push attempts. Convergent (agentd `reloadMu` serializes) but noisy. Not fixing until we see it in production logs.
- **`api_secret_auto_push_duration_ms` histogram.** Currently only counter. Add if the p99 latency becomes a support signal (e.g. "auto-push takes 10s on my cluster").
- **Full `SecretsHandler` migration off setter-DI onto explicit `SetAgentPusher`.** Currently the handler falls back to lazy-construction from `SetPodIPResolver`/`SetModelCache`/`SetLogger` for test compatibility. A future PR can migrate the ~15 tests that use the older pattern, then delete the fallback.

---

## PR #494 review pass (2026-07-02, same session)

The review bot caught a critical bug and three lower-severity issues. All addressed in the same PR before merge:

### CRITICAL — `pending_refresh` never cleared on successful auto-push

The initial implementation flipped `pending_refresh=TRUE` in `MarkPodIdentityTransition` and fired the push, but never called `MarkAgentReloaded` on success — so the `AgentReloadBanner` would appear permanently after every recreation, arguably worse than the pre-fix state (no banner at all).

**Fix:** `MarkPodIdentityTransition` now returns the DB-clock `last_credential_changed_at` timestamp it wrote. `runAutoPush` captures it, and on success calls the new `ClearPendingRefreshAfterAutoPush(ctx, workspaceID, priorChangedAt)` DB method, which wraps `MarkAgentReloaded` in a self-contained transaction. The optimistic-concurrency check (`currentChangedAt.After(priorChangedAt)`) correctly leaves `pending_refresh=TRUE` if a new credential was staged during the push window (banner re-appears for the fresh change) or clears it if not (banner disappears within a poll cycle).

**Tests added:**
- `TestPodIdentity_TransitionSuccessClearsPendingRefresh` — the load-bearing regression test with priorChangedAt round-trip assertion.
- `TestPodIdentity_TransitionSuccessWithClearFailureLogsWarning` — Clear failure doesn't panic; pod already got the secrets so it's UX-only.
- `TestPodIdentity_ClearPendingRefresh_Success` + `_KeepsFlagOnMidPushBind` — DB-level tests verifying the SQL transitions.
- Existing `TestPodIdentity_TransitionPushFailureLeavesPendingRefreshTrue` now also asserts `clearCalls == 0` on failure.

Adversarial validation: neutering the Clear call fails `_TransitionSuccessClearsPendingRefresh` on both the count assertion and the lastCall==="clear" assertion.

### MEDIUM — Metric double-counted user-initiated SetBindings pushes

The shared `agentpush.Service` was constructed with `WithMetricsHook(recordAutoPushOutcome)`, so every SetBindings/ReloadSecrets user-initiated push ALSO incremented `api_secret_auto_push_total`. Meanwhile `doReload` used a separately-constructed pusher without the hook, so it didn't increment. Inconsistent AND misleading: the metric's Help text says "pod-identity transitions on workspace status polls."

**Fix:** Removed `WithMetricsHook` from the shared pusher. The metric now lives on `wsAgentPusherAdapter.Push` (the workspace-service seam) — emitted only from the auto-push code path. Added a `classifyPushOutcome(err)` helper that maps agentpush errors to the four outcome labels: `success`, `no_pod`, `inject_failed`, `reload_failed`.

**Tests added (`api/internal/app/auto_push_metrics_test.go`):**
- `TestClassifyPushOutcome` (table-driven, 6 cases): all outcome mappings including wrapped errors.
- `TestWsAgentPusherAdapter_EmitsMetricOnEveryPush`: adapter increments correct label per call.
- `TestSharedPusher_DoesNotEmitAutoPushMetric`: proves calling `shared.Push` directly (the SetBindings/ReloadSecrets path) does NOT increment the auto-push counter.

### LOW / tech debt — `doReload` constructed a new agentpush.Service per call

`doReload` was retained as a compatibility shim (with a `literalInjector` for tests that pre-build payloads). Every call to `ReloadSecrets` constructed a fresh `agentpush.Service` with a fresh `http.Client` — defeating keep-alive for no benefit.

**Fix:** Deleted `doReload`, `literalInjector`, and `buildPusherOpts`. `ReloadSecrets` now calls `h.getPusher().Push(ctx, ...)` (the cached shared pusher, same instance used by SetBindings). The old `TestDoReload_EvictsModelCache` was removed — the equivalent invariant is covered by `TestPush_SuccessEvictsModelCache` in the agentpush package tests.

### Router auth-wiring test

Router closure at `router.go:1031` extracts `sessionID` from gin key `"sessionID"` and `jwt_signing_key` from gin key `"jwt_signing_key"` then calls `agentpush.WithAuth(ctx, sessionID, matchedKey)`. The bot correctly flagged this had no test — a key-name mismatch would silently degrade to skip-with-audit.

**Test added:** `TestGetStatusRoute_AttachesAgentpushAuthContext` verifies the exact key names round-trip; `TestGetStatusRoute_NoAuthValues_ContextStillCarriesZeros` locks in the empty-auth fallback. Adversarial-validated by swapping key names to `"session_id"`/`"signing_key"` — test fails on the empty signing-key assertion.

### Integration + regression tests

Added `workspace_pod_recreation_integration_test.go` (in the `workspace_test` package, so it exercises the real workspace.Service through its public API):

- `TestIntegration_PodRecreationTriggersFullAutoPush`: fake DB tracker + REAL agentpush.Service + REAL InjectSecrets shim + mock agentd via httptest. Asserts:
  1. Mock agentd received exactly one POST /v1/reload-secrets.
  2. The payload contained the user-DEK entry (`OPENAI_KEY`) — the exact material that was missing before #494.
  3. InjectSecrets saw the sessionID + signing key from the ctx (proving the router → service → pusher auth handoff works).
  4. Transition was recorded and Clear was called (proving the full state-machine cycle).

- `TestIntegration_PodRecreationFailureLeavesPendingRefresh`: same wiring but mock agentd returns 500. Asserts transition happened, Clear did NOT, banner-UX contract preserved.

Adversarial-validated by neutering the `agentpush.WithAuth` call in the test — the `sawSigningKey` assertion fails.

### Stale comment + duplicate type-assert cleanup

- Removed the "Wire below" stale comment at `app.go:557-558`.
- Reduced the pod-recreation wiring surface: `SetSecretPusher` + `SetPodIdentityTracker` now live in one shared type-assert block right after `agentPusher` is constructed. A second `svc.Workspace.(*workspace.Service)` type-assert still exists ~130 lines later for the unrelated credential/secret-provisioner + org-store wiring, which is a legitimate separation of concerns (that wiring block runs after other objects it depends on are constructed).

### Edge case: `GetLastSeenPodIdentity` with NULL start_time

The bot flagged an untested edge case: a row with `last_seen_pod_name='pod-x'` but `last_seen_pod_start_time=NULL` (e.g. inherited from `MarkCredentialChanged` before #494). COALESCE substitutes epoch; the Go code checks `Unix()==0` to zero it. Without this normalization, the initial-observation branch (`priorName == "" && priorStart.IsZero()`) would silently be false and every workspace's first post-deploy status poll would trigger a spurious transition.

**Test added:** `TestPodIdentity_GetTreatsEpochStartTimeAsZeroTime` locks in the normalization.

### PR #494 review pass 2 (approval)

The bot's second review approved the PR with three LOW-severity findings, all addressed in the same PR before merge:

- **Stale comment on `SetSecretPusher`** — said metric emission was the pusher's responsibility (via WithMetricsHook); reality post-fix is that emission lives on the workspace-side adapter. Comment corrected.
- **Stale comment on `RecordSecretAutoPush`** — claimed workspace.Service and agentpush.Service call it directly; reality is only the adapter's callback does. Comment corrected.
- **Double `InjectSecrets` call in `ReloadSecrets`** — the endpoint called `h.svc.InjectSecrets` for typed-error mapping, then `h.getPusher().Push` which internally invoked `InjectSecrets` again. Fixed by unwrapping the pusher's `"inject secrets: %w"` error and routing the inner error through `handleSecretError` on the sole Push call.
- **Worklog "consolidation" wording** overstated what was done — corrected to accurately describe the layering (two type-asserts remain because they wire different concerns at different points in the wiring order).
