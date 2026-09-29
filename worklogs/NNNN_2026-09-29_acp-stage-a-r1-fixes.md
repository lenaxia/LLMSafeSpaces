# Worklog: ACP Stage A — r1 review fixes (PR #1599 round 1)

**Date:** 2026-09-29
**Session:** ses_f499ee9e6ffe52BJ8jxc2TEQQJ lane (worker w1's preserved WIP 6cb9b551 assessed and completed)
**Status:** Complete — r1 findings addressed, iterating to APPROVED.

## Objective

Address all six round-1 review findings on PR #1599 (CHANGES_REQUESTED, review 5347637264 against e398dfd7): the SSE-bridge coverage gap, the FileChange no-diff behavior + malformed-patch bug, the two live inline status switches in translate_abi.go, the false golden-ABI claim, the missing re-scope records, and the stale doc comment.

## Work Completed

### Assessment of the preserved WIP (6cb9b551)

Working tree clean; all five files committed. Findings 1, 2, and 6 were already complete and green in the WIP (verified by running the tests before touching anything): the three SSE-bridge subtests, the unifiedPatch fix + fidelity/no-diff/empty-chunk tests, and the translate.go doc-comment cleanup. Finding 3 was half-done — `mapToolStatus` genuinely routed through the new `AcpToolStatusFromNativeStrict`, but `abiToolStatus` remained a string switch carrying an **unvalidated provenance comment** ("the session-state store persists ACP names" — false: its only inputs are the same-file literals `"running"`/`"completed"`/`"failed"` from `translateNextTool`, and `"running"` is a native name, not an ACP one; grep-verified no other caller). Findings 4 and 5 were untouched.

### r1 fixes

- **Finding 3 completed** (`translate_abi.go`): the lifecycle arm now passes **typed `AcpToolStatus` constants** (`AcpToolStatusInProgress/Completed/Failed`) through the single shared table `acpToolStatusToABI`; `abiToolStatus` is deleted (its three literal inputs map identically through the table — RUNNING/COMPLETED/ERROR preserved, pinned by `translate_abi_test.go:137,157` and `replay_capture_test.go:115`). `mapToolStatus` keeps the strict normalizer with the historical UNSPECIFIED default for unrecognized native strings — new pin: the `tool unknown status stays UNSPECIFIED, never coerced to pending` row in `TestTranslateABI_PartVariants`. No native-string status switch remains in the package (grep-verified); every status translation routes through acpvocab.go + the one table.
- **Finding 4 (claim correction, recorded here per the append-only rule):** the prior worklog's "golden ABI + history regression through the new path" was **false** — `TestTranslateABI_GoldenFixtures` drives `ABITranslator.Parse`, which the original Stage-A diff did not touch. True claim: the **history-regression** suites (`TestAdapter_GetHistory_FlatToolShape_ReturnsContractShape`, `TestParseHistoryWire_ToolNull_ProducesNoToolPart`) pass through `translateTool`'s new path; after this round the golden ABI suites additionally exercise the vocabulary's status table (mapToolStatus/toolPartPayload) as regression evidence for unchanged output. Test counts, unambiguous: `acpvocab_test.go` = **12 top-level test functions / 22 results with subtests**; the SSE-bridge pin adds **3 subtests** to `TestAdapterClientEventsFromEvent` (10 subtests total); `TestTranslateABI_PartVariants` gains **1 subtest**.
- **Finding 5 (re-scope records):** design 0063 §6.1 chartered three Stage-A deliverables; this PR delivers the tool-status names + `agent_thought_chunk` as adapter-**internal** vocabulary. The two deviations, and where each lands:
  - **The §4.3 consent outcome vocabulary** (allow_once/allow_always/reject/cancelled) does **not** land here — its consumer is the #1582 consent-flow lane, which does not exist yet; it lands with that lane (or a chartered Stage-A.2 if #1582 slips past it), NOT silently dropped.
  - **The adapter-surface rename + golden regeneration** (ACP-aligned output discriminators) does not land here — output stayed byte-identical by guardrail. It lands with US-65.8's SSE-bridge freeze (design 0063 §10's open question leans frontend-visible-now; that decision belongs to the US-65.8 charter).
  Until those land, design 0063 §6.1 and the implementation disagree on Stage-A scope in exactly these two points.

## Key Decisions

- Typed-constant migration over honest-scoping for finding 3: the reviewer offered "migrate OR scope the claim honestly"; migrating removed the divergence risk outright and cost three literals + one signature. The unreachable-default difference between the old `abiToolStatus` (RUNNING) and the table (UNSPECIFIED) is unobservable — the only inputs are the three constants, compile-checked.
- The false `abiToolStatus` comment was deleted rather than reworded: with typed inputs there is no provenance story to tell.

## Blockers

None.

## Tests Run

- `go test ./pkg/agent/opencode/ -run 'TestAcp|TestTranslateToolThroughVocabulary|TestAdapterClientEventsFromEvent' -count=1` — green (baseline verification of the preserved WIP before any edit).
- `go test ./pkg/agent/opencode/ -run 'TestTranslateABI|TestAcp|TestTranslateToolThroughVocabulary|TestAdapterClientEventsFromEvent' -count=1` — green (post-refactor).
- `go test ./pkg/agent/opencode/ -count=1` — **full package green, 12.965s** (includes goldens + history regression + replay captures).
- `go test ./cmd/workspace-agentd/ -run 'SessionState|Sessionstate|RestartInterrupt' -count=1` — green (the parser's downstream consumer).

## Next Steps

- Push, let the reviewer re-review, iterate to APPROVED, notify orchestrator (ses_f499ee9e6ffe52BJ8jxc2TEQQJ).
- Follow-up lanes (NOT this PR): #1582 consent vocabulary; US-65.8 rename decision.

## Files Modified

- `pkg/agent/opencode/translate_abi.go` (typed lifecycle constants; abiToolStatus deleted; single table; comment fixes)
- `pkg/agent/opencode/translate_abi_test.go` (UNSPECIFIED pin row)
- `worklogs/NNNN_2026-09-29_acp-stage-a-r1-fixes.md` (this file)
- PR #1599 body (claim corrections + re-scope records — via `gh pr edit`)
