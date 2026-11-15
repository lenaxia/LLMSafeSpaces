# NNNN — pi Adapter (API-server side): the full agent.Adapter over pi RPC (tsk-elder-welder-tramps)

Board story: tsk-elder-welder-tramps (pi second-harness epic, story S4 of
the scoping report; siblings S1 overlay / S2 supervisor / S3 configwriter
are done on their branches).

## The gap

`pkg/agent` had exactly one Adapter implementation (opencode, ~3,400
lines). The owner selected pi (earendil-works/pi) as the second harness;
scoping report §1.3 mapped the platform's Adapter surface onto pi's RPC
mode (JSONL over stdio) and this story landed the API-server-side
implementation: the full interface minus the declared gaps.

## Changes

New `pkg/agent/pi` adapter surface (beside S3's configwriter):

1. **Transport seam** (`transport.go`) — `Transport`/`SessionClient`:
   the per-workspace bridge to the pi runtime (process-per-session
   children + the persisted session dir). The adapter never sees URLs
   or processes; the production transport is S5's wiring (agentd
   bridge), and the recorded fixture transport (`fixture_test.go`) is
   the reference implementation the contract suite runs against — the
   pkg/abi/abitest stance applied to the pi seam. File-level surfaces
   (`ListSessionFiles`/`OpenSessionFile`/`RemoveSessionFile`) model
   pi's no-list-RPC / unlink-is-delete reality directly.
2. **Sessions** — create mints a pi-charset id and pins the child via
   the transport (`--session-id`); rename is `set_session_name`;
   delete is the file unlink (typed gone verdict for #1479); list is
   the session-dir scan (works while pi is down, title from the latest
   `session_info` entry with first-user-message fallback).
3. **Send/steer/abort** — `Send` maps `session.Admission` 1:1 onto the
   V2Delivery queue/steer surface (`steer`/`follow_up` commands; the
   default prompt carries `streamingBehavior: followUp` so a
   mid-stream prompt queues at the boundary instead of erroring),
   subscribes BEFORE prompting (rpc.md), and waits on `agent_settled`
   — NOT `agent_end` (retries/compaction/queue may follow). `Abort`
   performs the pi-documented dance — clear_queue → abort → re-queue
   each drained text as `follow_up` — so the platform's
   queued-input-survives contract holds (pi's bare abort would RUN the
   queue immediately).
4. **Stream translation** (`events.go`, `client_events.go`) —
   `message_update` text/thinking/toolcall deltas → part.start/delta/
   end; `tool_execution_*` → ToolPart state transitions (running →
   completed/error, output = the raw result payload); compaction →
   status + a transcript MessageCompaction pair; `session_info_changed`
   → session.updated title; `extension_error`/provider errors → error
   events. Streaming-id honesty: pi events carry NO message ids —
   per-subscription pseudo ids (`pi-msg-N`) until history surfaces the
   durable entry ids (pi's own TUI correlates by position; documented).
5. **History** (`history.go`) — GetHistoryPage rides the NATIVE
   `get_entries {since}` durable cursor ("an entry id works as a
   durable cursor … even across client restarts", rpc-commands.md):
   a bounded replay ring per session (cursor + newest-message window),
   steady-state pages fetch only the delta, cold/stale/moved-branch
   reads fall back to one full fetch. `leafId` mismatch (fork/rewind)
   invalidates the window — linear append is only trusted when the
   delta's tail IS the active leaf.
6. **VerifyDelivery** (`sessionfile.go`) — the #987/#1119 oracle by
   JSONL file scan: streaming decode (no full-file buffering, #737
   posture), 2m clock-skew floor, multi-block text concatenation, EOF
   = definitive absence, mid-file read failure = inconclusive, gone
   session = definitive absence. Works while pi is down (the file IS
   the store) — pinned by the contract test with the RPC surface
   hard-down.
7. **CountMessages** — file scan of the active branch (§5.3's "cheaper
   than opencode's walk"), equal-by-construction to the transcript
   length (same translateEntries both sides).
8. **Models** — `get_available_models` over a sessionless control
   surface (`--no-session` child, pooled by the transport); usage via
   cumulative `message_update.usage` (Used = input+cacheRead+
   cacheWrite) and `get_session_stats.contextUsage`; SetSessionModel /
   SetModel are `set_model` (per-prompt overrides cannot ride pi's
   prompt command, so Send applies the model first — the documented
   V2-delivery pattern).
9. **The §1.5 gap** — NO ask/permission affordances: Capabilities is
   steer/queue/reasoning only; ListPending is an authoritative empty
   (the not-implemented contract case); Reject/Answer/Reply return the
   typed `ErrInputNotSupported`. No instance-dispose (restart-based).
10. **Registry** — `AgentTypePi` + `Register()` (credentials runtime;
    adapter construction stays with S5's wiring).
    `systemnotices.Wrap` wraps the interface — harness-neutral by
    construction, unchanged.
11. **Containment** — repolint's agent-import gate now covers
    `pkg/agent/pi` (the TestNoHandWrittenWire posture: no hand-written
    pi wire structs outside the seam; the S2 supervisor's in-pod
    structs are agentd-side, inside its allow-set). Wire types are
    unexported; decode is defensive throughout (unknown fields/types
    never fatal — §7, no protocol handshake), pinned by the
    fixture-coverage drift gate over every event type in testdata.

## Fixtures (recorded)

- `testdata/fixture_session.jsonl` — the real persisted session from
  the 2026-11-13 pi v1.5.2 capture (fixture OpenAI endpoint), carrying
  observed drift (`usage.reasoning`, `cost.totalFromProvider`).
- `testdata/get_state_response.json`, `get_available_models_response.json`
  — recorded response envelopes from the same capture;
  `get_session_stats_response.json` — documented shape with the
  capture's numbers.
- `testdata/events_stream.jsonl` — the canonical event sequence per
  json.md + the S2-validated lifecycle records (agent_start/…
  /agent_settled, tool execution, compaction, auto-retry, queue,
  extension error).
- `testdata/rich_session.jsonl` — doc-derived tree exercising tool
  fusion, error results, orphan results, compaction, model_change,
  bashExecution, custom display messages, branches.

## Evidence

- `go test ./pkg/agent/pi/ ./pkg/agent/ ./pkg/abi/... ./pkg/repolint/`
  green (incl. `-race`); pkg/abi/abitest passes (the acceptance
  criterion's suite); `go build ./...` green; `make fmt-check` clean;
  `golangci-lint run` 0 issues on the touched packages; `make repolint`
  green with the new pi leg.
- The contract suite asserts the acceptance surface directly: the warm
  GetHistoryPage issues get_entries WITH `since` (one round trip),
  VerifyDelivery answers definitively with every RPC surface down,
  Capabilities carries no ask affordances, and Abort's clear→abort→
  requeue order is command-inspected.

## Deliberate non-goals / notes for review

- `ClientEventsFromEvent` is stateless per call (the interface shape);
  pi part correlation across records is the Stream translator's state.
  The stateless path mints DETERMINISTIC per-record ids (pi-msg-1 /
  c<index> — no empty contract IDs) — correlation across records holds
  only by that determinism; documented.
- The transport contract requires sessionId injection into raw event
  records (the fixture does it; S5's production transport must too) —
  required by ContextUsageFromEvent's non-empty-sessionID contract.
- Ring eviction is dumb-full-evict at 256 sessions; rings are pure
  caches, correctness never depends on them.
- validateSessionID is deliberately STRICTER than pi's charset (no
  dots — UUID-safe subset only; minted ids are [0-9a-f]) so ids stay
  path-safe for every transport implementation.

## Review trail

- PR #2027 (https://github.com/lenaxia/LLMSafeSpaces/pull/2027).
- **r1 CHANGES_REQUESTED** (github-actions[bot], SHA 8b7f640e):
  1. warm-ring rewind-then-append ghost leak — CONFIRMED and FIXED
     (ringDelta's linear-trust now requires cursor==leaf AND the
     delta's first entry to chain from the cursor AND its tail to be
     the leaf; the reviewer's repro is the regression test
     TestReviewR1_RewindThenAppend_RebuildsInsteadOfLeaking).
  2. dead branch in DeleteSession — collapsed.
  3. byte-slicing UTF-8 titles — rune-safe truncateRunes + test.
  4. raw misspell fixer rewrote the British wire tag — the field was
     never consumed; dropped entirely (tolerated-as-unknown, the
     defensive-decode posture; no British literal remains in source,
     fixer verified noop).
  5. self-pass commit had no reproducing test — mutation-caught now:
     TestReviewR1_MidStreamMessageEnd_MintsID pins it (both paths).
  6. stateless ClientEventsFromEvent empty/colliding ids — mints
     deterministic non-empty ids + doc + test.
  7. all five enumerated missing test classes added
     (review_r1_test.go): validateSessionID paths, Abort unhappy
     (clear_queue failure tolerated / abort-failure-after-drain
     restores), Send unhappy (no-disposition error, mid-run stream
     error, handled + queued verdicts).
- govulncheck FAIL is pre-existing on main (GO-2026-6859, net/http
  stdlib, go1.26.10 → 1.26.11) — no traced symbol touches
  pkg/agent/pi; repo-wide toolchain bump is outside this story.
- **r2 CHANGES_REQUESTED** (SHA 28334183): all r1 fixes verified
  (mutation-verified by the reviewer in a scratch worktree — the
  ringDelta guards, the mid-stream mint, the rune truncation each go
  red when reverted). One remaining gap: the adapter-level stateless
  id mint (ClientEventsFromEvent's beginMessage) was load-bearing for
  message_update/tool_execution_* traffic yet unpinned.
- **r2 fix**: TestReviewR2_StatelessIDsOnEveryEvent pins non-empty
  MessageID/PartID on text_delta, toolcall_start, and
  tool_execution_end stateless events; mutation-verified locally
  (removing the mint turns it red), then re-verified by the reviewer.
- **r3 APPROVED** (github-actions[bot], 2026-11-14T02:41:07Z, head
  822e17f2347205b5be5fcbd38c194084fbd3838e): "The r2→r3 delta is
  exactly the one-line-class fix r2 demanded … No production code
  changed since the r2-reviewed head. CI is fully green on this exact
  head." — Test (full suite, race), Test (-short), SDK contract,
  Harness ABI, Lint, builds all SUCCESS on 822e17f; govulncheck
  remains the pre-existing main-branch stdlib finding (no traced
  symbol touches pkg/agent/pi).
