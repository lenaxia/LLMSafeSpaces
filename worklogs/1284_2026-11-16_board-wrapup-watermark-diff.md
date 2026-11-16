# Worklog: wrap-up watermark + delta computation + aggregate-diff window (epic wrap-up review)

**Date:** 2026-11-16
**Session:** wrapup-watermark-diff implementation (tsk-sacred-papaya-drills)
**Status:** Complete

---

## Objective

Land the watermark's DATA plane (low-level design §§4-5; high-level D4/D5): the `advanceWatermarkTx` settle primitive (advance-to-MINT-time, guarded no-op on replay/race, one-transaction-atomic with its caller's commit), the §5 delta story list over `board_transitions` (first run covers ALL done stories ever via `-infinity`; re-done rows at their LATEST done; evidence fallback chain transition → verdict → "(none recorded)"), the `RenderWrapUpCharter` contract pins (byte-exact §5 block, deterministic, frozen at mint), and the no-git-execution pin for pkg/board. The verdict verb that CALLS the advance is wrapup-gap-recursion's; the mint trigger is wrapup-engine's (this branch builds on the engine branch, PR #2076, approved-but-unmerged at cut).

---

## Work Completed

### `advanceWatermarkTx` — `pkg/board/wrapup.go`

The §4 settle SQL verbatim: `last_wrapped_at = (SELECT created_at FROM tasks WHERE id=$wrapupTaskID)`, `wrapup_count+1`, `wrapup_task_id=NULL`, guarded `WHERE id=$epic AND wrapup_task_id=$wrapupTaskID`. Runs inside the caller's transaction (`pgx.Tx`); returns whether the guard matched. No production caller yet by design — RecordWrapUpVerdict (sibling story) is its only one; the direct pins land here so the sibling lands against a pinned seam.

### Delta story list — `wrapUpWindowStories` rework

Signature `(ctx, q, epicID, watermark *time.Time)` (was `windowFrom time.Time`): the predicate is now `tr.at > COALESCE($2::timestamptz, '-infinity'::timestamptz)` — first run (NULL watermark) covers ALL done stories ever, including a legacy row whose latest done transition predates the epic's own created_at. LATERAL latest-→done per row gains the `id DESC` tiebreak (re-done rows count at their LATEST done; deterministic pick). Evidence column: `COALESCE(NULLIF(tr.evidence_url,''), <latest review_verdict event's payload->>'evidenceUrl' ordered created_at DESC, id DESC>, '')` — the §5 fallback chain. The RENDERED window's from-bound stays `coalesce(last_wrapped_at, epic.created_at)` (D5 — the diff range the reviewer reads); the coverage predicate is the watermark's alone, a strictly weaker bound (no skip, no double-cover). Mint call-site updated to pass `e.LastWrappedAt`.

### Verdict evidence persisted — `pkg/board/verdict.go`

`Verdict.EvidenceURL` (`evidenceUrl,omitempty`) + `buildOutcome` carries it in the `review_verdict` event payload. Additive; the fallback chain's second link reads exactly this.

### Renderer contract — `WrapUpCharterInput` + byte fix

Dropped the unused `Project` field (the renderer is pure over epic/window/stories/capState per the charter's signature — one call-site change in the mint). Fixed the one byte deviation from §5's block: no blank line between the story list and the `Aggregate diff:` instruction (the engine's raw-string render emitted one).

### The pins (born-red first)

- `pkg/board/wrapup_settle_internal_test.go` (NEW, PG, in-package — the primitive is unexported): settle advances to the backdated MINT stamp (not close time), count+1, pointer cleared; replayed close = clean no-op (no double increment/advance); raced close on a foreign pointer refuses cleanly (exactly one settle wins); both-or-neither (ROLLBACK unwinds row-done AND advance; COMMIT lands both — the primitive-level atomicity pin; the settle-SQL failpoint inside the verdict transaction lands with wrapup-gap-recursion).
- `pkg/board/wrapup_watermark_integration_test.go` (NEW, PG, suite): first run covers ALL done stories (the backdated legacy row included — RED before: the engine's minimal query bounded at epic.created_at) + the rendered RFC 3339 window pair; evidence fallback chain end-to-end (transition URL / verdict URL / "(none recorded)"); mid-review completion falls in the NEXT window end-to-end (W1 covers 2, settle at W1's MINT time, W2 is "(delta 2)" scoped to the 1 mid-review re-done, window-from == W1.created_at, W1's charter byte-frozen after all later activity, epic watermark == W1 mint).
- `pkg/board/wrapup_charter_unit_test.go` (NEW, no tag): the §5 charter block BYTE-EXACT (golden), determinism (same input → identical bytes; every mutated field → distinct bytes), empty-window "(none)", and the engine-seeded criteria are exactly §5's three.
- `pkg/board/wrapup_nogit_unit_test.go` (NEW, no tag): D5 pin — no non-test source under pkg/board imports os/exec or invokes exec.Command (green before AND after, the regression-pin posture; the charter's `git log …` line is instruction STRING, not engine code).

### Verification

`make fmt-check` clean; full `pkg/board` suite green in both lanes (`-tags=integration -race` 61s locally against the migrated PG; `-short` clean); `go build ./...` + `go vet ./...` (both tags) clean; handler (`Verdict`) and `pkg/mcp` suites green (the Verdict struct change is additive).

---

## Key Decisions

- **Coverage predicate vs rendered window are different bounds, deliberately** (§5): the SELECT uses `-infinity` on first run while the charter renders `epic.created_at`. A story done before the epic's created_at is LISTED (covered) even though it falls outside the rendered diff range — "first run covers all" wins over a cosmetic window match; the story list is "the union to read".
- **`advanceWatermarkTx` stays unexported** with in-package PG pins (the `wrapup_reviewer_internal_test.go` precedent): its only production caller is in-package (RecordWrapUpVerdict, sibling story); exporting for testability would widen the seam the design keeps closed.
- **The verdict-fallback data source is the `review_verdict` event payload** (additive `evidenceUrl` key) — the verdict chain was the only place the reviewer's evidenceUrl already lived; a new column would duplicate state the events table already owns.
- **Frozen charter beats live mutation in tests**: the fallback fixtures must be in place BEFORE the drain-triggering done (the charter renders in the mint's own transaction) — the evidence tests seed a fourth story as the trigger so the chain fixtures land first.
- **Built on `origin/feat/board-wrapup-engine`** (PR #2076, APPROVED + green, unmerged at cut): my story's charter makes the engine's minimal render swap to this renderer — the base is the only branch where the swap site exists. Merges cleanly once #2076 lands.

---

## Blockers

None. Depends on PR #2076 (wrapup-engine) merging first — this PR is based on it and will rebase trivially (it touches the same file additively).
