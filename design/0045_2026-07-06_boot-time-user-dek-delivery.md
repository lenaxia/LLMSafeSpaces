# Boot-time user-DEK secret delivery

**Author:** ops (via investigation of the "workspace stuck without GH_TOKEN" incident 2026-07-06)
**Status:** Approved
**Related:** worklog 0591 (secretautopush service), worklog 0371 (session-aware restart)

## Problem

When a workspace pod is recreated (`restartGeneration++`, node reboot, OOM
outside the container-restart replay window, or first-time pod boot after a
long suspend), user-DEK-encrypted secrets (env-secrets like `GH_TOKEN`, SSH
keys, user-owned LLM providers) can take between 30 seconds and 2 hours to
land in the `opencode serve` process's environment.

Observed incident (2026-07-06): four workspaces recreated together after
a node-taint fix. Three had `GH_TOKEN` never reach opencode until manual
intervention (`pkill opencode`). Root cause was two separate but compounding
bugs on the credential-delivery hot path.

## Root causes (validated against source)

### RC1 — user-DEK secrets are delivered asynchronously after pod boot

The init container's `pod-bootstrap` call (`api/internal/handlers/pod_bootstrap.go`)
uses `SecretService.InjectSessionlessSecrets`, which explicitly **skips**
user-DEK bindings (`pkg/secrets/injection.go:123`). The pod boots with
server-KEK creds only.

User-DEK creds arrive later via the auto-push path:

```
controller polls agentd /v1/healthz every 15s (healthCheckInterval)
  → writes UserCredsPresent=false to Workspace CRD status
  → API watcher sees the transition, fires secretautopush.OnWorkspaceUpdate
  → GetDEKForUser(userID) unwraps DEK from any active jwt_sessions row
  → agentpush.Push POSTs to agentd http://podIP:4097/v1/reload-secrets
  → agentd's reloadSecretsHandler materializes and writes secrets-env
  → schedules opencode restart to source the new env
```

Best case: ~30 seconds (dominated by the 15s controller poll).
Worst case: never, if the session-aware restart defer triggers (RC2).

### RC2 — session-aware restart wrongly defers on cold boot

`trackerHasBusyOrUnknown` (`cmd/workspace-agentd/secrets.go:130`) treats
"tracker empty + opencode has session records" as "sessions might be busy,
defer restart". This is semantically wrong:

- The tracker is populated by live `session.status` SSE events from opencode
- `/session` returns a list of session records from `opencode.db` — session
  *records*, not *busyness*. Any workspace that has ever been used has
  records
- On cold boot, no one is actually doing work — the tracker is empty
  because nothing has happened, not because SSE is broken

The defer duration is bounded by `defaultMaxDefer = 2 * time.Hour`. So a
credential update on a cold-booted workspace is deferred for up to 2h,
during which opencode runs with stale env. Observed for `a127833a` in
the 2026-07-06 incident: `session-aware restart: deferring restart,
session status unknown (tracker empty, opencode alive — SSE disconnected?)`.

## Design principles

1. **Do the credential delivery synchronously in the init container,
   not asynchronously via the auto-push path.** The auto-push path exists
   for runtime credential *changes* and should not be the primary
   delivery mechanism for boot-time secrets.
2. **The SSE session tracker is the only truth source for busyness.**
   `/session` returning non-empty is not evidence of busyness; it is
   evidence of history.
3. **Keep the auto-push path as-is.** It is correct for runtime
   credential changes and is the fallback for workspaces whose DEK is
   temporarily unwrappable at boot time.

## Design

### Change 1 — new `InjectSecretsForPodBootstrap` in `pkg/secrets/injection.go`

Add a new method to `SecretService` that is the pod-bootstrap-path
variant of `InjectSecrets`. Behavior:

- Attempts DEK unwrap via `KeyService.GetDEKForUser(userID)`, which walks
  all non-expired rows in `jwt_sessions` for the workspace owner and tries
  each with every retained signing key
- On successful unwrap: `matchedSigningKey` set; user-DEK bindings decrypt
  via the standard `decryptBinding` path
- On unwrap failure (no active jwt_sessions, none unwrappable): degrades
  to `InjectSessionlessSecrets` behavior — user-DEK bindings audited-and-
  skipped, server-KEK bindings returned. Pod boots with reduced set; the
  existing auto-push flow will still fire when the user next logs in

```go
// InjectSecretsForPodBootstrap is the pod-bootstrap-path variant of
// InjectSecrets. See design/0045_boot-time-user-dek-delivery.md.
//
// Best-effort: attempts to unwrap the workspace owner's DEK via
// GetDEKForUser (which iterates jwt_sessions rows and the enumerator's
// retained signing keys). On success, returns full user-DEK secrets
// alongside server-KEK secrets — as if the user had made a
// JWT-authenticated request via InjectSecrets. On DEK-unavailable, falls
// back to InjectSessionlessSecrets behavior (user-DEK bindings audited-
// and-skipped, server-KEK-only payload returned).
//
// Trust model: caller (pod-bootstrap handler) has already authenticated
// the workspace SA token and confirmed request is on behalf of workspace X.
// The workspace CRD lists X's owner as the principal whose DEK is fetched.
// This is not privilege escalation: the pod would receive these same
// secrets via reload-secrets anyway (see design section "Threat model").
func (s *SecretService) InjectSecretsForPodBootstrap(
    ctx context.Context, userID, workspaceID string,
) ([]byte, error)
```

Add to the `SessionlessSecretInjector` interface (single caller extension —
no split needed).

### Change 2 — pod-bootstrap handler uses the new method

`api/internal/handlers/pod_bootstrap.go:203`:

```go
// Before
secretsJSON, err := h.injector.InjectSessionlessSecrets(c.Request.Context(), ws.UserID, req.WorkspaceID)

// After
secretsJSON, err := h.injector.InjectSecretsForPodBootstrap(c.Request.Context(), ws.UserID, req.WorkspaceID)
```

One-line change plus the interface update on `bootstrapInjector`.

### Change 3 — materialize writes reload cache

*(Note: this change was initially proposed for container-OOM recovery,
briefly rejected during stress testing round 1 as redundant with the
`/sandbox-cfg` emptyDir persistence, then restored during stress testing
round 2 for a different — and load-bearing — reason: preventing a
spurious auto-push per pod-recreate.)*

The API-side `secretautopush` service fires whenever the workspace
watcher observes `Status.UserCredsPresent=false` on an Active workspace.
Agentd populates that field via its `hasUserCreds` probe
(`cmd/workspace-agentd/healthz.go:83`), which returns true iff the
reload cache file at `/sandbox-runtime/last-reload-secrets.json` exists
and contains a non-empty batch.

Without Change 3, the reload cache is written only by
`reloadSecretsHandler` (`secrets.go:723`) — after a runtime reload.
On a fresh pod boot:

1. Init container's materialize applies user-DEK secrets (Change 1
   delivered them via pod-bootstrap). **Cache is NOT written.**
2. Main container starts, agentd's first healthz reports
   `UserCredsPresent=false` (cache absent).
3. Controller polls, propagates to CRD status.
4. Watcher fires `secretautopush.OnWorkspaceUpdate`. Filter passes.
5. Auto-push applies the same batch that pod-bootstrap already
   delivered.
6. Reload handler triggers session-aware restart. With Change 4
   (empty tracker → immediate restart), opencode restarts ~30s
   into pod life.

**Wasted work per pod-recreate: one opencode restart + several disk
writes.** Not a correctness issue (idempotent re-application), but
observable as a ~5s user-facing latency spike shortly after pod-recreate.

Change 3 closes the loop: materialize writes the cache with the applied
batch. `hasUserCreds` sees a populated cache. Reports true. Watcher
observes true. `secretautopush` emits `skipped_ucp_true`. No spurious
restart.

The cache write is non-fatal on failure (empty-path guard for tests,
empty-batch guard to avoid writing a spurious empty file). On write
failure, agentd degrades to the pre-Change-3 behavior (one spurious
auto-push post-boot) — no user-visible impact beyond the wasted restart.

### Change 4 — `trackerHasBusyOrUnknown` semantic fix

`cmd/workspace-agentd/secrets.go:130-145`. Replace:

```go
// tracker is empty — probe opencode to decide (C2b).
if lister == nil {
    return false
}
probeCtx, cancel := context.WithTimeout(ctx, sessionListerProbeTimeout)
defer cancel()
ids := lister(probeCtx)
// nil = unreachable → restart (nothing to lose).
// non-nil + len>0 = opencode alive with sessions → defer (might be busy).
// non-nil + len==0 = opencode alive, no sessions → restart (nothing to lose).
return len(ids) > 0
```

with:

```go
// Tracker is empty — the SSE stream (our only truth source for session
// busyness) has not observed any busy session on this agentd's connection.
// The /session count is a list of session records from opencode.db, not
// a busyness signal: a session that was busy 8 hours ago but idle now
// still appears in /session. Historically this branch probed /session and
// deferred if any records existed, which caused cold-boot credential
// updates to defer for the full maxDefer window (up to 2h) — see
// design/0045_boot-time-user-dek-delivery.md RC2.
//
// The remaining edge case is the brief window between agentd start and
// SSE reconnect (~2s, bounded by session_tracker.go's backoff): if a
// session is genuinely busy in that window, we lose the mid-turn
// protection. That trade is acceptable because (a) the window is
// bounded and short, (b) the previous behavior silently held stale
// credentials for hours in vastly more common cases, and (c) even the
// old branch was best-effort — SSE can disconnect at any time.
return false
```

Remove the lister-probe call from the empty-tracker path. The `lister`
parameter is retained on `trackerHasBusyOrUnknown`'s signature for the
non-empty-tracker path (`pruneFromLister` in the deferred goroutine),
so no interface change is needed elsewhere.

### Change 5 — reduce `defaultMaxDefer` from 2h to 15m

`cmd/workspace-agentd/secrets.go:78`.

Rationale for 15 minutes: covers legitimate long-running agentic turns
(reasoning models, slow tool calls, chained multi-step workflows) with
generous headroom. 2 hours was chosen to cover extreme cases that in
practice indicate a stuck session; 15 minutes is long enough that a real
in-progress turn completes cleanly and short enough that a credential
update reaches opencode in a bounded time even if the tracker misreports
busyness.

## Threat model

### Change 1's expansion of pod-bootstrap authority

**Before:** workspace SA token authorizes pod-bootstrap to return
server-KEK-decryptable creds (admin/org LLM providers, sessionless
bindings) for that specific workspace. User-DEK bindings are audited
and skipped.

**After:** workspace SA token additionally authorizes pod-bootstrap to
attempt DEK unwrap for that workspace's owner and, on success, return
user-DEK bindings.

**Is this a privilege escalation?**

No. The pod already receives user-DEK plaintext today:

1. The `secretautopush` flow POSTs user-DEK creds to
   `http://podIP:4097/v1/reload-secrets` — plaintext — within ~30s of
   pod boot. The pod (and any process inside it, including an escaped
   container) can read these from `secrets-env`.
2. The workspace SA token is projected into the pod. Anyone with code
   execution in that pod (which is the *whole point* of a workspace —
   the user runs code there) can already read the SA token and call
   pod-bootstrap.
3. The trust boundary for user-DEK plaintext is "is this workspace X's
   pod?", not "is this a live user session?". The change formalizes what
   already happens; it does not widen who can obtain the plaintext.

**Container escape scenario:** an attacker escapes the workspace
container (gVisor sandbox breach — a Sev-1 in its own right) and reads
the SA token. Today they get server-KEK immediately + user-DEK ~30s
later. After the change, they get both immediately. The delta is
seconds, not scope.

**Compromised API service scenario:** unchanged. A compromised API
already has the KEK, the DEKs (via `GetDEK`), and both cred stores. It
does not need pod-bootstrap to reach any secret.

### Change 4's remaining protection

The session-aware defer still protects against restarts during a
genuinely busy session (tracker has SSE-observed busy entries). The
protection is unchanged in that case. The only path removed is the
"empty tracker but /session says records exist" path, which was
provably wrong (see RC2).

## Edge case matrix

| # | Scenario | Today | After fix |
|---|----------|-------|-----------|
| 1 | Fresh pod boot, owner has active JWT session | user-DEK arrives 30s–2h post-boot | user-DEK in pod-bootstrap response, opencode env has it from PID 1 |
| 2 | Fresh pod boot, owner has no active JWT session (>30d idle) | user must log in, then auto-push | Same — bootstrap DEK unwrap fails, falls back to server-KEK, awaits future auto-push |
| 3 | Fresh pod boot, workspace has no user-DEK bindings | no-op | no-op (no user-DEK to fetch) |
| 4 | Fresh pod boot, PG down when init container calls | init writes empty, main boots without creds, auto-push retries | Same; DEK fetch is another DB call in the same window |
| 5 | Fresh pod boot, Redis cache warm | GetDEK hit | GetDEKForUser walks jwt_sessions rows, Redis hit for cached DEK |
| 6 | Runtime credential change (user rotates GH_TOKEN) | reload-secrets pushes, defer logic applies | Unchanged — runtime path stays as-is; but Change 4 makes defer no longer misfire on empty tracker |
| 7 | User adds new binding while pod is running | reload-secrets fires | Unchanged |
| 8 | Pod restart via `restartGeneration++`, owner offline (JWT still active) | 30s–2h latency (RC1+RC2) | user-DEK arrives instantly via pod-bootstrap |
| 9 | Pod restart, owner's JWT expired between old pod boot and new pod | never (until re-login) | Same — bootstrap DEK unwrap fails, awaits future auto-push |
| 10 | Two workspaces owned by same user, both recreated simultaneously | two concurrent auto-pushes, share Redis cache | two concurrent pod-bootstraps, share Redis cache. Same profile |
| 11 | pod-bootstrap succeeds partially (one binding decrypt-failed) | N/A | Return what succeeded, audit failures via existing `credential_decrypt_failed` path. Auto-push retries later |
| 12 | Init container succeeds with user-DEK, main container OOMs before auto-push fires | secrets.json (in `/sandbox-cfg` emptyDir) survives container restart with the user-DEK content from step 1. Materialize on restart replays it. Change 3's cache write means hasUserCreds returns true on the recovery boot too. | User-DEK survives container restart; no spurious auto-push on recovery boot |
| 13 | Auto-push has already fired for this pod, then container OOMs | Reload cache replays | Unchanged |
| 14 | agentd's SSE reconnect races a genuine busy session by <2s | Correct defer (protects mid-turn) | Change 4 changes this to "restart" — brief regression window (~2s), acceptable trade for eliminating the 2h wrong-defer |
| 15 | Attacker with workspace SA token calls pod-bootstrap twice | Two identical responses (idempotent) | Same, plus DEK unwrap on each; Redis absorbs the second call |

## Rollout

Single PR containing all five changes. No feature flags — behavior is
strictly better in every scenario except #14 (brief SSE-reconnect race).

Rollout order per PR review:
1. Change 1 (new inject method) + Change 2 (bootstrap uses it) — the
   core boot-time delivery
2. Change 3 (materialize writes reload cache) — closes the spurious
   auto-push loop
3. Change 4 (tracker semantic fix) + Change 5 (maxDefer 15m) — closes
   the runtime-defer bug (RC2)

Tests:
- Unit: new inject method's success + degrade paths, bootstrap handler's
  use of the new method, materialize cache write, trackerHasBusyOrUnknown
  new semantics, session-aware restart no longer defers with empty tracker
- E2E: `tests/` — create workspace with `GH_TOKEN` binding, force pod
  recreate via `restartGeneration++`, assert `GH_TOKEN` in opencode env
  within 15 seconds of pod Ready
- Regression: existing `session_aware_restart_test.go` tests that codified
  the wrong behavior are updated to reflect the fix

## Non-goals for this PR

- **Frontend "credentials pending" toast:** the boot-time race is
  eliminated; the runtime defer window drops from 2h to 15m. If users
  still hit a legitimate defer, that's a real busy session — surface
  as UI state in a follow-up if needed.
- **New agentd → API endpoint (agentd-ready signal):** rejected. The
  15s controller poll was a real latency source, but with pod-bootstrap
  now delivering user-DEK, the poll no longer matters for cold boot.
  Runtime credential changes still go through the (working) auto-push
  path.
- **Describe-workspace-health API:** a legitimate follow-up for
  observability, but out of scope here. The fix should land first;
  observability improvements are a separate concern.
- **`credentialsPendingSince` annotation cleanup:** may become stale on
  workspaces created before this PR; harmless (annotation is display-only,
  auto-push filters on `UserCredsPresent` not the annotation). Follow-up
  if it bothers us.

## Assumptions and validation

Per README-LLM rule 7:

| # | Assumption | Validation |
|---|------------|------------|
| A1 | `secrets-env` is sourced by the main container entrypoint before opencode spawns | Verified: `runtimes/base/tools/entrypoints/entrypoint-opencode.sh:10` `source /sandbox-runtime/secrets-env`, `defaultOpencodeCmdFactory` uses `buildEnvFrom(SecretsEnvPath)` at every spawn |
| A2 | Init container's `agentd materialize` writes env-secrets to `secrets-env` | Verified: `pkg/agentd/secrets/secrets.go:497, 587` appendFile to `SecretsEnvPath` for env-secret and api-key types |
| A3 | `GetDEKForUser` works without a live browser session — an active DB row in `jwt_sessions` with an unwrappable `wrapped_dek` suffices | Verified: `pkg/secrets/key_service.go:638`, `jwt_session_store.go:200` (`WHERE expires_at > NOW()` — DB retention, not live-session state) |
| A4 | The tracker is not populated by "session exists in opencode.db" events, only by SSE-observed `session.status` / `session.next.step.ended` events | Verified: `cmd/workspace-agentd/session_tracker.go:237-286` — only two event types populate the map, both requiring live SSE emit |
| A5 | The 15s controller poll interval is the delay source for auto-push (not other bottlenecks) | Verified: `controller/internal/workspace/health.go:46` `healthCheckInterval = 15 * time.Second`; observed in the 2026-07-06 incident logs |
| A6 | Workspace SA tokens can be created only by K8s for pods with the matching SA; the API validates via TokenReview | Verified: `api/internal/handlers/pod_bootstrap.go:165-191` (TokenReview + SA name pattern + namespace check) |
| A7 | The reload cache is on tmpfs (survives container restart, wiped on pod death) — no persistent-storage cost | Verified: `pkg/agentd/types.go:22`, mount is `emptyDir { medium: Memory }` in the workspace pod spec |
| A8 | Auto-push idempotency after Change 1: if pod-bootstrap already delivered user-DEK, subsequent auto-push should no-op | Verified: `secretautopush/service.go:166-169` filter on `UserCredsPresent=true` → `skipped_ucp_true` outcome. Agentd reports `UserCredsPresent=true` when materialize finished; materialize runs in the init container, so by first `/v1/healthz` call agentd already reports true |

## Adversarial questions

Q: What if `GetDEKForUser` becomes slow (large `jwt_sessions` table,
many signing keys)? Does it block pod boot?

A: `GetDEKForUser` is bounded — `jwtSessionUserLookupLimit = 5` limits
rows, signing keys are typically 1-3 (active + retained). Redis cache
hit is typical. Sub-100ms in the common case, sub-1s in the worst.
`pod-bootstrap`'s existing 10s HTTP timeout in the init container
(`bootstrap.go:121`) absorbs this. Failure = graceful degrade to
server-KEK only (`bootstrap.go:88`).

Q: What if a signing key rotates out of retention between pod boots?

A: Same as today: user must log in with the current key. `GetDEKForUser`
returns `ErrDEKUnavailable`, pod-bootstrap degrades. Auto-push will
also fail for the same reason. Not a regression.

Q: Can this create a thundering herd on API restart?

A: No. `pod-bootstrap` fires once per pod init, not on watcher events.
Same call frequency as today; per-call cost adds one DEK unwrap
(bounded per A9 above).

Q: What happens if the API cache is cold and the DB is momentarily
overloaded?

A: `pod-bootstrap` degrades to server-KEK on any error. The init container
never blocks pod boot on API failures (existing invariant from Epic 35).
When the API recovers, controller polls resume, healthz reports
`UserCredsPresent=false`, auto-push fires normally.

Q: Change 4 removes the empty-tracker probe. Is there a scenario where
the tracker is legitimately empty but a session is genuinely busy?

A: Yes — the ~2s window between agentd start and SSE reconnect. This is
the acceptable trade documented in Change 4. If a credential update
lands in that window (extremely narrow), we restart what might be a
newly-busy session. The alternative (pre-fix) was silently holding
stale credentials for hours in the vastly more common cold-boot case.
Net: strictly better on aggregate.
