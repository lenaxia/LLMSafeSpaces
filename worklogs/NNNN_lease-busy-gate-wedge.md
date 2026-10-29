# Busy-gate: the wedge verdict never kills a mid-turn worker

2026-10-29 · branch `lease-wedge-busy-gate` · owner ruling: "implement 2"
(post-mortem of the 3-attempt failure of tsk-teal-vocalist-orders / PR #1982)

## §1 The incident

Three healthy workers (`remote-vignette-indicates`, `linear-outpost-paints`,
`fuzzy-melon-rushes`) burned a task's attempt cap with the work intact.
Every transcript ends the same way: a running bash call
(`sleep 240/420/480/600 && gh pr checks 1982`), status running, forever.

Mechanism: a long silent tool call inside a BUSY turn is observationally
identical to a wedged session on the frame stream. The tool-call START
stamps the hub's frame index once; minutes of sleep produce nothing. The
nudge delivers into the session's queue — busy sessions take messages at
the turn boundary, which never comes — so the sentinel's transcript
position never advances and the wedge verdict (nudged ∧ no post-sentinel
events ∧ no post-nudge heartbeat ⇒ wedge) fired on workers that were
alive and working the whole time.

CI in this repo takes 15-40 min (the AI reviewer runs its own empirical
checks), so poll-sleep loops were long and doomed.

## §2 The fix (option 2 of the post-mortem menu)

One fact the harness already held: the turn is BUSY. Layering the
verdict now respects:

- the harness's tool TIMEBOX bounds a busy turn (workers set 300-660s —
  a hung command always ends, the turn ends, the queued nudge delivers);
- the wedge ladder owns IDLE silence (a real wedge);
- nudge → wake → response renews an idle-but-healthy worker.

Killing a busy turn inverted that layering — the wedge detector
duplicated the bash timebox's job and fired first.

### The seam (pkg/board/liveness.go)

`AssigneeObservation.Busy` — the fourth POOR question. Busy ⇒ ALIVE,
OR'd into the renewal arm ahead of every quiet/wedge arm. Zero value is
inert: the Nil contract and every existing pin pass unchanged (the full
integration suite ran green before the ladder change was even exercised
— the field simply sat false).

### The ladder (pkg/board/reconciler.go)

`case o.MovedSinceLastSweep, o.EventsAfterNudge, o.Busy:` → renew.
Renewal closes the nudge episode (existing semantics, nudge_id=NULL), so
post-turn quiet re-climbs from the NUDGE rung — a FRESH sentinel, never
a wedge measured against a dead episode's ordinal question. The verdict
is deferred for exactly as long as the turn runs, never canceled.

### The adapter (api/internal/app/board_liveness.go)

`boardBusySource` = `Consumer.DerivedBusy(workspace, harnessID)` — the
usagestream gate's consult-maintained `g.derived` map (#1602 flip
baseline; #1574 BusyComponents via consultBusy; re-seeded from the
authority snapshot on reconnect by onState; dies with its gate). One map
read per sweep candidate; no new RPCs, no new state, no new TTLs.

Trust anchor: honored only when `stamp > 0` — this process saw a frame
for the session. The event that flips a session busy (a PART_START
consult) also stamps the hub's index, so busy-with-no-frame-ever is an
answer about a session this replica never observed: distrust, in the
safe direction (today's semantics — never a wrong verdict). Residual
staleness (stream dropped mid-turn, gate holding a stale-true) is
bounded by the gate's own lifecycle: reconnect re-seeds, idle-drop
tears down.

resolveRefs now SELECTs workspace_id (the gate lookup is per-workspace);
scan order changed accordingly — the only schema-touching delta and it
is read-only.

## §3 Review-loop findings (pre-empted)

- **Renew clears the nudge slot** (reconciler.go:450): my first test
  draft assumed the episode stayed open through busy windows. Wrong —
  and semantically RIGHT that it closes: post-turn silence must never be
  measured against a sentinel the busy epoch already answered. Both pins
  rewritten to the true semantics (subtest 2 pins the re-climb).
- **The wiring test must compose the PRODUCTION nudger**: a bare
  counting closure never calls hub.markNudge, so NudgeMarkKnown stays
  false and the engine re-nudges forever instead of wedging. The pin now
  wraps `newBoardNudger(boardScopeFor(pool), …, hub)` — which is also a
  more honest pin of the production composition.
- **The distrust arm needs seeded sessions**: unindexed refs answer
  no-evidence before the busy leg; the BusyGateLeg pin seeds all four.

## §4 Tests

- `TestPin_Lease12_BusyMidTurnNeverWedges` (pkg/board, PG): both orders.
- `TestIntegrationLeaseLiveness_BusyGateLeg` (app, PG): four arms + inert.
- `TestIntegrationLeaseLiveness_ProductionWiring_BusyWorkerNeverWedges`
  (app, PG): the live-fire end-to-end — three frame-silent mid-turn
  windows renewed with the nudger NEVER called, then post-turn quiet
  opens the episode, then the verdict.
- `TestDerivedBusyReadsGateTruth` (usagestream, unit).
- Full suites: pkg/board (integration) 39.6s green; app; usagestream;
  `go build ./...`; golangci-lint 0.

## §6 Review round 1 (CHANGES_REQUESTED on `227bb647`)

The reviewer mutation-verified the core (reverting `o.Busy` from the
renewal arm reddens both PIN 12 subtests AND the production-wiring pin
at exactly round 2) and raised one blocking robustness finding with two
non-blocking minors:

- **BLOCKING — the busy-deferral was unbounded**: pod dies mid-turn +
  stalled recreation + Active workspace ⇒ `g.derived` holds stale-true
  forever (no reseed — the gate never reconnects; no teardown — the
  idle watchdog can never fire while `busyNow` is true) ⇒ renewal every
  sweep with no cap ⇒ the row holds its lease and slot indefinitely and
  the ladder loses failure detection for it. The tool-timebox bound my
  comments leaned on is a harness convention, not an engine guarantee.
  **Fix**: the engine-side cap — `tasks.busy_renews` (migration 000053)
  counts consecutive busy-ONLY renewals per claim epoch; at
  `boardBusyRenewCap`=3 the ladder stops honoring Busy for the epoch
  and falls through to the ordinary quiet arms. Reset by movement
  renewal, heartbeat response, every claim mint, and every stage
  transition; deliberately NOT reset by the nudge write (a stuck source
  must not cycle cap→nudge→reset forever). Two new PIN 12 subtests:
  bounded (cap → nudge → verdict on the same stuck-busy silence) and
  consecutive-not-cumulative (movement restarts the count).
- **Minor — doc width**: "Busy ⇒ ALIVE at every rung" was wider than
  the implementation (the no-evidence rung never consults Busy; Busy
  shadows `!Exists` for self-contradictory observations). Softened to
  "every OBSERVED rung" with both boundaries stated in the field doc.

## §5 What this does NOT do (the roadmap)

- The event-driven end-state (option 3): review verdicts publish on the
  0072 bus and wake the idle worker — nobody polls anything. Needs the
  agentd deploy + an audit/SSE consumer group; this fix removes the
  death, not the polling.
- Charter prose (option 1) stays as belt-and-braces: workers are told to
  never sleep-poll, push, end the turn with a report; the orchestrator
  wakes them on verdicts.
