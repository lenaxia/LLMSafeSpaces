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

## §4 Verification (live Postgres 16, not skip-masked — final tree, post review r1)

- Round 0's findings, all fixed in r1: (1) the 2065-C seed ran a bare
  `pending_input=true` UPDATE violating the 000052 paired-form check —
  CI red on the pushed head; fixed to `pending_origin='worker'` (the
  production writer's shape, reconciler.go:1354). (2) The consumer's
  `dropped` loud-once latch was a plain bool raced between the fold
  and drain goroutines — now `atomic.Bool` (CAS latch / Store reset) +
  the OverflowWhileDraining pin. (3) Rule 5: the two known-red
  live-DB tests in the WIRED package repaired (see §7). (4) The
  evidence record now states the FINAL tree's runs, below.
- `go test -tags=integration -race -count=1 ./pkg/board/` — ok, 64s,
  on the final tree: all 7 new pins (2065-A..G) green AND the full
  pre-existing lease/ladder/persona/kill suites — the R5
  byte-identical evidence.
- `go test -race -count=1 ./api/internal/app/` — ok, 50s, FULLY green
  on the final tree: the 6 consumer pins + the entire app suite
  INCLUDING the two repaired #2047 tests (previously red on any live
  DB, skip-masked in CI).
- `go build ./pkg/board/ ./api/internal/app/`, `go vet` (both tags),
  `make fmt-check`, `make imports-check` — clean on the final tree.
- Born-red honesty note: the pins were developed red-first; the
  guarded write's guards surfaced real test-shape defects before any
  implementation existed, and round 0 caught that one fix (the
  pending_origin guard) had not persisted into the pushed commit —
  the record and the tree now agree.

## §5 Pins (the acceptance map)

| Pin | Asserts |
|---|---|
| 2065-A | the incident shape, delta-pinned: live-lease row survives a sweep untouched (today's behavior) → ONE event dues it (`trigger=event` +1) → ONE sweep arms the nudge (`sweep_lapse` +1) — no wall-clock anywhere |
| 2065-B | advance=yes: lease byte-identical, due-mark counter unmoved, `turn_advances` +1 |
| 2065-C | pending_input (origin-paired, the production shape) / paused rows never due-marked |
| 2065-D | armed nudge slot + already-due: the sweep owns the episode; duplicates never double-count |
| 2065-E | user-session / foreign-workspace / unattributable events: silent no-ops |
| 2065-F | row-side epoch guard: an event observed before the claim mint drops |
| 2065-G | R5: silent stream — natural lapse processes exactly as today; only `sweep_lapse` moves |
| app×6 | filter/identity/observation-stamp; watermark (dup/stale/foreign-session/advance); overflow drop never blocks; **overflow WHILE draining (the r1 race pin: CAS latch vs Store reset, conservation-asserted, `-race`)**; sink panic contained; composite zero-liveness |

## §6 Process

Worktree off origin/main (post-#2075/#2081 head, 27c3b61aa); origin/main
merged before push; no sleep-polling (the consumer pins signal via
channel handoffs; the panic pin polls a bounded counter window).
Adversarial review to APPROVED — never self-merge (the orchestrator
merges). Closes #2065 and #2066 (folded) when merged.

## §7 The rule-5 repairs (the wired package's known-reds, fixed)

`TestIntegration_QuietLookup_InProgressAssignees` and
`TestIntegrationQuietSource_ComposedTruthAndPurity` (#2047's PG pins)
were red against any live DB, skip-masked in CI — three layered
defects the mask hid:

1. The lookup test's seeds violated the 000043 phrase-id checks
   (`prj-ql-a` etc. — two segments, never a valid form) and the
   000052 paired pending form (`pending_input=true` without
   `pending_origin`). Repaired to valid 3-word phrases + the
   origin-paired shape.
2. The repaired seed then unmasked a self-contradiction: the
   explicit-empty contract queried workspace B, whose own seed holds
   the scoping-probe row. Now queried on a genuinely empty third
   workspace.
3. `TestIntegrationQuietSource` exposed a REAL production defect:
   `AssigneeQuiet` omitted dead/absent rows from the answer map
   entirely (`continue` before `out[row.ref]`), contradicting its own
   comment ("unknown here, or provably gone: nil") and #2047's
   membership contract — callers cannot distinguish "resolved to
   silence" from "never asked". Fixed: present-with-nils
   (`board_quiet_source.go`, one line + comment).
