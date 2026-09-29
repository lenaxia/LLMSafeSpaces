# Worklog: ACP Stage A — the adapter vocabulary (design 0063 implementation, issue #1595 follow-on)

**Date:** 2026-09-29
**Session:** ses_f4990c442ffei8kVvWzcN9qMHU
**Status:** Implementation on branch `feat/acp-stage-a-vocabulary`; PR up; iterating to APPROVED.

## Objective

Design 0063 Stage A (owner-chartered, guardrails binding): introduce the ACP sessionUpdate vocabulary as the adapter layer's internal event shapes — mapped to/from the five contract part types per the §4 table — with AT LEAST ONE REAL CONSUMER (the tool event class, end-to-end through both the history path and the US-65.8 SSE bridge), NO Stage-B creep (no stdio/responders/wedge machinery), and the contract part types remaining canonical (nothing inverts; pkg/session/ untouched).

## Work Completed

- Branch `feat/acp-stage-a-vocabulary` off a3a1db38 (post-0063-merge main + v0.34.11).
- TDD red-first: `acpvocab_test.go` written against types that did not exist yet (compile-red), covering every §4 mapping-table row (agent_message_chunk→Text; agent_thought_chunk→Reasoning; user_message_chunk replay→Text; tool_call pending→Tool Pending; tool_call_update in_progress/completed/failed→Running/Completed/Error with input/output/error carried; available_commands_update→Custom acp.available_commands; plan→Tool row; unknown→Custom acp.unknown never dropped; usage_update→NOT a part).
- `acpvocab.go`: the vocabulary (AcpUpdate + the 8 kinds; AcpToolCall with kind/status/content unions and ACP wire names so Stage B can parse real ACP streams; AcpCommand/AcpUsage/AcpPlanEntry) + the mappers: ToPart (§4 rows, tolerant-forward unknowns), AcpUpdateFromPart (reverse direction), AcpToolKindFromName (the kind table), AcpToolStatusFromNative + ToContractStatus (the state machine), FileChange() from diff content (unified patch), AcpToolCallFromNative (the input seam).
- The pins, structurally: NativeChunkKinds() excludes user_message_chunk (replay-only — no native producer, tested); todowrite→kind "other" (tested); plan marked IsNeverEmittedByNative (tested); unknown kinds never dropped (tested).
- **The real consumer:** translateTool now flows native ocTool → AcpToolCallFromNative → AcpUpdate{tool_call}.ToPart() → ToolPart — used by BOTH paths (history translatePart/translateMessage AND the live SSE bridge clientEventsFromPartUpdate, which calls translatePart). translateToolStatus delegates to AcpToolStatusFromNative().ToContractStatus(). Byte-identical output verified by the existing golden/history-regression suites passing through the new path (full package green, 11.8s).
- Consumer test: TestTranslateToolThroughVocabulary (state machine owned by the mapper; times/error/IO preserved; unknown native status keeps the historical pending default; nil safety).

## Key Decisions

- Tool as the first (and only, this lane) consumer: richest §4 row, exercises status machine + kind + content; text/thought rows are trivial appends the chunk helpers already express.
- The vocabulary carries ACP wire names in JSON tags (Stage B reuse) but the contract output is unchanged — zero edits to pkg/session/.
- FileChange() exists on the vocabulary for direct diff consumers, but the history path's authoritative FileChange parts remain the filediff producer's (git-based) — no behavior change.
- unifiedPatch is a minimal deterministic renderer for vocabulary-carried diffs only.

## Blockers

None.

## Tests Run

- `go test ./pkg/agent/opencode/ -run 'TestAcp|TestTranslateToolThroughVocabulary' -count=1` — all green (10 tests + table subtests).
- `go test ./pkg/agent/opencode/ -count=1` — full package green (11.8s; includes golden ABI + history regression through the new path).

## Next Steps

- PR → full protocol → APPROVED → notify orchestrator.

## Files Modified

- `pkg/agent/opencode/acpvocab.go` (new — the vocabulary + mappers)
- `pkg/agent/opencode/acpvocab_test.go` (new — the §4 table + pins + consumer test)
- `pkg/agent/opencode/translate.go` (translateTool/translateToolStatus routed through the vocabulary)
- `worklogs/1078_2026-09-29_acp-stage-a-vocabulary.md` (this file)
