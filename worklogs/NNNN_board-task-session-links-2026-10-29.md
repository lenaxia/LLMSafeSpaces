# Worklog — 2026-10-29 owner ruling: session refs on the board card/drawer click through to the session

Task: `tsk-teal-vocalist-orders`.

Owner ruling, verbatim:

> "Please add a task for adding a link/ref to sessions so from the board task
> info cards we can click straight through to the session. And via api mcp sdk
> we can look up the session ref if we need to fetch it. I suspect the info is
> already there just need wire."

The suspicion was right: `assignee_session` already rode every task read with
the #1922 read-edge phrase translation (the 3-word phrase when the session
index knows the session, raw harness id + `assignee_session_deprecated` in the
design-0066 cold window), and the chat route (`/chat/:workspaceId/:sessionId`)
already accepts both the canonical id and the phrase. The only genuinely
missing wire was `implementer_session` — #1959's bounce identity, captured at
the in_progress→in_review forward exit, deliberately `json:"-"` until now.

## 1 — The read edge: `implementer_session` exposed, same #1922 seam

`pkg/board/rows.go`: `ImplementerSession` loses `json:"-"` (now
`implementer_session,omitempty` on reads) and gains its cold-window degrade
flag `ImplementerSessionDeprecated` (`implementer_session_deprecated,omitempty`)
— the identical shape to the assignee fields. Write paths keep it internal
addressing (the stored row stays canonical; only read-edge response copies
translate — see §4's storage pin).

`pkg/board/phrase_read.go`: the whole translation seam generalizes from
"the assignee field" to "the task's session fields" via
`payloadSessionKeys = {assignee_session, implementer_session}`:

- `translateTaskReads` — the per-read copy rewrite every public read path
  already calls (GetTask, ListTasks, task-events, search/health, write
  echoes) — now translates BOTH fields off ONE consult set: both raw ids
  join the same per-workspace `session_index` lookup (the implementer lives
  in the assignee's workspace by construction — it IS a prior assignee of
  the row), then each field resolves independently via the new
  `translateSessionField` helper (warm phrase / cold raw+flagged / empty
  stays unflagged / already-phrase is a no-op).
- `flagUntranslated` (the workspace-resolve failure path) flags each raw
  field it finds, independently.
- `PayloadAssigneeIDs` + `TranslatePayloadAssignees` (the SSE
  `board.event` emission edge, consumed by `api/internal/handlers/board_sse.go`)
  walk both keys — task snapshots in board events translate the implementer
  too, same never-worse degrade (`implementer_session_deprecated` sibling
  flag).

No new endpoint, no new consult: the phrase machinery, the degrade posture,
and the one-bounded-consult-per-workspace cost profile are all #1922's,
extended by one key.

## 2 — The frontend: the refs are links

Chat-route target: `/chat/{workspaceId}/{sessionId}` — the SAME target as
clicking the session in the sidebar; the route accepts the phrase or the
canonical id, and the read edge already serves the phrase (the short
3-word display text the ruling's "canonical ID is too long" concern asks
for). The href and the display text are the same field value.

- `TaskDrawer.tsx` — the ids popup (the declutter ruling's click-through
  where ids live): the Claim/lease row's assignee becomes a `SessionLink`
  (React Router `Link`, `drawer-assignee` testid kept), and a new
  Implementer row renders `implementer_session` as the second link
  (`drawer-implementer`) whenever the identity exists — during review the
  assignee is the reviewer, so this is the row's worker identity. The
  shared `SessionLink` renderer: dotted-underline mono link,
  `data-deprecated` attr in the cold window, and two never-broken
  degrades — no `workspaceId` (isolated render) → plain `<span>`, no link;
  archived/deleted session → the link still renders, the chat page owns
  its not-found state.
- `TaskCard.tsx` — the card's busy/assignee area: the assignee ref (next
  to the `BusyIndicator`) becomes the same link
  (`assignee-link-{short_id}`), `stopPropagation` so it is not a card
  click, truncating mono text; degrades to plain text without a
  workspace id.
- `BoardPage.tsx` — threads `project.target_workspace_id` down through
  `StageCell` to the cards and resolves it for the drawer from the task's
  project (the drawer opens from search/notices too, not just a lane).
- `api/board.ts` — `implementer_session` / `implementer_session_deprecated`
  types, documented as resolvable session refs.

## 3 — The API/MCP/SDK surfaces: the field IS the session ref

- REST: `assignee_session` + `implementer_session` (both with their
  deprecation flags) ride every task read/write-echo — the handlers
  serialize `board.Task` directly; nothing new to wire.
- openapi.yaml (`BoardTask` schema): both fields + flags documented —
  assignee_session's description now states it is a RESOLVABLE session ref
  (phrase or raw, both address the chat route / the session REST surface
  / `session_read` / `session_metadata`). `make -C sdks sdk-check` green
  (spec valid + router parity).
- Go SDK (`sdks/go/board.go`) + TS SDK (`sdks/typescript/src/types.ts`):
  the four fields added with the same resolvable-ref docs.
- MCP descriptions: `board_read` and `board_list` (agentd catalog,
  `cmd/workspace-agentd/mcp_server.go`) and the api-side `pkg/mcp/board_tools.go`
  board tool now say it in prose: the session fields are RESOLVABLE refs —
  pass either form to `session_read` / `session_metadata` to inspect the
  worker behind a claim. (The ruling's "via api mcp sdk we can look up the
  session ref" — verified: no new endpoint needed.)

## 4 — Tests

Go (unit, `pkg/board`): `TestTranslateSessionField` (per-field matrix),
`TestFlagUntranslated` (implementer-only rows, both-fields rows),
`TestPayloadAssigneeIDs`/`TestTranslatePayloadAssignees` extended to the
implementer key.

Go (PG integration, born-red posture): `TestReads_ImplementerSessionPhrase`
— cold window serves raw+`implementer_session_deprecated":true` on GetTask
AND ListTasks; minting the phrase converges BOTH fields flag-free; the
MCP surface (`GetTaskInWorkspace`) serves the phrase; and the stored row
stays canonical in BOTH columns (read-time translation only).

Frontend (vitest): `TaskDrawer.test.tsx` +4 — assignee link target
`/chat/ws-1/amber-falcon-glides`; implementer link; the cold-window row
(raw id href + `data-deprecated`, never broken); the no-workspace degrade
(span, no anchor). `BoardPage.declutter.test.tsx` +1 — the ids popup's
session ids are clickable (assignee + implementer legs). `BoardPage.test.tsx`
+2 — the CARD's assignee link target/display; the cold-window card link;
unclaimed rows carry no link. `boardTestUtils.tsx` fixtures updated to the
warm wire shape (phrase values) + a review-lifecycle implementer row.

Gates run locally: `go build` (board/mcp/agentd/api), `go vet`,
pkg/board + pkg/mcp FULL integration suites (PG on :5433) green,
`make -C sdks sdk-check` green, handlers Phrase suite green, frontend
`tsc --noEmit` + FULL vitest (223 files / 2562 tests) green, Playwright
`board.spec.ts` (33 rows) green, TS SDK typecheck + tests (91) green.

## 5 — Notably NOT done (scope guards)

- No new endpoint, no new projection, no per-field second consult — the
  ruling's "just need wire" taken literally; the one enrichment (read-time
  phrase resolution) reuses the #1922/#1831 `session_index` replica consult
  pattern already on every task read.
- The card/drawer never mint their own session lookups (no client-side
  fetch per card) — the read edge carries the phrase, full stop.

## 6 — Review round 1 (GitHub AI reviewer, CHANGES_REQUESTED on 2d9764ff1)

Two findings; both addressed on the branch.

**Finding 1 — the REST task-events edge served raw unflagged
`implementer_session` in delete/cascade payload snapshots.** The actor
leg of the events read translated (`translateEventActors`) but
`Event.Payload` rode byte-identical — and delete/cascade events embed
`snapshotOf(board.Task)`, which since this PR serializes
`implementer_session` (raw, stored canonical) with no cold-window flag:
a never-worse violation on a read edge. Fix: the seam generalizes to
`translateEventReads` (`pkg/board/phrase_read.go`) — the consult set
unions the actor session ids with `PayloadAssigneeIDs(ev.Payload)` (both
session keys), and the apply loop runs `TranslateActorSessionPhrase` on
Actor AND `TranslatePayloadAssignees` on Payload. One consult, both
legs, same degrade posture (ws-resolve failure or cold window →
raw+flagged, logged). Routing note for the pin: a deleted task's OWN
timeline is hidden behind TaskEvents' resolveTask gate (honest 404), so
`TestReads_TaskEventsPayloadSnapshots` copies the REAL delete-event
payload bytes (delete.go's own writer, raw ids and all) onto a LIVE
sibling task's timeline and reads them through the genuine TaskEvents
path — warm leg (phrase on both fields, no flags) + cold leg (raw on
both fields, both flags) + storage immutability (every stored delete
payload keeps the canonical raw implementer).

**Missing click-through coverage (the PR's headline behavior was never
clicked).** Three tiers added:

- vitest CLICK pins — new `TaskCard.test.tsx` (3 rows: card link
  navigates to `/chat/ws-1/amber-falcon-glides` via a routed probe;
  the click does NOT fire `onOpen` — the stopPropagation contract,
  previously an untested code-comment claim; the cold-window raw id
  navigates too) and `TaskDrawer.test.tsx` +2 click rows (assignee +
  implementer legs navigate; `renderDrawerLinked` grew real `Routes`
  with a `ChatRouteProbe`).
- Playwright `board.spec.ts` +1 real-browser row — the card leg
  (assignee link lands on `/chat/{ws}/{phrase}`, edit dialog NOT
  open) and the drawer leg (review row's implementer link lands on
  the chat route); sessions-list mocked quiet for the destination.
- The REST events-read integration pin above (red before the
  Finding-1 fix by construction — the seam never touched payloads).

Gates re-run locally: `go build ./...`, `go vet` (+ `-tags integration`
vet), `go test -race ./pkg/board/`, handlers suite, frontend `tsc
--noEmit` (0 errors), FULL vitest (224 files / 2567 tests), Playwright
`board.spec.ts` (34 rows) — all green. (The PG integration tier skips
in this sandbox — no server binary; vet-with-tag covers compile, and
the suite's structure follows the file's existing born-red pattern.)
