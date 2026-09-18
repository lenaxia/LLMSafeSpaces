# Worklog: #1452 — preserved routine sessions invisible in platform session list

**Date:** 2026-09-18
**Session:** Localize + fix issue #1452 (routine sessions with preserveSession=always never appear in the session list); branch `fix/1452-routine-session-listing`
**Status:** Complete

---

## Objective

Determine with evidence whether routine-trigger sessions preserved via `preserveSession=always` are (a) registered in opencode's store but filtered by its list route, or (b) never registered — then fix the gap so preserved routine sessions are externally discoverable from the workspace side.

---

## Work Completed

### Phase 1 — localization (evidence-based)

- Read issue #1452 + #1380 + the pre-existing `/analyze` localization comment; independently verified every load-bearing claim.
- **Live-pod experiment** (this workspace, opencode 1.18.15 pinned): created routine trigger `wt1452-cleanup-listing-probe` (cron `* * * * *`, `preserveSession=always`, `captureMode=full`); two fires delivered (`3a2a390a` 21:37:07Z → `ses_f498c5af8ffe…`, `826431c9` 21:38:07Z → `ses_f498b7092ffe…`). Both session IDs **appear in the native `session_list` tool** (which passes through opencode's live `GET /session` verbatim — `mcp_server.go:468` → `SessionListRaw`), and both transcripts are readable via `session_read` (keyed `msg_wf_<trigger>_routine-agent_<fire>` per #1327). Trigger deleted and probe sessions renamed `wt1452-cleanup-*` afterwards.
- **Code verification:** `GET /workspaces/:id/sessions` (`router.go:1422`) → `ListWorkspaceSessions` (`workspace_service.go:1792`) → `sessionIndex.ListByWorkspace` — PostgreSQL `session_index` only; the adapter live list (`ProxyHandler.ListSessions`) is mounted on no production route. Every `session_index` INSERT site is adapter-route or usage-stream-gated (`proxy_handlers.go:67`, `proxy_adapter_crosscutting.go:121`, `proxy_lifecycle.go:443`, `proxy_usagestream.go:264`); the routine fire path (`engine.go executeRoutine`) goes engine → agentd directly and wrote only `session_origins`. Sidebar decorates index rows with origins (`Sidebar.tsx:548-553`, badge at :899-908) — an origins row without an index row contributes nothing.
- **Conclusion:** both issue unknowns answered negatively for the pod (row IS registered, list does NOT filter). The gap is platform-side: no `session_index` write path for routine fires. Posted as [issuecomment-5736502312](https://github.com/lenaxia/LLMSafeSpaces/issues/1452#issuecomment-5736502312).

### Phase 2 — fix (TDD)

- **RED:** `api/internal/workflows/engine_routine_sessionindex_test.go` — 6 tests over a recording `SessionIndexWriter` mock (PreserveAlways indexes; PreserveNever doesn't; PreserveOnFailure-delete-succeeds doesn't; delete-fails does — mirroring the #762 origin fallback; nil writer no-panic; UpsertTitle error non-fatal and independent of the message/origin writes). Confirmed failing (compile: unknown field).
- **GREEN:** `Scheduler.SessionIndex SessionIndexWriter` (new 2-method caller-shaped interface, satisfied by `sessionindex.Service`); `indexPreservedSession` helper called inside the existing `sessionID != "" && !sessionDeleted` block (same gate as `RecordSessionOrigin` — indexes exactly the sessions that still exist); best-effort semantics (errors logged, never fail the fire; mirrors `PasswordProvider` nil-skip pattern). Wired `SessionIndex: sessionIndexSvc` in `app.go` (`sessionIndexSvc` already in scope at the `wfScheduler` literal). No wire-contract, agentd, or opencode changes.

### Review round 1 (CHANGES_REQUESTED → addressed)

- **Integration seam:** `TestExecuteRoutine_SessionIndexIntegration` — engine drives the REAL `sessionindex.Service` (started drainer + `MockDatabaseService`): asserts `UpsertSessionTitle` hits the DB and the queued `RecordMessage` drains into `UpsertSessionMessage` (Stop() drains-and-joins, so the mock is quiescent when asserted — a first Eventually-poll draft raced the drainer on the mock's Calls and failed `-race`, which is exactly the concurrency the drainer introduces). Covers the queue/drain the recording mock bypassed.
- **App wiring pin:** `api/internal/app/workflow_scheduler_wiring_test.go` — source-scan pin (handlers' `no_session_derivation_test.go` precedent): `app.go` must contain `SessionIndex: sessionIndexSvc` inside the `apiwf.Scheduler` literal. Deleting the one-line production wiring now fails a test (previously every test stayed green — the reviewer reproduced the bug by deleting exactly that line).
- **e2e rows (kind nightly):** `local/issue1452-routine-session-index-e2e.sh` — R1 happy: signed-webhook PreserveAlways routine fires → delivered fire's captured `session_id` appears in `GET /workspaces/:id/sessions` titled with the trigger name; R2 negative: PreserveNever delivers but adds no row. Deterministic (HMAC webhook firing, no cron wait); registered in `.github/workflows/e2e-nightly.yml` (port 18087). Pinned by `local/issue_1452_e2e_script_test.go` (bash syntax, row/assertion needles, workflow registration) per the issue-1410 script-test pattern.
- **e2e unhappy (index-write failure mid-fire): deliberately NOT row-ed** — isolating a session_index write failure at cluster scale means partitioning Postgres, which also breaks `UpdateTriggerFireResult` itself (the fire can never be marked delivered), so the row would assert nothing the unit row `TestExecuteRoutine_IndexTitleError_NonFatal` doesn't already pin exactly. Documented here for the reviewer to adjudicate.
- **Robustness findings — documented decisions** (in `indexPreservedSession`'s doc comment): (a) title re-stamp only overlaps a user rename on a re-driven pending fire (each fire owns its session — verified live: two fires → two sessions), accepted like the re-drive message-count double-count; (b) `has_unread` true until opened = normal unopened-session semantics (MarkSessionSeen clears); (c) startup window verified non-defect (events buffer 1024, flushed at Start); (d) shutdown race loses at most one `last_message_at` (title row persists synchronously) — accepted, low impact.
- **Follow-ups surfaced (not blockers per review):** failed PreserveOnFailure fires never index their preserved session (mirrors the pre-existing `RecordSessionOrigin` delivery-only blind spot — the failure branch has no session_id to index); duplicate PR #1461 from a parallel workflow escalated to the orchestrator.

### Orchestrator adjudication + #1461 absorption

- Orchestrator: **#1464 survives, #1461 closed** (parallel /fix automation had implemented the same Option A; claim + localization + review completeness favored this PR).
- **Absorbed from #1461** (with credit): nil-logger guards in `sessionindex.Service.Start`/`Stop` — `RecordMessage` and `drain` already nil-guard, so `New(db, nil)` + `Start()` panicking was an internal inconsistency this PR's own integration test hit verbatim during development. Regression test `TestStartStop_NilLogger_NoPanic` (Start → RecordMessage → Stop on a nil-logger service, drain asserted); the engine integration test now uses the nil-logger construction it originally wanted.

---

## Key Decisions

- **Index at fire time (Option A from the /analyze comment), not read-time merge.** Follows the repo's index-at-write precedent ("the platform OWNS session CRUD; index where it happens"). Read-time merge would add a pod round-trip to every sidebar render.
- **Reuse `UpsertTitle` + `RecordMessage` rather than a new DB upsert.** Title gives the sidebar a display name (trigger name, matching the origin row's title); RecordMessage sets `last_message_at`/`message_count` so the row sorts by fire time and reads has-unread (last_seen NULL). Zero new DB surface.
- **Same gate as `RecordSessionOrigin`** — no product call left: sessions that still exist (PreserveAlways; PreserveOnFailure whose delete failed) are indexed; deleted/ephemeral sessions never are. Scope matches the issue exactly.
- **Best-effort, non-fatal** — the fire has already succeeded when the index write runs; an index outage logs and never flips a fire to failed.

### Assumptions stated and validated (Rule 7)

| # | Assumption | Validation |
|---|---|---|
| A1 | opencode v1.18.15 registers + lists routine-created sessions pod-side | Live experiment: both `ses_` IDs present in `session_list`; transcripts readable |
| A2 | The platform list surface reads only `session_index` | `router.go:1422` → `workspace_service.go:1792` → `ListByWorkspace` → `db.ListSessionIndex` |
| A3 | No existing index write fires for a routine fire | Routine path never touches proxy adapter routes; `Scheduler` had no index dependency pre-fix |
| A4 | `sessionindex.Service` satisfies the new interface | `app.go` assignment compiles (`SessionIndex: sessionIndexSvc`); method signatures match verbatim |
| A5 | Re-drive double-count risk (`processPendingRoutineFire` re-drives a pending fire → `message_count` +1 again) is acceptable | Same semantics as the adapter path (one RecordMessage per platform-observed send, retries included); re-drives only occur after a mid-fire process death; the transcript itself is deduped by the #1327 harness key |

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 300s -race -count=1 ./api/internal/workflows/` — ok (includes the 7 tests of this fix: 6 unit + 1 integration)
- `go test -timeout 120s -count=1 -run TestExecuteRoutine_ -v ./api/internal/workflows/` — all PASS
- `go test -timeout 60s -count=1 -run TestWorkflowScheduler_SessionIndexWired ./api/internal/app/` — PASS
- `go test -timeout 120s -count=1 -run TestIssue1452 ./local/` — 3/3 PASS (script syntax, row pins, workflow registration)
- `bash -n local/issue1452-routine-session-index-e2e.sh` — clean
- `go test -timeout 300s -race -count=1 ./api/internal/app/ ./api/internal/services/sessionindex/ ./api/internal/services/workspace/` — ok
- `go test -timeout 600s -race -count=1 ./cmd/workspace-agentd/ ./pkg/agent/...` — ok (pre-push gate)
- `go build ./...` (GOPROXY=direct) — exit 0
- Mutation-resistance of the pins: removing the `indexPreservedSession` call fails 3 tests; moving it outside the `!sessionDeleted` gate fails `DeleteSucceeds_DoesNotIndexSession`; deleting the nil-check fails `NilSessionIndex_StillDelivers` (panic); deleting the `app.go` wiring line fails `TestWorkflowScheduler_SessionIndexWired`; dropping an e2e row's assertion fails its needle pin.

---

## Next Steps

- Iterate through adversarial PR review until APPROVED; orchestrator (ses_f499ee9e6ffe52BJ8jxc2TEQQJ) merges.
- Live-pod re-verification of the fixed behavior is possible post-merge by re-running the probe (the fix is API-server-side, so the deployed platform must pick it up first).

---

## Files Modified

- `api/internal/workflows/engine.go` — `Scheduler.SessionIndex` field + `SessionIndexWriter` interface; `indexPreservedSession` helper; one call in `executeRoutine`'s preserved-session block
- `api/internal/workflows/engine_routine_sessionindex_test.go` — new (6 unit tests + recording mock + real-service integration test)
- `api/internal/app/app.go` — wire `SessionIndex: sessionIndexSvc` into the scheduler literal
- `api/internal/app/workflow_scheduler_wiring_test.go` — new (app wiring source pin)
- `local/issue1452-routine-session-index-e2e.sh` — new (kind e2e rows R1/R2)
- `local/issue_1452_e2e_script_test.go` — new (script structure pins)
- `.github/workflows/e2e-nightly.yml` — register the e2e script (port 18087)
- `api/internal/services/sessionindex/service.go` — nil-logger guards in Start/Stop (absorbed from #1461)
- `api/internal/services/sessionindex/service_test.go` — `TestStartStop_NilLogger_NoPanic`
