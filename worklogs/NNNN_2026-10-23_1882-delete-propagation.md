# Worklog: #1882 — platform-initiated session deletes never propagate (mass ghost rows in the sidebar)

**Date:** 2026-10-23
**Branch:** `fix/1882-delete-propagation`
**Status:** PR opening (r1)

---

## Objective

Issue #1882 (owner report, live investigation 2026-10-23): every
prompt-routine fire mints an ephemeral opencode "carrier" session that
agentd hard-deletes seconds after the turn — and that delete
propagates NOWHERE. No `session.status: deleted` SSE event, no wsstate
tombstone, no `session_index` row removal: only the user-initiated
proxy delete route (`proxy_handlers.go` `HardDeleteSession`) ran the
effect sequence. Meanwhile the live event bridge mints index rows for
the carrier DURING its brief life, the sidebar serves only
`session_index` (`ListSessionIndex`), and the #1340 reconcile's
`DefaultReapGuard` refuses history-bearing rows (`kept_for_operator`
forever). Result: ~75 ghost sidebar rows per hour per workspace with
the live watch-trigger fleet; each vanishes only when clicked (the
typed 410's lazy eviction), and history-bearing rows survive even
clicks. This lane implements the issue's fix items 2+3 (delete
propagation + self-heal + read-path belt-and-braces); item 1
(sessionless script dispatch) is the #1878 lane and is untouched.

## Root cause (as confirmed by the issue's live experiment)

1. **agentd's ephemeral teardown is invisible to the engine.** The
   `#1470` envelope reports `sessionId` iff a session EXISTS after the
   node — for a torn-down ephemeral it is `""`, so the engine never
   learns WHICH session died and cannot clean the index row the bridge
   minted for it.
2. **The engine's authorized deletes are equally silent.**
   `deleteSessionAuthorized` (PreserveOnFailure success cleanup,
   retry-intermediate cleanup, DAG node-retry cleanup) performs only
   the agentd HTTP delete — none of the three effects.
3. **The read path has no death filter.** `ListSessionIndex` returns
   every workspace row; nothing durable records "this row's session is
   dead" once the row-delete is missed.
4. **The reconcile's triage guard blocks self-heal.** History-bearing
   absent rows are `kept_for_operator` — correct for a possible
   harness-store reset, wrong for `session_origins`-marked platform
   carriers the platform itself deleted.

## Fix design

### 1 — One effect sequence, every delete (extract + reuse)

`ProxyHandler.ApplySessionDeleteEffects` is the extracted tail of the
user route's `HardDeleteSession` (byte-compatible order, plus one new
step):

1. `recordActivityIfTracked`
2. wsstate tombstone (`MarkSessionDeleted` — late pod-event suppression)
3. **NEW** durable index death mark (`session_index.deleted_at`,
   migration 000044 — written BEFORE the row delete)
4. index row delete (+ descendants, `DeleteSessionTree` shape)
5. SSE `session.status: deleted` on both streams +
   active-session/parents cleanup (`removeActiveSessionEvents`)

Wiring:

- **agentd envelope** gains `deletedSessionId` (success + failure
  legs; a DELETE-404 counts as gone — the session IS dead; a failed
  teardown sets nothing — the leak stays discoverable per #1470).
  `sessionId` semantics unchanged.
- **Engine routine path**: on `deletedSessionId` → origin-MARK the
  carrier (`session_origins`, origin=routine) → effects → origin-row
  CLEANUP (marks must not accumulate per fire; the mark exists so a
  partially-failed effects run still leaves a re-apable row).
- **`deleteSessionAuthorized`**: a CONFIRMED agentd 204 runs the
  effects (Scheduler + Reconciler `DeleteEffects` field; nil =
  graceful no-op). Covers PreserveOnFailure success, retry-
  intermediate, DAG node-retry.
- **DAG path**: `executeNode` propagates `deletedSessionId` on both
  the errorCode and delivered legs; `executeWithRetry` propagates
  DISCARDED attempts' carriers via a new `intermediateDeleted`
  callback (the final response carries only the final attempt's
  fields).

### 2 — Read-path belt-and-braces (durable death filter)

Migration 000044 adds `session_index.deleted_at`;
`MarkSessionIndexDeleted` stamps it (recursive tree, the
`DeleteSessionTree` shape) before the row delete; `ListSessionIndex`
filters `si.deleted_at IS NULL`. A row whose DELETE failed (crashed
tail, raced replica) stops rendering immediately. Row-creating
upserts never clear the mark — late pod events cannot resurrect a
death-marked row (and the tombstone suppresses them same-replica).

### 3 — Reconcile self-heal for marked carriers

`runSessionIndexReconciliation` builds the workspace's
`session_origins` mark set once per pass (fail-safe: a lookup error
degrades to NO marks) and the reap guard admits
`DefaultReapGuard(row) || marked[row.InternalID]`. The #1850 by-ID
probe confirmation still gates every admitted row (veto/defer/confirm
matrix unchanged — pinned). Read path (`reapSessionIfGone`) admits
marked carriers too, so the owner's click on a carrier ghost answers
`sessionGoneReaped` (removed) instead of `sessionGoneKept`
("pending review" — a lie for a carrier). New outcome metric
`reaped_carrier` for observability.

## Red-first evidence (all pins failing before the fix)

- agentd envelope (runtime RED, 4 pins), e.g.
  `TestExecAgentNode_SuccessEnvelope_EphemeralReportsDeletedSessionId`:
  `Not equal: expected: "ses_1"  actual: ""  ... the torn-down
  carrier's id must ride the envelope so the engine can propagate the
  delete (#1882)`
- engine effects (compile RED):
  `engine_delete_propagation_test.go:73:3: unknown field
  DeleteEffects in struct literal of type Scheduler`
- database (compile RED):
  `session_index_deleted_at_test.go:62:13: svc.MarkSessionIndexDeleted
  undefined (type *Service has no field or method
  MarkSessionIndexDeleted)`
- handlers (compile RED):
  `proxy_delete_propagation_test.go:59:4: h.ApplySessionDeleteEffects
  undefined (type *ProxyHandler has no field or method
  ApplySessionDeleteEffects)`

## Test matrix (all green post-fix)

- `cmd/workspace-agentd`: envelope field set exactly when an ephemeral
  teardown deleted the carrier; 404-delete counts; teardown-failure
  omits; preserved omits.
- `api/internal/workflows`: routine ephemeral (delivered + failure
  legs) propagate effects + mark/clean origin; authorized delete
  (204) propagates, refused delete (502) does NOT; nil effectuator
  non-fatal; DAG node-retry cleanup propagates; DAG ephemeral
  propagates; E2E-shaped integration (real sessionindex.Service):
  carrier minted by the bridge → fire → death mark + row delete land
  (sidebar rows return to zero).
- `api/internal/handlers`: `ApplySessionDeleteEffects` emits
  tombstone + mark + row delete + SSE deleted event; user route also
  marks; reconcile reaps origin-marked history-bearing carriers
  (+`reaped_carrier` outcome); probe-veto still applies to marked
  rows; origin-lister error fails safe; read-path reap admits marked
  carriers.
- `api/internal/services/database`: `ListSessionIndex` SQL carries
  the death filter (sqlmock regex pin); `MarkSessionIndexDeleted`
  tree-stamps.
- #1865 regression matrix re-run green (reap_vetoed_live /
  reap_deferred_probe / confirm / kept_for_operator pins unchanged);
  full handlers + workflows + agentd + services suites green.

## Residuals

- Pre-fix UNMARKED ephemeral-carrier residue (bridge-minted rows for
  carriers that pre-date this fix; no `session_origins` row, history
  present) remains `kept_for_operator` by the #1340 triage — the
  harness-store-reset rationale. Draining that class requires either
  a policy change to the unmarked-history reap (out of this lane's
  risk budget; pinned by
  `TestSessionIndexReconcile_HistoryBearingGhostKeptForOperator`) or
  manual surgery. New carriers leave no residue and pre-fix
  PRESERVED-then-deleted sessions (origin-marked) now drain via the
  reconcile.
- A death-marked row whose hard DELETE failed is hidden but lingers
  in Postgres (the reconcile's list is filtered too, so it will not
  self-purge). Rare (the mark and delete run back-to-back); harmless
  (invisible everywhere); a `deleted_at IS NOT NULL` sweep could be a
  follow-up.
- User-route deletes of PRESERVED routine sessions leave their
  `session_origins` rows (pre-existing behavior, invisible — the
  badge join rides index rows).
- `TestLive_Worklogs_NoDuplicates` fails on origin/main's own
  unnumbered `NNNNN_2026-10-23_chatux-1845...` sentinel (pre-existing,
  not this branch).

## Mutation-verify suggestion

Delete the `MarkSessionDeleted` call inside
`ApplySessionDeleteEffects` (proxy_handlers.go) →
`TestApplySessionDeleteEffects_EmitsAllEffects` (markedDeleted) and
`TestExecuteRoutine_EphemeralCarrier_SessionIndexIntegrationToZero`
(MarkSessionIndexDeleted assert) must fail. Revert the
`si.deleted_at IS NULL` predicate in `ListSessionIndex` →
`TestListSessionIndex_ExcludesDeletedRows` must fail (sqlmock regex
miss). Drop the `|| marks[row.InternalID] != ""` guard clause →
`TestSessionIndexReconcile_OriginMarkedCarrierReaped` must fail while
`..._HistoryBearingGhostKeptForOperator` stays green (the unmarked
policy is intact).
