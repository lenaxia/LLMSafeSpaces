# Lane — reviewer personas can record criteria-met (#1946, owner ruling 2026-10-25) (PR #TBD)

Date: 2026-10-25 · Lane: lane-1946 (orchestrator novel-nutmeg-verifies) · Branch: fix/1946-reviewer-criteria-met · Base: origin/main c5f68348d.

## Objective — the owner ruling (verbatim)

> "Why is set criteria met manager only. It should not be."

Issue #1946 (live probe tsk-thematic-octave-tints, project fixes-verification-043, 2026-10-25): the auto-review reviewer persona (#1941 stage-persona claim-then-spawn under the #1939 flipped defaults) verified both acceptance criteria MET with evidence, then hit two composing guards — move_stage→done refused by the done guard (R2: flags still false) and board_update_task/setCriteriaMet refused for the reviewer session ("manager board tools attach to user-collaborator sessions only", ruling #1747 §5). Auto-review cannot close a task end-to-end. Direction per the recorded interpretation: reviewer-persona claims GET setCriteriaMet, scoped to their OWN claimed row; task-worker self-certification REMAINS forbidden. Bonus observation on the issue (idempotency keys re-evaluate against current guard state) — noted there as a documentation item for R2, out of this lane's scope.

## Validation on origin/main (c5f68348d) — the refusal path + the seam

- Tier 1 (agentd, pre-wire): `cmd/workspace-agentd/board_manager_tools.go` `boardManagerSession` — `board_update_task` sits in the manager dispatch set; a session recorded in the worker registry (`boardWorkerSessions.isWorker`, spawn-time append-only JSONL) refuses with "this session was spawned as a board worker — manager board tools attach to user-collaborator sessions only (ruling #1747 §5)". A reviewer persona IS a spawned board worker (NodeID "board-worker") → refused. Pinned live by `TestMCPBoard_ManagerGrantGate` (ran green on main pre-change).
- Tier 2 (platform belt): `api/internal/handlers/board_automation_manager.go` `resolveManager` → `IsWorkerSession` (live-assignee SQL, `pkg/board/pause.go`) — 403 for live assignees; fail-closed 503 on belt errors. The reviewer is the in_review assignee → refused.
- The seam (#1941 mint provenance): the persona mint (`pkg/board/persona.go` `personaClaimMintTx`/`personaClaimSQL`) writes the SAME row shape as any claim — its provenance lived only in the `persona_claim` audit event (payload.persona) and the spawned `Claim` envelope (`Persona`, `Stage`); NOTHING on the tasks row distinguished a reviewer-persona claim from a task-worker claim. in_review tokens are minted by nothing else (stage entries with no persona clear token+lease; bounce/exit/grant mints are in_progress).
- NOT already solved: no in-repo references to #1946; the reviewer path dead-ends at both tiers. Proceeded to fix.

## Change — the scoped grant (all three axes narrow)

| # | Site | Change |
|---|---|---|
| 1 | `api/migrations/000050_reviewer_criteria_met.{up,down}.sql` + helm mirror | `tasks.claim_persona text NULL` — the #1941 mint's provenance made durable. NULL on every ordinary claim; the ONLY in_review mint writes it. |
| 2 | `pkg/board/persona.go` | `personaClaimSQL` stamps `claim_persona=$4` (the persona name); `personaClaimMintTx` takes it; both callers (stage entry, reconciler re-arm) pass it. |
| 3 | `pkg/board/{stage,engine,rearm,reconciler,review}.go` | every non-persona mint and every token-clearing write sets `claim_persona=NULL` (worker grant, bounce re-claim, pending-input exit re-claim, throttle requeue, forward exits, lapse/re-arm/drift/review-settle clears) — the invariant: non-NULL ⟺ a persona episode's token holds the row. |
| 4 | `pkg/board/criteria_met.go` (new) | `Service.SetCriteriaMet` — grant guard tier 1 (stage=in_review AND claim_persona non-empty, else GuardManagerSurface carrying the §5 sentence verbatim inside its new refusal text — the task-worker refusal), tier 2 (live claim-token arbitration), per-criterion bounds via `validateCriteriaUpdate`, row-locked server re-arbitration + #1747 pause guard inside the tx, guarded UPDATE, per-criterion audit event (action `criteria_met`: index, met, criterion text, persona). |
| 5 | `api/internal/handlers/board_automation.go` | worker route handler `BoardSetCriteriaMet`: strict loud vocabulary (allowed keys: taskId/shortId/claimToken/setCriteriaMet/requestId/workspaceID/lsp_injected_session — anything else refuses BY NAME), the #1837 P2b MutatedCriterion echo; `boardService` interface widened. |
| 6 | `api/internal/server/router.go` | `POST /internal/v1/automation/board/tasks/set-criteria-met` (worker group; parity pin updated). |
| 7 | `pkg/agent/opencode/loopback.go` | `BoardSetCriteriaMet` (boardWorkerPost). |
| 8 | `cmd/workspace-agentd/mcp_tools.go` | `board_update_task` dual-routes on claimToken presence (the #1832 WS2 board_move_stage shape): token → worker route; token-less → manager path byte-identical (tier-1 gate unchanged). |
| 9 | `cmd/workspace-agentd/mcp_server.go` | tool schema documents the claimToken route + grant scope. |
| 10 | `pkg/board/persona.go` `DefaultReviewerPersona` | charter teaches the self-completing loop: verify → record flags (board_update_task with claimToken) → move done. |
| 11 | `design/0067 §10.9`, `CHANGELOG [Unreleased]` | amendment + release notes. |

## move_stage→done carrying criteria assertions — considered, NOT implemented

The issue's alternative direction (done-move carrying criteria-met assertions) composes poorly with R2: the Done guard exists precisely so flags are recorded judgments, not transition riders; an assertion-carrying done would re-create self-certification pressure at the weakest seam and split the audit trail (flags recorded inside a transition event vs the per-criterion criteria_met events). The scoped grant + the existing worker move_stage (in_review→done needs no token guard) already compose: reviewer sets flags, reviewer moves done, doneGuard passes on the recorded flags. Implemented the SCOPED GRANT only, per the fix scope.

## Tests (red-first reasoning: each pins a behavior the pre-change engine could not produce)

`pkg/board/criteria_met_integration_test.go` (PG):
- `TestCriteriaMet_1946_ReviewerGrantEndToEnd` — the probe-1 scenario now self-completing: implementer →in_review (mint stamps claim_persona='reviewer'), reviewer records BOTH flags through the scoped grant, done passes R2; per-criterion audit rows carry the persona; provenance clears with the token on done.
- `TestCriteriaMet_1946_TaskWorkerSelfCertificationRefused` — the back-compat pin: implementer's live claim AND token-less callers refuse GuardManagerSurface (the §5 sentence preserved verbatim inside the new engine text; the token-less agentd tier-1 refusal itself is untouched); no audit rows.
- `TestCriteriaMet_1946_GuardMatrix` — stale token, foreign row, out-of-bounds (loud), walk-both-ways, every write audited.
- `TestCriteriaMet_1946_BounceClearsProvenance` — the bounce re-claim is implementer-class (provenance NULL) and cannot self-certify with its token.

`api/internal/handlers/board_criteria_met_seam_test.go`: actor stamping (owner/agent-surface/token/workspace), MutatedCriterion echo, loud vocabulary (extra field refuses by name; injected session key tolerated; missing check loud), guard-class mapping through renderBoardError (§5 class carries manager_surface code + text; claim-token 409).

`cmd/workspace-agentd/board_update_task_1946_test.go`: dual-route wire pins — token → POST …/tasks/set-criteria-met (no manager identity stamp), nested {task:{…}} tolerated, token-less → manager PATCH byte-identical, spawned-worker token-less → the §5 tier-1 refusal unchanged (no wire call).

## Verification

- `go build ./...` clean; `go vet` clean on all touched packages.
- Unit: `api/internal/handlers`, `cmd/workspace-agentd`, `pkg/agent/opencode`, `api/internal/server`, `pkg/board`, `pkg/mcp` — all ok.
- Integration (PG, sequential — the two suites share one DB and go test parallelizes packages): `pkg/board` ok, `api/internal/handlers` ok.
- Pre-existing environmental flakes documented, NOT caused by this change (verified identical on pristine main via stash): `TestMintUniqueOwnerWideLadder` re-fails on any rerun against a persistent DB (its fixed-UUID workspace row is never cleaned by its own t.Cleanup; CI uses fresh DBs); the local PG instance moved ports mid-lane (5433→5434), and migration 000050 was applied to both instances for local runs.

## Review round 1 (formal REQUEST_CHANGES on ae6c74b44) — addressed

1. `reconciler.go` RC2 closed-unmerged drift: BOTH landings now clear `claim_persona` — the recorded-identity re-claim mint (→in_progress, a non-persona mint) and the no-identity backlog landing (the token's pre-existing retention untouched); pinned by `TestCriteriaMet_1946_ClosedUnmergedDriftClearsProvenance` (both branches + the drifted worker cannot self-certify).
2. Pause-guard coverage gap: `TestCriteriaMet_1946_PausedRowRefuses` (GuardPaused refusal, no audit rows, resume re-arms the grant).
3. The #1946 bonus item (R2 idempotency-key re-evaluation) deferral now recorded in design 0067 §10.9 (deferred paragraph) — a future reader of the design finds it.
4. Migration 000050 up-comment corrected: under the #1939 flipped defaults a live in_review persona claim at upgrade reads NULL → the grant refuses fail-closed until the RC1 re-arm re-mints with provenance (transient, self-healing).
5. Wording: "byte-identical §5 text" softened — the token-less manager path IS byte-identical (gate untouched); the engine refusal is a NEW string preserving the §5 sentence verbatim (class `manager_surface`, pinned by `Contains`).
