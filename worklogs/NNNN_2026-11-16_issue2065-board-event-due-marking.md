# Board event-driven due-marking — turn-end(no-advance) enters the quiet arms within one sweep (issue #2065, epic #2061; #2066 folded)

2026-11-16 · branch `feat/2060-event-due-marking` · task tsk-rested-dune-reviews

## §1 The story and its rulings

Epic #2061 story 2 (with story 3 — #2066 — folded in by audit ruling):
the board consumes the turn-end(no-advance) event story 1 (#2064, PR
#2075) emits at the attested settle fold, and marks the claim's lease
due so the EXISTING quiet arms (nudge → ordinal → wedge) engage within
one sweep — the measured incident (stop 19:51:13 → first nudge
21:38:59, 1h47m; worker answered in 20s) closes to minutes. The
2026-11-15 audit rulings govern where they narrow:

- **R2 as written**: advance=no on an attested turn-end ⇒ the row's
  lease is marked due; advance=yes ⇒ telemetry only
  (`board_turn_advances_total`). Entry acceleration, not new machinery:
  the sweep backstop (R5) and every ladder arm untouched.
- **ONE KNOB**: no second 90m tuning surface. The stale threshold is
  #2043's landed `HeartbeatLapseThreshold` (PR #2067) — the event path
  defers every decision to the ladder, whose lapse arm already rides
  the honest progress clock. `due_mark.go` adds no config field; the
  doc records the consumption (claim-lifecycle-design.md Part 6).
- **Guards (R4)**: no due-mark while pending_input or paused; a row
  with an armed nudge slot is the sweep's episode (never re-marked);
  already-due rows are idempotent no-ops (also the cross-replica
  collapse); epoch mismatch dropped on BOTH sides — consumer watermark
  (strictly-newer per session; duplicates and stale ordinals) and the
  row's claim mint (`stage_entered_at <= observed_at`).
- **R3**: the consumption path never stamps the liveness hub's worker
  index — pinned end-to-end through the production composite observer
  (`TestBoardTurnEndConsumption_NeverStampsWorkerIndex`).
- **R6**: `board_due_marks_total{trigger=event|sweep_lapse}` — the
  event path's share of ladder entries per scrape
  (`rate(event)/rate(sweep_lapse)`); sweep_lapse counts the victim
  scan's discoveries (event-marked rows converge into it one cycle
  later, by design, documented in the metric help).
- **Replica arbitration (open-question 1, resolved)**: the reconciler's
  own single-row guarded-UPDATE pattern (the freeze arm,
  suspend.go:118) — predicates re-checked at write time,
  `RowsAffected==0` = converged by them. No SKIP LOCKED (that is the
  sweep's multi-row arbitration), no leader election.

## §2 What landed

- `pkg/board/due_mark.go`: `Service.ConsumeTurnEnd` — the single new
  write. advance=yes counts telemetry and leaves; advance=no runs the
  guarded UPDATE (`lease_expires_at=now()`, workspace join, both
  identity keys per #1817, every R4 guard inline) and increments
  `trigger=event` only on rows affected.
- `pkg/board/ladder_metrics.go`: the due-marks CounterVec +
  `board_turn_advances_total` + accessors (`DueMarks()`,
  `TurnAdvancesCounter()`).
- `pkg/board/reconciler.go`: the scan-time `trigger=sweep_lapse`
  increment — the ONLY sweep-side change (a counter; behavior
  byte-identical, R5).
- `api/internal/app/board_due_mark.go`: the fold observer — filter,
  identity extraction, bounded FIFO epoch watermark, non-blocking
  buffered enqueue (overflow drops counted on `board_due_marks_dropped`
  expvar — the R5-safe direction), single-drain `Run` with panic
  containment; `chainFrameObservers` composite helper.
- `api/internal/app/app.go`: wired after `boardSvc` exists — the
  composite re-install over the hub-only observer (hub first,
  byte-identical for every frame the consumer ignores;
  SetFrameObserver is late-bound by design and pods cannot stream
  before serving starts).
- #2066 folded: `reports/specs/claim-lifecycle-design.md` (the #2060
  port — states as LEXICON per the ruling, verbatim-plus-corrections,
  the deploy-lag decision record, the phase-graph drift note, closed
  open-questions) + the cross-link from `seamless-lease-design.md`.

## §3 The pi journal, scoped honestly

Story 1's pi leg journals the event payload to
`/workspace/.opencode-board-turnends` as the emission artifact; this
PR's consumer rides the ABI fold (the opencode stream). Wiring the
journal's tail into the attested stream is a SECOND emission site under
the anti-#2040 emission-site rule — story-1 follow-up scope, not
silently smuggled in here. Until it lands, pi pods ride the sweep
backstop verbatim (epic R5 posture; recorded in the design doc Part 7).

## §4 Verification (live Postgres 16, not skip-masked)

- `go test -tags=integration -race -count=1 ./pkg/board/` — ok, 95s:
  all 7 new pins (2065-A..G) green AND the full pre-existing
  lease/ladder/persona/kill suites — the R5 byte-identical evidence.
- `go test -race -count=1 ./api/internal/app/` — ok, 148s: the 5
  consumer pins + the full app suite.
- Born-red honesty note: the pins were developed red-first against the
  unimplemented consumer (the guarded write's guards surfaced two real
  test-shape defects before any implementation existed: the
  pending_input origin constraint and the suite's 200ms lease
  requiring the armLiveLease helper).
- `go build ./pkg/board/ ./api/internal/app/`, `go vet` (both tags),
  `make fmt-check`, `make imports-check` — clean.

## §5 Pins (the acceptance map)

| Pin | Asserts |
|---|---|
| 2065-A | the incident shape, delta-pinned: live-lease row survives a sweep untouched (today's behavior) → ONE event dues it (`trigger=event` +1) → ONE sweep arms the nudge (`sweep_lapse` +1) — no wall-clock anywhere |
| 2065-B | advance=yes: lease byte-identical, due-mark counter unmoved, `turn_advances` +1 |
| 2065-C | pending_input / paused rows never due-marked |
| 2065-D | armed nudge slot + already-due: the sweep owns the episode; duplicates never double-count |
| 2065-E | user-session / foreign-workspace / unattributable events: silent no-ops |
| 2065-F | row-side epoch guard: an event observed before the claim mint drops |
| 2065-G | R5: silent stream — natural lapse processes exactly as today; only `sweep_lapse` moves |
| app×5 | filter/identity/observation-stamp; watermark (dup/stale/foreign-session/advance); overflow drop never blocks; sink panic contained; composite zero-liveness |

## §6 Process

Worktree off origin/main (post-#2075/#2081 head, 27c3b61aa); origin/main
merged before push; no sleep-polling (the consumer pins signal via
channel handoffs; the panic pin polls a bounded counter window).
Adversarial review to APPROVED — never self-merge (the orchestrator
merges). Closes #2065 and #2066 (folded) when merged.
