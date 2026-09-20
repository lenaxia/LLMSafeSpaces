# Worklog: #1476 DAG adoption — executeNode retry-cleanup for superseded attempt sessions

**Date:** 2026-09-20
**Session:** Apply the #1477 retry-cleanup pattern to the workflow-run path (the "separate decision" deferred from #1476/#1477); branch `fix/dag-retry-session-cleanup` (off 89fe639b — main moved one repolint chore past the briefed bc501185; noted here per transparency)
**Status:** Complete

---

## Objective

executeNode's `maxAttempts` loop leaks superseded agent-node attempt sessions exactly like executeWithRetry did pre-#1477 — adopt the cleanup with the DAG-specific semantics enumerated FIRST (Rule 7, per delegation).

---

## Rule-7 enumeration: node-retry vs attempt-retry semantics (the mapping, decided BEFORE coding)

| Dimension | Routine (`executeWithRetry` + #1477) | DAG (`executeNode`, this change) | Mapping |
|---|---|---|---|
| Retry trigger | Classifier-gated: ONLY transient 5xx shapes (`retryableAgentdFailure`) | **Unconditional** on any failed attempt (author-set `maxAttempts`; deterministic errors retry too) | Clean: "superseded" = any non-final failed attempt — the orphan class is BROADER, and the orphan is equally real (the retry supersedes it regardless of error class). Pinned by `DeterministicErrorIntermediateAlsoCleaned`. |
| Cleanup gate | retry will follow (`a < attempts`) && envelope carries SessionID | Same + **spec-pinned sessions never cleaned** | NEW gate: `data.sessionId` is the author's SHARED session — every attempt reuses it (agentd: `createdEphemeral=false`, no teardown by design); cleaning any attempt's envelope id would delete the live shared session. Parsed once per agent node; unparseable/non-agent specs pin nothing. Pinned by `PinnedSessionNeverCleaned`. |
| Who records the final session | Engine records origin+index (delivered/failed) | **Nobody** — DAG sessions are never origin-recorded or indexed (the DAG-side #1470-class gap, explicitly out of scope) | Clean: cleanup is orphan-removal only; FINAL-attempt sessions are never cleaned (nothing supersedes them — deleting them would destroy an author's failure artifact with no recording to compensate). Pinned by `FinalAttemptSessionNotCleaned`. |
| Ghost-record scrub (r4's lesson) | Required — the returned resp feeds the recording branch | **N/A** — executeNode returns (output, branch, err); failed responses are discarded entirely, no recording consumer exists | The r4 invariant (`cleanedSession == resp.SessionID`) degenerates: each iteration's resp is its own; no cross-attempt leak is possible. Documented rather than defensively coded. |
| Transport-error attempts | resp nil → nothing to clean | Same | Same gate (`resp != nil`). |
| Ephemeral specs | agentd tears down (#1471) → no envelope id → no-op | Same; a teardown-FAILED ephemeral reports its leak → cleaning it is correct repair | Identical semantics. |

**Verdict: the mapping is clean** with the two documented deltas (unconditional-retry breadth; the pinned-session gate). No design stop needed.

---

## Work Completed

- `Reconciler` gains `PasswordProvider` + `AgentdPort` (nil provider → cleanup skipped, unwired/embedded reconcilers keep today's behavior — pinned).
- `deleteSessionAuthorized` extracted as the caller-neutral core (the Scheduler method delegates — zero behavior change; every log line keeps its purpose label). New purpose: `dag_node_retry_cleanup`.
- `executeNode`: parses the agent spec's pinned-session once before the loop; in the `attempt < maxAttempts` block, cleans the superseded attempt's envelope session (best-effort; failures log inside the delete path and the retry proceeds).
- app.go wires `PasswordProvider: proxyHandler` (the same wiring the Scheduler uses; port stays default).

---

## Key Decisions

- **Cleanup inline in the existing retry block** rather than a callback parameter — executeNode's loop is caller-local (no shared helper like executeWithRetry to parameterize); a callback would be indirection with one caller. The pattern (gates, purpose label, honest 204/502 route) is reused verbatim.
- **Spec parse is fail-closed for cleanup**: unparseable spec → not pinned → cleanup COULD run — but an unparseable agent spec fails the dispatch with no session anyway (agentd rejects invalid_node_data pre-create), so the gate no-ops naturally. Pinned sessions are the only skip.

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | Spec-pinned sessions are reused across attempts (not per-attempt) | execAgentNode: `sessionID := data.SessionID; if != "" → no create`, `createdEphemeral=false` → no failure teardown |
| A2 | Final-attempt sessions have no recording consumer to lose | grep: DAG path never calls RecordSessionOrigin/index (routine-only) |
| A3 | Script/condition nodes never carry envelope sessions | Only execAgentNode stamps SessionID (#1471) |
| A4 | The repolint-numbering commit between the briefed base and my branch is inert | 89fe639b is `chore(repolint): assign worklog numbers [skip ci]` |

---

## Blockers

None.

---

## Tests Run

- `go test -run "TestExecuteNode_" ./api/internal/workflows/` — ok (7 new red-first rows + the existing identity/retry-stability pins)
- Full package `./api/internal/workflows/` + `./api/internal/app/` — ok 75.5s (all #1464/#1471/#1476/#1477/#1481 suites; race deferred to CI per memory directive)
- `go build ./...` — exit 0 (GOPROXY=direct; caches cold post-refresh, first builds slow as briefed)
- Mutation checks: dropping the cleanup call fails rows 1/2/7; removing the pinned gate fails row 4; cleaning on the final attempt fails row 3; unwiring the provider fails row 6's outcome-neutral twin (row 5).

---

## Next Steps

- Adversarial review loop until APPROVED; orchestrator merges (may ride v0.34.6 if it lands before the cut).
- The DAG-side session-recording gap (DAG sessions never origin-recorded/indexed — the #1470 class) remains the deferred "separate decision" it has always been; this PR changes only orphan cleanup.

---

## Files Modified

- `api/internal/workflows/engine.go` — Reconciler fields; deleteSessionAuthorized extraction + dag_node_retry_cleanup purpose; executeNode cleanup with the pinned-session gate; agentdPort helper
- `api/internal/workflows/engine_dag_retry_cleanup_test.go` — new (7 tests + dagCleanupHarness)
- `api/internal/app/app.go` — Reconciler PasswordProvider wiring
