# NNNN — fix #1947: engine double-spawn + mechanical in_review flip + missing reviewer persona

Issue: https://github.com/lenaxia/LLMSafeSpaces/issues/1947

VALIDATE-FIRST (owner ruling): everything below was reproduced or
pinned in the PG integration harness BEFORE any fix landed. The
repro file is `pkg/board/persona_spawn_race_integration_test.go`
(the #1941 convention: per-symptom pins, mutation-verified).

## The production spawn shape the #1941 tests never modeled

The decisive fact (read off `newBoardSpawner` → `HTTPAgentExecutor`
→ agentd `execAgentNode`): **the charter dispatch blocks for the
spawned session's entire first turn** — the spawner returns only
when turn 1 completes, and the guarded assignee stamp lands only
then. A freshly-granted task therefore sits claimed-but-UNSTAMPED
(assignee NULL) for the whole first turn, and every stage-persona
mint opens a mint→stamp window. The existing persona suite ran
sync spawners (stamp = instant), so none of these windows existed
under test. The probe-1-vs-probe-2 differential is exactly this:
probe 1's worker moved at T+30s (post-stamp); probe 2's move hit at
T+1s (mid-flight). The new harness models it with a gated spawner
(record at entry, park on a per-call gate).

## Symptom 1 — the "mechanical flip" (T+1s, "no worker action")

**Finding: the engine contains NO mechanical in_progress→in_review
writer.** Exhaustive pass over every `stage=` write: the only
in_progress→in_review transition is `moveStageTx`, behind the live
claim-token guard (`actor.ClaimToken == cur.ClaimToken`) — the
reconciler's lapse/re-arm passes are in_review-predicated, rc1 goes
to backlog, pendingCoupling goes to pending_input, rc2Drift consumes
in_review rows. Outside the DB, the token exists in exactly one
place: the spawned worker's charter envelope. The T+1s flip
therefore required a token-bearing MoveStage from the freshly
spawned session — which is the probe's own design (the differential:
deliberate delayed move vs immediate). "Worker turns take ~30s" is a
model-latency prior, not a floor on the first tool call.

- **Pin S1** (`TestPersonaSpawnRace_S1_NoMechanicalFlipWithoutWorkerMove`):
  grant parked mid-dispatch + a full sweep + a dispatch drive ⇒ the
  row stays in_progress, exactly one spawn, no persona_claim event,
  no in_review transition. If any mechanical flipper ever appears,
  this goes red.
- **Instrumentation still missing** (requested in the PR): silver-
  porcupine-weeds' transcript with per-tool-call timestamps for
  03:23:05–07, and the board_events actors for the 03:23:06
  move_stage + persona_claim. That would turn "consistent with the
  probe's immediate-move design" into a closed case. No blind fix
  shipped for this symptom — the engine's flip path is correct.

## Symptom 2 — the "second worker with the TASK charter"

Two spawns, ~1.2s apart, same-looking parentage. Reconstruction:

1. 03:23:05 — grant (token A); implementer dispatch (silver) parks
   for its first turn.
2. 03:23:06 — the token-bearing →in_review move (S1's analysis):
   forward exit clears token A, the persona mint layers token B,
   the post-commit nudge spawns the REVIEWER persona — open-estate-
   leaves. The "SAME board-worker parentage" is an elision artifact:
   the reviewer RunID (`<task>-in_review-<unix>`) and the implementer
   RunID (bare task id) share their first 16 chars (the task UUID),
   and `workflowAgentMessageKey` truncates each component to 16 —
   the rendered keys differ only in the hash8 suffix the issue's
   elision hid.
3. The reviewer, however, was **mischartered** (symptom 3) and acted
   as an implementer; silver's late stamp lost the race (token A
   dead) and its session idled out — the observed transcript pair.

- **Pin S2** (`TestPersonaSpawnRace_S2_ImmediateMoveRacingInFlightSpawn`):
  under the production async shape the engine does the designed
  thing — flip+mint (attempts untouched, provenance false), exactly
  ONE reviewer-persona spawn carrying the MINTED token, the parked
  implementer's late stamp LOSES and the session is ABORTED (the
  double-execution guard), no third spawn. Mutation-verified against
  the #1941 seam itself: reverting the guarded stamp's stage
  predicate to the `stage='in_progress'` literal reddens it.

## Symptom 3 — "no reviewer persona ever fired"

The reviewer DID spawn — and could never do its job. Two composition
defects, both in the #1941 surface:

**(a) The persona charter carried the implementer trailer.**
`boardCharterPrompt` appended "Work the task to completion … move
the task to in_review when the work is done" to EVERY charter,
persona or not. The live reviewer (open-estate-leaves) read it,
executed the task's Round 1, and called `board_move_stage` — refused
in_review→in_review — then idled into the 15-minute lapse. Fix:
persona-bearing charters close on the persona's outcome; the
persona-less charter is byte-identical to today. Pin:
`TestCharterPrompt_PersonaCharterDropsImplementerTrailer` (red
pre-fix, mutation-verified).

**(b) The verdict channel did not exist for spawned reviewers.**
`DefaultReviewerPersona.Instructions` command `board_record_verdict`
— a tool absent from the agentd worker registry (the verdict REST
route is owner-surface only, and spawned workers are refused manager
dispatch). The reviewer could not record a verdict even if it tried:
Finding B's "verdict channel absent end-to-end". Fix: the full worker
projection — pkg/board agent-surface guard (the in_review row's LIVE
claim token — the reviewer's own), pod-automation handler + route
(`POST /internal/v1/automation/board/tasks/verdict`), loopback client
method, agentd registry entry + dispatcher cases + schema, parity
matrix column. Pins: `TestPersonaSpawnRace_S6_VerdictChannelHoldsReviewersToken`
(red pre-fix: a wrong-token verdict recorded), the registry-shape +
claim-token-schema + wire + pre-wire-refusal tests in agentd, and the
extended parity columns.

## The 03:49Z finding — heartbeat stillborn on the re-arm window

Reproduced red-first (S5). Between the re-arm's mint and the fresh
reviewer's spawn-stamp, the row carries the NEW token + the DEAD
prior assignee (the persona lapse pass cleared token+lease but —
unlike rc1's in_progress reclaim, which clears `assignee_session` —
left the corpse identity). The #1832 WS4 stillborn guard reads the
row's assignee, so the fresh claimant's heartbeats consulted the
dead prior worker's zero-turn evidence and refused — every re-armed
claimant unrenewable, churning every 15 minutes (the live
observation). Fix: the lapse pass's accounting branch clears
`assignee_session` — rc1 parity. Pin S5 holds the window open with a
gated re-arm dispatch, asserts the fresh claim extends, AND asserts
WS4 stays intact post-stamp. The identity's real consumer (the
bounce's never-token-less guard) is unaffected: a stamped reviewer
records its own identity, and the unclaimed no-persona branch keeps
today's retain-identity semantics.

## Mutation verification (every pin)

| Pin | Mutation | Result |
| --- | --- | --- |
| S5 | lapse branch without `assignee_session=NULL` | RED |
| S2 | guarded stamp predicate reverted to `stage='in_progress'` literal | RED |
| S6 | svc agent-surface token guard disabled | RED |
| charter trailer pin | persona branch disabled (unconditional implementer trailer) | RED |
| agentd registry/wire/parity | (red-first: the tool surface did not exist pre-fix) | RED pre-fix |

## Test inventory

- `pkg/board/persona_spawn_race_integration_test.go` — S1, S2, S5, S6
  (PG harness, `gatedSpawner` + `turnEvidenceStub`, BoardSuite
  conventions).
- `api/internal/app/board_review_charter_test.go` — the persona
  trailer pin (byte-identity of the persona-less charter preserved).
- `cmd/workspace-agentd/mcp_board_tools_test.go` — registry shape,
  claim-token schema, wire contract, pre-wire token refusal.
- `api/internal/handlers/board_parity_test.go` — route table +
  registry column + matrix row for the verdict channel;
  `board_automation_test.go` fake gains `RecordVerdict`.

## Notes

- Environment: sibling lanes share this host and the harness's
  default port-5433 DSN — run with a private `TEST_DATABASE_URL`
  (this lane: PG 15 on 5547) or sibling suite runs TRUNCATE mid-run.
  `TestMintUniqueOwnerWideLadder` fails standalone on the CLEAN tree
  in this environment (fixed-id workspace seed, no truncate) —
  pre-existing, unrelated, not touched.
- The probe board (fixes-verification-043 / tsk-chalky-oxbow-stencils)
  was never contacted; evidence is the issue + comment only.
