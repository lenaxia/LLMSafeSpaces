# 0749 — False "Session was interrupted" banners: root cause + 5 fixes

**Date:** 2026-08-14
**Status:** Complete
**PR:** (this branch)

---

## Session Overview

User-reported: "Session was interrupted while waiting for your input" appears
frequently when the agent asks a question — killing live, answerable prompts.
A second symptom followed the most recent turn: a chat-UI error bubble after
the aborted session's next queued message.

Every finding below was validated against live cluster state (pod logs, the
opencode SQLite-backed session API inside the pod, API-server request logs)
before any fix was written.

---

## Live evidence (workspace `c86d71e0`, session `ses_000b6d9efffeo45DUT2m4dNQoF`)

| Time (UTC) | Evidence | Source |
|---|---|---|
| 07:58:31 | `relay injector: failed to fetch free models, skipping error="decode /provider: unexpected EOF"` — relay injection permanently skipped for pod lifetime | agentd pod log |
| 07:58:36 | `Failed to fetch models.dev cause=Timeout` — boot-time network transient | opencode log in pod |
| 08:21:00.213 | `POST .../sessions/ses_000b6d9efffeo45DUT2m4dNQoF/abort` from the browser | API server log |
| 08:21:00.842 | `ERROR message=process session.id=ses_000b6d9... error=Aborted` — the frontend auto-abort killed a LIVE run | opencode log in pod |
| (still true now) | `/permission` returns a live pending permission `per_fff6b0513001m7bOudywNU4VpR` (external_directory, /sandbox-cfg/*); last tool part `state.status=running` | opencode API queried in pod |
| 08:25:56 | `Failed to drain Session cause=ModelUnavailableError: Model unavailable: opencode/deepseek-v4-flash-free` — the chat-UI error after the next turn: the session used the free-tier model DIRECT because relay injection had been skipped | opencode log in pod |

The permission was **live in opencode's queue** when the frontend aborted the
session. The abort was false. The follow-on "failed to fetch" error is a
downstream consequence of the relay-injection skip.

---

## Root causes (all code-validated)

### F1 — Reconnect mode arms mid-session (`ChatPage.tsx`)

`isReconnectMode` armed by an effect on every `[isSessionBusy, localStreaming]`
transition. When the agent asks a question, the session stays busy while
`localStreaming` drops to false (60s send timeout / send completion) → the
effect arms reconnect mode mid-conversation, enabling the auto-abort against
the live prompt. No page refresh required.

### F2 — Auto-abort races the SSE input snapshot (`ChatPage.tsx`)

The abort effect fires as soon as history (with a running question/permission
tool) is loaded and `pendingPromptCount === 0`. It never waits for
`emitPendingInputRequests` (up to 5s fetch budget) to prove opencode's queue
is actually empty. History landing before the snapshot → live prompt killed.

### F3 — D9 marker commit wipes post-commit live prompts (`SessionActivityProvider.tsx`)

Input events were only staged while the workspace was uncommitted
(`!committedWsRef.has(ws)`). After the first `snapshot_complete`, re-emitted
live questions were NOT staged — so the next marker (fired on every ChatPage
mount opening workspace SSE and on every user-stream reconnect, which calls
`emitPendingInputRequests` for all Active workspaces) committed an empty
staged set and **wiped every pending prompt of that workspace** — even when
the pod fetch succeeded and had just re-emitted them. Wipe →
`pendingPromptCount === 0` → auto-abort (with F1/F2) → banner. This is the
"frequency" driver: any SSE reconnect or in-app chat navigation while a
question was pending destroyed it.

Additionally, a FAILED pod fetch (opencode mid-restart — exactly when
reconnects happen) still committed empty by design (D9 comment), wiping live
prompts on transient failures.

### F4 — Relay injector restart bypasses session-aware deferral (`main.go`)

`KillOpenCode: func() { deps.proc.restart() }` — an immediate kill. The
credential-reload path uses `makeSessionAwareRestartDecision` (defer while
busy, max 15 min). The relay injector killed opencode regardless of pending
questions; opencode's input queue is in-memory only, so the question died
while SQLite kept `toolState:"running"` → permanently busy session → the
exact state the auto-abort was built to detect.

### F5 — One transient fetch error permanently skips relay injection (`relay_injector.go`)

`fetchFreeModels` errors returned immediately (only empty catalog lists were
retried). Boot-time transients (observed: `decode /provider: unexpected EOF`
while models.dev was unreachable) skipped the relay for the pod's lifetime →
free-tier sessions route direct-to-Zen → `ModelUnavailableError` surfaced as
chat error bubbles.

---

## Fixes

| # | Fix | Files |
|---|---|---|
| F1 | Reconnect-mode activation only within a 15s window of mount/session-change or workspace-SSE reconnect; user send closes the window | `frontend/src/pages/ChatPage.tsx` |
| F2 | Auto-abort additionally requires a successful input snapshot (`ok:true`) received AFTER arming, plus a 1.5s dwell before firing | `ChatPage.tsx` |
| F3 | Server: `agent.input.snapshot_begin` opens a per-flight staging window; `snapshot_complete` carries `snapshot_ok`. Provider: events stage during a flight even post-commit; `ok:false` keeps existing pending state (no empty-commit wipe). New `useWorkspaceInputSnapshot` hook exposes `{ok, at}` | `api/internal/handlers/proxy_input.go`, `api/internal/types/sse_event.go`, `SessionActivityProvider.tsx` |
| F4 | `KillOpenCode` routed through `makeSessionAwareRestartDecision` via new `relayKillFunc` seam | `cmd/workspace-agentd/main.go` |
| F5 | Transient fetch errors retried within the fetch deadline (5s default interval, 30s default deadline; configurable for tests) | `cmd/workspace-agentd/relay_injector.go` |

Backwards compatibility: markers without `snapshot_ok` (older API replicas
during rollout) are treated as ok — legacy commit semantics preserved.

---

## Tests

- Go: `TestEmitPendingInputRequests_BeginAndOKMarkerOnSuccess`,
  `..._MarkerOKFalseOnBackendError`, `..._MarkerOKFalseOnK8sFailure`
  (proxy_input_test.go); `TestRelayKillFunc_{IdleTracker,BusyTracker,NilProc}`
  (session_aware_restart_test.go);
  `TestStartRelayInjector_RetriesTransientFetchErrors`,
  `..._FetchErrorDeadlineExhausted_Skips` (relay_injector_test.go).
  Full `api/internal/handlers` + `cmd/workspace-agentd` + `eventbroker`
  suites pass.
- Frontend: 6 new provider tests (D10 describe) + 4 new ChatPage gating tests
  (no-abort-before-snapshot, no-abort-on-ok:false, no-abort-on-stale-snapshot,
  no-arming-after-window-closed) + 3 updated abort tests. Full vitest suite:
  1636 passed.

## Known pre-existing issues (not touched)

`npm run lint` reports 35 errors in `frontend/src/api/workflows.ts`,
`src/components/workflows/*`, `src/pages/RunDetailPage.tsx`,
`src/pages/TriggersPage.tsx`, `frontend/tests/e2e/passkey.spec.ts` — all
pre-existing on main, in files outside this change's scope.

## Assumptions validated

| Assumption | Validation |
|---|---|
| Session stays busy while a question/permission is pending | Worklog 0185 + live pod: busy + running tool + live /permission entry |
| Marker commits happen mid-lifetime (not only post-reconnect) | `proxy_stream.go:76` fires `emitPendingInputRequests` on every workspace-SSE connect; `stream_user_events.go:293` on every user-stream (re)connect |
| Frontend abort fired while permission was live | API log abort at 08:21:00 + opencode log `error=Aborted` + permission still in queue afterwards |
| "failed to fetch" chat error = ModelUnavailableError from direct-to-Zen free tier | opencode log 08:25:56 + relay skip at 07:58:31 + session model `opencode/deepseek-v4-flash-free` |
| Parts lack timestamps in the normalized contract | `frontend/src/api/messages.ts` ContractMessage + `pkg/session` mapping |

---

## Review round 2 (PR #852 — REQUEST CHANGES → addressed)

All findings independently validated before fixing:

| ID | Finding (validated) | Resolution |
|---|---|---|
| C1 (critical) | `Adapter.ListPending` returned `(out, nil)` on transport errors and ≥500 — the adapter comment claimed the opposite. Production always wires the adapter, so `snapshot_ok:true` was emitted for failed fetches and the F2/F3 fixes were inert on the production path. | `ListPending` now returns typed `ErrPendingUnavailable` on transport/≥400(!404) failures; 404-not-implemented stays an authoritative empty. Interface doc codifies the contract. 4 new adapter tests (transport/5xx/partial-404/404) replace the test that codified the broken behavior; 2 new handler tests assert adapter-path `snapshot_ok` semantics. |
| C2 (high) | Two concurrent snapshot flights (workspace-SSE + user-stream connect on hard page load) shared one staging map: the second `snapshot_complete` committed an authoritative empty over the first's commit — live prompts wiped on the most common load path. | `snapshot_id` flight identifier on begin/complete (`WorkspaceSSEEvent.SnapshotID`); provider stages per-flight (`flightsRef: ws → flightId → entries`). 3 new interleaving tests (A/B ok, failed+ok both orders, superseded staging). |
| C3 | In-workspace session switch into a stuck session re-armed reconnect mode but no stream (re)connects → no fresh flight → abort gate starved forever (recovery regression vs pre-PR). | New `POST /workspaces/:id/input-snapshot` endpoint fires a flight on demand; ChatPage calls it when reconnect mode arms. Endpoint test asserts begin/complete share a flight ID. |
| C3-minor | `zap.Duration("deadline", cfg.FetchDeadline)` logged `0s` in production. | Logs the effective deadline. |
| C3-minor | `relayKillFunc` used rootCtx; reload path uses BgCtx — deferred restart not cancelled at shutdown, `bgWg` never drains. | `maybeStartRelayInjector(rootCtx, bgCtx, ...)`; kill decision uses bgCtx. |
| C3-minor | `outcome="success"` metric counted config applications before a possibly-deferred restart. | Documented in-code (metric counts applications; restart may defer ≤15min). |
| Test gap | Adapter-path `snapshot_ok` assertions missing; interleaved-flight coverage missing; wire-contract (omitempty `*bool`) unguarded; no e2e. | All added: 2 adapter-path handler tests, 3 interleaving tests, `TestSnapshotOK_WireContract` (asserts `"snapshot_ok":false` literally on the wire), Playwright e2e `stuck-session-abort.spec.ts` (live-question survival + answerable; genuine-stuck abort + banner). |
| Style | Dead eslint-disable; worklog manually numbered; collapsed test formatting. | Removed; renamed to `NNNN_` sentinel; formatted. |

Additional fix found while writing the e2e (validated by reproducing in the
spec): the abort dwell timer restarted on every history refetch (SSE
reconnect churn), deferring the abort indefinitely. Now anchored
(`abortDwellStartRef`) with only the REMAINING dwell scheduled. The
snapshot-freshness gate moved from per-arming `armedAt` (which the same
churn livelocked — each re-arm demanded a newer snapshot, perpetually
clearing the anchor) to a stable per-view `sessionMountedAt` — the
anti-stale property is preserved (a pre-mount snapshot could predate the
question) and the C3 on-demand flight covers the session-switch case.

### Deploy-window caveat (explicit)

Markers from old API replicas during a rolling deploy lack `snapshot_ok` and
are treated as ok — legacy (buggy) commit semantics persist through the
upgrade window. Transient and self-healing (any new-replica marker
reconciles), but the false-abort fix is only fully active once API replicas
are upgraded.

### Validation note

`TestValidateProbeBaseURL_PrivateRanges` performs live DNS resolution of
`ai.thekao.cloud` and fails in DNS-restricted sandboxes — environment-specific,
green in CI (full suite + race detector passed on this PR).

### CI round 3

- OpenAPI contract: new route documented in `sdks/openapi.yaml` (router contract test enforces parity).
- E2E anchor livelock round 2 (CI-timing): the dwell anchor also survived
  only while `isReconnectMode` stayed armed — reconcileOnIdle clears it on
  every workspace-SSE reconnect and the window re-arms immediately, so churn
  faster than the dwell cleared the anchor indefinitely on slow machines.
  The anchor now survives reconnect-mode flips; only strong-evidence breaks
  (live prompt arrived, stuck tool gone, snapshot not ok/too old, session
  change) clear it.

### Review round 3 (second REQUEST CHANGES — "close")

| ID | Finding (validated) | Resolution |
|---|---|---|
| R1 | Activation effect deps lacked `sessionId` — busy→busy same-workspace switch never re-armed (isSessionBusy never transitions), so the C3 recovery gap remained for that path. | `sessionId` added to deps. Regression test: SPA-navigate (react-router `useNavigate`) from busy A to stuck B → re-arms, requests snapshot, abort fires for B. Verified the test fails with the dep reverted. |
| R2 | Unknown-flight `complete(ok)` on a committed workspace committed the empty legacy bucket — reachable when the broker drops the `begin` under channel backpressure (128-deep, drop-counted) — a false-abort path of the PR's target class. The provider also ignored the broker's `resync` sentinel. | Guard: a complete carrying a flight ID matching no open flight, on a committed workspace, is non-authoritative (no commit). `resync` now clears committed/flights/legacy staging so events stage again. Two provider tests (dropped-begin survival, resync re-arm). |
| minor | `resolve()` failure in ListPending returned an untyped error. | Wrapped with `ErrPendingUnavailable` (`errors.Is` chainable). |
| minor | Two mangled test lines. | Fixed. |
