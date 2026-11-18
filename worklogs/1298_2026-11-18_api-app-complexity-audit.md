# Worklog — api/internal/app + handlers board/observability complexity audit (tsk-tidal-wolfhound-restricts)

Read-only audit session (2026-11-18, ~2h): inventory + cost assessment of the board/observability
surfaces of `api/internal/app` (13 `board_*.go` adapter files + the composition root) and the five
alert/outbox/terminus/SSE files of `api/internal/handlers`. Single artifact:
`reports/audits/api-app-complexity.md` (no code changes).

## Assumptions

- "Complexity cost" is grounded per finding in origin (issue/design/incident cites carried in the
  code's own provenance comments), LOC where measurable, and drift risk against the claim-lifecycle
  design's principles 1/5 ("one progress contract… never re-derived"; "poor facts, rich semantics")
  and design 0055's "API proxies, never re-derives".
- Classification vocabulary per the task charter: proportionate (cost earned) / collapsible
  (functional-equivalence-arguable consolidation) / dead (dead-in-fleet or dead-in-principle).
- pkg/board internals are OUT of scope — only its seam with api/internal was audited (the engine's
  interfaces are quoted, its internals are not assessed).
- Sibling audit PR #2094 owns the CI/release + AI-review-lane facts; not re-derived here.

## Data sources (first-hand)

- Full-file reads at base `9f74d3020`: `board_liveness.go` (615), `board_busy_source.go` (104),
  `board_quiet_source.go` (130), `board_quiet_lookup.go` (107), `board_suspend_source.go` (67),
  `board_worker_comms.go` (417), `outbox_terminus.go` (349), `proxy_lifecycle.go` (739),
  `proxy_worker_silent.go` (355), `handlers/board.go` (917), `board_sse.go` (302); headers of
  `board_due_mark.go`/`board_lease_events.go`/`board_turn_evidence.go`/`board_bus.go`;
  `app.go` board block 1600–1922 + wiring-call census; `usagestream/consumer.go` DerivedBusy;
  `proxy_busy_snapshot.go`.
- Measured counts (repo, non-test): app package 7,408 LOC; board adapters 3,307; handlers 34,231.
  `New()` spans 185–2078 (~1,894 lines); 128 `Set*(` sites / 81 distinct setter names.
  Duplication census: max-of-two-keys stamp idiom 5×; derived-busy distrust guard 3×.
- Repo-wide `\.QuietFor(` grep: test hits only (dead-in-production, F9).
- `git diff bd56d8464..9f74d3020 -- api/internal pkg/board` = empty (audited paths identical
  pre/post 0.44.17 release).
- Design grounding: `reports/specs/claim-lifecycle-design.md` (principles, the lexicon ruling),
  design 0055 (authority), 0067 (thin-adapter §4.2/§4.3a), 0072 (S4/S6 bus posture).

## Decisions (verdict summary)

- PROPORTIONATE (keep): the hub itself as the poor-facts seam (F12); two-class frames + #2040
  allowlist; worker-comms closures (shared core already single-owned, F8); hand-rolled
  Connect-JSON ABI transport as a documented trade (F11); snapshot DerivedBusy as an authority
  projection (F10, note added); the two alert arms' evidence vocabularies (F3 keeps the gates,
  unifies only the machinery); dual-key stamping tax under heterogeneous refs (F6, bounded).
- COLLAPSIBLE: hub read-edge consolidation (F1+F7, top-5 #1); engine-owned quiet facts behind the
  unchanged narrow interface (F2, top-5 #2); board-block extraction from `New()` — one-function
  seams KEPT, location moved (F5, top-5 #3); alert cooldown unification (F3, top-5 #4).
- DEAD: outbox V2 promotion-wait leg — dead on the pinned harness (#755), alive-by-design (0055
  M4); verdict = keep + boot-warn (F4, top-5 #5). `QuietFor` — zero production callers (F9).

## Review trail

- AI review round 0 (head `d6be4fa0d`): **APPROVED**, ledger
  `api-app-complexity-worklog-absent` (style, advisory — this worklog closes it) plus one
  non-ledgered prose note (the "107-line interface" miscount — boardManager is 71 lines,
  `handlers/board.go:37-107`; corrected in the same follow-up commit).
