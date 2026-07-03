# Worklog: watcher-driven auto-push replaces #494's pod-identity detection

**Date:** 2026-07-03
**Session:** Replace #494's pod-identity-transition detection with a watcher-driven auto-push using agentd's own signal (via a new CRD status field) and the DEK-by-user primitive from #497. Deletes #494's DB tracker + migration 000005 columns.

**Status:** Complete. Ready for PR.

## Motivation

PR #494 solved the "silent pod recreation → user's ~/.ssh empty" symptom by hooking user-DEK auto-push to GET /workspaces/:id/status. That approach depends on the frontend polling /status while the transition is observable, which fails in real deployments (verified on 890fad31 immediately after v87 deploy): the pod recreated silently, /status was polled zero times for ~40 minutes because the user was on the chat page (which polls /sessions, not /status), and the CRD's pod-identity tuple in DB was stale so the transition-detection would have fired anyway had /status been polled.

The root architectural issue was that #494 relied on a **request-context-supplied DEK**. #497 introduced `KeyService.GetDEKForUser` which retrieves the user's DEK without a live request. That primitive is now the foundation for the detector to run from anywhere — including the K8s watcher, which fires reliably on every CRD update regardless of what endpoint the frontend is polling.

## Design

Three-tier scrape-and-mirror architecture:

1. **Agentd** (base image): computes `userCredsPresent` from the `last-reload-secrets.json` cache. Non-empty cache → true, absent/empty/corrupt → false (safe-default; a "corrupt" false makes the API's push re-materialize and re-cache). Exposed as a new field on the existing `/v1/healthz` response.

2. **Controller**: existing `checkAgentHealth` (15s cadence) mirrors `healthz.userCredsPresent` onto a new tri-state field `workspace.status.userCredsPresent *bool`. Cleared to nil on unreachable/undecodable/agent-unhealthy so a stale "true" from a previous pod doesn't suppress the API's push after recreation.

3. **API workspace watcher**: new `onWorkspaceUpdate` callback fires on every Added/Modified event for any Workspace CRD. A new `secretautopush.Service` filters:
   - Phase == Active
   - `Status.UserCredsPresent` is non-nil AND explicitly false (nil is "not scraped yet" — treat as unknown)
   - Workspace has at least one row in `user_secret_bindings`
   - `KeyService.GetDEKForUser` returns a DEK (a valid active JWT session exists for the workspace's owner)
   
   If all conditions match, fire a fire-and-forget push via the existing `agentpush.Service` shared with SetBindings/ReloadSecrets. Per-workspace in-flight lock prevents duplicate pushes when the watcher emits multiple Modified events during the same recreation cycle.

## What's deleted

- `workspace.Service.PodIdentityTracker` interface + `SetPodIdentityTracker` setter + `maybeAutoPushOnPodTransition` method + `runAutoPush` method.
- `workspace.Service.SecretPusher` interface + `SetSecretPusher` setter.
- `database.Service.GetLastSeenPodIdentity` + `UpsertLastSeenPodIdentity` + `MarkPodIdentityTransition` + `ClearPendingRefreshAfterAutoPush` methods.
- `workspace_agent_state.last_seen_pod_name` + `workspace_agent_state.last_seen_pod_start_time` columns (migration 000006 drops).
- Router `/status` handler's `agentpush.WithAuth` ctx-plumbing (was for the removed workspace.Service auto-push).
- Test files: `workspace_pod_identity_test.go`, `workspace_pod_recreation_integration_test.go`, `pod_identity_test.go`, `router_status_auto_context_test.go`.

## Coverage improvement over #494

#494 fired on `GET /status` when the pod-identity tuple changed. This covered:
- Pathway 3 (restartGeneration++ via user click) — only if user was on the workspace details page.
- Pathway 2 (resume from Suspended) — only if user was polling /status after the resume.

#494 missed (as documented in worklog 0590):
- Pathway 1 (fresh CreateWorkspace) — initial observation branch skipped the push.
- Pathway 4-5 (silent pod recreation) — /status not polled.
- Pathway 6 (container-only restart) — cache tmpfs survives, no push needed (correctly).
- Pathway 7 (node crash) — same as 4-5.
- Pathway 8 (PVC restore) — API state didn't know about the pod change.
- Pathway 9 (API state loss) — API forgot the identity tuple.

This PR's design covers all 9 pathways uniformly because the trigger is on-pod state (via agentd's `last-reload-secrets.json` presence) rather than API-side heuristics. The watcher fires on every Modified event, including the seed loop on API restart (pathway 9), so recovery is automatic.

## Files added

- `pkg/agentd/types.go` — `UserCredsPresent bool` on `HealthzResponse`.
- `cmd/workspace-agentd/healthz.go` — `hasUserCreds()` + healthz handler signature change (adds cache path param).
- `cmd/workspace-agentd/has_user_creds_test.go` — 5 tests: absent, empty, all 5 user-DEK types (llm-provider, ssh-key, secret-file, git-credential, env-secret), corrupt cache, healthz wire-format, healthz-liveness-not-blocked.
- `pkg/apis/llmsafespaces/v1/workspace_types.go` — `WorkspaceStatus.UserCredsPresent *bool`.
- `pkg/apis/llmsafespaces/v1/zz_generated.deepcopy.go` — pointer copy for the new field.
- `charts/llmsafespaces/crds/workspace.yaml` — CRD schema mirror.
- `controller/internal/workspace/health.go` — mirror healthz.UserCredsPresent → status.UserCredsPresent, clear on unreachable.
- `controller/internal/workspace/health_user_creds_test.go` — 3 tests: mirror both values, clear on unreachable, keep nil pre-scrape.
- `api/internal/services/workspace/watcher.go` — `WorkspaceUpdateCallback` type + `SetWorkspaceUpdateCallback` setter, fires from `handleWatchEvent` + the API-restart seed loop.
- `api/internal/services/secretautopush/service.go` — new package. `Service` with filter + in-flight lock + DEK fetch + push.
- `api/internal/services/secretautopush/service_test.go` — 8 tests including in-flight-lock, lock-release, DEK-unavailable, no-bindings, non-Active phases (table-driven 7 phases), UserCredsPresent=nil skip, UserCredsPresent=true skip.
- `api/internal/handlers/proxy.go` + `proxy_lifecycle.go` — plumb `workspaceUpdateCb` through to the watcher.
- `api/internal/app/secrets_adapters.go` — `bindingsCheckerAdapter` + `agentpushAuther`.
- `api/internal/app/app.go` — wire the whole thing.
- `pkg/secrets/key_service.go` — `GetDEKForUser` return signature changed from `(dek, err)` to `(dek, jti, err)` so caller can build auth ctx (from #497).

## Files removed / modified for deletion

- `api/internal/services/workspace/workspace_service.go` — removed `PodIdentityTracker` interface, `SecretPusher` interface, `SetPodIdentityTracker`, `SetSecretPusher`, `maybeAutoPushOnPodTransition`, `runAutoPush`.
- `api/internal/services/database/database.go` — removed 4 pod-identity methods.
- `api/internal/server/router.go` — removed agentpush.WithAuth plumbing from /status.
- Migrations 000006 up + down (drop + restore columns).
- Chart migrations 000006 (auto-mirrored).
- Deleted test files (see above).

## TDD + adversarial validation

Every load-bearing branch adversarial-validated:

- `hasUserCreds` returning always-true → 3 tests fail (absent/empty/corrupt).
- Controller mirror-neutered → phase-both tests fail.
- `secretautopush` phase filter neutered → 7 non-Active phase tests fail.
- `secretautopush` in-flight lock neutered → 5-concurrent test fails on double-fire.

## Design worklog trail

- #493 (upstream issue): symptom — secrets missing after pod recreation.
- #494 (merged 6a0dc526): pod-identity detection via workspace_agent_state. Solved the specific case where API had seen a prior pod. Missed pathways 1, 4-5, 7-9.
- #497 (merged a9ad2602): `KeyService.GetDEKForUser` primitive. Foundation for background auto-push.
- This PR: agentd signal + watcher-driven detection + delete #494's tracker + drop migration 000005 columns.
