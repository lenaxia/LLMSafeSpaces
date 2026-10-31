# Worklog: the DEPOSED directive — first-class salvage handoff (S5)

Task `tsk-glassy-okra-wiggles` (epic: Liveness and observation: every
claim observable, every lapse visible — the 2026-10-30 incident
remediation, S5). Branch `sprint-board-story-machinery` (the epic
wave's working branch; S1's stamp-hardening commits are its prior
heads).

## The problem (from the incident transcripts)

When an attempt lapses holding pushed work, recovery was ad-hoc. In the
2026-10-30 incident (the `tool_18e3*` transcripts) the orchestrator
read transcripts and hand-wrote a steering note:

> "Salvage state you should build on instead of starting cold: the
> story's first worker (deposed by a lease lapse at 17:44) completed
> the full rename sweep and PUSHED it — branch
> feat/sprint-board-story-vocabulary (head 65e4d9dcd: worklog 1237,
> CHANGELOG [Unreleased] entry, COORDINATE claim row; the shared
> checkout /workspace/LLMSafeSpaces is sitting ON that branch, clean)."

That note cost 4 worker sessions and 3 claims: `viridian-factory-
triumphs` (the lapsing worker), `suave-keystone-tramps` (the hand-note
recipient, which re-derived the state anyway), `rival-octave-feels`
and `zesty-archer-leaves` (the glassy-shovel lane, where each pickup
began "let me verify what's actually in it, whether it's pushed, and
the PR state" — re-derivation every time). One worker pivoted to
salvage on its own initiative; another re-did work. The pushed work
lived nowhere on the row.

## What the directive must capture (mined from those transcripts)

branch, PR number, last known head, what was done (worklog/CHANGELOG/
gates state), and what the inheritor should do — plus WHO deposed and
when (audit). That is exactly `SalvagePointer{branch, pr, head, state,
note}` + the `depose` audit event.

## What landed

1. **Migration 000054** — `tasks.salvage` JSONB, latest-only (the
   `nudge_id` house pattern): each depose overwrites; the trail is the
   audited events. Synced to helm (`make chart-sync-migrations`;
   repolint green).

2. **`Service.DeposeTask`** (`pkg/board/depose.go`, the manager route
   — `taskProjectForActor` guards, the pause.go house pattern):
   - in_progress + LAPSED: the guarded UPDATE requeues EXACTLY as
     rc1's reclaim would (failure_count+1, backoff cooldown, episode
     slots cleared, attempts untouched — the increment belongs to the
     next claim) plus the pointer; transition written; DEPOSED
     directive delivered to the captured pre-write assignee
     post-commit (best-effort, the HALT/RESUME V1 transport — never an
     abort); `pokeDrive`.
   - backlog (the reclaim already ran — the incident's actual shape):
     the pointer alone. No stage move, no second failure count.
   - refusals: done (immutable), failed (rearm lane), in_review
     (persona pass), pending_input-flagged (its enumerated exits), and
     the empty pointer (GuardInput).
   - textguard on the free-text fields (512B refs, 32KiB state/note).

3. **The live-claim guard (`claim_live`, pinned)** — two belts:
   - Go-side pre-check under the row lock: unexpired lease → refuse;
     expired + life evidence (seam movement / Busy / EventsAfterNudge,
     or a heartbeat after the latest nudge — the ladder's own renew
     signals) → refuse. An unreadable evidence read defers to the
     write predicate (the b1a73e79c lesson: the unanswerable question
     never kills).
   - SQL-side arbitration at UPDATE time (DB clock): a renewal racing
     the check converges refused (0 rows → GuardClaimLive). No-evidence
     (no seam / no assignee) degrades to the wall-clock lease — S1's
     stamp/lapse evidence is the trust chain, which is why this story
     depended on S1.

4. **Charter embed (inherit, don't restart)** — `Claim.Salvage`
   attaches at every spawn path (spawnAfter like VerdictChain; spawnFor
   from the loaded task), `boardCharterPrompt` renders
   `board.RenderSalvage`: "SALVAGE HANDOFF … INHERIT this state …
   never as verified truth". Pointer-less claims render
   byte-identically (pinned).

5. **Directive text** — `deposedDirectiveText` in the
   `newBoardMessenger` switch: the stale session is told the pointer
   was recorded, the token is revoked, and its next write 409s.

6. **Surfaces** (the rearm template, all live — the parity grid row):
   REST `POST /me/tasks/:id/depose` + `BoardHandler.DeposeTask`; the
   pod seam `BoardDeposeTask` + route; `pkg/mcp` tool +
   `BoardDeposeTask` client; agentd registry/dispatch/allowlists +
   `BoardDeposeTask` loopback; the origin-plugin injection list;
   openapi (`deposeBoardTask`, `BoardSalvagePointer`,
   `BoardDeposeRequest`, `BoardTask.salvage` — schema-parity green);
   frontend `claim_live` in the guard-codes fixture +
   `BOARD_GUARD_MESSAGES`.

## Tests

- `pkg/board/depose_integration_test.go` — 5 suites on real PG:
  requeue+pointer+audit-payload+directive+requestId-replay; the
  live-claim refusal (unexpired lease; movement; busy; post-nudge
  heartbeat; each refuse with NOTHING written) and the degradable
  no-evidence depose; the backlog pointer-only shape (cooldown-cleared
  re-claim still embeds); the next-claim charter embed (fresh grant
  AND post-reclaim, latest-only overwrite); the enumerated refusals
  (done/failed/in_review/empty-pointer/foreign-owner).
- `TestBoardWorkerComms_Messenger_Deposed_RideV1Detached` — the
  directive rides the LIVE V1 route, fire-and-forget, with the stop
  contract.
- `TestCharterPrompt_RendersSalvageHandoff` / `NoSalvageNoSection`.
- The parity grid's `depose_task` row + both route-table pins + the
  guard-codes fixture test + the agentd registry/injection pins + the
  external-MCP 58-tool count pin.

## Verification

Local PG 15 (port 5433, migration 054 applied):
`go test -p 1 ./pkg/board/ ./api/internal/handlers/ ./local/
./pkg/mcp/ ./cmd/workspace-agentd/ -tags integration` — ALL ok
(pkg/board 41.8s, handlers 87.1s, local 34.0s, mcp 2.2s, agentd
391.5s). NOTE: these packages share one test DB — run with `-p 1`
(serialize) or the suites truncate each other's fixtures (the first
full run's "failures" were exactly that, green on the serialized run).
Unit: `go test ./pkg/board/ ./pkg/mcp/ ./api/...` green;
`make fmt-check` green; `make openapi-validate` green; `make
repolint` green; `chart-sync-migrations` applied; frontend
`vitest run src/lib/boardRules.test.ts` 66/66.

## Design notes (the decisions worth remembering)

- Depose does NOT invent new accounting: an in_progress requeue is
  byte-for-byte rc1's reclaim overlay — depose is the reclaim,
  accelerated by a manager judgment, with the handoff attached. The
  next sweep would have done the same write.
- Latest-only pointer, never consumed-on-claim: a stillborn re-claim
  (spawn dies instantly) keeps the handoff instead of silently losing
  it; the charter says "verify" so staleness is the inheritor's first
  check. A new depose overwrites with fresher truth (pinned).
- The DEPOSED directive is deliberately content-free (no pointer
  prose): the deposed worker needs the STOP contract, not the handoff
  — the inheritor's charter carries the pointer.
