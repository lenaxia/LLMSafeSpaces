You are finalizing an already-reviewed pull request for the LLMSafeSpaces repository via an explicit `/merge`. This command makes no code changes — it only merges an approved PR.

Rules:
1. This command runs on a PR thread. Identify the current PR (e.g. `gh pr view`).
2. **Verify approval before merging — no exceptions:**
   - Check the latest automated review state. The most recent review from the AI reviewer MUST be **APPROVE**. If the latest review is `REQUEST_CHANGES`, `COMMENT` with open findings, or there is no review yet, DO NOT merge.
   - **The approving review must be for the CURRENT head.** Fetch the PR's head SHA (`gh pr view <N> --json headRefOid`) and the latest bot APPROVE's `commit_id` (`gh api repos/{owner}/{repo}/pulls/<N>/reviews` — or the `**Commit reviewed:**` line in the review body). If they differ, the approval is stale: a commit was pushed after the review. DO NOT merge — request a re-review of the new head and stop.
   - If not approved: post a comment stating the current review state and what remains (e.g. "Latest review is REQUEST_CHANGES — resolve the open findings and re-review before /merge"). Stop. Do not merge.
3. Confirm the PR branch is not `main` and that all required CI checks are green (or note any non-blocking skipped checks). If a required check is failing, DO NOT merge — report it and stop.
4. Merge with the **squash** method: `gh pr merge <N> --squash --delete-branch` (delete the remote branch to keep the repo tidy, matching the repo's squash-merge convention).
5. If the merge is blocked by GitHub (e.g. branch-protection, behind main), report the exact blocker in a comment and stop. Do not force-push or force-merge.
6. After a successful merge, post a comment on the original thread confirming the merge with the squash-commit SHA and a one-line summary. If this PR was triggered by an issue, link back to that issue.
7. Never use `git push --force`. Never commit directly to `main`.
8. Do not modify files, tests, or the PR diff in this step. `/merge` is a finalize-only operation.
