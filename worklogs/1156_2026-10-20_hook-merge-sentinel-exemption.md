# Worklog: pre-commit worklog sentinel — repair the merge-commit exemption for worktree lanes (#1778)

**Date:** 2026-10-20
**Session:** Fix the pre-commit worklog sentinel gate false-positiving on union merge commits that carry main's repolint-bot-numbered worklogs; red-first pins for both legs.
**Status:** Complete

---

## Objective

Issue #1778: `run_worklog_prefix_gate` in `.githooks/pre-commit` blocks a lane's union merge of `origin/main` when main's bot-numbered worklogs (1143/1144/1145, later 1149/1150) arrive as `--diff-filter=A` additions new to the branch — the gate demands `NNNN_` sentinels, and renaming would corrupt the bot's canonical numbering on main. Observed on #1772's event-loss recovery merge (lane escaped via `--no-verify` with every real gate green). Fix hook-side so the merge conclusion passes without `--no-verify`, while the sentinel still fires for genuinely unnumbered NEW worklogs on regular commits.

---

## Work Completed

### Root cause isolated (empirically)

The gate already HAD the intended exemption — "Merge commits are exempt entirely" (comment above `run_worklog_prefix_gate`) — but it tested `[ -f .git/MERGE_HEAD ]`. In a linked worktree (`.git` is a FILE containing `gitdir: <main>/.git/worktrees/<name>`) that path never exists, so the exemption was dead code in the exact per-lane-worktree topology the multi-agent workflow mandates. Reproduced end-to-end in a scratch repo + linked worktree: `git merge main --no-commit` staging `worklogs/1143_…`/`1144_…`, the real gate function exits 1 demanding sentinels for both — the precise #1778 false positive. (In the shared checkout proper — a normal repo where `.git` is a directory — the old check worked; the bug was worktree-specific.)

### Fix (surgical — one condition + comment)

`.githooks/pre-commit` `run_worklog_prefix_gate`: merge detection now `git rev-parse -q --verify MERGE_HEAD >/dev/null 2>&1` — ref resolution, correct in normal repos AND linked worktrees, no filesystem-path assumption. Verified against git 2.39.5 (the version the repo's git-credential work pins). Comment updated to record the worktree trap so the check doesn't regress to a literal path test.

### Red-first pins (`pkg/repolint/precommit_hook_worklog_gate_test.go`)

New integration test file following `binary_integration_test.go`'s cross-tree pin pattern: a scratch git repo wired to run the REAL hook via `core.hooksPath` (stub `repolint`/`gitleaks` Make targets isolate the sentinel gate), plus a linked worktree — the lane topology. Four tests, skipped under `-short` (CI's full-suite job runs them):

1. `TestPreCommitHook_MergeCommitWithBotNumberedWorklogsPasses` — union merge (`--no-commit` → manual conclusion, the recovery-merge shape) carrying `1143_`/`1144_` worklogs commits WITHOUT `--no-verify`; asserts MERGE_HEAD precondition, `.git`-is-a-file precondition, and that HEAD really has two parents. **RED before the fix** with the exact #1778 block message; green after.
2. `TestPreCommitHook_RegularCommitWithUnnumberedWorklogStillFires` — unnumbered new worklog on a regular commit in the worktree still blocked (proves the fix didn't disable the gate in worktrees).
3. `TestPreCommitHook_RegularCommitWithAuthorPickedNumberStillFires` — author-picked `1234_…` on a regular commit still blocked (the sentinel scheme's anti-race guarantee; locks the "exempt merges" choice over the "accept numbered files" alternative, which would have weakened this leg).
4. `TestPreCommitHook_RegularCommitWithSentinelPasses` — control: `NNNN_` sentinel on a regular commit passes the whole hook.

### Mutation hygiene

- Fix reverted (red state observed pre-fix): pin 1 fails with the sentinel block; pins 2–4 pass.
- Over-exemption mutation (`if true`): pins 2 and 3 fail (author-numbered commit went through) — the pins catch both regression directions.
- Fix restored: all four green.

---

## Key Decisions

- **Repair the merge detection rather than accept numbered worklogs on all commits.** The issue offered two shapes; accepting `^\d{4}_` files unconditionally would re-open failure mode #1 (authors picking their own numbers race the bot) on every regular commit. The documented original intent was full merge exemption — it just never fired in worktrees. Decision pinned by test 3.
- **`git rev-parse -q --verify MERGE_HEAD` over `--git-path` + `-f`.** Both are worktree-correct on git 2.39; ref resolution additionally makes no assumption about how git lays out pseudorefs on disk (per-worktree git-dirs have moved across versions; ref resolution is the stable contract).
- **Tests live in `pkg/repolint/`** (the sentinel logic's Go home) rather than a `hack/*.test.sh` — the shell-test pattern there is not CI-wired; a Go test runs in CI's full suite automatically.
- **Bot's numbering commit in the scratch setup uses `--no-verify`** — mirrors reality (the post-merge bot commits from CI where local hooks aren't installed) and keeps the pin's attribution on the lane's merge conclusion.
- **Assumptions stated and validated:** (a) MERGE_HEAD presence at pre-commit time ⇔ the commit concludes a merge — validated by the two-parent assertion and git semantics (any commit with MERGE_HEAD set produces a merge commit); (b) `git merge main` clean-auto-merges never run pre-commit (git runs `pre-merge-commit` instead) — the false positive only ever manifested in manually-concluded merges, which is what we pin.

---

## Blockers

None.

---

## Tests Run

- `go test -timeout 120s -run 'TestPreCommitHook_' ./pkg/repolint/ -v` — pre-fix: merge pin RED with the exact #1778 sentinel block; post-fix: 4/4 PASS.
- Mutation (unconditional exemption): still-fires pins RED as required; restored: green.
- `go test -timeout 300s -count=1 ./pkg/repolint/` — ok (21.6s), package intact.
- `gofmt -l`, `goimports -l` on the new test file — clean; `go vet ./pkg/repolint/` — clean; `bash -n .githooks/pre-commit` — syntax ok.

---

## Next Steps

Reviewer: mutation-verify pin 1 (revert the hook condition to `[ -f .git/MERGE_HEAD ]`, confirm RED). Residuals accepted by design (documented here, not blockers): (1) an author can deliberately stage a new unnumbered worklog inside a merge conclusion — merge exemption is total per the original design; CI's repolint WARN and the post-merge bot are the net. (2) `git cherry-pick` of a main commit that adds numbered worklogs still false-positives (CHERRY_PICK_HEAD ≠ MERGE_HEAD) — pre-existing, different scenario than #1778's merge, none observed.

---

## Files Modified

- `.githooks/pre-commit` — `run_worklog_prefix_gate` merge detection: worktree-correct `git rev-parse -q --verify MERGE_HEAD`; comment documents the `.git`-is-a-file trap (#1778).
- `pkg/repolint/precommit_hook_worklog_gate_test.go` — new; four red-first pins (merge exemption + still-fires×2 + control).
- `worklogs/1156_2026-10-20_hook-merge-sentinel-exemption.md` — this worklog.
