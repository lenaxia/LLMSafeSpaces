# Worklog: #1327 — keyed harness POST on the workflow agent node (dedupe key from execution identity)

**Date:** 2026-09-17
**Session:** workflow_execute.go POSTed /session/{id}/message keyless — workflow reruns/retries re-POSTed the same prompt as fresh unkeyed transcript appends, the duplication class #1315 closed for the outbox path (this was the follow-up flagged in PR #1323's review).
**Status:** Complete

---

## Objective

Give the workflow agent-node message a deterministic dedupe key — `msg_wf_<workflowID>_<nodeID>_<runID>`-shaped, stable across retries of the SAME logical node execution, distinct across runs — and include it in the POST body the way the harness's existing mechanism expects (`messageID`, the seam #1323 added for the admission path).

## What was validated first

- **Mechanism:** the pinned opencode 1.18.15 harness validates the `msg_` prefix, uses a `messageID` verbatim as the user message's store ID, and UPSERTS on same-ID re-POST (repo-recorded G1 live probe, PR #1323). The workflow executor's body carried no `messageID` at all — every retry appended unconditionally.
- **Retry reachability:** `Reconciler.executeNode` loops `attempt := 1..maxAttempts` re-dispatching the same `node.ID` with the same input (`engine.go`); `processPendingRoutineFire` re-executes pending fires, so the routine path (`routine-agent`) had the same exposure through the same agentd handler.

## Work Completed

- **Identity flows from the API; the harness shape stays in agentd.** `NodeExecRequest` (api) / `workflowExecuteRequest` (agentd) gain typed `WorkflowID`/`RunID` fields (wire: `workflowId`/`runId`, `omitempty`). `executeNode` stamps `run.WorkflowID`/`run.ID` on every dispatch (retry-stable by construction); `executeRoutine` stamps the routine's trigger/fire IDs.
- **agentd derives the key at the harness write:** `workflowAgentMessageKey` → `msg_wf_<wf>_<node>_<run>-<hash8>`, included as `messageID` in the POST body — same field + semantics as the #1323 admission path. The `msg_` prefix stays out of `api/` (harness vocabulary; session-contract discipline).
- **Hash suffix over the raw identity** (`sha256(wf ␟ node ␟ run)[:4]`, unit-separator framed): node IDs are user-authored and any sanitization folds distinct raw IDs together (`a/b` vs `a_b` → both `a_b`); without the suffix one node's POST could upsert ANOTHER node's transcript message (silent data loss). Components sanitized to `[A-Za-z0-9._-]` (16-char component cap, 64-char key cap).
- **Partial identity ⇒ keyless (fail open).** Any missing component keeps the exact pre-#1327 keyless wire body — a partial key would be shared by every identity-less dispatch and collapse distinct executions. Mixed-fleet safe both directions (old agentd ignores the new fields; new agentd tolerates their absence).
- **Testability en route:** the three harness calls in `workflow_execute.go` (message POST, session create, session delete) bypassed the `getAgentAddr()`/`agentAddrAtomic` seam every other harness caller uses, hard-coding `127.0.0.1:4096` — untestable. Switched to the seam; production-identical (`main.go` stores `http://localhost:4096`, same pod netns).

## Key Decisions

- **Routine paths GET keys** (trigger ID as the workflow analog, fire ID as the run analog): `processPendingRoutineFire` re-drives the SAME fire after an API restart, re-POSTing the same prompt — the exact exposure #1327 describes. A trigger is the routine's reusable definition, a fire its logical execution, matching the key's stability/distinctness contract. `routine-script` carries NO identity: script nodes never write to the harness transcript, so the key has no consumer there.
- **`nodeRunID` excluded** — it is minted per attempt; `run.ID`+`node.ID` (or fire.ID+trigger.ID) are the retry-stable parts, per the issue's stability requirement.
- **Ephemeral nuance:** ephemeral retries mint a fresh session each dispatch, so the upsert bound is per-session (the persistent-session case); orphaned sessions on failure leak today regardless — unchanged, out of scope.
- **One-time migration note (from the issue triage):** executions already written keyless before this deploys will append one keyed copy on their next retry — bounded to the deploy instant; the unbounded loop is what closes here.

## Blockers

None.

## Tests Run

- `cmd/workspace-agentd/workflow_dedupe_test.go` (new, 10 tests): keyed body golden (`msg_wf_wf-1_n1_run-1-5a812c0d`, hash computed independently), retry re-POSTs the same key into the same session, distinct runs key apart, keyless-without-identity exact-shape compat, partial-identity table, harness-500 → `script_failed` + retry reuses the key, sanitization incl. the `a/b`/`a_b` hash-apart case, ephemeral create→POST→delete flow with session-independent key, bounded/deterministic derivation unit.
- `api/internal/workflows/engine_identity_test.go` (new, 4 tests): `executeNode` carries run identity, retry loop keeps identity stable (in-band failure then success), routine dispatch carries trigger/fire (and routine-script carries none), wire pin that `HTTPAgentExecutor` marshals `workflowId`/`runId` — the exact spellings agentd decodes.
- Both suites red pre-fix by construction (verified by stashing the source changes: `undefined: workflowAgentMessageKey`, unknown fields).
- `go test -race -count=1 ./cmd/workspace-agentd/` (304s) and `./api/internal/workflows/` (9.4s) — pass. `go build ./...`, `go vet`, `gofmt` clean; `golangci-lint run` on both touched packages — 0 issues.

## Next Steps

PR → AI review; follows #1323's merged admission-path seam.

## Files Modified

- api/internal/workflows/engine.go (+engine_identity_test.go)
- cmd/workspace-agentd/workflow_execute.go (+workflow_dedupe_test.go)
