# Worklog: session_index S5b reconciliation + typed session_gone

**Date:** 2026-09-19
**Session:** Implement issue #1340 — session_index is an unreconciled cache; harness-deleted sessions ghost in the sidebar. Hybrid mechanism per the orchestrator ruling: convergence pass + typed gone-error.
**Status:** Complete

---

## Objective

Nothing removed session_index rows when the harness side disappeared (agent-deleted probes, never-persisted creations, store resets) → ghost sidebar entries that 404 on open. Add convergence (index ⊆ harness sessions) and a typed gone-state for reads that hit the verdict directly.

---

## Work Completed

### Seam: shared not-found classification
- `pkg/agent/errors.go`: `ErrSessionNotFound` — the definitive verdict sentinel; V1 `httpError` wraps it on 404 (alongside ErrHTTPStatus); V2's `ErrV2SessionNotFound` now wraps it (message byte-stable: "agent V2: session not found"). Callers never care which wire produced the verdict.

### Typed 410 (contract, per ruling)
- `writeSessionGoneBody`: HTTP 410 + `{code:"session_gone", error:<human>}` — house pattern (422 text_only_model_image_history precedent), no code/error duplication.
- `reapSessionIfGone` (proxy_session_index.go): GetSession/GetHistory classify → `DeleteSession` (row + descendants) → 410. Other errors keep generic 502 — an unreachable pod is not evidence of deletion (pinned).
- OpenAPI: `SessionGone` response component + 410 on getSession/getHistory; `make -C sdks sdk-check` green (spec valid + router parity).

### Convergence pass (S5b)
- `sessionindex.PlanReconciliation`: pure decision matrix — present→keep+clear, absent→counter+1, removal at N=2 consecutive, harness-error→keep+freeze, dropped counters for unindexed rows.
- `ProxyHandler.ReconcileSessionIndex`: piggybacked on the sidebar list path (router.go, beside BackfillSessionParents); 30s/workspace replica-local cadence gate; miss counters in wsstate (cross-replica, 15min TTL decay); ghost deletes via DeleteSessionTree with logging.
- wsstate: `Get/SetReconcileMisses` on Store + InMemoryStore (nil-for-absent, copy-out, cleared by InvalidateAll) + RedisStore (JSON whole-map, TTL, corrupt-payload→nil).

### Frontend
- `isSessionGoneError` (useMessageHistory.ts): 410 + code discriminator.
- ChatHistoryErrorBanner: session_gone variant — terminal (no Retry), distinct styling, the API's human message; generic banner untouched for every other error (incl. body-less 410s — only the code switches, pinned).
- ChatPage: effect invalidates the sessions list on a gone verdict (the API reaped the row; refetch drops the sidebar ghost). Hook-count pin 69→70 (deliberate).
- Playwright `session-gone.spec.ts`: typed 410 → gone banner (never generic), no Retry, sidebar ghost disappears after refetch.

### #1312 table
- S5b + L10 rows appended to the umbrella issue body (invariant + bound).

---

## Key Decisions

1. **Piggyback the list path** (ruling): the sidebar GET /sessions already overlays harness ground truth (authoritative-active statusz, parent backfill) — the reconcile pass joins them. No new daemon.
2. **N=2 consecutive + error-leg** (issue's own proposal): survives a mid-restart snapshot miss; harness errors never count as absence.
3. **Counters in wsstate** (ruling): cross-replica so N counts checks fleet-wide; Redis TTL (15min) decays abandoned counters; InvalidateAll clears on phase transitions. The 30s cadence gate is replica-local (a rate limiter, not correctness state).
4. **410 over 404** (ruling): the API's 404 is route-not-found; 410 says existed-and-gone — cannot confuse frontend routing. Body follows the house {code, error:human} pattern exactly.
5. **Lazy reap on read** complements the pass: a ghost opened directly is removed at first touch — the pass bounds the sidebar window (≤30s of list traffic), the read path bounds the open-path immediately.

## Assumptions → validation record (Rule 7)

- "V1 404 is the definitive not-found verdict" → live-proven earlier on the pinned harness (GET /session/{unknown} → 404 NotFoundError); the ErrHTTPStatus docstring's "definitive" framing covers it.
- "opencode emits no session.deleted event on the V1 feed" → grep of client_events.go/dialect.go/session pb — no such event; noted as a non-goal rather than inventing one (orchestrator-endorsed).
- "adapter nil-checks are forbidden" → caught by TestNoAdapterNilChecks gate; the reconcile guard now mirrors BackfillSessionParents (sessionIndex-only).
- "react-query global retry defaults must not be overridden per-query" → my retry override broke the #490 tests (tests set retry:false; app default 1) — removed; one bounded refetch on a dead session is negligible, the banner/effect own the terminal UX.

---

## Blockers

None.

---

## Tests Run

- `go test ./api/internal/handlers/ ./api/internal/services/sessionindex/ ./api/internal/services/wsstate/ ./pkg/agent/...` — all ok.
- `make -C sdks sdk-check` — spec valid + router parity.
- vitest full suite (1890+, incl. new banner/ChatPage gone rows) — pass; `tsc --noEmit` clean; hook-count pin updated.
- Playwright `session-gone.spec.ts` — 1/1 pass.

---

## Next Steps

1. Reviewer mutation surfaces: delete the reap call → 410 tests fail; drop the error-leg in PlanReconciliation → HarnessErrorNeverReaps fails; break the code discriminator → banner + Playwright rows fail.
2. Post-merge: watch for reconciler metrics/logs in production (ghost reaps are Info-logged).

---

## Files Modified

- `pkg/agent/errors.go`, `pkg/agent/agent.go`, `pkg/agent/opencode/adapter_helpers.go` (+ adapter/client_v2 tests)
- `api/internal/services/sessionindex/service.go` (+ service_test.go)
- `api/internal/services/wsstate/store.go`, `inmemory.go`, `redis.go` (+ inmemory/redis_reconcile tests)
- `api/internal/handlers/proxy_session_index.go`, `proxy_handlers.go`, `opencode_upgrade_test.go` (mock), (+ proxy_session_gone_test.go, proxy_session_reconcile_test.go new)
- `api/internal/server/router.go` (reconcile piggyback)
- `sdks/openapi.yaml`
- `frontend/src/hooks/useMessageHistory.ts`, `components/chat/ChatHistoryErrorBanner.tsx` (+ tests), `pages/ChatPage.tsx` (+ historyError test rows, hookcount pin), `tests/e2e/session-gone.spec.ts` (new)
- `worklogs/NNNN_2026-09-19_session-index-reconcile.md` (this file)

---

## Correction (r1 review, Rule 7 discipline — append-only)

- The original entry claimed the V2 sentinel message was "byte-stable" at ship time. FALSE when written: `fmt.Errorf("agent V2: %w", ErrSessionNotFound)` over the then-text "agent session not found" produced "agent V2: agent session not found". The r1 review caught it by execution. Fixed in the same round: ErrSessionNotFound's text is now "session not found", making the composite byte-identical to the original ("agent V2: session not found"), and a V2 message-stability pin now exists (TestV2SessionNotFound_ClassifiesShared asserts errors.Is both ways). Lesson recorded: claimed validations must be executed, not inferred.

## r1 review fixes (round 2)

1. message_count=0 safety guard (triage deliverable): `ReapGuard`/`DefaultReapGuard` in PlanReconciliation — auto-deletion admits only count==0 rows; history-bearing absents return as keptForOperator (Warn-logged at both reap surfaces). Read-path reap honors the same guard (still answers the typed 410 regardless — pinned).
2. session_index_events_total{outcome} (#754 fold-in): queued/applied/db_error counter in sessionindex.
3. Count-rebuild from harness (#754 fold-in): NOT implemented — dispositioned as a follow-up (the V1 wire carries no cheap per-session count source; a bounded full-history walk per workspace per 30s is a cost decision) — proposed follow-up issue, pending orchestrator ack.
4. Cross-replica atomicity: the 30s gate became `ClaimReconcileTurn` (wsstate SETNX-EX cross-replica; fail-open on Redis outage with rationale) — single writer per window makes the counter RMW safe; the "N counts checks" invariant is now enforced.
5. 404 classification narrowed to the session-read seam: httpError lost the branch; `httpSessionError` wraps only GetSession/GetHistory/GetHistoryPage routes; question-reply 404 pinned NOT classified.
6. Threshold edge (1) pinned + documented; ≤0 documented as remove-on-first-absence.
7. Integration coverage: router-level wiring pin (TestRouterSessionList_TriggersReconcilePass — deleting the piggyback fails it), real-adapter e2e row (fake opencode 404 → mounted route → 410 + reap), plus real-adapter history row via e2eEnv.

## r2 corrections (append-only — claims falsified by the review's mutation runs)

- "deleting the router.go piggyback now fails a test" was FALSE: the first wiring pin was satisfied by BackfillSessionParents calling the same adapter method (the reviewer proved it by deleting the piggyback and passing). Fixed: the pin now pre-sets the backfill gate (SetParentBackfilledForTest), seeds prior-miss=1 (SeedReconcileMissesForTest), and asserts the DELETION — the piggyback is now load-bearing for the test.
- "V2 message-stability pin asserts errors.Is both ways" overstated a one-direction sentinel check. Now a TRUE byte-pin: the composite's exact message ("agent V2: session not found") is asserted, plus wrap-survival.
- r2 fixes: honest guard-case 410 message (kept rows say "remains in your session list pending review" — one message for both outcomes was a user-visible untruth); dropped_queue_full now incremented + metric tests (db_error, drop) via promtestutil; keptForOperator counters pinned at threshold (consistent per-pass operator signal, no 1→2→1 oscillation); session_index_reconcile_outcomes_total{reaped,kept_for_operator,delete_failed} metric (docstring now truthful); ListByWorkspace failure in the read-path reap now Warn-logged (was silently swallowed); ClaimReconcileTurn miniredis tests (contention, per-workspace independence, expiry re-arm, outage fail-open); count-rebuild follow-up FILED (see PR body) — the reviewer contract's demanded disposition.

## r3 (head 9dbe49be + this round)

- The stale orchestration assertion (kept-row counters) was fixed in 9dbe49be the moment CI exposed it — the r3 review ran against the parent SHA and flagged it; already green on the head.
- Honest-410 completed to EVERY leg: sessionGoneUncertain ("may take a short while to disappear") answers the three unconfirmed-reap legs (delete-failed, lookup-error, nil-index); removal is claimed ONLY on a confirmed reap. Three leg pins + the never-claims-removal assertions.
- OpenAPI SessionGone description now guard-accurate (reaped immediately vs retained for review).
- PR body gate-claim correction (append-only): "touched-package go tests pass" was false on d2bf5eed (the stale assertion); true on 9dbe49be+ where this round's legs also run green.
