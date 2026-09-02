# Worklog: pool → lenaxia-dind at full scale; runner rollout root cause

**Date:** 2026-09-02
**Session:** Move the US-70 delivery pool to the repo-scoped `lenaxia-dind` runner set at `RESUME_SCALE=100` (the Epic 70/69 evidence bottleneck), including the ops-prod GitHub-App wiring and the corrected registration root cause.
**Status:** Complete

---

## Objective

The 2-core GitHub-hosted runner could not absorb the AC-13 batch (pool runs 10–16: API crash-loops, provisioner starvation, scrape starvation — documented in the run-10..16 comments and the #1226 diagnose bundles). Restore the full-100 leg on capacity-appropriate hardware: the repo-scoped `lenaxia-dind` privileged runner set (ops-prod #2385, repo-scoped to this repo by 2baafa74).

---

## Work Completed

### talos-ops-prod (runner rollout)

- Wired the owner's shared GitHub-App secret (`actions-runner`, sops) into both runner sets: `githubConfigSecret: actions-runner`, removed the per-set stub secrets (3e34a76).
- **Root cause of the registration 404 (supersedes my earlier diagnosis):** NOT an App-permission issue — `lenaxia` is a **personal account** (type: User); ARC maps a bare `githubConfigUrl` (`https://github.com/lenaxia`) to the **org** API, which does not exist for users → permanent 404 on `/orgs/lenaxia/actions/runners/registration-token` regardless of permissions or install scope. My "Administration: Read & write" diagnosis was disproven by retest (all-repos install did not fix it). Fixed by the in-cluster ops agent (ops-prod 2baafa74): repo-scoped `githubConfigUrl: https://github.com/lenaxia/LLMSafeSpaces` — the only supported pattern for personal accounts (the working gokore set is repo-scoped the same way). `lenaxia-general-runner` dropped as unused. Listener Running, runner-scale-set-id=1, zero controller errors.
- Rule 7 lesson recorded: I asserted the permission hypothesis from the 404 shape without validating the account type — one `gh api /users/lenaxia` would have disproven it.

### LLMSafeSpaces (PR #1238)

- `us-70-delivery-pool.yml`: `runs-on: lenaxia-dind-runner` (privileged set, dispatch/schedule-only per the US-70 posture), `RESUME_SCALE: 25 → 100` with the run-10..16 history documented in the comment. Boot-storm hardening (#1236 two-phase wait, wave-boot 3ffe5a9d, runsc pod-pin #1232) stays landed and finally exercises at 100.
- New `lenaxia-dind-smoke.yml`: <1min dispatch-only smoke (dind reachable, capacity, passwordless sudo, pull+run with a registry-fetched digest — not hand-written).
- Pin: `TestUS70PoolWorkflow_Pins` asserts `runs-on: lenaxia-dind-runner` + `RESUME_SCALE: 100`.
- **Review correction adopted:** the PR's original pre-merge smoke gate was structurally unmeetable — `workflow_dispatch` requires the workflow on the default branch, so a branch-only smoke cannot be dispatched. Restructured: merge gate = registered listener (in-cluster evidence recorded above); smoke dispatches on main immediately post-merge as the pre-pool gate.

---

## Key Decisions

1. Repo-scoped runner registration (personal-account constraint) over the org-scoped design — cross-repo pools would require an org migration (recorded as a future option, not a blocker).
2. Smoke as a post-merge pre-pool gate rather than a pre-merge gate — GitHub API constraint, documented in the PR.
3. Shared `actions-runner` secret design kept (the owner's preference); rotation blast radius = all sets at once, acceptable at this scale.

---

## Blockers

None. Awaiting #1238 review; then: smoke on main → pool dispatch → expected harvest (AC-13@100 + REV rows + F1–F5 + Epic 69 #1218/#1219 evidence).

---

## Tests Run

`go test ./local/ -count=1` green (incl. the extended pool-workflow pin); YAML parse checks on both workflows; digest fetched from registry-1.docker.io.

---

## Files Modified

`.github/workflows/us-70-delivery-pool.yml`, `.github/workflows/lenaxia-dind-smoke.yml` (new), `local/us70_harness_script_test.go`, this worklog; ops-prod: `runners/lenaxia-dind/helmrelease.yaml`, `runners/{lenaxia-dind,lenaxia-general}/…` (stub secret removal + general set drop).
