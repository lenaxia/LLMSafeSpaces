# Worklog: Typed waits on claims — board_wait verb, wait_on/wait_reason, wire+health+UI

**Date:** 2026-11-18
**Session:** Implement `tsk-ebony-javelin-tramps` (typed wait on claims per the patched `reports/specs/claim-wait-lifecycle-low-level-design.md` §1–§3), then address PR #2104 round-0 review findings.
**Status:** Complete

---

## Objective

Land the typed-wait story: migration `000060_board_typed_wait` (wait_on/wait_reason columns, no backfill, COALESCE read rule), the `board_wait` worker verb (MCP+REST+agentd parity), event-wait visibility on GetTask wire / board_health `waiting` section / UI badge, the full wait-clear wiring across every advance/mint/reclaim/terminal site, both ladder-boundary pins — then iterate PR #2104 to APPROVED with green CI.

---

## Work Completed

### Implementation (commit 617741bcc)

- Migration `000060_board_typed_wait` mirrored in `api/migrations/` and `helm/migrations/`: `wait_on` TEXT CHECK in ('user','event') NULL-default, `wait_reason` TEXT, additive `.down` (drops constraint + columns; events are history and stay). No backfill — pre-verb flagged rows render `wait_on='user'` by COALESCE at the read edge (`pkg/board/store.go` scanTask; `pkg/board/rows.go` docs).
- `pkg/board/wait.go` (new): `DeclareWait` — waitOn='user' is today's flag-op enter byte-identically (pending_input=true, pending_origin='worker', wait_on='user', wait_reason; same `pending_input_flag` deterministic event id + payload as the move_stage alias), waitOn='event' parks the claim (pending_input stays FALSE, one active event-wait per story, guard `wait_already_declared`), condition parsed+validated at the verb edge (`parseWaitCondition`: closed grammar `pr_merged(N) | ci_green(N) | story_done(tsk-…)`) and recorded ONLY in the `wait_declared` event payload. Conversions both ways audited (`wait_cleared` events).
- Wait clears wired at every site: claim/task_start mints, persona mint, forward stage moves (`updateStageTx`), verdict completes, #1946 criteria grant, bounce, exitPendingReclaim, depose, rearm, rc1 wedge/reset/escalation terminals, rc2 drift landings. User-wait entry sites (flag ops, drift parks, pendingCoupling) stamp `wait_on='user'`.
- Wire/surfaces: GetTask `waitOn`/`waitReason`/`waitingOn` (camelCase, #2047 precedent); board_health `waiting` section `{short_id, wait_on, wait_reason, since}` on BOTH surfaces; UI badge (user=amber, event=sky) + tooltip + health panel row; REST route `POST /tasks/wait` (api handlers + router), MCP catalog + dispatcher + strict-args stamp + loopback client + origin-plugin injection.
- Pins: refusal matrix (no/wrong token, bad grammar, user+condition, missing/placeholder/over-cap reason, unknown waitOn, wrong stage, nothing-landed), second-event-wait, conversions, forward-move/mint/pending-exit clears, pendingCoupling hygiene, request-id replay, `TestPin_EventWait_StaysLadderCitizen`, `TestPin_UserWait_FreezesLadder_Unchanged`. The victims scan's `NOT t.pending_input` predicate and `due_mark.go` (#2100) untouched.

### Round-1 review fixes (this commit)

- `pkg/board/reads.go` `BoardHealthInWorkspace`: init `Waiting: []HealthWait{}` in the Health literal — the workspace surface emitted `"waiting": null` when empty while the owner surface emitted `[]` (nil-vs-empty wire split on one shared struct).
- `pkg/board/wait_integration_test.go`: new pin `TestDeclareWait_WorkspaceHealth_WaitingSection` — non-nil-empty before any wait, both wait types render with facts (wait_on/reason/since), sibling workspace sees its own empty section (no cross-workspace disclosure).
- PR #2104 body rewritten from the wrong verbatim (#2072's heartbeat-orbit budget text) to this story's actual summary + verification; dropped the misattributed `Closes #2044` (that issue landed via #2072/a9ad956b4).
- `sdks/openapi.yaml`: BoardTask gained `waitOn`/`waitReason`/`waitingOn` (CI round-1 caught `TestOpenAPISchemaParity_BoardTask` — the spec is the SDK contract source); BoardHealth gained the `waiting` array + `BoardHealthWait` schema (`{task_id, short_id, title, project_id, wait_on, wait_reason, since}`). `ack_waits` spec drift is pre-existing and left for its own lane.
- This worklog (round-0 style finding).

---

## Key Decisions

- Event ids carry a `:<nanos>:<seq>` suffix beyond the charter's `wait:<taskID>:<claimToken>` — the persona-escalation nanos precedent, so within-episode re-declarations get their own events; guards converge replays. Documented in-code; reviewer accepted (summary called the implementation in good shape).
- An event-wait on a flagged row CONVERTS (flag clears) and vice versa, each audited as `wait_cleared` — the flag and the event-wait are mutually exclusive row states, so pendingCoupling's auto-stage machinery can't re-stage under an event-wait.
- Grammar is closed and refused loudly at the verb edge (nothing executable crosses the worker surface); the broker shares `parseWaitCondition`.
- `wait_reason` column uncapped TEXT (house posture for guarded prose); 512-byte cap enforced at the verb edge via the textguard seam.

---

## Blockers

None. (Local sandbox has no Postgres/Docker — DB-gated integration pins verified in CI's postgres:16 job and by the reviewer's own `-race` run against a real migrated Postgres.)

---

## Tests Run

- `make fmt-check` — clean.
- `go build ./...` — green.
- `go vet ./pkg/board/... ./api/internal/handlers/... ./cmd/workspace-agentd/` — clean.
- `go test ./pkg/board/...` — ok (unit: parser/compose/reason-validation; PG-gated suite skips locally, runs in CI).
- `go test ./api/internal/handlers/ -run 'TestBoard|TestWait|Parity' -count=1` — ok.
- `go test ./cmd/workspace-agentd/ -run 'TestMCP|TestBoard|TestStrict' -count=1` — ok. (One full-package watchdog flake under 92% disk pressure passes in isolation; unrelated to this diff.)
- `go test ./api/internal/server/ -run 'TestOpenAPISchemaParity' -count=1` — PASS (BoardTask parity restored after the openapi.yaml fix).
- CI (PR #2104 round 0): build green; all 14 wait integration pins green with `-race` per the reviewer's verification.

---

## Next Steps

- Reviewer re-review round 1 on PR #2104 (comment `/review` after push); orchestrator merges on APPROVED — do not self-merge.

---

## Files Modified

- `api/migrations/000060_board_typed_wait.up.sql`, `.down.sql` (new)
- `api/internal/handlers/board_automation.go`, `board_automation_test.go`, `board_guard_codes_parity_test.go`, `board_parity_test.go`
- `api/internal/server/router.go`
- `cmd/workspace-agentd/mcp_server.go`, `mcp_tools.go`, `mcp_board_tools_test.go`, `strict_board_args.go`
- `frontend/src/api/board.ts`, `boardErrorCodes.json`
- `frontend/src/components/board/StoryCard.tsx`, `StoryDrawer.tsx`
- `frontend/src/lib/boardRules.ts`
- `helm/migrations/000060_board_typed_wait.up.sql`, `.down.sql` (new)
- `pkg/agent/opencode/loopback.go`
- `pkg/board/board.go`, `criteria_met.go`, `depose.go`, `engine.go`, `persona.go`, `reads.go`, `rearm.go`, `reconciler.go`, `review.go`, `rows.go`, `stage.go`, `store.go`, `wait.go`, `wrapup.go` (`wait.go` new)
- `pkg/board/wait_integration_test.go`, `wait_test.go` (new)
- `runtimes/opencode/plugins/llmsafespaces-origin.js`
- `sdks/openapi.yaml` (BoardTask waitOn/waitReason/waitingOn; BoardHealth waiting + BoardHealthWait)
- `worklogs/NNNN_2026-11-18_board-typed-wait-verb.md` (this file)
