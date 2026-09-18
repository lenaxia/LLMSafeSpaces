# Worklog: #1457/#1458 — retry coverage for the session-create and pre-script legs

**Date:** 2026-09-18
**Session:** Finish branch fix/1457-1458-retry-coverage — agentd session-create error split (#1457) + wiring tests for the ScriptPath pre-script retry (#1458)
**Status:** Complete

---

## Objective

1. **#1458**: pin the WIP routing of the routine-fire ScriptPath pre-script leg through `executeWithRetry` with real wiring tests (transient agentd 5xx retried → fire delivered, no budget burn; deterministic 4xx not retried), mutation-testable on revert of the one-line fix.
2. **#1457**: split agentd's `createOpencodeSession` failure collapse (transport/5xx/unparseable → `""` → HTTP-200 `session_not_found`) into a distinguishable `session_create_failed` code carrying status/error detail, and add the new code's transient shapes to `retryableAgentdFailure`. Genuine missing-session stays non-retryable; timeouts stay out of the retry class.

---

## Work Completed

### #1458 wiring tests (engine)
- `TestScheduler_RoutineFireScriptLegRetriesTransient5xx`: ScriptPath trigger, script leg returns transport 502 then succeeds, agent leg succeeds → fire `delivered`, 3 executor calls (script retried once), 0 `consecutiveFailures` burned.
- `TestScheduler_RoutineFireScriptLegDeterministic4xxNoRetry`: transport 400 on the script leg → fire `failed`, 1 call, exactly ONE failure burned (guards the retry class from widening).
- Mutation-verified: reverting the one-liner (direct `AgentdClient.Execute` in the ScriptPath branch) fails the transient test (fire `failed` at 1 call).

### #1457 agentd end (`cmd/workspace-agentd/workflow_execute.go`)
- `createOpencodeSession` now returns `(sessionID, failCode, failDetail)`; every failure mode carries `session_create_failed` with distinguishing detail:
  - transport error → `opencode session create: <err>`
  - non-200 → `opencode returned <N>` (the file's existing message-leg convention)
  - unparseable 200 body → `cannot parse created session: <err>`
- `execAgentNode` passes code/detail through `writeWorkflowError` (still HTTP-200 `{errorCode, detail}` wire shape). A failed CREATE is never reported as `session_not_found`.
- Incidental pre-existing bug fixed (Rule 5): the non-200 path never closed the response body; `defer` close now covers all paths.

### #1457 engine end (`api/internal/workflows/engine.go`)
- `retryableAgentdFailure`: `session_create_failed` joins `script_failed` in matching the transient opencode 500/502/503 wrap shapes. Comment updated with the create-leg rationale (nothing sent yet → no double-execution risk) and the explicit exclusions.
- NOT retried (pinned by tests): `session_create_failed` transport-to-opencode detail (consistent with the message leg, which never retried opencode transport errors), deterministic 4xx wraps, `session_not_found` always (genuine missing session is permanent; retrying would triple-burn auto-disable budget), all timeout shapes (unchanged #1451 r2 rationale).

### T7 e2e row (`local/issue-1417-templating-e2e.sh`)
- The structural "wiring pins exist in the tree" loop now also asserts `TestScheduler_RoutineFireRetriesSessionCreate5xx` and `TestScheduler_RoutineFireScriptLegRetriesTransient5xx` (deleted pins fail the nightly row instead of rotting). `bash -n` verified.

### Review round 1 (Finding 1 — empty-ID phantom success, fixed)
- The reviewer traced (and live-reproduced) that a 200 create body of `{}` or `{"id":""}` passes `decodeStrict` (single-JSON-value check only) → `parseCreatedSessionID` returns `("", nil)` → the split returned `("", "", "")` → the engine read `ErrorCode==""` as SUCCESS → fire "delivered", failure counter reset. A silent phantom success — strictly worse than the pre-PR `session_not_found` visible failure. Validated independently (RED reproduced on all four new subtests), then fixed: empty parsed ID now returns `session_create_failed` / `cannot parse created session: empty id`. Docstring invariant updated to name the empty-ID shape. Mutation-verified: removing the one-line guard fails 6 test lines (unit + handler, both bodies).
- Reviewer Finding 2 (non-blocking, acknowledged): a 502-after-commit create retried 3× can leak up to three orphan sessions (`deleteOpencodeSession` runs only on the success path). Pre-PR leaked one per fire attempt cycle as well (next tick re-fired the create); the create POST carries no idempotency key to dedupe on. No double-execution risk (reviewer verified: create failure precedes any message POST). Documented here as known behavior; mitigation needs an upstream idempotent-create surface — out of scope for this PR.

### CI flake fix (out-of-lane, flagged to orchestrator)
- The required "Test (full suite, race detector)" check failed twice on this PR from `cmd/relay-router` tests landed with #1432 (US-72.2) — this branch's diff cannot reach that package. Both are test-side async races, fixed test-only:
  - `TestDRAfterRotationStaysMonotonic` (byo_review2_test.go): `byoWatchDelete` runs `Bootstrap` in a goroutine and the pub-Secret rewrite lands AFTER the keypair write inside it; the test's `Eventually` settled only the keypair secret, then asserted pub generation — read the stale pub (CI: kp=3, pub=2, twice). Now waits for BOTH secrets to settle (kp > 2 AND pub == kp) before asserting.
  - `TestMetricsScrape` (byo_review1_test.go): the request counter incs after the server's stream copy loop completes; the test closed the response body without draining and scraped /metrics once, immediately. Now drains to EOF and settle-polls the scrape (bounded 5s).
  - Verified: 20× `-race` runs of the DR test, 10× of MetricsScrape, plus the full `./cmd/relay-router/` package under `-race` — all green. Production code untouched.

---

## Key Decisions

1. **Three-value return over an error type** for `createOpencodeSession` — the wire needs `(code, detail)` strings anyway and the file's style is plain returns; single caller (verified by grep across cmd/api/pkg).
2. **Create-leg 5xx retry is safe where timeout retry is not**: session create sends no prompt — a fresh attempt cannot double-execute anything. This is why `session_create_failed`+5xx joins the class while `script_timeout` stays out (each timeout attempt is a fresh session that MAY have a still-running background turn).
3. **Transport-to-opencode on create is surfaced but not retried** — matches the message leg's posture (`script_failed` + raw transport detail never matched the classifier); retrying a crash-looping opencode would only burn latency.
4. **`session_not_found` keeps a second, legitimate producer** (message-leg 404, workflow_execute.go) — only the create-leg producer was wrong; the split removes exactly that conflation.

### Assumptions stated and validated (Rule 7)

- The WIP one-liner landed → verified `git show 24fca1c2` (engine.go ScriptPath branch calls `executeWithRetry`).
- Error codes surface as HTTP-200 `{errorCode, detail}` → verified `writeWorkflowError` + asserted in the new handler tests.
- `createOpencodeSession` has one production caller → verified via `grep -rn` (only `execAgentNode`).
- `scriptedExecutor` results are consumed in call order across legs → verified reading engine_test.go:1927-1945 (sequential index, ignores req).

---

## Blockers

None. (Noted: `golangci-lint` was not installed in the pod; installed to `/tmp/opencode/bin` via `go install ...@latest` (v2.13.2) — `make lint` passes with 0 issues. The agentd package is timing-flaky under this pod's load: one full run failed at ~340s in `TestManagedProcess_StopDuringCrashBackoffReturns`; it passes standalone and passed on subsequent full runs (269s plain, 317s -race). Pre-existing, managed-process tests untouched by this diff.)

---

## Tests Run

- `go test -timeout 300s ./api/internal/workflows/` — ok (26.7s)
- `go test -timeout 600s -race ./api/internal/workflows/` — ok (37.8s)
- `go test -timeout 900s -race ./cmd/workspace-agentd/` — ok (317.4s)
- `go build ./...` — ok
- `make lint` — 0 issues (golangci-lint v2.13.2)
- `bash -n local/issue-1417-templating-e2e.sh` — ok
- TDD red evidence (pre-fix failures witnessed):
  - agentd: `TestWorkflowExecuteHandler_AgentNodeSessionCreate5xx`, `...TransportError` FAILED on the pre-split collapse
  - engine: `TestExecuteWithRetry_SessionCreateFailedTransient5xxRecovers`, `TestScheduler_RoutineFireRetriesSessionCreate5xx` FAILED pre-classifier
- Mutation evidence (fix reverted → red, restored → green):
  1. #1458 one-liner revert → `TestScheduler_RoutineFireScriptLegRetriesTransient5xx` FAIL
  2. classifier `session_create_failed` arm removed → both #1457 engine tests FAIL
  3. agentd collapse restored at the call site → both create-leg handler tests FAIL
  4. r1 empty-ID guard removed → 6 FAIL lines (unit + handler empty-ID subtests)

---

## Next Steps

- none for this branch — awaits adversarial review, then orchestrator merge + release train (CHANGELOG/Chart/appVersion are the orchestrator's lane).

---

## Files Modified

- `api/internal/workflows/engine.go` — retryableAgentdFailure learns session_create_failed; comment
- `api/internal/workflows/engine_test.go` — 6 new tests (classifier unit ×2, wiring pins/guards ×4)
- `cmd/workspace-agentd/workflow_execute.go` — createOpencodeSession split + execAgentNode passthrough + body-close fix + r1 empty-ID guard
- `cmd/workspace-agentd/workflow_execute_test.go` — 6 new tests + `withStubAgentAddr` helper (t.Cleanup) + r1 empty-ID rows
- `cmd/relay-router/byo_review1_test.go` — TestMetricsScrape async-settle fix (CI flake, #1432)
- `cmd/relay-router/byo_review2_test.go` — TestDRAfterRotationStaysMonotonic async-settle fix (CI flake, #1432)
- `local/issue-1417-templating-e2e.sh` — T7 wiring-pin list extended
- `worklogs/NNNN_2026-09-18_retry-coverage-session-create-script-leg.md` — this worklog
