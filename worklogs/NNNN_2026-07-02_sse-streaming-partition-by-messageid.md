# SSE streaming partitions parts by opencode `messageID`

## Problem

During an SSE-streamed assistant turn, all parts (text, tool calls, more text,
more tool calls) were collapsed into a single `MessageBubble`. After the turn
ended and history refreshed, opencode's transcript showed the same content
partitioned into multiple assistant bubbles — one per opencode message —
because opencode terminates each internal "message" at a tool call and starts
a new one for the next text/tool pair.

Reported instance:
`https://chat.safespaces.dev/chat/a127833a-…/ses_0eb6352b5ffe9xiApZ5P7KeVLo`.
Streaming rendered ~10 separate `<div class="prose">` blocks interleaved with
tool `<details>` inside one bubble; after refresh the same content rendered
as several separate assistant bubbles, each with its own timestamp/model/copy
footer.

## Root cause

`ChatView` composed all `sseStreamParts` into a single `MessageBubble`
(`id: "streaming"`). Parts were never grouped by their originating opencode
message. The parser (`ChatPage.parseStreamEvent`) also did not read
`part.messageID` from the payload, so downstream code had no signal to
partition on even if it wanted to.

## Fix

1. `parseStreamEvent` now reads `part.messageID` from every
   `message.part.updated` payload and attaches it to the emitted `StreamPart`.
   Deltas piggy-back on the last-appended part's `messageID` because
   `activePartTypeRef` already routes them onto that same entry (no per-partID
   lookup needed).
2. `ChatView` groups `streamParts` by `messageID` in encounter order and
   renders one `MessageBubble` per group. Parts without a `messageID`
   (older paths, tests) collapse into a single default bubble — pure
   backward compat.

## Assumptions and validation

| Assumption | Validation |
|---|---|
| Opencode's part payload includes a `messageID` field | Confirmed in fixture `api/internal/handlers/proxy_filter_test.go:26-29` and `pkg/agent/opencode/dialect_test.go:229`. Field name is camelCase (`messageID`). |
| Opencode splits an assistant turn into multiple messages, each ending at a tool call | Confirmed by the user's reported DOM: the post-refresh render showed one bubble per (text, tool) pair, each with its own timestamp footer — i.e. distinct opencode messages. |
| Deltas follow their parent snapshot and never target an older part | Confirmed by the current parser design (`activePartTypeRef` routes to last-appended part). Any pre-existing violation would already be broken independent of this change. |

## Test evidence

TDD, red → green demonstrated per test group:

- `frontend/src/components/chat/ChatView.test.tsx` — 3 new tests
  - partitions streaming parts into separate bubbles by messageID
  - groups parts without messageID into a single bubble (backward compat)
  - preserves messageID encounter order across bubbles
- `frontend/src/pages/ChatPage.sse.test.tsx` — 4 new tests
  - attaches part.messageID to text parts from message.part.updated
  - attaches part.messageID to tool parts from message.part.updated
  - partitions consecutive parts by messageID (text→tool→text→tool across
    two messages)
  - preserves messageID across delta accumulation

Full suite: `1284 passed / 119 files` (frontend). Typecheck + eslint clean.

## Adversarial self-review

- Removed dead code (`partIdToMessageIdRef`) after realising deltas do not
  need a per-partID lookup: they already append to the last part which
  carries the messageID.
- Verified `part.messageID` is the correct camelCase field name against
  in-repo fixtures.
- Backward compat is preserved: parts without a messageID collapse into a
  single bubble (test `groups parts without messageID into a single bubble`).

## Files changed

- `frontend/src/components/chat/ChatView.tsx` — `StreamingPart.messageID`,
  `partitionStreamPartsByMessage`, one `MessageBubble` per group.
- `frontend/src/pages/ChatPage.tsx` — `StreamPart.messageID`,
  `parseStreamEvent` reads `part.messageID`.
- `frontend/src/components/chat/ChatView.test.tsx` — +3 tests.
- `frontend/src/pages/ChatPage.sse.test.tsx` — +4 tests.
