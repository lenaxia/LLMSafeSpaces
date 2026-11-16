# Epic wrap-up review — high-level design (owner-settled direction, 2026-11-15)

**Status:** Design (2026-11-15) — every product decision below is owner-settled in the epic
charter ("Epic wrap-up review (holistic, delta-scoped, role-configurable)"); this document and
its companion low-level design (`epic-wrapup-review-low-level-design.md`) elaborate **HOW**, not
whether. The four implementation stories on board project `prj-orient-mint-stencils`
(wrapup-engine, wrapup-watermark-diff, wrapup-config-persona, wrapup-gap-recursion) are patched
from these designs; their patched charters are binding.
**Companion:** `reports/specs/epic-wrapup-review-low-level-design.md` — schema/migration, exact
functions/files, verdict schema, charter renderer, cap/ack state machine.
**Seam verification:** all file/line anchors in both documents were verified against
`de99d5361` (main, 2026-11-15).

---

## 1. The problem, in half a page

Story-level reviews see fragments: each in_review worker reads one story's diff against that
story's criteria, and the epic's outcome field is checked by nobody. When the last story lands,
`epicCompletionCheck` (pkg/board/store.go:449) flips the epic to `completed` on the next
transition — coherence across stories, integration seams, dead/orphaned code, and
built-but-never-wired features are never examined as a whole. Epics also accumulate drift:
PR evidence that never merged, features registered nowhere, flags left at their default because
nobody decided.

The wrap-up review closes that hole with ONE synthetic review pass per epic-drain: when an epic
with `wrapUpReview` enabled drains (its story set reaches all-terminal), the engine mints a
wrap-up story claimed by a configurable reviewer role, DEFERS epic auto-completion until that
review closes, and scopes the review to the delta since the last wrap-up. The reviewer reads the
aggregate diff against the epic's outcome and either passes the epic or files gap stories; gap
completion triggers the next, delta-scoped wrap-up. A recursion cap keeps a pedantic reviewer
from looping forever.

## 2. Owner-settled rulings (transcribed from the epic charter — binding)

- **TRIGGER**: engine detects the epic's story set all-terminal; if the epic has `wrapUpReview`
  enabled (epic flag, default off), mint a synthetic wrap-up story claimed by the reviewer
  persona; epic auto-completion is DEFERRED until the wrap-up closes. Wrap-up rows are EXCLUDED
  from the epic's own completion math (no self-blocking paradox).
- **SCOPE**: `last_wrapped_at` watermark on the epic; the wrap-up covers stories completed since
  the watermark (first run: all). The watermark does double duty: coverage tracking AND bounding
  the aggregate diff the reviewer reads.
- **THE REVIEW ITSELF (the point)**: a holistic code review of the aggregate diff since the
  watermark (or epic start) against the epic's outcome field — coherence across stories,
  integration seams, dead/orphaned code, built-but-never-wired features. Story-level reviews see
  fragments; the wrap-up reads the union. Evidence verification (PRs merged, issues closed) and
  wiring (registered/mounted/reachable, flag states are decisions not defaults) are LENSES
  within this review, never a mechanical checklist replacing it.
- **GAPS**: the reviewer files gap stories (tagged `gap`, deduped against existing rows) via
  normal create paths; their completion triggers the next wrap-up (delta-scoped). Recursion cap:
  ~3 consecutive wrap-ups per epic, then human ack is required — a pedantic reviewer cannot loop
  forever.
- **REVIEWER ROLE**: `wrapUpReviewer` config slot (bare role-ref, stagePersonas pattern), user
  default + project override ONLY. Default: a seeded built-in wrap-up reviewer persona whose
  system prompt carries the holistic-aggregate-diff charter. Unresolved refs refuse at set time.
  ENGINE OWNS PROCEDURE (charter, verdict schema, gap discipline, caps); ROLE OWNS JUDGMENT
  (lens, model, review philosophy) — a custom role cannot opt out of the procedure.

## 3. The lifecycle (one pass, end to end)

```
                ┌──────────────── epic active, wrapUpReview=true ────────────────┐
                │                                                                │
  story →done ──┴─► drain check (inside the done transaction)                    │
                        │ all kind='story' rows done? ── no ──► nothing (today)  │
                        │ yes                                                  │
                        ├─ wrap-up in flight? ─────────── yes ──► defer (no-op)  │
                        ├─ wrapup_count ≥ 3, unacked? ──── yes ──► ACK-WAIT      │
                        │        (epic.wrapup_ack_pending=true, loud event,      │
                        │         surfaced on epic reads + health; human ack     │
                        │         resets the count and mints fresh)              │
                        └─ else MINT: wrap-up story born in in_review,           │
                          reviewer-persona claim minted in the same transaction, │
                          charter description rendered from the epic state       │
                          (window, story list, verdict procedure).               │
                          Epic completion is impossible while it flies.          │
                │                                                                │
  reviewer closes it via board_record_wrapup_verdict (engine-owned verb):        │
    PASS  → wrap-up row done (evidence=report URL), watermark advances to the    │
            row's mint time, count++, and the epic completes iff still drained   │
            (atomically, same transaction).                                     │
    GAPS  → wrap-up row done, watermark advances, count++, gapRefs validated     │
            (tagged gap stories in this epic, filed since mint); epic stays      │
            open — the non-done gap rows are what hold it open.                  │
                │                                                                │
  gap stories complete ──► drain check again ──► mint the NEXT wrap-up,          │
  scoped to the delta since the new watermark. Cap applies to consecutive        │
  wrap-ups; ack resets the ledger.                                              │
```

## 4. Component map (what exists, what is new)

| Component | Today (verified anchors) | Wrap-up change |
|---|---|---|
| Epic drain / auto-complete | `epicCompletionCheck` — pkg/board/store.go:449-463; five call sites: stage.go:516 (`doneCascade`), review.go:141, service.go:866 (`epicLifecycle` recheck), reconciler.go:1308 (PR drift), reconciler.go:1517 (`rc4Net`) | becomes `wrapUpAwareEpicCompletion`: lock the epic row, branch complete/mint/defer/ack-wait. Single seam — every call site inherits the behavior |
| Story minting | `CreateTask` — service.go:886 (owner actor, replay guards, `mintUnique` phrase id, INSERT, deps) | engine-internal `mintWrapUpTx` mirrors the shape (phrase id, event, post-commit spawn) but bypasses owner actor + node cap; born `kind='wrapup'`, stage `in_review`, persona claim minted in-tx |
| Claim/lease machinery | `ClaimNext` engine.go:76; persona claim `personaClaimMintTx` persona.go:276 + `personaClaimSQL` :264; in_review entry mint stage.go:225; reconciler re-claim pass reconciler.go:~1005-1040; `spawnFor` engine.go:675 | wrap-up rows are CLAIMS like any other: renewal/nudge/wedge ladder applies unchanged. Two carve-outs: `ClaimNext`'s candidate predicate excludes `kind='wrapup'` (never a worker story); persona resolution for wrap-up rows returns `wrapUpReviewer` (mint pass, reconciler re-claim pass, `spawnFor`) |
| Store schema | epics/tasks — migrations/000036; additive-column pattern 000054 | two additive migrations: 000055 (config key, wrapup-config-persona) + 000056 (`tasks.kind`, `epics.wrap_up_review`, `epics.last_wrapped_at`, `epics.wrapup_count`, `epics.wrapup_task_id`, `epics.wrapup_ack_pending`, wrapup-engine) |
| Config surface | config.go keys :48-55, `BoardConfig` :196, set-time role-ref validation `validatePersonaRoleRefs` :670, views :785; key-CHECK growth pattern migrations 000042→000049 | new `wrapUpReviewer` key on BOTH tiers (user + project); default = seeded role slug `wrapup-reviewer`; rendered with source annotations |
| Persona seeding | `DefaultReviewerRoleSlug`/`SystemPrompt` persona.go:70-95; builtin pairs builtin_roles.go; `seedBuiltinReviewerRole` / `seedBuiltinBoardRoles` — api/internal/app/board_role_directory.go:101,144, wired app.go:1678-1698 | sibling: `wrapup-reviewer` platform role seeded at startup, system prompt = the holistic-aggregate-diff charter (judgment half; procedure stays engine-side) |
| Charter rendering | `boardCharterPrompt` — api/internal/app/board_spawner.go:47-108 | unchanged shape; the wrap-up story's DESCRIPTION (engine-rendered at mint, pkg/board/wrapup.go `RenderWrapUpCharter`) carries the window + story list + procedure — the generic charter renderer needs no wrap-up branch |
| Verdicts | `VerdictInput`/`RecordVerdict` verdict.go:55-117 (approve/fail, per-story criteria) | new `RecordWrapUpVerdict` (pass/gaps) — the epic-level close verb; story verdicts untouched |
| Events/audit | `recordEventTx`, PK `(project_id, id)` | actions `wrapup_mint`, `wrapup_mint_deferred`, `wrapup_verdict`, `wrapup_epic_complete`, `wrapup_ack_required`, `wrapup_ack`; deterministic ids where replay-convergence matters |
| Surfaces | REST router.go:1378-1403; external MCP pkg/mcp/board_tools.go; agent tools cmd/workspace-agentd/mcp_tools.go:2590-2600 | new: `board_record_wrapup_verdict`, `board_ack_epic_wrapup` (manager); epic reads/health carry the wrap-up state |

## 5. Design decisions (the "why" for each seam)

**D1 — The completion check is the single interception seam.** All five completion call sites
already funnel through one function (`epicCompletionCheck`). Making THAT function wrap-up-aware
(instead of adding a parallel sweep) means: PR-drift completions, review-settle completions,
manager moves, and the rc4 net all get identical wrap-up behavior with no per-site patching, and
the mint decision is transactionally inside the same `→done` write that reached drain — no
window between "drained" and "wrap-up minted" in which a concurrent writer could complete the
epic. The epic row is locked (`SELECT … FOR UPDATE`) first; concurrent drains serialize on the
lock and the second sees the in-flight wrap-up and defers.

**D2 — Self-exclusion by `tasks.kind`, deferral by explicit state, not row-counting.** Wrap-up
rows carry `kind='wrapup'`; the drain predicate and the completion predicate count
`kind='story'` rows only. The epic's wrap-up lifecycle state is explicit columns
(`wrapup_task_id` = the in-flight row, `wrapup_count`, `wrapup_ack_pending`, `last_wrapped_at`)
— never derived from "does a wrap-up row exist" (racy) and never implied by row counting (the
paradox the owner ruled out). `wrapup_task_id IS NULL` is the mint's arbitration guard: exactly
one mint wins under any concurrency.

**D3 — A wrap-up claim is a claim.** The reviewer persona worker gets a real token + lease via
the same `personaClaimMintTx`; movement renews, quiet gets nudged, silence wedges to `failed`,
rearm recovers. Nothing about the lease ladder changes. The only lifecycle carve-outs are
dispatch-facing: `ClaimNext`/`dispatchDrive` never serve `kind='wrapup'` rows to worker
spawns, `rearm` of a failed wrap-up row returns it to `in_review` (where the reconciler's
existing unclaimed-in_review pass re-mints the reviewer claim) rather than to `backlog`, and
persona resolution for wrap-up rows returns the `wrapUpReviewer` role.

**D4 — The watermark advances at CLOSE, to the MINT-TIME snapshot.** `last_wrapped_at` is set
to the closing wrap-up row's `created_at` (its mint moment), inside the verdict transaction.
Stories that completed while the review was in flight fall after the snapshot and are covered
by the NEXT wrap-up — no coverage hole, no double-cover. Atomicity: the advance, the verdict
event, the row's `→done`, and (on PASS) the epic's completion are one transaction; the epic
update is guarded `WHERE wrapup_task_id = <row>` so a replayed or raced verdict is a clean
no-op.

**D5 — The diff range is a rendered window, not engine-side git.** The engine never runs git.
The minted charter embeds the bounded window — `[coalesce(last_wrapped_at, epic.created_at),
wrapup_row.created_at]`, both RFC 3339 UTC — plus the covered story list with their evidence
URLs. The reviewer (running in the project's target workspace, where the repo lives) computes
`git log --since --until origin/main` after a fetch, and reads each story's PR head where work
is unmerged. The engine supplies DATA; the ROLE supplies the lens — exactly the
procedure/judgment split.

**D6 — PASS/GAPS is an explicit engine verb, not an inferred one.** A plain `move_stage→done`
on a wrap-up row cannot distinguish "approved" from "filed gaps", so the close path is
`board_record_wrapup_verdict` (claim-token guarded, `kind='wrapup'`-only): `{verdict: pass|gaps,
reportUrl, gapRefs[], notes}`. PASS → epic completes (atomically with the watermark advance).
GAPS → gapRefs must resolve to `gap`-tagged stories in this epic filed since mint; the epic
stays open because those rows are non-done `kind='story'` rows — the ordinary drain predicate
does the holding with zero new machinery. Gap stories are filed through the NORMAL create path
(`board_create_story`); dedup against existing rows is reviewer discipline written into the
charter, with the engine validating refs at verdict time as the belt.

**D7 — The cap counts consecutive closes; ack is a manager verb.** `wrapup_count` increments at
every wrap-up close (pass or gaps). At a drain with `wrapup_count ≥ 3` and no pending ack, the
engine sets `wrapup_ack_pending`, records a loud `wrapup_ack_required` event, and mints nothing.
The ack (`board_ack_epic_wrapup`, manager authz — the rearm/depose posture) resets the count,
clears the flag, and re-runs the drain check in the same transaction (an acked drain mints a
fresh wrap-up immediately — the human said "keep going"). Surface: the flag rides the epic read
(`board_read`/`board_list` on the epic projection) and `board_health`; it is NOT a
pending_input flag (epics have none — a dedicated epic-level ack state is the honest shape, not
a contortion of the task-level flag vocabulary).

**D8 — Config follows the stagePersonas pattern exactly, minus the stage map.** One key,
`wrapUpReviewer`, on both tiers (user default + project override; per the owner ruling there is
deliberately NO per-epic reviewer override beyond the epic's own on/off `wrapUpReview` flag).
Values are bare role refs (id or slug), validated against the RoleDirectory at set time — an
unresolved ref refuses the whole write, identically to `stagePersonas` today. The default is
the seeded `wrapup-reviewer` platform role; `board_get_config` renders the slot with source
annotations like its siblings.

**D9 — Disabled epics are byte-identical.** `wrapUpReview=false` (the default) takes the
today-branch of the completion check verbatim — same SQL shape, same semantics. The regression
pin is explicit in wrapup-engine's criteria.

## 6. Failure modes, stated plainly

- **Wrap-up worker dies mid-review.** It held a real claim: the seamless-lease ladder runs
  (nudge → wedge verdict → `failed` with evidence). A failed wrap-up row parks like any failed
  row; `board_rearm_story` returns it to `in_review` (D3 carve-out), the reconciler's
  unclaimed-in_review pass re-mints the reviewer claim, and a fresh reviewer re-runs the SAME
  window (the charter description is frozen on the row). The epic simply stays open a little
  longer. At escalation the row waits for the human like every exhausted row — the epic never
  silently completes past an unfinished review.
- **Crash between verdict and watermark advance.** Impossible to observe: one transaction. A
  crash before commit leaves the row in_review (verdict never landed); after commit, both
  landed. The verdict event id is deterministic (`wrapup:verdict:<taskID>`), so a client retry
  replays to the original outcome — no double-advance.
- **Two gap stories complete concurrently.** Both transactions reach the drain check; both lock
  the epic row; they serialize. The first mints (sets `wrapup_task_id`), the second sees the
  in-flight row and defers. Exactly one wrap-up per drain, always.
- **Reviewer files a gap story that races its own verdict.** The GAPS verdict validates
  `gapRefs` against rows that exist at verdict time; a gap filed but not referenced is simply
  part of the next delta (it holds the epic open through the ordinary drain predicate, and the
  next wrap-up's charter lists it).
- **`wrapUpReviewer` unresolvable at mint** (role deleted post-config). The mint defers (warn
  event, no row) and the drain re-fires on the next qualifying transition/sweep — mirroring
  persona degradation discipline elsewhere: never a role-less spawn, never a claim nobody can
  hold. The seeded default makes this near-impossible; the posture is fail-closed.
- **Human never acks.** The epic sits in ack-wait indefinitely, visibly (`wrapup_ack_pending`
  on every epic read + health). That is the intended human circuit-breaker, not a wedge —
  archive/pause/delete remain available.

## 7. What this design deliberately does NOT do

- No engine-side git execution (D5) — the charter carries a window; the worker's workspace
  holds the repo.
- No new lease/heartbeat semantics (D3) — wrap-up claims ride the existing ladder.
- No pending_input vocabulary growth (D7) — ack is an epic-level state + manager verb.
- No per-story verdict changes (D6) — `board_record_verdict` is untouched; the wrap-up verdict
  is a sibling verb for a different object (the epic's aggregate).
- No instance-tier config (house ruling from 000042 stands: user → project only).

## 8. Open questions for the owner (non-blocking, defaults chosen)

1. **Cap value = 3** (of "~3" in the charter). Pinned as `WrapUpConsecutiveCap = 3`, an
   `EngineConfig` field — one-line change if the owner prefers 2 or 4.
2. **Ack resets the count to zero** (fresh ledger) rather than decrementing by one. Chosen:
   the ack semantically says "this loop is productive, continue" — a fresh ledger matches the
   rearm precedent (attempts reset to 0).
3. **Wrap-up rows count against `MaxNodesPerEpic`?** Chosen: no — the engine mint bypasses the
   user-facing blast-radius cap (it is not owner-authored graph growth, and capping it could
   make a drained epic unreviewable). The cap still guards human-created rows.
