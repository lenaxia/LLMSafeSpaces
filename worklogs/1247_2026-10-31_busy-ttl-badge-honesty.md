# 1241 — RC-C S2: busy TTL and badge honesty — the spinner tells the truth (tsk-acidic-refectory-reviews)

## The task

The busy badge was a CLAIMED-SPINNER: `frontend/src/components/board/TaskCard.tsx` ~:201
rendered BusyIndicator iff `stage==="in_progress" && assignee_session`, and BusyIndicator
is a bare Loader2 animate-spin — no liveness input of any kind. A lapsed, dead, or wedged
claim spins forever (the 2026-10-30 incident's dishonest signal). Server-side, no busy
TTL: `boardBusyRenewCap=3` bounds busy-ONLY renewals, but nothing stops a stale claim
from rendering busy indefinitely. Depends on S1 (done — 10710ac72: the assignee stamp is
reliable, so badge truth has something to key on).

## What landed

Two belts, split across a seam (the liveness.go house pattern — POOR questions, Nil-inert):

1. **Badge honesty** — `pkg/board/busy.go`: `AssigneeBusySource` (one batched question:
   "which of these assignee refs are mid-turn right now?"), `WithBusySource` option,
   `decorateAssigneeBusy` read-edge pass on GetTask/ListTasks/SearchTasks. TRUE only when
   the derived-busy truth says MID-TURN; idle/unknown/lapsed quiet. Wire field
   `assignee_busy` (omitempty, read-edge derived, never stored).
2. **Busy TTL** — claim-side in `busyReportable`: busy requires a LIVE lease
   (`lease_expires_at > now`). A claim past its lease without renewal quiets whatever the
   (possibly stale-latched) busy source says. Ladder semantics untouched: busy-only
   renewals still extend the lease within the cap, movement uncapped; the TTL is the
   read-side mirror of the ladder's bound.

Production adapter — `api/internal/app/board_busy_source.go`: the boardLivenessHub's PURE
`BusyAssignees` (resolveRefs identity resolve → frame-index guard (stamp>0: this process
must have SEEN a frame — the ObserveAssignees distrust, verbatim) → busyFn consult).
PURITY is the contract: no cursors advance, no movement delta is consumed — a read that
ate the sweep's same-source delta would blind the lease ladder. Degradation is always
QUIET. Wired once in app.go's spawnOpts (`board.WithBusySource(boardLivenessHub)`).

Frontend — `BusyIndicator` takes `busy?: boolean` (default true: the session-activity
surfaces are byte-identical); false renders the DISTINCT quiet marker (static muted dot,
aria "Agent claimed — quiet", data-busy="false"). `TaskCard` renders
`busy={task.assignee_busy === true}` — absent = quiet, never the spin. `board.ts` carries
the field with the honest-absent doc.

## Evidence

- `pkg/board/busy_ttl_integration_test.go` (BoardSuite, real PG): claimed+busy spins /
  claimed+quiet quiets (list AND GetTask); the TTL (past-lease-without-renewal and
  NULL-lease NOT busy under a busy-true-forever source; the gate consults ONLY the
  live-lease claim — the asked-refs tape); the inert seam never busy; the renewal-budget
  chain (busy through the within-cap renewals, the nudge rung after exhaustion, quiet
  after the wedge verdict, quiet in the pre-verdict lapsed window).
- `api/internal/app/board_busy_source_test.go`: composed truth (busy ONLY on
  frame-seen ∧ derived-busy; idle/unframed/deleted quiet; keyed by the ref as passed,
  phrase form included); PURITY (the next ObserveAssignees still reports the frame as
  movement — the badge query consumed no cursor); degraded resolve all-quiet.
- Full `pkg/board` integration suite (38.8s) + full `api/internal/app` suite green on an
  ISOLATED test DB (see blockers); frontend: TaskCard/BusyIndicator pins + full vitest
  (225 files / 2590 tests) + `tsc --noEmit` green; `make fmt-check` green.

## Blockers, digressions, process notes

- **Shared-checkout race with the S4 lane**: the S4 worker's uncommitted eventbus work
  sat in the tree when I started; a mid-flight revert of theirs cost me one service.go
  edit (re-applied — always re-read shared files after any gap). Later their `git add -A`
  swept my in-flight files into their 2897d9a3f and unmerged them forward at e4df25d82;
  verified byte-integrity of every file myself (line counts, diff-vs-HEAD = exactly my
  insertions, HEAD carries none of my symbols) before proceeding. Lesson recorded in
  COORDINATE.md terms: stage BY PATH on a shared tree, never `add -A`.
- **Shared test-DB race**: my first integration runs failed flakily (rows vanishing
  mid-test) — the sibling lane's suite runs were truncating the shared
  `llmsafespaces_test` on localhost:5433 concurrently. Fixed by creating a private
  `llmsafespaces_test_s2` (template copy at a quiet moment) and running my suites with
  `TEST_DATABASE_URL` pointed at it. The flake reproduced 3×, then never again.
- **A wrong test of mine, twice**: the chain pin initially expected the wedge verdict one
  sweep early (the sentinel must be POSITIONED first — pin 12's exact flow), and the hub
  pin asserted the deleted row reaches the busyFn (it dies at the deleted guard). Both
  were test bugs; the code was right. Mutation-checked in the sense that each failure
  named its line precisely.

## Verification commands

    go build ./... && go vet ./pkg/board/ ./api/internal/app/
    make fmt-check
    TEST_DATABASE_URL='postgres://postgres:testpass@localhost:5433/llmsafespaces_test_s2?sslmode=disable' \
      go test -tags=integration -count=1 ./pkg/board/
    TEST_DATABASE_URL='postgres://postgres:testpass@localhost:5433/llmsafespaces_test_s2?sslmode=disable' \
      go test -count=1 ./api/internal/app/
    cd frontend && npx vitest run && npx tsc --noEmit
