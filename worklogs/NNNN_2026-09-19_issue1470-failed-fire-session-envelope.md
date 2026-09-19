# Worklog: #1470 — failed-fire routine sessions recorded via the error-envelope sessionId

**Date:** 2026-09-19
**Session:** Close the delivery-only blind spot: routine fires that fail with a surviving session (and delivered fires with drifted output) must still origin-record + session_index their sessions; branch `fix/1470-failed-fire-session-origins`
**Status:** Complete

---

## Objective

Make every routine-fire session that EXISTS after its fire discoverable platform-side — including failed PreserveOnFailure/PreserveAlways fires (the exact sessions an operator wants to inspect) and delivered fires whose output envelope drifted. Filed by this lane in #1464's review; #1464 deliberately indexed only the delivered branch.

---

## Work Completed

- Region clearance verified: #1466 (#1457/#1458 lane) MERGED — `cmd/workspace-agentd/workflow_execute.go` open; worker 5 (wt-1465, #1469) confirmed zero file overlap (their surface is mcp_* + origin-plugin files); both sides noted the package-main co-tenancy rebase rule.
- **Contract (#1470's core decision): the node-execute envelope carries `sessionId` iff a session still exists when the node finished** — success and error envelopes alike, camelCase per the envelope convention. Reality over intent: preserved modes report their session on any outcome; ephemeral modes tear down and omit; a FAILED teardown reports the leak. Old API servers ignore the field; old agentd never sets it (engine falls back to output parsing — both skew directions safe).
- **agentd** (`workflow_execute.go`): `workflowExecuteError`/`workflowExecuteResponse` gain `sessionId,omitempty`; `writeWorkflowErrorSession` + `writeWorkflowSuccessSession` writers (the session-less writers delegate); `deleteOpencodeSession` now reports success (bool; fire-and-forget call sites unchanged); `execAgentNode` gains `failAgentNode` — tears down ephemerals on EVERY failure path (previously only success tore down — failed ephemeral fires leaked their sessions) under `context.WithoutCancel` + 10s bound (the timeout leg's dead context can neither skip nor wedge the cleanup DELETE), then writes the surviving id. `session_not_found` stays session-less (a 404 means no session exists). Success path: `session_deleted` is now set only when the delete actually succeeded and a leaked ephemeral reports its real id in both payload and envelope.
- **engine** (`engine.go`): `NodeExecResponse.SessionID`; `routineSessionID` prefers the envelope field, falls back to the old output-payload parse; the failure branch (ErrorCode != "") records origin + index via the shared `recordRoutineSessionArtifacts` helper when the envelope reports a surviving session; the delivered branch uses the same helper (refactor, no behavior change for the current wire). The transport-error branch stays blind (no response object) — documented residual.
- **Retry classifier untouched**: `retryableAgentdFailure` keys on ErrorCode/Detail strings and transport-error text; the added field and teardown change no code paths it inspects (`script_timeout` still 504, still non-retryable).
- Live-pod probe (this workspace, pinned 1.18.15): opencode returns **200** for a message POST with an unknown `agentID` — so there is NO deterministic API-side lever for an agent-leg failure in a routine e2e row (the levers that exist — script-node failure, input validation, bad cron — all fail BEFORE a session exists). Failure-path coverage is therefore unit + handler-integration level; documented for the reviewer.

---

## Key Decisions

- **Envelope existence contract (reality over intent).** A leaked ephemeral (teardown failed) is REPORTED, not hidden: it exists, it must stay discoverable for cleanup, and the engine's single gate (`sessionId != ""`) stays honest. Engine-side consequence: a delivered PreserveNever fire whose teardown failed now records its leaked session (previously invisible) — intended.
- **Ephemeral teardown on failure paths** was required for correct gating: without it, failed ephemeral fires would leak sessions AND (with failure-branch recording) index them — surfacing sessions the operator asked to never see. Now they're gone, hence omitted, hence unrecorded.
- **Timeout leg** (504) carries sessionId in its body (future-proof) but the engine discards non-200 bodies — the session stays unrecorded there; documented, not fixed (changing 504→200 would alter failure-payload shapes mid-fix; separate decision if ever needed).
- **Intermediate retry attempts** (executeWithRetry swallows all but the final response) leak their sessions unrecorded — pre-existing for the delivered branch, inherent to engine-level retries, out of scope.

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | Region open (workflow_execute.go) | #1466 merged on main (git log); orchestrator's assignment; worker-5 confirmation |
| A2 | Engine-level retries create a fresh session per attempt (so failure-path teardown can't delete a session a retry still needs) | execAgentNode creates per-dispatch; retry re-enters Execute — new dispatch, new session |
| A3 | `context.WithoutCancel` available | Go 1.26 toolchain |
| A4 | Old-engine/new-agentd skew harmless | Extra JSON field ignored by `json.Unmarshal` into the old struct |
| A5 | New-engine/old-agentd skew harmless | Envelope field absent → `routineSessionID` falls back to output parse (pinned by test) |
| A6 | No deterministic e2e lever for agent-leg failure | Live probe: unknown agentID → 200; no model override in the routine spec surface |

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 600s -race -count=1 ./cmd/workspace-agentd/` — ok (322s; includes 8 new envelope tests)
- `go test -timeout 400s -race -count=1 ./api/internal/workflows/` — ok (49s; includes 6 new failure/drift tests, all pre-verified RED)
- `go test -timeout 600s -race -count=1 ./api/internal/app/ ./local/` — ok
- `go build ./...` — exit 0; `make imports-check` clean; gofmt clean
- Mutation-resistance: dropping the failure-branch `recordRoutineSessionArtifacts` call fails 2 tests; reporting the id on `session_not_found` fails its negative test; skipping the failure-path teardown fails 2 agentd tests; removing the envelope preference in `routineSessionID` fails the drift-rescue test; the back-compat fallback is pinned by its own test.

---

## Next Steps

- Adversarial review loop until APPROVED; orchestrator merges (rebase first if #1469's lane lands in cmd/workspace-agentd package main first).
- If reviewers want the timeout-leg session reported: separate decision on mapping `script_timeout` to a 200 envelope (failure-payload shape change).

---

## Files Modified

- `cmd/workspace-agentd/workflow_execute.go` — envelope structs + sessionId writers; deleteOpencodeSession returns bool; failAgentNode with failure-path ephemeral teardown; success-path reality reporting
- `cmd/workspace-agentd/workflow_session_error_envelope_test.go` — new (8 tests + envelope decoder)
- `cmd/workspace-agentd/workflow_dedupe_test.go` — harness: messageStatus/garbageBody/failDeletes modes (additive, defaults off)
- `api/internal/workflows/engine.go` — NodeExecResponse.SessionID; routineSessionID; recordRoutineSessionArtifacts; failure-branch recording
- `api/internal/workflows/engine_routine_failure_session_test.go` — new (6 tests)
- `api/internal/workflows/engine_test.go` — mockAgentd.sessionIDs (additive)
