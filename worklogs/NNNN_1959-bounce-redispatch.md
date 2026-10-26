# Worklog — #1959: verdict-driven bounce transition + original-worker re-dispatch

Lane: `fix/1959-bounce-redispatch` (lane-1959, orchestrator novel-nutmeg-verifies).
Issue: #1959 (owner ruling recorded on comment 6405969037). Do not regress #1958
(episode-unique RunIDs — verified unregressed, pins green + new bounce-round pin).

## 1. Validate-first (born RED, then root cause)

Live evidence re-read from the reviewer transcripts (not just the issue prose):

- **cloven-loggia-snorts**: spawned 23:26:17Z, verdict 23:27:13Z → `applied:false,
  transition:"refused:stage_transition"`; manual bounce 23:27:50Z → 409
  `stage_transition: "bounce requires a recorded assignee (no token-less in_progress rows)"`.
- **drafty-persimmon-generates**: spawned 23:45:11Z, verdict **23:46:05Z**, assignee
  stamped **23:46:37Z** (tool-call timestamps extracted from the transcript) — the
  orchestrator's "stamp in place" was a post-hoc board_list read; the verdict fired
  **32s INSIDE the mint→stamp window**. Identical refusal.

Repro (PG harness, `TestBounce1959_*`, born RED on the pre-fix tree):
`armRearmWindow1959` drives the exact live shape — implementer grant (stamped) →
forward exit → reviewer persona mint → reviewer dies still unstamped → #1947 lapse
pass clears claim AND assignee identity → re-arm mint (identity stays NULL) → fresh
reviewer claim, verdict imminent, `assignee_session` NULL. Pre-fix: the verdict pin
fails with `applied:false transition="refused:stage_transition"` (byte-identical to
the live tool evidence); the manual pin fails with the 409 guard; the no-identity pin
fails refused.

### Root cause (WHICH field the guard reads)

The bounce branch (stage.go, in_review→in_progress) guarded on
`cur.AssigneeSession != ""` — the **live-claim** identity column. That column serves
two masters with conflicting lifecycles:

1. **The mint→stamp window (the refusal)**: since #1947's rc1-parity fix, every
   reviewer persona lapse CLEARS `assignee_session`; every re-arm mint
   (`personaClaimSQL`) preserves it as it stands (NULL). The fresh reviewer's
   ASYNC spawn-stamp lands ~90s later in prod (pod activation latency). A prompt
   reviewer ALWAYS beats its own stamp → the guard reads `""` → refused. Same for a
   first-round reviewer when the implementer's own stamp had not landed at the
   forward exit (the column was never populated). The error message's condition was
   accurate for the column it read — disproven only against the *board_list*
   observation, which raced the verdict (the stamp landed after).
2. **The stamp clobber (the wrong target)**: once the reviewer's stamp DOES land,
   the column holds the REVIEWER's session — not the implementer the bounce wanted.
   And `reclaimTx` ignored its `assignee` parameter entirely (`_ = assignee`) — the
   bounce ALWAYS spawned a fresh worker, never the original.

The manual `board_move_stage` bounce refused with the same guard because both paths
funnel through `moveStageTx`'s single in_review→in_progress arm — confirmed by pin.

### The 0.44.0 contrast (bisect)

`git diff v0.44.0..v0.44.1` touches ONLY `api/internal/app/board_spawner.go` (the
#1958 RunID work) — zero transition-code delta. The 0.44.0 16:47Z success bounced a
row whose reviewer stamp HAD landed (guard passed, mint fired); the 0.44.1 probes
verdicted inside the window. The regression is STATE (the #1947 lapse-clear made the
window recur on every reviewer lapse→re-arm cycle), not the 0.44.1 diff. #1958 is
unregressed: all its pins green on this head, plus a new bounce-round RunID pin.

## 2. Fix

- **Migration 000051** (`tasks.implementer_session`): the bounce-consumer identity,
  captured at the in_progress→in_review forward exit (`implementer_session =
  assignee_session` — the SET reads the pre-update column), preserved through the
  whole review lifecycle (the reviewer mint/stamp/lapse never touches it), cleared
  on every new episode (grant, TaskStart, pending_input exits, throttle requeue,
  in_progress lapse — NOT the in_review persona lapse, whose preserve IS the fix).
  Backfill: pre-migration in_review rows without a persona claim. Not exposed on
  read edges (`json:"-"`) — internal engine addressing, no phrase-translation debt.
  Chart mirror synced (`helm/migrations/`).
- **The bounce arm**: guard now reads `bounceImplementer(cur)` — implementer_session,
  falling back to the legacy retain semantics (assignee when no persona claim holds
  the row) for pre-migration rows. The `assignee_session == ""` refusal is GONE: a
  no-identity bounce mints for a FRESH implementer (never a refusal, never a
  token-less row — the fallback stamp or the direct mint covers it). The paused-row
  refusal stays (#1747).
- **bounceTx** (replaces reclaimTx): fresh token + lease, attempts unchanged,
  persona cleared, and — when the implementer is known — `assignee_session` stamped
  with it IN the mint statement (the never-token-less invariant by construction).
- **Re-dispatch (the ruling)**: `nudge.redispatch` → `redispatchBounce` post-commit:
  never-turned check (turn evidence ≤ 0), then delivery of the re-dispatch turn to
  the ORIGINAL implementer's session via the new `BounceRedispatcher` seam
  (app wiring: `newBoardRedispatcher` on `adapter.SendAsync` — existing-session
  delivery, the same machinery as the halt/resume directives; NOT a session:new
  dispatch, so no workflow RunID is involved and #1958's uniqueness is untouched).
  Fallback to `spawnFor` (fresh implementer) ONLY on: no seam installed (inert
  engine), never-turned original, or delivery error (archived/reaped).
- **Verdict chain embedding**: `RenderVerdictChain` extracted to pkg/board; the
  fresh-spawn charter AND the re-dispatch message render the identical block — the
  original worker sees byte-equivalent findings whether re-dispatched or replaced.

## 3. Pins (all born RED where the defect lived; mutation-verified)

- `TestBounce1959_VerdictFiresInsideMintStampWindow` — the live refusal, red-first.
- `TestBounce1959_ManualBounceInsideMintStampWindow` — same guard arms the manual path.
- `TestBounce1959_RedispatchReachesOriginalImplementer` — delivery to the ORIGINAL
  session id, fresh token + findings embedded, mint stamps the implementer, NO fresh
  spawn when the original is live.
- `TestBounce1959_FallbackOnDeadOriginal_NeverTurned` / `_DeliveryFailure` — the
  ruling's two dead-original arms.
- `TestBounce1959_NoIdentityFallsBackToFreshSpawn` — pre-migration shape.
- App side: `TestBoardSpawnRequest_BounceRoundsDistinctRunIDs` (RunID uniqueness
  across bounce rounds), `TestBoardCharterPrompt_RidesSharedVerdictChain` (the
  shared renderer).

Mutations applied and RED-checked, then restored (suite green after each restore):
- **A** old live-claim guard re-added → verdict + manual + redispatch pins RED.
- **B** re-dispatch diverted to fresh spawn → RedispatchReachesOriginal + the
  delivery-failure arm RED (the never-turned arm stays green — why the second arm
  exists: it is the discriminator B can't fool).
- **C** mint skips the implementer stamp → RedispatchReachesOriginal RED (this
  mutation initially passed — the fixture's spawner returned IDENTICAL ids per
  non-persona spawn, blinding "mint-stamp" vs "fallback-stamp"; the fixture now
  mints unique per-spawn ids and the stamp assertion lives in the seam-equipped pin).

## 4. Suites

- `go test ./pkg/board/ -tags integration` — green (34s).
- `go test -short ./...` — green.
- `go vet ./...` — clean. `golangci-lint run ./...` — 0 issues.
- `go test -race ./pkg/board/ -tags integration` — green;
  `go test -race -short ./api/...` — green.
- Fixture repair en route: `TestMintUniqueOwnerWideLadder` never deleted its
  fixed-UUID workspace row — second run on the same DB collided on
  `workspaces_pkey` (pre-existing run-twice flake, invisible in CI's fresh
  containers); its cleanup now deletes the row.

## 5. Notes / deliberately untouched

- The drift RC2 closed-unmerged path (reconciler) keeps its own inline re-entry
  (fresh spawn; reads assignee on no-persona rows where it IS the implementer) —
  different consumer, not the verdict path; noted for a possible follow-up to ride
  implementer_session too.
- The reviewer session left idle after its verdict is not aborted (its token died
  with the mint; it lapses naturally).
- Incident note: a concurrent lane (lane-deflake) switched this checkout's branch
  mid-flight and wiped uncommitted pkg/board edits once; work was recommitted
  immediately after recovery (commit dc744076f) and every step since committed early.
