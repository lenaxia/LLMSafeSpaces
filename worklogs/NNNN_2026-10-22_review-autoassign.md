# Worklog: Config-gated review auto-assign + verdict chain (#1832 item 5, part 2/2)

Stacked on PR-B1 (feat/1832-b1-verdicts); consumes A1's merged config.

## Scope
- pkg/board/review_assign.go: the WithReviewAssigner seam;
  effectiveConfigForProject (A1 merge semantics: user defaults +
  project overrides); maybeAutoAssignReview (post-commit on
  ->in_review; fires ONLY under reviewColumn=auto AND
  autoAssignReviewer=true; records a review_assign event; dispatches
  DIRECTLY — a reviewer is a manager-surface actor, no claim token, no
  queue, no lease: the single-reviewer latency multiplier is the
  design driver). manual/off/auto-without-assign keep today's behavior
  exactly (no event, no dispatch) — pinned by the 5-combo gating
  matrix.
- Claim.VerdictChain + population at the spawn path: the next
  claimant's charter carries every prior verdict + findings
  (iteration history, never a failure count).
- api/internal/app: reviewerCharterPrompt (adversarial review vs
  criteria + the board_record_verdict contract; never merge/claim),
  newReviewAssigner (the same activator/executor seam as the worker
  spawn, "board-reviewer" node, 20m), boardCharterPrompt renders the
  chain; wired in app.go beside the spawner.

## Tests
- Harness (PG, CI): TestReview_1832_AutoAssignGatingMatrix (5 combos:
  exactly auto+true dispatches once + one review_assign event; all
  others zero/zero), TestReview_1832_VerdictChainRidesNextClaim
  (fresh claim empty chain -> fail verdict -> lapse -> re-claim's
  charter carries the fail verdict + findings).
- Unit: boardCharterPrompt chain rendering (+ empty-chain absence),
  reviewerCharterPrompt contract.

## Gates
go build ./... clean; go vet (incl. -tags integration) clean; app +
board + mcp suites ok; repolint all-pass. No new REST/MCP surface
(engine-internal + spawn-side only) — no parity additions needed.
