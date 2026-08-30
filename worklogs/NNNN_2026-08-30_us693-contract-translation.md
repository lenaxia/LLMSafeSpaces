# Worklog: US-69.3 — contract projection & the sole session.next.* → contract translation

**Date:** 2026-08-30
**Session:** Epic 69 (#1134) US-69.3 (#1137): the full opencode-dialect → ABI-contract translation table (golden-locked against the pinned live fixtures), the contract projection state machine (busy/streaming from step boundaries, in-flight parts with partials, pending inputs, session set), and the store-seed reader upgrade.
**Status:** Complete

---

## Objective

Make agentd the sole `session.next.*` → Epic-65 derivation point (design 0055 M1): every opencode event shape crosses one translation table; store IDs survive (I12); unknowns ride the Custom valve; the projection folds contract events into snapshot-complete state (I12); reseed rebuilds from store truth including pending inputs.

---

## Work Completed

### Translation (`pkg/agent/opencode/translate_abi.go`, new)
- `ABITranslator` (satisfies sessionstate's EventParser seam structurally): full taxonomy — session.status/idle/created/updated/error/diff, message.created/updated, message.part.updated/delta, session.next.{prompt.admitted,prompted,step.started,step.ended,step.failed,text.started,delta,ended}, question/permission asked+resolved. Store-truth `session.created/updated` info (title/tokens/cost) mapped to contract Session payloads; billing (tokens+cost, both opencode shapes: bare number and `{total}` — the goldens caught my wrong assumption) → contract Message.Cost (Epic 33).
- **ID preservation (I12)**: message/part/session IDs pass through verbatim from store shapes (`info.id`, `part.id`, `messageID`) onto event + payload fields.
- **Custom valve**: unknown event types → `PART_START` + Custom part `Kind:"opencode.event.<type>"`, raw properties preserved; unknown PART shapes → Custom part `Kind:"opencode.part.<type>"`. Known-but-non-session (server/plugin/catalog/file/reference/integration) → deliberate ok=false.
- **Busy from step boundaries**: prompted/step.started/status(busy) → busy; status(idle)/session.idle → idle; step.failed → ERROR event + busy cleared.

### Goldens (bump gate)
- `TestTranslateABI_GoldenFixtures` replays both pinned live captures (`sse_events_{1_18_10,1_18_15}_live.jsonl`) → `testdata/golden/*_abi.want.jsonl` (shape-stable summaries: no timestamps, text as lengths, IDs whole). REFRESH.md documents the regen procedure and the deliberate-diff rule.

### Projection (`cmd/workspace-agentd/sessionstate/projection.go`, new)
- `sessionRecord` per session: status, busy, in-flight parts (ordered, upsert-by-ID, PART_DELTA folds text), pending inputs map. `applyContractLocked` = the sole fold under the authority lock.
- `StoreReader.SessionStates(ctx) (map[SessionSeed])` — seed = store status + pending inputs; busy/in-flight are NEVER seeded (live-turn state).
- Reseed replaces the pending set + session set with store truth (orphaned busy + stale pendings structurally impossible); `podSnapshotsLocked` renders I12-complete SessionSnapshots (status w/ busy, in-flight parts, pending inputs; queue depth = 0 until US-69.7).

### Wiring
- `sessionstate_wiring.go`: the US-69.2 minimal parser is deleted — agentd now injects `opencode.ABITranslator{}`; `opencodeStoreReader.SessionStates` reads `/session` + `/question` + `/permission` (404/unreachable = authoritative-empty for pending lists only); Dialect gains `ParseQuestionListItem`/`ParsePermissionListItem` (shared with the adapter's ListPending path).

### Tests
Translator: unknown-valve (payload verbatim), ID-preservation, busy boundaries table, billing fields (tokens + both cost shapes), part variants (text/reasoning/tool/unknown→valve), question/permission lifecycle, known-non-session drops, session-set payloads — plus both golden replays. Projection: busy boundaries, orphaned-busy-after-reseed (the 2026-08-15 class), question/permission lifecycle incl. store-restored pending set, session set lifecycle, snapshot completeness (delta folding). All prior 69.2 suites updated to the richer reader/view shapes and green under `-race`.

---

## Key Decisions

1. **Custom valve mapping**: an unknown EVENT rides as `PART_START` + Custom part (`opencode.event.<type>`). Both frozen unions stay closed (no `EventType_CUSTOM` schema change, no contract amendment needed); payload + type survive; stock renderers ignore unknown parts.
2. **`message.updated` → MESSAGE_START (upsert)**, or MESSAGE_END when cost-bearing: opencode emits mutations, not lifecycle; the projection upserts by ID (I12) and step boundaries own busy. Both carry full store truth.
3. **Pending-input timeout semantics (documented in code)**: no synthetic timeout in S1 — pendings clear on resolved events or reseed-from-store; the store is truth.
4. **opencode cost dual-shape** (`0` vs `{"total":0}`): custom `costValue` unmarshaler; fixture-proven.

## Assumptions (validated)

| # | Assumption | Validation |
|---|---|---|
| A1 | Live fixtures cover the shapes worth locking | golden replay of both pinned versions, zero decode errors |
| A2 | `/question` + `/permission` mirror the asked-event properties shape | Dialect list parsers reuse ParseQuestion/PermissionRequest; adapter ListPending prior art |
| A3 | `session.diff` entries carry path/status/patch | empty in both captures — mapped per design 0049 rule 4, exercised via unit case, flagged for the next fixture refresh |

## Blockers

None.

## Tests Run

- `go test -race -count=1 ./cmd/workspace-agentd/...` — PASS (agentd 152s; sessionstate 55s incl. all 69.2 suites)
- `go test ./pkg/agent/opencode/` — PASS (incl. golden locks)
- `golangci-lint --new-from-merge-base` — 0 issues

## Next Steps

1. US-69.4 (#1138): surface — implement GetSnapshot/Deliver/GetDeliveryStatus/Act against the projection + capability report with real provenance wiring (0053 pins).
2. Wire `unknown_event_total` counter into ops metrics (translator counts via the authority's existing metrics plumbing).
3. Next fixture refresh: capture a non-empty `session.diff` to lock the FileChange mapping.

## Files Modified

- `pkg/agent/opencode/translate_abi.go` (new) + `translate_abi_test.go` + `translate_abi_golden_test.go` (new)
- `pkg/agent/opencode/testdata/golden/*_abi.want.jsonl` (new) + `REFRESH.md` (appended)
- `pkg/agent/opencode/dialect.go` (list parsers)
- `cmd/workspace-agentd/sessionstate/projection.go` (new) + `projection_test.go` (new)
- `cmd/workspace-agentd/sessionstate/authority.go` (sessions view, seed reader, snapshot build) + test updates
- `cmd/workspace-agentd/sessionstate_wiring.go` (ABITranslator + seed reader)
