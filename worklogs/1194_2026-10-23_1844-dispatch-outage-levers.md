# Worklog: #1844 (PR-1) — workflow_dispatch outage levers for the PR-triggered workflows

**Date:** 2026-10-23
**Session:** Lane #1844 (run watchdog), PR 1 of 2. PR-2 (the automated zero-run watchdog sweep) follows separately.
**Status:** Complete

---

## Objective

Every PR-triggered workflow that a suppressed branch needs for merge eligibility must be forceable via `workflow_dispatch` — the recovery lever verified live during the 2026-10-23 branch-scoped suppression (`gh workflow run ci.yml --ref <branch>` queued normally at 02:23Z while push events to the same branch produced zero runs of anything). At lane start, `pr-review.yml` and `secrets-integration.yml` had no dispatch trigger; `posture-gate.yml` and `ci.yml` already did.

## Root constraint discovered (the empty-PR-context trap)

The obvious change — add bare `workflow_dispatch:` to `pr-review.yml` — is WRONG and would ship a broken lever. The reusable `lenaxia/ai-workflows/.github/workflows/pr-review.yml@v0.2.23` keys every step on `github.event.pull_request.*`:

- job `if:` reads `github.event.pull_request.user.login` (empty on dispatch → `!contains(list, '')` → job RUNS),
- concurrency group `ai-review-${{ github.event.pull_request.number }}`,
- `PR_NUMBER`/`PR_AUTHOR`/`PR_HEAD_SHA` env in authmode, pre-flight, prompt-build, check-verdict, salvage ×2, and the verify gate (all `github.event.pull_request.*`).

A dispatched run would check out the right tree (default checkout of the ref) but build a prompt with an EMPTY head SHA, run a full LLM round, and redden in check-verdict against an empty SHA — a burned review round masquerading as a lever. Verified by reading the reusable workflow at the pinned tag (`gh api repos/lenaxia/ai-workflows/contents/.github/workflows/pr-review.yml?ref=v0.2.23`), not by firing a broken dispatch at the repo.

## Changes

- **`.github/workflows/pr-review.yml`**
  - `workflow_dispatch` with optional `pr` input (number; default auto-detects the unique open PR whose head branch is the dispatched ref, errors loudly on zero/many).
  - `review` (reusable) job pinned `if: github.event_name == 'pull_request'` — a dispatch can never reach it.
  - New `dispatch-recovery` job (dispatch-only, own `permissions: contents: read, pull-requests: write, actions: write`):
    1. resolve PR → number/head SHA/branch;
    2. verdict check (the check-verdict.sh sliver: official `github-actions[bot]` review pinned to the head SHA, or a sha-pinned `ai-workflows-verdict:` marker comment) — hit → noop notice;
    3. post the #1844 fault comment, deduped on the marker `<!-- run-watchdog: fault sha=<full SHA> -->` (same literal PR-2's watchdog posts and its Go tests pin — one comment per head SHA across both the manual shim and the automated sweep);
    4. `gh workflow run ci.yml --ref <branch>` — the verified lever. GITHUB_TOKEN-created `workflow_dispatch` events DO create runs (the documented recursion exemption — the only exemptions are workflow_dispatch and repository_dispatch).
  - Honest limitation documented in the YAML header: an AI review round cannot be forced from inside this repo at v0.2.23; review levers remain an admitted `/review` comment or an empty `ci: retrigger` push when events flow. An ai-workflows dispatch bridge is the owner-side follow-up.
- **`.github/workflows/secrets-integration.yml`** — bare `workflow_dispatch:`. Safe: the suite job's gate is `github.event_name != 'pull_request' || secrets_changed != 'false'`, so dispatch always runs the full suite (what an operator forcing recovery wants); the `changes` job's `git diff origin/main...HEAD` works on a branch dispatch ref.

## Audit (all PR-triggered workflows, dispatch inventory)

| workflow | PR-triggered | dispatch before | action |
|---|---|---|---|
| ci.yml | yes (no paths filter) | yes | none — the verified lever |
| pr-review.yml | yes | **no** | added (shim job) |
| secrets-integration.yml | yes (paths-gated) | **no** | added (bare) |
| posture-gate.yml | yes (paths-gated) | yes | none |
| e2e-pr.yml | yes | no | out of this lane's named scope; noted to orchestrator |
| ai-comment.yml | comment-triggered (not push) | n/a | n/a — not part of the synchronize-drop class |
| numbering-gate.yml | main-push + cron | yes | none (out of bounds — #1867 machinery) |

## Validation

- YAML parses (PyYAML safe_load): both files; jobs/`on` shapes as intended.
- Every `run:` block extracted and `bash -n` clean (pkg/repolint workflow_shell_syntax_test.go harness, #878/#881 — pre-dating #1867, which reused it).
- `go test ./tests/ghaworkflows/` green — `TestPrReviewCallerContract` pins intact (`review:` job id, `types: [opened, synchronize]`, permissions, `secrets: inherit`, `project_name`).
- Backtick escaping in the comment body audited (command-substitution hazard in double-quoted bash strings — all escaped).
- `make repolint` clean (pre-push gate).

## Residuals / follow-ups

- The dispatch shim cannot force a review round (v0.2.23 reusable contract); the comment names the working levers. ai-workflows dispatch bridge = owner's radar via orchestrator.
- e2e-pr.yml dispatch left to its owner lane.
- PR-2 (run-watchdog.yml + pkg/runwatchdog) automates the detection this shim performs manually.

## Addendum (r1) — pin the dispatch guards

Mutation-verification (orchestrator) held PR-1 on one gap: `TestPrReviewCallerContract` did not pin the dispatch guards — removing the `review:` job's `if:` gate left the suite green, while the repo's own convention pins if-gates (`TestAiCommentAuthorizationGatePresent`). Added `TestPrReviewDispatchGuardsPinned`: the workflow_dispatch trigger's existence, the `review:` job's `pull_request`-only gate (between job id and `uses:`), and `dispatch-recovery`'s `workflow_dispatch`-only gate (between job id and `runs-on:`). Red-first: each gate independently removed → test FAILs naming the hazard; restored → full `./tests/ghaworkflows/` suite green.

## Addendum (r2) — the dead dedup probe (review findings 1-4 + robustness)

Review reproduction caught what my validation could not: `gh api` has NO `--arg` flag — the dedup probe's `--jq --arg m ...` errored on every execution (`accepts 1 arg(s), received 4`), and the `2>/dev/null || echo 0` swallowed it, so EXISTING was always 0: every dispatch would have re-posted the fault comment and re-queued full CI. bash -n and the gha pins were green because the defect was runtime gh semantics, not syntax. Fixes shipped:

1. All probes now pass SHA via `env.SHA` inside the jq program (the sibling probes' verified pattern) and stream hits per page with `--paginate` (>100-comment PRs hid markers/reviews on page 2+ — finding 4).
2. Probe failures are LOUD: `|| { ::error; exit 1 }` — no `|| echo 0` anywhere; a transient API failure fails the job instead of inverting into delivered=false → spurious comment + dispatch (finding 2; secrets-integration.yml:50-52 never-a-silent-skip rule).
3. The ci.yml re-dispatch step's `if:` is coupled to the comment dedup (`steps.comment.outputs.duplicate != 'true'`) — a marker-present invocation no longer queues full multi-arch CI per invocation (finding 3).
4. Robustness guards while touched: `dispatch-recovery` gains a per-target concurrency group (near-simultaneous dispatches supersede instead of racing the dedup) and the resolve step refuses non-OPEN PRs (no fault comments on closed PRs, no dispatch at deleted refs).
5. Style: dead `tee /tmp/pr.env` dropped; false-provenance corrected above.

New pins (tests/ghaworkflows/pr_review_recovery_test.go, red-first where meaningful): TestPrReviewNoDeadProbeArgForm (the `--jq --arg` class is unpinnable-by-presence — asserted absent; env.SHA presence asserted), TestPrReviewProbeJqSemantics (every SHA-scoped --jq program executed through real jq against sample payloads — hit for the probed SHA, silence for another; the --arg form would fail this trivially), TestPrReviewFaultMarkerSelfConsistency (dedup probe's contains-key ↔ posted comment's marker skeleton, byte-consistent), TestPrReviewDispatchCoupledToDedup (both gate clauses + the duplicate flag emission), TestPrReviewRecoveryOpenOnly. Validation: full ./tests/ghaworkflows green; all three corrected probe invocations executed live against the API (exit 0, correct empties); the e2e dispatch exercise rides the post-merge live-fire per orchestrator ruling.
