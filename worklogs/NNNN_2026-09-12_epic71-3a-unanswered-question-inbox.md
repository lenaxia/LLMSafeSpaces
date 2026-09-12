# Worklog NNNN — epic-71 / 3a: unanswered-question inbox (backend, contract, frontend, cluster row)

**Date:** 2026-09-12
**Session:** Stream 3a of epic #1314 — implement #1313: the unanswered-question inbox. Claimed per the epic's reserved-comment protocol (comment 5639658842; updated in-session with implementation status).

## Objective

The walk-away incident class: an ask raised mid-turn dies (turn abort, pod recycle, lease resolve), and the returning user finds a dead end. Deliver the #1313 design — inbox record at ask time (Redis, outbox-sibling), snapshot-flight union (live ∪ inbox, `whileAway`-tagged), late answers as Q&A-in-history through the standard delivery path, dismiss as the second exit (S11), permissions default safe.

## Rule-7 gates (executed BEFORE implementation)

**G1 — timed-ask behavior on the pinned opencode 1.18.15** (local `opencode serve` + scripted OpenAI-compatible mock provider; procedure per testdata/REFRESH.md; scratch instances, no cluster):

- `question` tool schema (`GET /experimental/tool?provider=…&model=…`): `questions[{question, header, options[{label,description}], multiple}]` — **no timeout parameter**; no config-level question timeout exists.
- **Negative result (A9):** an unanswered question ask stays `status:"running"` in `GET /question` indefinitely (26-min observation); a pending permission ask likewise (24 min; no auto-grant/deny). The "ask times out" premise of #1313 is PLATFORM-side (abort/recycle/lease) — the walk-away E2E must kill asks via platform mechanisms, never wait for a harness timer.
- Reject → tool part `status:"error"`, `"The user dismissed this question"`, turn ENDS with **no second provider call**.
- Reply → provider re-invoked with `User has answered your questions: "<q>"="<a>". You can now continue with the user's answers in mind.` — the Q&A framing rides the same register.
- Permission reject → `"The user rejected permission to use this specific tool call."` (A8: not-granted is structural; no harness change needed).
- Captured as `pkg/agent/opencode/testdata/ask_terminal_states_1_18_15.json` (provenance in-file).

**G2 — Q&A framing:** validated by construction from G1 (the harness's own answer format is plain text). The live-model confirmation turn rides the walk-away E2E/pool (in-repo harnesses run the scripted mock); recorded as a deferred leg in the design doc, not silently dropped.

**Design decisions carried from the reclaimed claim:** (1) terminal records update in place (duplicate-event resurrection closed, S11 mechanical); (2) dismiss on a live ask rejects the live ask first; (3) snapshot-path record upsert (event-only recording insufficient under droppable events). The lost design doc was recreated at `design/stories/epic-71-input-delivery-robustness/US-71.3a-unanswered-question-inbox.md` with the full Rule-7 register (A1–A9, all validated).

## Work completed (TDD — every suite red-first)

1. **Inbox store** (`api/internal/services/inbox/`): HASH `inboxq:{ws}:{ses}` (ask ID → JSON record); Lua-atomic upsert (pending-refresh / terminal-immutable / cap-10 FIFO eviction of oldest pending), Lua-atomic in-place resolve (answered|dismissed), whole-key TTL 24h, `List` (session), `ListWorkspace`/`LookupPending` (SCAN, cross-session — the reply routes carry no session), `PendingIDs`. 17 unit tests incl. terminal immutability, eviction, reconnect survival, ctx cancellation.
2. **Record at ask time:** `usageBridge.InputRequested` records every surfaced ask (auto-approved permissions skip — they never surface). `INPUT_RESOLVED` deliberately does NOT touch the record: resolution means "the live ask is gone" (answered OR timed out OR lease-dropped); only the reply routes know the user answered — otherwise the walk-away record would clear on the very timeout it exists for.
3. **Snapshot union (S5 ext):** `emitPendingViaAdapter` re-records every live ask (the recovering write) then emits inbox-only records tagged `whileAway` (additive field on `agent.QuestionRequest`/`PermissionRequest`). Terminal records never re-present.
4. **Late answers (L8):** `QuestionReply`/`PermissionReply` pre-check `LookupPending` + harness liveness; non-live ask → compose Q&A (`Answering your earlier question — "<q>": "<a>"…` / permission guidance form) → `outbox.Accept` with cmid `inbox-{askID}-answer` (S2 via the outbox dedupe marker; duplicate → 202 `duplicate:true` with the original entry) → record answered. Live asks proxy unchanged; proxy success (2xx) terminalizes answered. `QuestionReject` on a live ask terminalizes dismissed (the user saw it and dismissed it).
5. **Dismiss exit (S11):** `DELETE /workspaces/:id/sessions/:sessionId/inbox/:requestID` — live ask rejected FIRST via the new `agent.Adapter.RejectInput` seam (opencode: question-reject endpoint w/ permission-"reject" fallback; question reply is `additionalProperties:false`, hence a distinct method), then record dismissed + resolved SSE (`data.reason="dismissed"`) for other tabs. Reject failure → 502, record stays pending.
6. **Wiring:** `SetInboxStore` (pre-Start, outbox convention) + ForTest twin; app.go cache-block construction `inbox.New(cacheSvc.GetClient())`; router route; OpenAPI (`InboxLateAnswerAccepted` schema, dismiss route, 202s on both reply routes; `make -C sdks validate` green — SDK regen rides 4b).
7. **Frontend:** `whileAway` types; `inputApi.dismissInboxRecord`; `WhileYouWereAway` variants on QuestionPrompt (title + hint; dismiss routes to the inbox endpoint) and PermissionPrompt (guidance hint — the original request already ended); fixed a pre-existing mock-implementation leak in PermissionPrompt.test ("loading state" poisoned later tests via never-resolving `mockImplementation`; root-caused, not test-hacked). 8 new vitest cases, 29/29 green.
8. **Cluster row** (`local/epic71-3a-walkaway-e2e.sh` + pool step before the fault arm): W1 L7 re-presentation (≤2s), W2 late answer + S2 duplicate depth, W3 dismiss 204/terminal/non-re-present, W4 suspend→resume→re-present. Seeding is honest: records staged directly into valkey = the state a dead live ask leaves behind; the recording/death mechanisms are pinned by unit suites + G1. Pin tests (`local/epic71_3a_walkaway_script_test.go`): bash syntax, lib sourcing, distinct UUID base, L7/S2/S11/A3 row pins, pool-order lockstep.

## Key decisions

- Resolution ≠ answered (see 2 above) — the crux of the stream; `InputResolved` is a liveness signal, not a disposition.
- Late answers reuse the reply routes (no new answer surface) — the pre-proxy liveness check keeps the live path byte-identical; unreachable harness is NOT proof of death (fail-open to the proxy path).
- Store is dependency-free (no pkg/session import) mirroring the outbox; handlers convert.
- `answer via SessionAction/AnswerInputAction` (MCP/SDK path) does not yet terminalize the record — noted in the design doc; lands with #1302's Act-centralized replies (4a) to avoid duplicating the disposition logic at two seams now.

## Tests run

- `go test -race -count=1 ./api/internal/services/inbox/` — ok (17 tests)
- `go test -race -count=1 -run TestInbox ./api/internal/handlers/` — ok (13 tests)
- `go test -count=1 ./api/internal/handlers/ ./pkg/agent/...` — ok (full package, no regressions)
- `go test -count=1 -run TestEpic71Walkaway ./local/` — ok
- `golangci-lint run` (inbox, handlers, pkg/agent) — 0 issues; `go build ./...` — clean
- `npx vitest run` (both prompt suites) — 29/29; `npx tsc --noEmit` — clean
- `make -C sdks validate` — valid
- Cluster row: pending the pool dispatch on this branch (merge gate)

## Blockers

- None. Merge gate per the epic protocol: walk-away pool row green on the branch.

## Next steps

- PR review cycle; pool dispatch (walk-away row + delivery rows) on the branch.
- 4a (#1302): route reply dispositions through `Act` centrally (closes the SessionAction note).
- G2 live-model confirmation on staging during the release window.
