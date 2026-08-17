# Worklog: Onboard PR review + AI commands to reusable ai-workflows

**Date:** 2026-08-17
**Status:** Complete

---

## Objective

Replace this repo's self-contained `pr-review.yml` and `ai-comment.yml` (the
pre-extraction ancestors of `lenaxia/ai-workflows`) with thin callers pinned
to `lenaxia/ai-workflows@v0.2.10`, fixing three measured review-cycle
pathologies: review verdicts posted as plain comments instead of official
reviews, stale reviews landing after newer pushes, and `/merge` accepting
approvals that predate the current head.

---

## Evidence (why now)

Week-by-week analysis of merged PRs (Jun 8 – Aug 17) counting a "round" as an
official bot review OR a review-verdict body dumped as a comment:

- Late June golden period (Jun 22 week): mean 1.12 rounds/PR, 1% of PRs ≥5 rounds.
- Jul 27 week: 2.87 mean, 55% ≥3 rounds. Aug 10 week: 2.36 mean, 13% ≥5 rounds, max 27 (#870).
- Aug 10 week: 170 dumped verdicts vs 54 official reviews; several PRs (#861,
  #858, #897) completed 8–10 review rounds with ZERO official reviews.
- #870: 103 comments, 22 commits, ~24 dumped verdicts, 3 official; contains
  `fatal: could not read Username for 'https://github.com'` mid-run — the exact
  end-of-run failure ai-workflows#17 Bug A tolerates via `continue-on-error`.
- Workflow-run timing (Aug 15): 10–45 min review runs with no concurrency
  group; multiple stale completions posted after a newer head's review.

All fixes already ship in the reusable workflows; this repo was never onboarded
(only `renovate-analysis.yml` was, PR #875).

## Work Completed

### Onboarded pr-review.yml

`.github/workflows/pr-review.yml` → thin caller:
`uses: lenaxia/ai-workflows/.github/workflows/pr-review.yml@v0.2.10`,
`secrets: inherit`, `with: {version: v0.2.10, project_name: LLMSafeSpaces}`.
Kept job id `review` (check name "PR Review / review" unchanged). Dropped the
local renovate[bot] `if:` — the reusable workflow skips automation bots at job
level. Gains: `concurrency: ai-review-{PR}` + `cancel-in-progress` (new push
cancels the outdated head's in-flight review), prompt-injected head SHA with a
mandated `**Commit reviewed:**` first line, "never a plain comment / never
COMMENT-only" verdict instructions, and the `Verify review submitted` gate
(job FAILS unless an APPROVED/CHANGES_REQUESTED bot review exists with
`commit_id ==` triggering head SHA).

### Onboarded ai-comment.yml

Same pattern (`ai-comment.yml@v0.2.10`, job id `respond` kept). The caller
`if:` command/association filter stays local per the ai-workflows caller
contract (lesson #4). Gains: routing from centrally-pinned
`route-command.sh` (v0.2.10 adds PR-head-SHA stamping to /review and
/ai-on-PR prompts), and /review // /ai concurrency sharing the
`ai-review-{N}` group with synchronize reviews (newest full review wins).
Consumer prompts remain authoritative (verified: the reusable workflow cats
`.github/prompts/` from this repo's checkout).

### Removed superseded local routing script + tests

Deleted `.github/scripts/route-command.sh` and `tests/gharouter/route_test.go`.
The local script was an older fork of the central one (missing the v0.2.10
SHA-stamping; `\ `-escaped-space patterns vs POSIX classes) and is no longer
sourced by any workflow after onboarding. The regression guard moves to
ai-workflows itself (`test-router.yml` runs against the central script at its
own pins). Keeping the fork would guarantee drift — the exact failure mode it
existed to prevent.

### Prompt alignment

- `pr-review.md`: verdict options restricted to APPROVE / REQUEST_CHANGES
  (COMMENT removed — the reusable workflow forbids COMMENT-only reviews);
  output format now opens with the `**Commit reviewed:** <sha>` line and
  mandates formal-review submission.
- `merge.md`: `/merge` gate hardened — the latest bot APPROVE's `commit_id`
  (or `**Commit reviewed:**` line) must equal the PR's current `headRefOid`;
  otherwise the approval is stale, merge refuses, and a re-review is
  requested.

---

## Key Decisions

1. **Onboard instead of patching locally** — the reusable workflows already
   implement concurrency, SHA stamping, verify-gate, and Bug A tolerance;
   local patching would re-fork what the org extracted precisely to
   de-duplicate. Verified at the v0.2.10 tag (not just default branch) that
   both workflows + `route-command.sh` SHA injection exist.
2. **Delete, not deprecate, the local router + tests** (Rule 5) — dead shell
   with an attached test suite is worse than none: it implies coverage that
   no longer guards the executing code path.
3. **Fail-the-job on missing verdict, no auto-retry** — the verify gate turns
   silent posting failures into a red X. Retry/salvage (re-posting a dumped
   verdict) deliberately deferred to ai-workflows proper (needs a central
   home so all consumers benefit); noise first is the signal we've been
   missing.
4. **Dropped delta/carry-forward review idea** — #870's rounds 16 and 18
   surfaced genuinely new findings on unchanged-from-prior-round code;
   full-pass reviews have real yield. Cost reduction must come from
   eliminating wasted rounds (posting failures, stale reviews), not from
   reviewing less.
5. Job ids kept (`review`, `respond`) so required-check names are unchanged.

## Assumptions (stated and validated)

1. v0.2.10 contains the concurrency/verify/SHA features → validated by
   fetching the workflows + route-command.sh at `ref=v0.2.10` directly.
2. `secrets: inherit` propagates OPENAI_API_KEY/OPENAI_API_BASE/GITHUB_TOKEN
   and `vars.OPENAI_MODEL` resolves against the caller repo → same mechanism
   as the production-proven renovate-analysis onboarding (#875).
3. The reusable workflow reads consumer prompts from `.github/prompts/` →
   verified in the workflow source (PROMPTS_DIR=.github/prompts, consumer
   checkout first).
4. No build/CI references the deleted gharouter package → repo-wide grep;
   only historical worklogs and design-story docs mention it (append-only
   history, left intact).

## Blockers

None. Follow-ups tracked for ai-workflows (separate repo, separate PRs):
verdict salvage step (re-post dumped verdict as official review), bounded
retry before the verify gate fails.

## Tests Run

- `python3 -c "import yaml; ..."` parse of both rewritten workflows → valid;
  job ids, `uses:` refs, and `with:` inputs asserted.
- Diff of local route-command.sh vs central v0.2.10 → central is a strict
  superset (SHA injection at 2 sites); confirmed central script exists at the
  pinned tag.
- Workflow-run and PR/comment data analysis scripts (gh api + GraphQL) → in
  session transcript; numbers reproduced in Evidence above.
- `go build ./...` unaffected (no Go code changed); `make test` scope shrinks
  by the deleted package (no other package imports it — repo-wide grep).

## Next Steps

1. Open PR with these changes; expect the FIRST run of the onboarded
   reviewer on that PR to demonstrate the new behavior (SHA-stamped verdict,
   verify gate).
2. Watch the first week of red-X "no verdict delivered" failures — each is a
   previously-invisible posting failure now surfaced; if volume is high,
   prioritize the salvage step in ai-workflows.
3. ai-workflows PRs: salvage step + bounded retry (see Blockers).
4. After a stabilization week, re-run the week-by-week rounds analysis and
   compare against the Jun 22 golden-period baseline (target: mean <1.5
   rounds/PR, zero PRs with 0-official-review multi-round cycles).

## Files Modified

- `.github/workflows/pr-review.yml` (rewritten — thin caller)
- `.github/workflows/ai-comment.yml` (rewritten — thin caller)
- `.github/scripts/route-command.sh` (deleted)
- `tests/gharouter/route_test.go` (deleted; dir removed)
- `.github/prompts/pr-review.md` (verdict format: SHA line, no COMMENT)
- `.github/prompts/merge.md` (head-SHA-pinned approval gate)
- `worklogs/NNNN_2026-08-17_onboard-ai-workflows-review-cycle.md` (this entry)
