# Worklog: SSE streaming partition by opencode messageID (frontend)

**Date:** 2026-07-02
**Session:** Streaming assistant responses rendered all parts inside a single `MessageBubble`. After the turn ended and history refreshed, opencode returned the same content as multiple assistant messages (each internal opencode message terminates at a tool call), so the same content re-rendered as several separate bubbles. Reported instance: `https://chat.safespaces.dev/chat/a127833a-d68c-4732-ba45-6dafd8081bfd/ses_0eb6352b5ffe9xiApZ5P7KeVLo` — during streaming the DOM held ~10 nested prose+tool blocks inside one bubble; after refresh the same content rendered as multiple bubbles with their own timestamp/model/copy footers.

**Status:** Complete (post-review-iteration)

---

## Objective

Make the streaming render match the post-refresh render. Partition SSE-streamed parts into one `MessageBubble` per opencode `messageID` so the user does not see a mid-turn structural change when opencode's history takes over.

---

## Work Completed

### Validated assumptions

1. **Opencode's part payload carries `messageID` (camelCase).** Verified against real fixtures in `api/internal/handlers/proxy_filter_test.go:26-29` (`{"type":"text","text":"Hello!","id":"p2","sessionID":"ses_1","messageID":"msg_1"}`) and `pkg/agent/opencode/dialect_test.go:229`.
2. **Opencode splits an assistant turn into multiple messages, each ending at a tool call.** Confirmed from the user's post-refresh DOM: one bubble per (text, tool) pair, each with its own timestamp footer — i.e. distinct opencode messages.
3. **Deltas always follow their parent snapshot and target the last-appended part.** Existing parser design (`activePartTypeRef` routes deltas to last text/thinking entry). Unchanged by this PR; deltas therefore inherit the messageID already attached to that entry.

### Root fix

Two files:

- `frontend/src/pages/ChatPage.tsx` — `StreamPart.messageID` field added; `parseStreamEvent` reads `part.messageID` from every `message.part.updated` payload and attaches it to the emitted `StreamPart`. Snapshot updates fall back to the existing entry's messageID (`partMessageID ?? prev[idx]!.messageID`). The delta handler spreads the previous part (`{ ...last, text: last.text + delta }`) instead of replacing type+text — without the spread, deltas would strip messageID mid-stream and break partitioning.
- `frontend/src/components/chat/ChatView.tsx` — `partitionStreamPartsByMessage` groups parts by `messageID` in encounter order (Map + parallel `order[]` array). One `MessageBubble` per group; parts without a `messageID` fall through to `DEFAULT_STREAM_BUBBLE_KEY` (single bubble, backward compat).

### Tests (TDD'd, red → green demonstrated per group)

- `frontend/src/components/chat/ChatView.test.tsx` — 3 new tests:
  - "partitions streaming parts into separate bubbles by messageID" — 2 messageIDs → 2 bubbles with content isolation asserted.
  - "groups parts without messageID into a single bubble (backward compat)" — missing messageID → default group.
  - "preserves messageID encounter order across bubbles" — interleaved A→B→A ordering.
- `frontend/src/pages/ChatPage.sse.test.tsx` — 4 new integration tests (real `ChatPage` + real `parseStreamEvent`):
  - text `message.part.updated` → messageID propagates to StreamPart.
  - tool `message.part.updated` → messageID propagates to StreamPart.
  - consecutive text→tool→text→tool across two messageIDs → four parts with the right id sequence.
  - delta accumulation preserves messageID (guards the spread fix).

Adversarial validation: stashed the implementation, verified 6 of 7 new tests failed against the pre-fix code (2 ChatView + 4 SSE red). Restored — green.

### Review-feedback iteration

First review returned APPROVE with two cosmetic findings, both addressed:

1. **Accidental indentation change at `ChatPage.tsx:61`** — `setLocalMessages([])` had drifted from 4-space to 6-space indent. Fixed to match the surrounding useEffect body.
2. **Worklog did not follow repo template** (`# Worklog:` title, `**Date:**`/`**Session:**`/`**Status:**` metadata, standard section names). Rewritten to match `worklogs/0588_…` structure.

---

## Key Decisions

- **Group by `messageID` in `ChatView`, not by remounting bubbles in `ChatPage`.** The partition is a rendering concern; keeping `sseStreamParts` a flat list keeps the parser simple. `partitionStreamPartsByMessage` is pure and localised to the component.
- **Encounter order, not messageID sort.** Opencode may reuse a messageID mid-turn (rare but possible for step retries); preserving encounter order matches the user's expected visual flow.
- **Backward-compat default group.** Parts without a `messageID` collapse into a single default bubble. Preserves existing behaviour for older tests and any legacy SSE emitter that omits the field.
- **Delta spread `{ ...last, text: last.text + delta }`.** Essential — the old `{ type: last.type, text: ... }` would have stripped `messageID` on every delta and broken partitioning mid-stream. The review specifically flagged this as an essential (not incidental) part of the fix.
- **Removed dead `partIdToMessageIdRef` map** during adversarial self-review. Deltas already route to the last-appended part via `activePartTypeRef`; that entry already carries the messageID. No per-partID lookup is needed.

---

## Blockers

None.

---

## Tests Run

- `npx vitest run src/components/chat/ChatView.test.tsx` — 26 tests, green (3 new).
- `npx vitest run src/pages/ChatPage.sse.test.tsx` — 68 tests, green (4 new).
- `npx vitest run` — full frontend suite: **1284/1284 pass** (was 1277 before this PR, net +7 for the new partition tests).
- `npx tsc --noEmit` — clean.
- `npx eslint src/components/chat/ChatView.tsx src/components/chat/ChatView.test.tsx src/pages/ChatPage.tsx src/pages/ChatPage.sse.test.tsx` — clean.
- Adversarial red step: stashed the implementation, ran the new tests, observed 6/7 failures with the expected messages (`expected 1 to be 2`, `expected 'msg_a' received undefined`). Restored — green.

---

## Next Steps

- No follow-up features. The streaming render now matches the post-refresh render for all cases where opencode emits `part.messageID`. Legacy SSE emitters (or forks) that omit the field render in a single bubble as before.
- Deploy to home-kubernetes on merge; workspace pods pick up the new frontend on their next refresh.

---

## Files Modified

- `frontend/src/pages/ChatPage.tsx` — `StreamPart.messageID`; `parseStreamEvent` reads `part.messageID`; delta handler spreads previous part.
- `frontend/src/components/chat/ChatView.tsx` — `partitionStreamPartsByMessage`; one `MessageBubble` per group.
- `frontend/src/components/chat/ChatView.test.tsx` — +3 tests (grouping + encounter order + backward compat).
- `frontend/src/pages/ChatPage.sse.test.tsx` — +4 tests (messageID extraction from text/tool events, cross-message partition, delta preservation).
- `worklogs/NNNN_2026-07-02_sse-streaming-partition-by-messageid.md` (this file). Reformatted in review round to match repo template.
