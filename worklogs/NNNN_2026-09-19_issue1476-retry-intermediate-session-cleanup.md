# Worklog: #1476 — retry-intermediate routine sessions cleaned in the retry loop

**Date:** 2026-09-19
**Session:** Owner-sanctioned triage of #1470's residuals, item 1: superseded retry attempts' sessions leak as unrecorded orphans; branch `fix/1473-retry-intermediate-session-leaks`
**Status:** Complete

---

## Objective

Stop the retry-intermediate session leak: `executeWithRetry` runs each attempt in a fresh session; a transient first attempt (preserved mode) keeps its session per the #1471 failure-leg contract, but the engine discards that response when retrying — the session survives in opencode forever, unrecorded and unindexed.

---

## Work Completed

- Investigation (code trace, no agentd change needed): the leak needs all three of (a) preserved-mode spec, (b) a transient-5xx first attempt — the only retryable-with-a-response shape (`retryableAgentdFailure`: `script_failed`/`session_create_failed` wrapping opencode 500/502/503; the create-failed variant has no session), (c) a later attempt that supersedes it. Ephemeral intermediates were already torn down by #1471's failure-leg teardown; transport-error retries carry no response object (the #1470 blind residual — proposal sent to the orchestrator separately). The DAG path's `executeNode` `maxAttempts` loop has the same class for `session:"new"` agent nodes — noted in the issue, not fixed here.
- **Fix (engine-only, zero wire change):** `executeWithRetry` gains an explicit `cleanupIntermediate func(ctx, sessionID)` parameter; invoked only when a retry will actually follow (`a < attempts`) AND the discarded response carries `SessionID`. Final-attempt failures are never cleaned (#1470 contract keeps them for inspection); non-retryable failures return immediately (nothing superseded); nil cleanup supported (script leg passes nil — script nodes never create sessions). The Scheduler backs the callback with `deleteRoutineSessionAuthorized` — the existing authorized path over the #1471 honest 204/502 delete route — best-effort: cleanup failure logs (inside the delete path) and the retry proceeds.
- Issue #1476 filed with the leak trace, the fix rationale ("the fire's session is the final attempt's; intermediates have no consumer"), the DAG same-class note, the old-agentd blanket-204 known-skew note, and the forensic side-finding (trigger_fires ON DELETE CASCADE makes deleted triggers' fire history unqueryable) as a related known-gap — per the orchestrator's triage instructions.
- Task 2 (transport-error/script_timeout blindness): proposal sent to the orchestrator — **Option A RULED IN and folded into this PR** (agentd maps the AGENT-NODE timeout leg from 504 to 200+errorCode "script_timeout"; consistent with the established envelope pattern, zero executor changes, #1471 recording and #1476 cleanup compose immediately, classifier untouched). **Scope guardrail honored:** the script/http-node timeout legs stay 504 (their engine consumer — the pre-script branch — has no ErrorCode handling; a 200 envelope there would read as success-with-empty-output). Folded: the agentd timeout mapping, the two timeout-leg pins updated to the 200 envelope, a new `TestExecScriptNode_TimeoutLeg_Stays504` guardrail pin, and an engine composition pin (`TimeoutEnvelope_RecordsSurvivingSession_Composition`: single dispatch — non-retryable — failed fire, surviving session recorded). Future work noted in the PR: making script-node timeouts consistent requires an engine pre-script ErrorCode branch first (separate lane).

### Review rounds 1–3 (CHANGES_REQUESTED → addressed)

- **R1 (log attribution):** the shared delete path's error logs no longer hardcode "for PreserveOnFailure" — every line carries a structured `purpose` field (`preserve_on_failure_success` | `retry_intermediate_cleanup`); `CleanupFails_StillDelivers` now records route hits (distinguishes "attempted and 502'd" from "never wired").
- **R2 (comment splice):** the purpose-const block had landed mid-godoc in `deleteRoutineSession` — re-flowed; added the explicit no-session-half guard pin; recorded the latency trade-off (synchronous delete ahead of backoff, 10s-bounded, worst case ~2×(10s+) per fire when agentd hangs — accepted with the best-effort contract).
- **R3 (F1 — real defect, overturning two earlier false-alarm adjudications):** ctx dying during the post-cleanup backoff returned the CLEANED attempt's response with its stale `SessionID` — the engine's ErrorCode branch then recorded a deleted session, and the ctx-free async `session_index` write made the ghost row land every time. Root fix: the cleanup callback now reports whether the session is **gone**; the ctx.Done return **scrubs the id only when the delete was confirmed** (a failed cleanup keeps the id — the session exists and deserves recording; the #1471 existence contract holds on every returned response). Four new pins: loop-level scrub-on-cleaned (deterministic — the cleaner cancels), keep-on-failed-clean, full-path no-ghost-record (cancellation ~100ms after the microsecond-scale cleanup, ~2s before backoff elapse), plus the scrub rule documented in `executeWithRetry`'s godoc. Worklog counts refreshed (12 tests in the retry-cleanup file at this head).


---

## Key Decisions

- **Latency note (review observation, accepted):** the synchronous delete inside the retry loop runs ahead of the backoff and is bounded by the 10s HTTP client — worst case adds ~2×(10s+) per fire when agentd hangs. Consistent with the best-effort contract; flagged here as the recorded trade-off.
- **Cleanup lives in the retry loop**, the only place that knows a response is being discarded. Agentd cannot know whether the engine will retry; unconditional agentd-side teardown would break #1470's keep-on-final-failure contract.
- **Explicit parameter over variadic-optional** (Rule 3): both call sites updated — script leg passes `nil` visibly.
- **Only when `a < attempts`** — the guard ordering matters; cleaning before the final-attempt check would delete the session #1470 exists to preserve.
- **Best-effort semantics** — a failed cleanup (delete route 502, agentd unreachable) leaves exactly today's behavior (leak + log), never blocks the retry, never fails the fire.

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | Superseded intermediates have no consumer | Fire recording uses only the final response (delivered branch / final ErrorCode branch); no other reader of attempt responses exists |
| A2 | Only preserved modes leak | Ephemeral failure-leg teardown is #1471-pinned (`TestExecAgentNode_FailureEnvelope_EphemeralTornDownAndOmitted`) |
| A3 | The retryable-with-session shape reaches the cleanup | `retryableFailure` envelope carries SessionID for preserved modes (#1471-pinned); test `CleansSupersededAttemptSession` proves the loop sees it |
| A4 | No other executeWithRetry callers | grep: script leg + agent leg in production; 10 test callers updated mechanically (nil) |

---

## Blockers

None.

---

## Tests Run

- `go test -count=1 -run "TestExecuteWithRetry_|TestExecuteRoutine_RetryIntermediate" ./api/internal/workflows/` — ok (red-first: the signature change failed compile pre-implementation); the retry-cleanup file carries 13 tests at the final head (r1's 7 → +no-session pin r2 → +timeout-composition Option A → +3 F1 pins r3 → +F1′ multi-attempt pin r4)
- `go test -timeout 300s -count=1 ./api/internal/workflows/` (full package, no race — memory directive; CI runs race) — ok 64.4s, includes the #1472 accounting pins and all #1464/#1471 routine suites
- `go vet` clean; gofmt clean
- Mutation-resistance: cleanup invoked unconditionally (ignoring `a < attempts`) fails `FinalAttemptFailureNotCleaned`; removing the nil-guard fails `NilCleanupNoPanic`; dropping the callback wiring at the agent leg fails `RetryIntermediateCleanedViaAuthorizedDelete` (delete route never sees the intermediate); a cleanup failure path is pinned non-fatal by `CleanupFails_StillDelivers`.

---

## Next Steps

- Adversarial review loop until APPROVED; orchestrator merges.
- Task 2 (Option A) is RULED IN and folded into this PR — see the round notes above; no open proposal remains.
- DAG `executeNode` same-class adoption is a separate decision if the owner wants it.

---

## Files Modified

- `api/internal/workflows/engine.go` — `executeWithRetry` cleanup param + invocation; `cleanupIntermediate` wiring in `executeRoutine` (script leg passes nil)
- `api/internal/workflows/engine_retry_cleanup_test.go` — new (13 tests at the final head + sequenceAgentd executor)
- `api/internal/workflows/engine_test.go` — 10 existing retry-wiring callers updated with the explicit nil param (mechanical)
- `cmd/workspace-agentd/workflow_execute.go` — agent-node timeout leg 504→200+errorCode (Option A, orchestrator-ruled; script/http-node legs untouched per guardrail)
- `cmd/workspace-agentd/workflow_session_error_envelope_test.go` — timeout pins updated to the 200 envelope + the script-node 504 guardrail pin
