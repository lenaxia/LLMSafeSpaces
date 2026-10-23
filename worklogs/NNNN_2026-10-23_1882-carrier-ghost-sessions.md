# Worklog: #1882 — carrier ghost sessions: delete propagation (four-part teardown) + script-only agent-leg gate

## The defect (owner report, 2026-10-23)

Routine trigger fires mint a full opencode "carrier" session per fire
(`preserveSession: never` default), run one turn, and hard-delete it
seconds later — but the delete never propagates: no
`session.status:deleted` event, no proxy tombstone, no `session_index`
row removal. The sidebar (backed solely by `session_index`) keeps
every carrier as a ghost row until a click fails the per-session fetch
(and history-bearing rows survive even clicks — `kept_for_operator`).
Scale in the live workspace: ~75 carrier sessions/hour. Second defect
stacked under it: the script-only agent-leg gate (#1812) tested
`ScriptLanguage` only, so ScriptPath-only + empty-prompt fires fell
through and minted a carrier for an EMPTY prompt.

Owner framing: *"we shouldn't need to create a session to run a
script."*

## Root cause (investigation-complete in the issue; confirmed on main)

- agentd `execAgentNode` creates the session unconditionally
  (workflow_execute.go), tears ephemerals down on success AND on the
  failure path — then WIPES the id from the envelope (existence
  contract #1470). The engine never learns which row to clean.
- The engine's authorized deletes (`deleteSessionAuthorized`:
  PreserveOnFailure success, retry-intermediate, DAG node-retry)
  perform only the pod-side HTTP delete — none of the three teardown
  effects the user route (`HardDeleteSession` → tombstone + index row
  + SSE `session.status:deleted`) performs.
- The pod-side short-id replica is append-only (#1850 design): a dead
  session's phrase kept resolving post-delete (live evidence:
  `session_metadata` resolved a hard-deleted carrier's phrase).
- engine.go's script-only early-return (`#1812`) tested
  `ScriptLanguage != ""` only — ScriptPath-only + empty prompt fell
  through to `routine-agent` with an empty prompt.

## The fix

### Bug 2 — four-part teardown convergence (commits dc8f8b7c, red eca2deb8)

Every confirmed session delete now converges on the SAME effects:

1. **wsstate tombstone FIRST** (`MarkSessionDeleted`) — ordering is
   load-bearing: a late pod event racing the row removal must find the
   tombstone set, or the phrase-bearing upsert re-births the row
   (#1881 made every row-creating upsert mint a phrase on INSERT).
2. **`session_index` row removal** (`DeleteSessionTree`, exact
   (workspace, harness-id) key + recursive descendants — unchanged
   semantics, now pinned from the delete side).
3. **`session.status:deleted` broadcast** on workspace + user streams,
   phrase-keyed via the no-mint translate (`ResolveShortID`'s LRU
   survives the row delete by design, #1817 piece 3).
4. **Pod-side short-id replica invalidation** — agentd's
   `deleteOpencodeSession` confirmed-delete leg calls the new
   `sessionShortIDStore.forget` (both directions removed, persisted).
   This is the ONE retraction the append-only replica allows; pulls
   never remove entries (#1850 posture unchanged).

Wiring:

- `workflows.SessionDeletePropagator` interface; `ProxyHandler.
  PropagateSessionDeleted` implements it (tombstone → row → SSE tail);
  `HardDeleteSession` converges on the SAME method (behavior-
  identical; existing user-route pins stay green untouched).
- `Scheduler.SessionDelete` / `Reconciler.SessionDelete` wired to
  `proxyHandler` in app.go. `deleteSessionAuthorized` runs the
  propagator after a confirmed pod delete (nil propagator = unwired
  engines keep pre-#1882 behavior).
- agentd reports `deletedSessionId` in BOTH envelopes (success
  teardown leg + `failAgentNode` leg) — disjoint from `sessionId` by
  the #1470 existence contract. `executeWithRetry` gains `onDeleted`
  (fires for EVERY attempt's reported teardown — the discarded
  intermediate attempts included, the only place they are observable);
  the DAG `executeNode` propagates observed teardowns the same way.

### Bug 1 — script-only agent-leg gate (commit 64ed8561, red 26ea082f)

The #1812 early-return now admits `ScriptPath != ""` when the prompt
is empty: the pre-script ran sessionless for its side effects, which
IS the delivery. No carrier minted, no LLM leg, no routing change —
where scripts RUN stays #1878's lane (explicitly untouched).

## Red-first pins

- RED eca2deb8: engine propagation pins (authorized delete,
  envelope-reported ephemeral teardown delivered/failed/retry-
  intermediate, DAG node, nil-propagator safety), agentd envelope +
  replica-forget pins, proxy `PropagateSessionDeleted` contract pin —
  all failing on main, green in dc8f8b7c.
- RED 26ea082f: `TestScheduler_RoutineScriptPathOnlyNoLLMLeg` —
  dispatched `[routine-script routine-agent]` on main; green in
  64ed8561.

## Mutation-verify recipes

1. **engine-propagate** — in `deleteSessionAuthorized`
   (api/internal/workflows/engine.go), delete the
   `prop.PropagateSessionDeleted` tail → RED:
   `TestExecuteRoutine_PreserveOnFailureSuccess_PropagatesDelete`.
2. **engine-ondeleted** — in `executeWithRetry`, remove the
   `onDeleted` invocation → RED:
   `TestExecuteRoutine_RetryIntermediateEphemeralTeardown_Propagates`
   (the ses_midA call disappears).
3. **agentd-envelope** — in `execAgentNode`'s success teardown leg,
   stop setting `deletedSessionID` (keep the payload's
   `session_deleted` flag) → RED:
   `TestExecAgentNode_EphemeralTeardown_EnvelopeReportsDeletedSessionID`
   AND `TestExecuteRoutine_EphemeralTeardown_ReportedDeletedIDPropagates`.
4. **replica-forget** — in `deleteOpencodeSession`, remove the
   `currentSessionShortIDs().forget(...)` call → RED:
   `TestExecAgentNode_EphemeralTeardown_ForgetsReplicaPhrase`,
   `TestWorkflowDeleteSessionHandler_ForgetsReplicaEntry`.
5. **phrase-safety (PG-gated)** — in `DeleteSessionTree`
   (database.go), key the seed SELECT on `short_id = $2` instead of
   `session_id = $2` → RED:
   `TestIntegration_DeleteSessionTree_RemovesOnlyDeadRow` (the live
   row's phrase/title assertions fail). CI-verifiable via
   secrets-integration; no local PG in pods (inspection-grade here —
   file compiles under the integration tag and testharness skips
   without a server, per house disclosure contract).
6. **tombstone-order** — in `PropagateSessionDeleted`
   (proxy_handlers.go), move `MarkSessionDeleted` AFTER the index
   delete → RED: `TestPropagateSessionDeleted_Tombstone_IndexRow_SSE`
   is order-blind today but the keep-green guard is
   `isSessionDeleted` after the call; the order pin is the #1881
   re-birth hazard documented in the method comment (the strict
   interleaving test would need a racing writer — accepted as
   comment-pinned, not test-pinned).
7. **script-only-gate** — revert the engine.go early-return to
   `ScriptLanguage != "" && prompt == ""` → RED:
   `TestScheduler_RoutineScriptPathOnlyNoLLMLeg`.

## Verification

- Unit: `api/internal/workflows`, `api/internal/handlers`,
  `cmd/workspace-agentd`, `api/internal/app`, `sessionindex`, `wsstate`
  full suites green locally (go test -count=1 per tree).
- Integration: the two `TestIntegration_DeleteSessionTree_*` pins ride
  CI's secrets-integration Postgres (skip-if-unreachable locally).
- repolint: all checks passed (this worklog numbered 1198; migration
  sequence untouched — no migration needed: the fix adds no schema).

## Accepted residuals (disclosed)

- The #1470 transport-error blind spot extends to the new field: a
  transport failure mid-attempt carries no response object, so an
  ephemeral agentd deleted before the failure is unobservable (the
  row converges via the #1340 reconcile path only if count==0; a
  history-bearing one is kept_for_operator). Same accepted class as
  the pre-existing #1470 residual.
- The USER-route delete's pod-side replica invalidation relies on the
  adapter/Act delete reaching opencode; the replica entry for a
  user-deleted session stays warm until pod restart (advisory
  posture; did-you-mean degradation — pre-existing, unchanged here;
  agentd sees those deletes only when they ride its routes).
- Bug 1 gap (b) (the DEPLOYED engine's script-only ~1s silent
  `delivered` signature) is deployment parity, not repo state — the
  repo path is correct post-64ed8561; re-measure after deploy
  (mirrors the issue's recommendation and #1878's lane).

## Sibling coordination

- #1878 (where scripts run): untouched. The script leg still
  dispatches to agentd's `execScriptNode` (sessionless since #1812);
  only the agent-leg GATE changed here.
- #1881 (write-once phrases): no UPDATE path touched; the teardown
  deletes whole rows by exact key; the delete-side safety pin
  (recipe 5) closes the regression class from this side.
- #1340 reconcile: guard/probe/grace semantics untouched — the r1 fix
  adds a tombstone consult BEFORE the sweep's row-creating writes
  only; with propagation in place the reconcile's
  kept_for_operator population stops GROWING from carriers. The reap
  safety net (issue recommendation 3) is #1901's lane.

## Review r1 remediation (CHANGES_REQUESTED → commits cbc77f62 + 13358ebc)

The automation's verdict: mechanisms verified sound (four-route
convergence, ordering, gate, wiring; 5/7 recipes independently
mutation-verified red by the reviewer; PG pins green in CI's
secrets-integration on 5d2549ad). CHANGES on three findings:

1. **Reconcile re-birth race (validated, real)** —
   `rebuildMessageCounts`/`backfillShortIDs` never consulted the
   tombstone; a confirmed delete landing during the count walk
   (240–490ms/page between list snapshot and write) re-minted the
   carrier's row via Register + RebuildMessageCount (both
   row-creating) → count>0 → kept_for_operator forever. FIXED: both
   loops consult `isSessionDeleted` before their row-creating writes
   (the count-rebuild consult sits AFTER the walk — the walk IS the
   race window). Red pins:
   `TestSessionIndexReconcile_CountRebuildSkipsTombstonedRow`,
   `..._BackfillSkipsTombstonedRow` (red cbc77f62, green 13358ebc).
   Mutation recipe 8: delete either consult → the corresponding pin
   reds.
2. **Recommendation 3 split** — filed as #1901 (reap safety net for
   origin-marked carriers + drain-time tombstone visibility +
   restart-wiped tombstones; includes the PreserveAlways-must-not-reap
   design question). PR body updated: no full-closure claim over the
   reap net.
3. **E2E coverage at the harm's level** — R3 (ghost stays gone across
   two reconcile passes; prompt-derived-title ghost signature) + R4
   (scriptPath-only fire delivers sessionless, no row) added to
   local/issue1452-routine-session-index-e2e.sh; structural pins
   extended (bash -n, needles, jq-compile).

Reviewer's non-blocking note (disclosed): ScriptPath-only scripts
exiting non-zero still record `delivered` (pre-existing #1812
transport-error-only semantics, now widened to ScriptPath-only) —
noted on #1901's lane as a semantic follow-up, not changed here.

