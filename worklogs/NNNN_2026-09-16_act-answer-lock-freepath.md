# Worklog: #1396 — Act answers bypass the admission lock (registry forwards, 5s budget, L12)

**Date:** 2026-09-16
**Session:** Issue #1396 (P1) — permission/question replies through Act hung ~2m05s then 502'd exactly when the user answered a pending ask. Implement the verb-classification carve-out, the forward deadline budget, and the L12 invariant proposal.
**Status:** Complete

---

## Objective

Stop answer actions (`answer_question`) from serializing behind the per-session admission lock. An ask exists exactly when a session is busy (an admission riding its 3-minute `Admit` context holds the sole-writer lock, #1342), so queuing the user's reply behind the turn waiting on the reply is deadlock-by-design. Deliver: (1) lock-free answer forwards with a principled verb classification, (2) a short typed deadline budget (never a 125s silent hang), (3) the L12 invariant proposed to #1312, (4) tests at every level.

---

## Work Completed

### Root cause (verified in source, not inferred)

- `cmd/workspace-agentd/sessionstate/actions.go` `act` took `a.sessionLock(sid)` for EVERY verb and held it across `a.cfg.Actor.Act(...)` — the harness forward included.
- `ledger.go` `attemptAdmission` takes the SAME per-session mutex and holds it across `Admit` under `AdmitterTimeout` (default 3 minutes — V1 admission is synchronous: the response IS the LLM turn, 30–120s; #1313/#1342).
- API leg forwards with `c.Request.Context()` (`api/internal/handlers/proxy_actions.go:69`): no agentd-side bound existed on the lock wait. Answer queues behind the admission → API request ctx cancels at ~125s → `context canceled` on `POST …HarnessABIService/Act` → 502 `failed to answer input`. Exactly the four incident signatures.

### The change

1. **Verb classification (the M1/W4 amendment).** `act` dispatches `ANSWER_QUESTION` to `actAnswer` — lock-free. Classification (documented in `actions.go` head): answer forwards target the ask REGISTRY (`/permission/:id/reply`, `/question/:id/reply|reject` — harness-side ephemeral state, #1312's ownership table); admissions write the TRANSCRIPT. Disjoint write sets ⇒ answers COMMUTE with in-flight admissions. `interrupt`/`switch_model`/`switch_agent`/`compact` mutate or sequence against the transcript and keep the unaltered sole-writer lock. Not a blanket unlock — the carve-out boundary is "serialize what shares a write domain; exempt what doesn't".
2. **`actAnswer`** (`actions.go`): forward under `context.WithTimeout(ctx, budget)` — `Config.AnswerTimeout`, 0 ⇒ `answerForwardBudget` = 5s (static-pinned; issue's proposed figure; harness answers in 53–307ms measured). Expiry → typed `CodeDeadlineExceeded` ("retry") + `Metrics().AnswerBudgetExceeded` (a.mu-guarded cumulative; Prometheus `llmsafespaces_answer_budget_exceeded_total` via the delta bridge). Caller cancellation checked FIRST → `CodeCanceled` wrapping the real `ctx.Err()` (never masked as the budget). 404 → `resolveByAbsence` unchanged (answer-only). I7 holds by construction: the path never reads or writes ledger/entry state.
3. **`resolveByAbsence` lock order updated:** takes `a.mu` ALONE — a leaf (edges `sessionLock → a.mu` exist in lease/reconcile/serialized actions; the reverse exists nowhere). Since answers no longer hold the session lock when calling it, the fold composes with the lease diff's own resolve half: both check `rec.pending` under `a.mu` before folding (idempotent, no duplicate seq), and `diffSessionLease`'s `resolvedSeq > seqAtGather` gate never resurrects a just-resolved ask.
4. **Comment accuracy:** `sessionLocks` + delivery-driver join comments in `authority.go` now state the transcript-verb scope; `Actor` seam doc likewise.

### Tests (TDD — all new legs observed RED pre-fix, GREEN post-fix)

New `act_answer_test.go` (+ helper variadic on `actionsAuthority`):
- `TestAct_AnswerUnderHeldAdmission_Completes` — today's exact repro: admission parked holding the lock, answer must land ≤2s. Pre-fix failed: "L12 violated: the answer is still queued behind the held session lock".
- `TestAct_AnswerRacingInFlightAdmission_BothLand` — the promote-race fault leg: answer lands mid-admission; the raced admission still lands `ADMITTED`; the harness's `INPUT_RESOLVED` folds cleanly after (what the projection sees).
- `TestAct_AnswerResolveByAbsence_UnderHeldAdmission` — the 404 fold (a.mu leaf) resolves a stale click under a held admission.
- `TestAct_AnswerBudget_TypedDeadlineError` — 3 subtests: budget expiry typed + fast + counter==1; caller-cancel → `CodeCanceled` (counter==0); compact parking past the same budget still completes (budget is answer-only — the matrix guard).
- `TestAct_AnswerL12_WireLevel_BusySession` — integration leg (abitest): REAL generated connect handler over HTTP + real delivery driver + ledger + blocking admission; answer lands ≤2s (L12), the harness registry clears, the unblocked turn's admission lands ADMITTED.
- `authority_internal_test.go`: static pin `answerForwardBudget == 5s`.
- Untouched and green (the M1/W4 matrix pin): `TestActOp_SerializesAgainstDelivery` (both directions), `TestActOp_InterruptAdmissionRace` (I7).

### L12 proposed to #1312

`L12 — answer latency ≤ 2s under a busy session (L2's missing half)`; fault leg: answer racing an in-flight admission. Posted as a comment on #1312 referencing this PR.

### Review iteration 1 (AI reviewer: REQUEST CHANGES → fixed)

The reviewer verified the root cause, the leaf-lock claim (read every `sessionLock` taker), and empirically reproduced the TDD RED state (backported compile prerequisites onto merge-base, ran the new tests: FAIL pre-fix, PASS at HEAD with `-race`). Findings + dispositions:

1. **[REAL, fixed] Prometheus delta bridge unpinned.** `answerBudgetDelta` + `llmsafespaces_answer_budget_exceeded_total` landed without the two pins the #1342 sibling convention carries. Fixed: `TestAnswerBudgetMetric_FunnelAdvances` (advance-by-1, no double-count on re-record, authority-recreation reset, +1 delta after recreation — deleting the bridge block fails it) + the name added to `TestMetricsScrape_Completeness`'s required list.
2. **[REAL, fixed] Exact counter semantics.** `errors.Is(err, context.DeadlineExceeded)` attributed ANY DeadlineExceeded-wrapping error to the budget (a harness-internal deadline would overcount the canary). Fixed: classification gates on `fctx.Err() == context.DeadlineExceeded` — the budget context, not the actor's error shape. New RED-then-GREEN leg: `harness-internal deadlines are not the budget` (counter stays 0; the error keeps its own shape through the passthrough paths).
3. **[REAL, fixed] One-line comment** at the caller-cancel-before-404-fold ordering (a NotFound racing the cancel skips resolveByAbsence; the lease diff's next tick converges — bounded, no user impact).
4. **[DONE] Tracking issue #1399** for the browser e2e follow-up (live-cluster tier — the Playwright tier is route-mocked and cannot exercise this lock), per the reviewer's "would make it airtight" note.

Reviewer's audit note recorded: the stale github-actions "Fixed" comment on #1396 references a branch that doesn't exist on the remote and artifacts that don't match this PR — closure rests on this PR's diff.

---

## Key Decisions

1. **Direct `a.mu` for the projection fold, NOT TryLock + event-queue retry.** The issue text offered TryLock+retry as an option ("can be"); rejected as over-engineering (Rule 4): `a.mu`'s critical section is the in-memory fold + one fsync'd uint64 cursor — microseconds, and every a.mu taker (ingest, serve, metrics) already blocks for exactly that. The ANSWER FORWARD (the L12-dominating step) is outside all locks anyway, so the retry queue would buy nothing measurable. Documented here and in the PR body.
2. **Classification over blanket unlock.** The reviewer-facing commute argument: the ask registry is harness-side ephemeral state (#1312 ownership table: "Pending asks | ground truth = harness live registry (ephemeral by nature)"); admissions write the transcript. Disjoint write sets commute. Transcript verbs keep the lock — the S1 matrix's "no exceptions" decision now reads "no exceptions for transcript verbs", which is what it always protected (a delivery in flight and a transcript action can never interleave).
3. **Budget classification order: caller-cancel first, deadline second.** The UI must distinguish "I gave up" (Canceled) from "the harness is busy" (DeadlineExceeded → API 504 `deadline_exceeded` → the pill re-enables and retries, per #1370's landed frontend half — validated in `QuestionPrompt.tsx`: `setSubmitting(false)` + `setError` on catch).
4. **Answer success writes no agentd state** (validated: on success `act`/`actAnswer` only stamp `res.SessionId`; resolution arrives via the harness's own `INPUT_RESOLVED` event / lease diff). This is WHY removing the lock is safe — there is nothing agentd-side to interleave.

---

## Blockers

None.

---

## Tests Run

- `go test ./cmd/workspace-agentd/sessionstate/ -run 'TestAct_Answer' -v` — RED pre-fix (all legs fail in ≤2.5s with named assertions, no hangs), GREEN post-fix.
- `go test ./cmd/workspace-agentd/... -race -timeout 420s` — ok (agentd 300s on loaded runner, faultmatrix 41.6s, sessionstate 58.5s).
- `go test ./... -timeout 600s` — full repo, zero failures.
- `go build ./...`, `go vet ./...`, `gofmt -l` — clean.
- `make lint` (golangci-lint v2.6.2) — 0 issues.

---

## Next Steps

- Review loop on the PR until APPROVE (the orchestrator decides merge).
- The browser e2e leg (click a pill while the agent runs, sub-second feedback) needs a live cluster; the wire-level L12 test exercises the identical server path end-to-end. Mapping it onto the epic-71 cluster suites is a natural follow-up there.
- #1380 (sessions-cluster Act, API-side) may add Act verbs; the classification table in `actions.go` is the seam to extend — new verbs default to the sole-writer lock (safe default).

---

## Files Modified

- `cmd/workspace-agentd/sessionstate/actions.go` — classification dispatch, `actAnswer`, budget, comments
- `cmd/workspace-agentd/sessionstate/authority.go` — `Config.AnswerTimeout`, `answerBudgetExceeded` counter + `Metrics.AnswerBudgetExceeded`, comment accuracy
- `cmd/workspace-agentd/sessionstate/act_answer_test.go` — NEW: the five test groups above
- `cmd/workspace-agentd/sessionstate/actions_test.go` — `actionsAuthority` variadic config mutators (helper)
- `cmd/workspace-agentd/sessionstate/authority_internal_test.go` — 5s default static pin
- `cmd/workspace-agentd/sessionstate_metrics.go` — `llmsafespaces_answer_budget_exceeded_total` + delta bridge
- `COORDINATE.md` — claim entry
