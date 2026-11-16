# Worklog: wrap-up verdicts — the close verb + gap discipline + recursion cap + human ack (epic wrap-up review)

**Date:** 2026-11-16
**Session:** wrapup-gap-recursion implementation (tsk-pallid-vestibule-picnics)
**Status:** Complete

---

## Objective

Land the epic wrap-up review's CLOSE half (low-level design §§4, 8-9; high-level D4, D6, D7): `RecordWrapUpVerdict` (the engine-owned close verb — PASS completes the epic directly and atomically, GAPS validates gapRefs and holds the epic open by the ordinary drain predicate), `AckWrapUp` (the recursion cap's human exit — a dedicated epic-level manager verb, never a pending_input flag), the `board_health` ack_waits surface, the epic read's cap, and every wire surface (owner REST, automation worker + manager, external MCP, agentd tools, OpenAPI). The mint/seam are wrapup-engine's; the settle primitive (`advanceWatermarkTx`) and renderer are wrapup-watermark-diff's — consumed, not rebuilt.

---

## Work Completed

### `RecordWrapUpVerdict` — `pkg/board/wrapup.go`

The §4 contract verbatim. Wire schema ENGINE-OWNED and closed (`WrapUpVerdictInput`): `verdict` pass|gaps (a story verdict's approve|fail REFUSES — sibling schemas, never interchangeable), `reportUrl` required on BOTH (the →done transition's evidence), `gapRefs` required non-empty IFF gaps, `notes` capped at 8 KiB (`MaxWrapUpNotesBytes`, textguard posture — the corruption heuristics ride through `validateTextField`), `reviewer` required. Guard order, each a loud GuardError: payload → `kind='wrapup'` → stage (anything ≠ in_review is a STALE REPLAY: 200-with-original — the current row + epic, never an error) → claim token (the row's LIVE token, UNCONDITIONAL on every surface — even the owner carries it or refuses; the criteria_met.go pattern, re-arbitrated under the row lock inside the tx) → Done-guard parity (`criteriaAllMet` + reportUrl) → GAPS refs (each resolves, did-you-mean on a miss, to a kind='story' row in the SAME epic, `'gap' = ANY(tags)`, `created_at >=` the wrap-up row's mint; the refusal names the offending ref and reason).

One transaction (the `eventWriter` + `recordEventTx` pair): the row's →done (the completeReviewTask statement shape, evidence=reportUrl) → the `wrapup_verdict.settle` FAILPOINT (the both-or-neither pin's fault seam) → `writeTransition` → `advanceWatermarkTx` (the watermark sibling's primitive — mint-time snapshot, ledger +1, pointer cleared, guarded no-op on replay/race) → the deterministic `wrapup:verdict:<taskID>` audit row (one verdict per row, EVER — ON CONFLICT inert; the canonical record a stale replay converges to) → on PASS, the DIRECT guarded completion UPDATE (`completeWrapUpEpicTx`: active + wrap_up_review + pointer NULL + no ack pending + the kind='story' drain predicate; 0 rows = a story reopened under the verdict → the epic stays open, the honest outcome) + the deterministic `wrapup:epic_complete:<epicID>` event riding the same tx. The verdict path NEVER re-runs `wrapUpAwareEpicCompletion` (§2 step-3's note — the seam's enabled branch has no completion path; a re-run would mint an empty-delta wrap-up or arm ack-wait). The cap does NOT gate the terminus (it bounds the mint loop): PASS at exactly the cap ALSO completes. GAPS does nothing further — no bounce, no implementer re-dispatch; the non-done gap rows hold the epic by the drain predicate alone. A race/stale inside the tx maps through the `errWrapUpStaleReplay` sentinel to the 200-with-original return; the requestID dup path renders the recorded outcome.

### `AckWrapUp` — `pkg/board/wrapup.go`

§8's human exit (D7): manager verb (`requireManagerAction` — workspace-scoped on the agent surfaces, ownership on REST/MCP). One transaction: the guarded reset (`wrapup_count=0, wrapup_ack_pending=false, wrapup_task_id=NULL` — the guard matches an ARMED ack-wait OR a PARKED pointer (the pointed row NOT in_review: the manager-override posture; a LIVE in_review review is NEVER acked out from under its reviewer — 0 rows is the original echo, no event, no re-drain), then `wrapUpAwareEpicCompletion` re-runs in the SAME tx: an acked, still-drained epic mints immediately (the human said continue), the mint's spawn riding the post-commit nudge. Event: `wrapup_ack` (requestID id — the eventWriter row IS the audit + replay marker).

### Surfaces

- `board_health` gains `ack_waits` (`HealthAckWait`: epic/short/project/title/count/cap/ack_pending/in_flight_short_id) on BOTH the owner and workspace projections (`healthAckWaits` — NULL-able bounds; the parked-pointer LEFT JOIN names the parked row).
- Epic reads carry `wrapup_cap` (config-derived, decorated at the read edge on wrap-up-enabled epics — beside the engine story's flat fields).
- Owner REST: `POST /api/v1/me/tasks/:id/wrapup-verdict` + `POST /api/v1/me/epics/:id/ack-wrapup` (`handlers/board_wrapup.go`; the claim token rides the body's actor).
- Automation seam: `POST /internal/v1/automation/board/tasks/wrapup-verdict` (worker; token from the body) + `POST /internal/v1/automation/board/epics/:id/ack-wrapup` (manager; `resolveManager` stamps the workspace).
- External MCP: `board_record_wrapup_verdict` (front-running validation: enum/token/report/iff checks; gap_refs array decode) + `board_ack_epic_wrapup` (`pkg/mcp/board_tools.go` + client methods; the ack rides the epic-action wire shape).
- Agentd: worker dispatch (`requireBoardClaimToken` → the loopback `BoardRecordWrapUpVerdict`), manager wire call (`BoardManagerEpicAction(…, "ack-wrapup", wire)`), the tool catalog schemas, `strict_board_args` + the origin plugin's stamp set (parity-pinned).
- OpenAPI: both paths + the four schemas (`sdks/openapi.yaml`).

### Events vocabulary (§8, complete)

`wrapup_verdict` (deterministic per-row id + the requestID-keyed replay row), `wrapup_epic_complete` (deterministic per-epic id, rides the PASS tx), `wrapup_ack_required` (the engine story's arm), `wrapup_ack` (requestID id).

### The pins (born-red first — commit 3989fda02)

`pkg/board/wrapup_verdict_unit_test.go` (closed schema: enum/report/iff/notes-cap/reviewer), `pkg/board/wrapup_verdict_integration_test.go` (the §10 map: PASS atomic + terminus at/below/at-exactly-the-cap; the 0-row reopened-story shape; the no-mint/no-ack-wait pins; GAPS ref matrix (wrong-epic/untagged/pre-mint/nonexistent/empty-on-gaps) + holds-open + no-bounce; gap completion → the next delta-scoped mint (window-from == the advanced watermark); the deterministic-id replay convergence; the guard chain (kind/token/done — including the owner token-less refusal); the mid-settle failpoint both-or-neither + the live retry; the full 3-cycle cap→ack-wait→ack→re-drain loop through REAL verdict increments; the parked-pointer ack clear + the health/epic read surfaces), `api/internal/handlers/board_wrapup_rest_test.go` (REST binds + passthrough + the automation twins' actor stamping), the agentd coverage/claim-token lists.

### Verification

`make fmt-check` clean; full `pkg/board` suite green in BOTH lanes (`-tags=integration -race` 63s against the migrated PG; `-short` clean); handler + `pkg/mcp` + agentd board/strict-args suites green; `go build ./...` + `go vet` (both tags) clean; OpenAPI parses (259 paths / 234 schemas).

---

## Notes

- The ack's live-pointer protection refines the design's guarded UPDATE (§8 sketches `WHERE wrapup_ack_pending OR wrapup_task_id IS NOT NULL`): the strengthened guard adds `AND the pointed row's stage <> 'in_review'` — without it, acking an epic whose OWN re-drain just minted would clear the fresh review's pointer and double-mint. The design's stated semantics ("0 rows = nothing to ack"; the parked-pointer un-wedge) are preserved exactly; the live review was an out-of-design edge the guard now refuses loudly-by-echo.
- Migration note: the epic-wrapup schema rode the engine story's `000058_epic_wrapup_review`; main's simultaneous `000058_session_alerts_payload` (#2074) duplicated the version. This branch takes the #2064 lane's renumber (`000059_epic_wrapup_review`, commit c72bcf18f) when merging main — never an independent renumber.
