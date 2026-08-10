# Worklog: Workspace git init on pod boot (Epic 65 follow-up)

**Date:** 2026-08-10
**Session:** Small prerequisite for US-65.3's FileChange parts — initialize `/workspace` as a git repo on every pod boot so `pkg/agent/opencode/filediff.Producer` can produce unified-diff text via `git diff HEAD`.
**Status:** Complete

---

## Objective

US-65.3 (PR #714) shipped `pkg/agent/opencode/filediff/` — the prototype resolving design 0049 §10's open risk. The Producer runs `git diff HEAD -- <paths>` against the file paths opencode's `session.diff` event reports. That requires `/workspace` to be a git repo.

grep'd `controller/` and `runtimes/` — no `git init` calls exist today. The PVC starts empty or with whatever was committed previously. Without this PR, FileChange parts degrade silently to no-ops (file paths are collected by the translator but no Patch text is produced).

This PR adds `git init` to the always-running `workspace-dirs` init container. Idempotent — `git init` is a no-op on an existing repo, so pod restarts and resumed workspaces both work cleanly.

---

## Work Completed

### Modified: `controller/internal/workspace/pod_builder.go`

- `buildWorkspaceDirsInit(runtimeImage, workspaceName)` — signature now takes the workspace name (used in the git identity for traceability).
- The init script now:
  1. `mkdir -p /pvc/workspace /pvc/home /pvc/tmp` (unchanged — still required for subPath mount on fresh PVC)
  2. `cd /pvc/workspace`
  3. `git init -b main`
  4. `git config user.email 'workspace-<name>@llmsafespaces.local'`
  5. `git config user.name 'workspace-<name>'`

The git config lines set a deterministic identity so future per-turn commits succeed without inheriting host config — CI pods often lack user.email and would fail commits.

### Test: `controller/internal/workspace/pod_builder_test.go`

- `TestPodBuilder_WorkspaceGitInit` — pins all five requirements: mkdir unchanged, cd to /workspace, `git init`, `git config user.email`, `git config user.name`, and the workspace name in the identity.

---

## Key Decisions

1. **Live in `workspace-dirs`, not `workspace-setup`.** workspace-dirs runs unconditionally on every pod; workspace-setup only runs when the workspace has packages or an initScript. Putting git init in workspace-setup would leave workspaces without packages without a git repo.

2. **No per-turn commit yet.** This PR only initializes the repo. Per-turn commits (so each agent turn's diff has a base) are a separate follow-up — the Adapter calls filediff.Producer, the caller decides when to commit. Filediff without commits produces diffs against the initial empty tree (every file appears as new), which is acceptable for the first-turn case but suboptimal. A small agentd-side goroutine that commits at session.idle is the natural next step.

3. **Identity uses the workspace name.** Operator inspecting the PVC via `git log` sees which workspace made each commit. The email uses `.local` to make it obvious this is a synthetic identity, not a real user.

4. **`-b main` explicitly.** git's default branch name has historically varied (`master` → `main`); pinning `main` keeps behavior consistent across git versions and matches what users expect today.

---

## Adversarial Self-Review (Rule 11)

| # | Finding | Class | Resolution |
|---|---|---|---|
| F1 | First implementation put git init in workspace-setup, which is conditional on packages/initScript | Real bug (test caught it) | **Fixed.** Moved to workspace-dirs which always runs. |
| F2 | git init might fail if /workspace is not empty AND not a git repo | Acceptable edge case | No fix. A user could in theory populate /workspace via PVC import without committing; in that case `git init` succeeds (it doesn't require an empty directory) and the first `git add -A && git commit` would track everything as new. That's the correct behavior. |
| F3 | Deterministic email/name might collide if two pods run the same workspace name | Acceptable | No fix. Workspace names are unique per namespace; pods running concurrently for the same workspace (e.g. during migration) would produce commits with identical identity, which is fine — git doesn't dedupe by identity. |
| F4 | Security: does the git config write to /workspace or to a global location? | Validated safe | `git config` (without `--global`) writes to `/workspace/.git/config`. The init container's ReadOnlyRootFilesystem doesn't interfere because `/workspace` is a PVC mount (writable). |

Phase 2 result: zero unresolved real findings.

---

## Tests Run

- `go build ./...` — clean
- `gofmt -l controller/internal/workspace/` — clean
- `golangci-lint run --new-from-rev=origin/main ./controller/internal/workspace/` — 0 issues
- `go test -timeout 30s -count=1 ./controller/...` — PASS (every package)

---

## Next Steps

This PR enables US-65.3's FileChange parts in production. Per Epic 65 sequencing:

1. **US-65.3 (PR #714)** — review in progress; the Adapter + filediff + translator are independent of this PR but production FileChange output requires both.
2. **Per-turn commit goroutine** (small follow-up, post-US-65.4) — agentd commits at session.idle so each turn's diff has a base. Without it, the first turn's diff is "every changed file as new", which is suboptimal but not broken.

---

## Files Modified

**Modified:**
- `controller/internal/workspace/pod_builder.go` (buildWorkspaceDirsInit signature + script)
- `controller/internal/workspace/pod_builder_test.go` (new TestPodBuilder_WorkspaceGitInit)
