# Worklog: boot-time user-DEK secret delivery (design 0045)

**Date:** 2026-07-06
**Session:** Fix the "workspace stuck with no `GH_TOKEN` after pod recreate" incident (chat.safespaces.dev/chat/e367ef82-… stuck; a127833a-… stuck; 890fad31-… stuck; four workspaces impacted). Two root causes identified and fixed. Full design in `design/0045_2026-07-06_boot-time-user-dek-delivery.md`.

**Status:** Complete. Ready for PR.

## Summary of incident

On 2026-07-06, four workspace pods were stuck in `Init:0/2` after control-plane node `cp-02` came up without its `NoSchedule` taint (three workspaces' PVCs failed to attach because `cp-02` doesn't run the Longhorn CSI driver). After the taint was restored and pods rescheduled to `worker-02`, three of the four still had opencode running without `GH_TOKEN` in env for 20+ minutes. Manual `pkill opencode` in each pod resolved.

## Root causes (validated against source)

### RC1 — user-DEK secrets delivered asynchronously after pod boot

The init container's `pod-bootstrap` call uses `SecretService.InjectSessionlessSecrets`, which explicitly SKIPS user-DEK bindings. The pod boots with server-KEK creds only.

User-DEK creds arrive later via the auto-push path (controller polls agentd `/v1/healthz` every 15s → mirrors `UserCredsPresent` to CRD → API watcher sees false → `secretautopush` → `agentpush.Push` → agentd's `reloadSecretsHandler` → materialize + write cache → session-aware restart).

Best case: ~30 seconds. Worst case: never (RC2).

### RC2 — session-aware restart wrongly defers on cold boot

`trackerHasBusyOrUnknown` (`cmd/workspace-agentd/secrets.go:130`) treats "tracker empty + opencode has session records" as "sessions might be busy, defer". That's semantically wrong: `/session` returns a list of session RECORDS from `opencode.db`, not a busyness indicator. A workspace that has ever been used has records.

On cold boot, no one is actually doing work — the tracker is empty because nothing has happened, not because SSE is broken. The old code deferred the credential-application restart for up to `maxDefer=2h`.

Observed in production: `session-aware restart: deferring restart, session status unknown (tracker empty, opencode alive — SSE disconnected?) maxDefer=7200`.

## Design changes (5)

1. **`InjectSecretsForPodBootstrap`** in `pkg/secrets/injection.go`: new interface + method. Best-effort user-DEK unwrap via `KeyService.GetDEKForUser` (which walks jwt_sessions rows and retained signing keys). On success, delegates to `InjectSecrets` with `sessionID=jti`. On DEK-unavailable, degrades to `InjectSessionlessSecrets`.

2. **pod-bootstrap handler uses the new method** (`api/internal/handlers/pod_bootstrap.go`). One-line change plus interface update on `bootstrapInjector`.

3. **Materialize writes reload cache** (`cmd/workspace-agentd/secrets.go`). Init-container materialize persists the applied batch. Prevents a spurious auto-push per pod-recreate (agentd's `hasUserCreds` reads the cache; without the boot write, first healthz reports `UserCredsPresent=false` → API auto-pushes the same batch → wasteful opencode restart ~30s into pod life).

4. **`trackerHasBusyOrUnknown` semantic fix**. Empty tracker → not busy (was: probe `/session`, defer if records exist). SSE is the sole truth for busyness; records ≠ busy.

5. **`defaultMaxDefer` reduced 2h → 15m**. With Change 4, the defer path is now reached only when a session is *genuinely* busy. 15m covers legitimate long-running agentic turns with headroom; 2h was excessive.

## Stress testing

18 scenarios validated (see design/0045 § "Edge case matrix" + inline in-doc). One real bug was caught by stress testing that would have shipped:

**Bug caught: `hasUserCreds` semantic.** Initial removal of Change 3 as "redundant" was based on the container-OOM recovery reasoning. Stress test #12 revealed that `hasUserCreds` reads only the cache, not `secrets.json`. Without Change 3 seeding the cache, every pod recreate triggers a spurious auto-push → spurious opencode restart ~30s into pod life. Restored Change 3 with the correct rationale documented.

## Verification

- Build: `go build ./...` clean
- Tests: `go test -race` clean across cmd/workspace-agentd, pkg/secrets, api/internal/handlers (targeted subsets — full package runs have pre-existing timeouts unrelated to this change)
- New tests: 5 unit test cases in `pkg/secrets/pod_bootstrap_injector_test.go` (nil KeyService, no jwt_sessions, empty rows, unwrappable row, happy path); regression tests in `cmd/workspace-agentd/container_restart_test.go` (materialize writes cache, empty-batch no-op); updated tests in `session_aware_restart_test.go` reflecting the corrected empty-tracker semantics
- gofmt clean, `go vet` clean

## What's not in scope

- **Frontend "credentials pending" UI toast**: the boot-time race is eliminated; runtime defer window drops from 2h to 15m. If users still hit a legitimate defer, that's a real busy session — surface as UI state in a follow-up if needed.
- **agentd → API `agentd-ready` endpoint**: rejected in design phase. With pod-bootstrap now delivering user-DEK, the controller-poll latency is no longer on the critical path for cold boot.
- **Describe-workspace-health API**: legitimate observability follow-up, out of scope here.
- **`credentialsPendingSince` annotation cleanup**: may become stale on workspaces created before this PR; harmless (annotation is display-only, auto-push filters on `UserCredsPresent` not the annotation).

## Files touched

Production code:
- `pkg/secrets/injection.go` (+80 LoC: new interface, new method)
- `api/internal/handlers/pod_bootstrap.go` (interface + one-line handler call)
- `cmd/workspace-agentd/secrets.go` (Change 3, Change 4, Change 5)

Tests:
- `pkg/secrets/pod_bootstrap_injector_test.go` (new file, 5 test cases)
- `api/internal/handlers/pod_bootstrap_test.go` (rename method on fake)
- `api/internal/handlers/pod_bootstrap_e2e_test.go` (comment update)
- `cmd/workspace-agentd/session_aware_restart_test.go` (updated empty-tracker tests, comment update)
- `cmd/workspace-agentd/container_restart_test.go` (new tests for Change 3)

Docs:
- `design/0045_2026-07-06_boot-time-user-dek-delivery.md` (new)
- `worklogs/NNNN_2026-07-06_boot-time-user-dek-delivery.md` (this file)
