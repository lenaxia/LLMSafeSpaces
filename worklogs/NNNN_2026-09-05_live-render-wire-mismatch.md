# Worklog: live render wire mismatch — session.next.* streaming translated against the real format (#1288 fix 2)

**Date:** 2026-09-05
**Session:** The frontend showed only the newest agent message during a turn (full list only after turn-end history load). Root-caused by capturing the pinned opencode's raw event stream live on production and replaying the captures through the real translator.
**Status:** In Progress

---

## Objective

Make the frontend's live renderer receive the events it was designed for.

## Work Completed

### Root cause (captured evidence, production)

- The pinned opencode 1.18.10's live stream is the `session.next.*` family ONLY — a successful streaming turn emits ZERO `message.*` events.
- `translateNextText` decoded `partID`/`messageID` — field names that appear on NO pinned-version frame. The wire carries `textID`/`assistantMessageID`. Replay through the real translator: every live part event had partID="" and messageID="".
- `session.next.reasoning.*` and `session.next.tool.*` were entirely unhandled (Custom valve, no IDs, no message attribution).
- Frontend consequences (its logic is correct — the wire was broken): parts could not be keyed (upsert appends degenerate bubbles), could not be attributed per message (all fall into one default group), and text DELTAs were dropped outright (`if (evt.partId)`).

### Review r2 hardening

- Memo-miss END (translator restart mid-turn): DROPPED, not emitted empty — consumers replace-by-key; a nameless END is the wipe bug again. Pinned.
- The memo is purged on step.ended AND step.failed (session-scoped, both pinned) — the process-lifetime instance cannot accumulate aborted-turn state.
- Wire taxonomy extended (wire.IsKnownEventType + repolint event literals) with the 9 now-first-class families per REFRESH.md step 3.
- Named the content[]/structured shapes (ocContentItem/ocStructuredResult/ocToolError); marshalOrEmpty replaces the panic-implying name; stale toolPartPayload comment corrected.
- E2E level (r5): the production capture streamed through the real Ingest seam to the DELIVERED surface — Stream subscriber + State() — asserting the completed tool part (name+input+output, COMPLETED) reaches a live consumer without any history reload; a suppressed tool END fails the test.
- Ingest hardening (r5): properties-shape drift on a claiming frame — (nil, true, err) — SIGSEGV'd applyLocked(nil) on the production hot path; err now governs over ok, pinned with the shape-drift frame + parser-failure accounting.
- Integration level (r3): both fixtures drive the REAL translator → REAL authority projection (Parser+Passwords+PlatformDir wired, IngestForTest), asserting partID+messageID on every part event and name-retention on tool ENDs. The r2 version silently skipped (fixture paths two-levels shallow, construction requirements unmet) — it now runs (~0.1s/fixture of real ingestion).

### Review r1 hardening

The tool-lifecycle translation was half-done: the pinned success frame carries the result as content[]/structured{exit} (NOT `output`) and NO name/input — a bare PART_END wiped the running bubble at completion. The translator now memoizes tool.called frames by callID (per-connection state; Parse moved to a pointer receiver — value copies lost the map) and the END emits the COMPLETE part. The failure path is pinned too (error text into output, ERROR status).

- `translateNextText`: decodes `textID`+`assistantMessageID` with legacy-name fallbacks.
- NEW `translateNextReasoning`: reasoning.started/delta/ended → PART_REASONING lifecycle with `reasoningID`.
- NEW `translateNextTool`: tool.called → PART_START (input), tool.input.* folded (no contract event for input streaming), tool.success/failure → PART_END with output/state; `callID` is the part key.
- Regression harness: the two production captures embedded as fixtures, replayed through the real translator, asserting the #1288 fix-2 invariants (every PART_START/END carries non-empty partID and messageID; every PART_DELTA a non-empty partID).

## Key Decisions

1. Legacy field names stay as fallbacks — older opencode builds that send `partID`/`messageID` keep working; `firstNonEmpty` picks whichever the wire carries.
2. Tool-input streaming deltas are folded silently (the contract has no tool-input delta event; the TUI accumulates input text live — surfacing it would need a contract change, deferred).
3. Fixtures are production captures redacted per testdata/REFRESH.md: synthetic ID mapping (ses/msg/rs/call prefixed, intra-fixture-consistent), >120-char strings trimmed, non-frame lines dropped — byte-shape faithful apart from redaction.

## Blockers

None.

## Tests Run

- `go test ./pkg/agent/opencode/` — green (13.8s), includes the replay harness + memo/purge tests over both fixtures.
- `go test ./cmd/workspace-agentd/...` — green (218s + 30s), includes the projection integration test.

## Next Steps

- Ship as part of 0.27.2 with the frontend unchanged (its accumulation logic is correct once the wire carries IDs).
- Manual verification: a multi-step agentic turn should now render every message live.

## Files Modified

- pkg/agent/opencode/translate_abi.go — nextStreamIDs decoding, translateNextReasoning, translateNextTool, routing.
- pkg/agent/opencode/replay_capture_test.go + testdata/events-{text,tool}-turn.txt — the replay harness and production captures.
