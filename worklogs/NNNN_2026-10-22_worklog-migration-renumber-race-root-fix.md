# Worklog: #1838 — root fix for the worklog/migration renumber next-free race (taken-set + merge-tree gate + CAS bot)

**Date:** 2026-10-22
**Session:** Lane #1838. The check-then-act race documented in the issue (five occurrences this week: worklog 1161x2, 1173x2, 1175x2, migration 41x2, plus 0498/0500 historical and 1164x2 live) has three distinct sub-mechanisms; this work closes all three with one soft layer, one hard layer, and one serialization layer.
**Status:** Complete

---

## Objective

No merge may land a duplicate numbered artifact (worklogs OR api/migrations), and no near-simultaneous pair of assignments may mint the same number — without touching lease/liveness or board-engine semantics (#1847's lane) or #1837's tooling surface.

---

## Race Anatomy (evidence)

- **R1 — bot vs lag window**: `worklog-renumber.yml` had NO concurrency group, checked out the TRIGGERING SHA (stale once queued), and pushed with no non-FF retry. Two runs in one window → same next-free → 1161x2, 1164x2.
- **R2 — branch vs branch**: `.githooks/post-rewrite` assigns from a main-only view; sibling OPEN branches are invisible → both lanes mint N → file-wise auto-merge lands both → 1173x2, 1175x2.
- **R3 — hand-numbered**: migrations have no assignment automation at all; two PRs carried 000041 (41x2). No bot in the loop — bot-only fixes cannot touch this class.
- **The enforcement gap**: `repos/.../branches/main/protection` → 404 (no required checks; ci.yml documents the merge gate as process), and ci.yml's Lint evaluates merge-checkout only at PR-push time — a main advance after the check ran merges on a stale green. That is exactly the 41x2 sequence (#1823 merged 17:1x, #1840 merged 17:25).

## Work Completed (red-first)

- **Red suite first, verified failing against pre-fix logic** (`pkg/repolint/numbering_race_test.go` + `gate_test.go`):
  - `TestRenumberRace_ClaimedAwareAssignmentDisjoint` — RED as "claimed-aware assignment still collided: both took 1181".
  - `TestRenumberRace_ThreeBranchesOneWindow_Disjoint` — RED as "duplicate assignment 1181 across three branches".
  - `TestNumberingGateDecision/clean_with_prior_red_CLEARS_(clear-on-heal)` — RED as "noop, want clear".
  - Controls green and staying: `TestRenumberRace_NaiveAssignmentCollides` (pins that claims-blind assignment — the offline degraded mode — still collides; the #1807 invariant is its net) and `TestMigrationRace_TwoPairsSameVersion_GateReddens` (pins the 41x2 detection shape).
- **Soft layer** (`pkg/repolint/open_claims.go`): `OpenClaims` taken-set across open PR heads (`gh pr list --json number,headRefOid` + `repos/{owner}/{repo}/git/trees/{oid}?recursive=1`), consumed by `fixWorklogs`/`assignSentinels`/`nextFreeWorklogNumber` — one wiring point covers the bot, the post-rewrite hook, and `make pre-commit-fix`. Own-PR excluded via HEAD SHA; per-PR fetch failures skip that PR; any list failure degrades to nil (pre-#1838 behavior). Claims are numbers, not incumbents: sibling claims never win newcomer selection, they only block assignment.
- **Hard layer** (`.github/workflows/numbering-gate.yml` + `repolint -numbering-only`): on push to main touching either artifact class (and a :43-offset 6h cron sweep), re-evaluate every open PR's merge tree (`git fetch refs/pull/N/merge` + worktree) with the two sequence gates. Colliding tree → failing `numbering-gate` check run on the PR head (namespaced title/summary naming the collision and the heal lever — not the PR's own lint); healed → success run replaces the red. Decision matrix pinned as `NumberingGateDecision`; bash only gathers inputs and executes.
- **CAS layer** (`worklog-renumber.yml`): `worklog-renumber` concurrency group (queued, never cancelled), `ref: main` checkout (a queued run numbers the current tip), and a ≤3-attempt push→fetch→rebase→re-fix→amend retry. Git's FF check on main is the compare-and-swap; no manifest file (a manifest would serialize only the bot — 3 of 5 occurrences had no bot in the loop — and would add a conflict-prone artifact).
- **`repolint -next-numbers`** (`pkg/repolint/next_numbers.go`): next free worklog + migration vs local ∪ origin/main ∪ open-PR claims; report filters to claims above max (a live smoke showed the unfiltered report drowning in shared-ancestry noise — all 9 open PRs "claiming" all 43 migrations).

## Live Validation

- Gate primitive e2e in a scratch repo: merge tree carrying `0006_lane-a` + `0006_lane-b` → `-numbering-only` FAILS naming both files; clean tree passes.
- Discovery live against the repo (fixed en route): the first live run reported zero claims — `openPRHead` lacked JSON tags, `headRefOid` never mapped, every SHA arrived empty and silently blinded the scan (the exec seam worked; the unmarshal was the hole). With tags + an empty-SHA guard: `next worklog: 1183, open-PR claims: 1181 (PR 1864,1863,1862), 1182 (PR 1864)` — three open PRs really do hold 1181 right now; the taken-set is netting live fire.
- Workflows: YAML parsed via sigs.k8s.io/yaml; every `run:` block extracted and `bash -n` clean; all jq expressions executed against sample payloads.
- Full `go test ./pkg/repolint/` green (35.8s incl. live tests); `go vet` clean; gofmt clean.

## Key Decisions

- **Taken-set + merge-tree gate over manifest/merge-queue** (ratified by orchestrator): no branch protection exists, so required-check and merge-queue designs are not implementable from a PR; git's tip already IS a CAS register, so a manifest adds nothing for R1 and cannot see R2/R3.
- **Clear-on-heal is pinned, not emergent**: a gate that only reddens pins healed PRs forever — the freeze class #1838 exists to end, recreated by its own fix. Likewise the red check is namespaced (title says "not this PR own-lint"; summary names files + heal lever) because lanes twice this week chased gate reds as their own diff's failure.
- **Bot commits keep [skip ci]** — it skips the gate sweep too, which is sound (the bot cannot introduce a duplicate; it skips occupied + claimed) and loop-prevention depends on the marker.
- **Residual, documented**: offline/gh-less environments degrade to main-only assignment (control-pinned); a colliding PR that pushes a new head goes un-swept until the next main push, but a new head re-runs ci.yml's Lint against a fresh merge ref, which covers that window.
