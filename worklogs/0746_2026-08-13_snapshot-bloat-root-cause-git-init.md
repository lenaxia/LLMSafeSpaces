# Worklog: Root cause of snapshot bloat — `git init /workspace` (US-65.3) triggers opencode snapshot self-inclusion recursion

**Date:** 2026-08-13
**Severity:** P0 — causes every user-visible symptom reported since v0.14.0
**Status:** Root cause identified and proven. Cleanup applied to one pod. Permanent fix not yet implemented.
**Related:** Epic 65, US-65.3 (PR #715, commit `9775e3d2c`), opencode issue #18072

---

## Executive Summary

The `git init /workspace` that Epic 65 / US-65.3 added to enable `filediff.Producer` is the **root cause** of the snapshot bloat spiral that has been causing stuck-busy, inconsistent history rendering, load failures, and halted sessions since v0.14.0. It is not an opencode bug, not an Epic 65 adapter bug, and not a wire-shape issue — it is a self-inflicted architectural error: making the workspace container itself a git repo triggers opencode's snapshot system, which has no self-exclusion and recurses on its own object database.

The `filediff.Producer` this change was meant to enable adds **zero value** — opencode already emits `FileDiff.Info[]` with full patch text natively via its `session.diff` event and `Snapshot.diffFull`. The Go-side `filediff` re-runs `git diff HEAD` against an empty repo (no commits), producing noise.

**Evidence grade:** Every claim below is backed by live pod inspection, opencode DB queries, opencode source diff, and the git history of this repo. No assumptions.

---

## 1. The user's reported symptoms

User reported (on 2026-08-13, against production v0.15.3):

1. **Stuck busy** — sessions show busy indefinitely when not actually working
2. **Inconsistent history rendering** — new agent messages take a long time to show, or don't appear until app/switch refresh
3. **Not showing busy when actually working** — reverse of #1; UI shows idle while agent is mid-turn
4. **Load failures and halted sessions** — frequent, with no surfaced errors
5. **Phantom symptoms** — behaviors so inconsistent the user cannot tell which are real vs phantom

Example session: `https://chat.safespaces.dev/chat/d3e35405-530d-44e5-baa5-5bc2a46d1476/ses_00678beb6ffew1mmVrEGG8iQVq`

---

## 2. The causal chain (proven end-to-end)

```
PR #715 (US-65.3, 2026-08-10, commit 9775e3d2c)
  buildWorkspaceDirsInit adds: cd /pvc/workspace && git init -b main
    → /workspace becomes a git repo (previously it was a plain directory)
      → opencode boots with cwd=/workspace, detects /workspace/.git
        → project table: id=global, worktree=/workspace, vcs=git
          → Snapshot.enabled() returns true (snapshot/index.ts:168)
            → Snapshot.track() creates gitdir at /workspace/.local/opencode/snapshot/global/<hash>/
              → runs `git add --all --sparse` against /workspace (the worktree)
                → /workspace/.local/ is inside the worktree
                  → NO /workspace/.gitignore exists (verified — none created by init script)
                    → git add stages snapshot's own objects directory
                      → new objects appear as new untracked files
                        → next track() call stages those too
                          → exponential recursion
```

### 2.1 The self-inclusion mechanism

opencode's snapshot system (`packages/opencode/src/snapshot/index.ts`, verified identical blob `4da9bc3ca86467bddaf19b69e570c9051da0842a` at v1.18.10 and v1.18.18) works as follows:

1. Creates a git "object database" (bare gitdir) at `$XDG_DATA_HOME/opencode/snapshot/<project>/<hash>/`
2. Sets worktree to `/workspace`
3. On every `track()` call (per agent step), runs `git add --all` to snapshot the worktree state
4. Has no self-exclusion — it does NOT exclude its own `XDG_DATA_HOME` path from the add

The only exclusion mechanism is `info/exclude`, which is populated **reactively** by the `sync()` function with individual large-file paths — whack-a-mole, not a blanket `/.local/` exclusion.

### 2.2 Why it spirals instead of stabilizing

Each `git add --all` that stages the snapshot's own `objects/` directory creates new git objects (loose objects + pack). Those new objects are new files in the worktree. The next `git add --all` sees them as new untracked files and stages them too, creating more objects. This is exponential growth.

The `seed()` function (added in commit `51891d56e7`, "fix(snapshot): reuse source git objects to avoid re-hashing huge repos") was intended to help large repos by sharing the source repo's object database via `objects/info/alternates`. But here the "source repo" IS the empty `/workspace/.git` that US-65.3 created — it has no useful objects, so seeding adds nothing.

### 2.3 The `.bloated` red herring

During investigation, a `.bloated` suffix directory was found at `/workspace/.local/opencode/snapshot/global/<hash>.bloated`. This was **not** created by opencode, git, or any LLMSafeSpace code — the string `.bloated` does not exist in any opencode version (verified by `git grep` across all tags) or in this repo (verified by `grep -rn`). It was created by a concurrent diagnostic session (`ses_00673fd89...`, "Broken session resume issue") that manually ran `mv <snapshot-gitdir> <snapshot-gitdir>.bloated` as a backup during a performance experiment. The experiment's Step 9 timed out before Step 11's cleanup ran, leaving the backup behind. This is confirmed by querying the host's opencode DB for that session's bash tool calls.

---

## 3. Live evidence (pod `d3e35405-530d-44e5-baa5-5bc2a46d1476-697ecb32`)

### 3.1 Before cleanup

| Measurement | Value |
|---|---|
| Total `/workspace` size | 4.3 GB |
| Actual worktree (excluding `.local/`) | 129 MB |
| Snapshot gitdir (`objects/`) | 4.1 GB |
| Loose git objects | 45,449 |
| Pack files | 3.4 GB |
| Files in snapshot index | 61,833 |
| Files in index matching `.local/` | **61,832** (99.998%) |
| Files in index NOT matching `.local/` | 1 (`llmsafespaces` — a symlink) |
| `info/exclude` lines (whack-a-mole list) | 137 (individual object paths, no blanket exclusion) |
| `/workspace/.gitignore` | **Did not exist** |

### 3.2 During CPU starvation (when symptoms were active)

| Measurement | Value |
|---|---|
| Concurrent `git add --all` processes | 2 (one at 99% CPU, one at 59%) |
| Pod CPU limit | 2 cores (both pinned) |
| Pod memory | 97% of 8GB cap (`MemoryPressure=True` in statusz) |
| opencode `/global/health` response | Timeout (14s+, then `context deadline exceeded`) |
| agentd `/global/health` poll | `context deadline exceeded` (every 2s in logs) |
| agentd `/provider` poll | `context canceled` (repeating in logs) |
| API adapter `ListPending` | `GET /permission: context deadline exceeded` |
| API `wsstate` | `Redis GetWorkspaceConfig failed — context deadline exceeded` |

### 3.3 After git processes complete (intermittent recovery)

| Measurement | Value |
|---|---|
| statusz response | HTTP 200 in 7ms |
| Session state | `idle`, `sessions_active: 0` |

This explains the **inconsistency** the user reported: when git processes finish, opencode recovers and works normally; when the next `track()` fires, the spiral restarts and everything breaks again.

### 3.4 After cleanup (manual `rm -rf` of snapshot gitdir + `.gitignore` added)

| Measurement | Before | After |
|---|---|---|
| `/workspace` size | 4.3 GB | 225 MB |
| opencode statusz | Intermittent timeouts | HTTP 200 in 8ms (stable) |
| `git` processes | 2 at 99%+59% CPU | None |

### 3.5 Other active workspace (same pattern)

Pod `a2846397-c0d4-4188-bf98-01a717848a72-407b3375`:

| Measurement | Value |
|---|---|
| Total `/workspace` | 1.4 GB |
| Actual worktree | 120 KB |
| Snapshot dir | 1.3 GB |

Same bloat pattern, earlier in the spiral. Every workspace with `git init` will eventually hit this.

---

## 4. opencode version investigation

### 4.1 Is this fixed in 1.18.11+?

**No.** The snapshot source code is byte-identical across all 1.18.x releases:

| Tag | Blob hash of `packages/opencode/src/snapshot/index.ts` |
|---|---|
| v1.15.12 | `f974a457ad7bef7c7e3ac9258c93cdf0e7cd01aa` |
| v1.18.9 | `4da9bc3ca86467bddaf19b69e570c9051da0842a` |
| v1.18.10 | `4da9bc3ca86467bddaf19b69e570c9051da0842a` |
| v1.18.18 | `4da9bc3ca86467bddaf19b69e570c9051da0842a` |

`git diff v1.18.10 v1.18.18 -- packages/opencode/src/snapshot/index.ts` returns empty. No snapshot fix shipped in any 1.18 release. There is also no unreleased fix on `main` (`git log v1.18.18..HEAD -- packages/opencode/src/snapshot/` returns empty).

### 4.2 What changed between 1.15.12 and 1.18.9?

Two relevant commits landed in the 1.18 line:
- `51891d56e7` (2026-06-10) — "fix(snapshot): reuse source git objects to avoid re-hashing huge repos" (#31798). Adds `seed()` to share source repo objects via alternates. Does NOT address self-inclusion.
- `dcf7b4e792` (2026-06-23) — "fix(opencode): handle snapshot paths from subdirectories" (#33506). Path handling fix. Does NOT address self-inclusion.

Neither commit adds a self-exclusion for the snapshot's own directory.

### 4.3 Upstream issue #18072

[Issue #18072](https://github.com/anomalyco/opencode/issues/18072) ("Snapshot git add runs indefinitely on worktrees with large non-code files") reports the same class of problem — `git add .` running indefinitely at ~90% CPU, with the hourly `git gc` consuming 3.7GB RAM. It was closed as completed on 2026-03-25, but the fix (the `seed()` + large-file blocking logic in the current source) does NOT prevent self-inclusion when the snapshot directory itself is inside the worktree. Our case is a variant: the worktree is small (129MB), but the self-inclusion recursion generates unbounded object growth.

opencode's own docs acknowledge this class of problem ([config.mdx:632](https://opencode.ai/config)):
> "For large repositories or projects with many submodules, the snapshot system can cause slow indexing and significant disk usage as it tracks all changes using an internal git repository. You can disable snapshots using the `snapshot` option."

The `snapshot: false` config option disables the entire snapshot system.

### 4.4 The pod's opencode config

The pod does NOT set `"snapshot": false`. The agent-config (`/sandbox-runtime/agent-config.json`) only configures providers and models. There is no `opencode.json` at `/workspace` or in the project. This means opencode uses the default (`snapshot: true`), which combined with `vcs=git` (from US-65.3's `git init`) enables the snapshot system unconditionally.

---

## 5. Why `filediff.Producer` adds zero value

### 5.1 What opencode already does natively

opencode has a complete, built-in file-diff system:

1. **`session.diff` event** (`packages/schema/src/v1/session.ts:644`) — emits `FileDiff.Info[]` (with file, patch, additions, deletions, status) on every turn via SSE
2. **`SessionSummary.computeDiff`** (`packages/opencode/src/session/summary.ts:98`) — diffs between step-start and step-finish snapshots via `snapshot.diffFull(from, to)`
3. **`Vcs.diff`** (`packages/opencode/src/project/vcs.ts:373`) — produces `FileDiff[]` against the actual project repo (the one the user cloned)
4. **`Snapshot.diffFull`** (`packages/opencode/src/snapshot/index.ts`) — full diff engine between two snapshot trees

All of these operate at the **per-project level**. When a user clones a repo to `/workspace/myproject`, opencode detects `/workspace/myproject/.git` and operates on THAT repo — correctly.

### 5.2 What `filediff.Producer` does

`filediff.Producer` (`pkg/agent/opencode/filediff/filediff.go`) runs:
```
git -C /workspace add -N -- <paths>   # intent-to-add
git -C /workspace diff HEAD -- <paths>  # diff against HEAD
```

This is wrong for two reasons:

1. **`/workspace` is a container, not a repo.** The `/workspace` git repo was `git init`'d with **zero commits** (the US-65.3 commit message admits: *"first turn's diff is 'every changed file as new'"*). So `git diff HEAD` diffs against nothing — producing noise, not signal. And HEAD doesn't exist until a commit is made, which the commit message says is a "separate follow-up" that was never implemented.

2. **It duplicates work opencode already did.** opencode's `session.diff` event already carries `FileDiff.Info[]` with patch text. Our adapter's `translate.go` already extracts file paths from the event. The Go-side `filediff.Producer` then re-runs `git diff` to produce patch text that opencode's native `Snapshot.diffFull` already computed and emitted.

### 5.3 The design doc's own risk flag

Design 0049 §10 flagged this as an open risk. US-65.3 was supposed to resolve it by prototyping `filediff.Producer`. The prototype works mechanically, but the design never considered that:
- Making `/workspace` a git repo enables opencode's snapshot system
- The snapshot system has no self-exclusion
- The snapshot system would spiral on the empty container repo

---

## 6. What was missed — root cause of the root cause

### 6.1 No end-to-end verification (Rule 0 violation)

US-65.3 (PR #715) shipped with `TestPodBuilder_WorkspaceGitInit` which verifies the init script contains `git init`. It does NOT verify:
- That opencode's snapshot system activates as a result
- That the snapshot system doesn't spiral
- That `/workspace/.gitignore` is needed
- That `filediff.Producer` actually produces meaningful diffs against the empty repo
- That opencode's native `session.diff` event already provides the same data

The test pins the implementation, not the outcome. This is the same pattern worklog 0744 §"What is NOT yet verified" flagged across Epic 65.

### 6.2 The git init was added to solve a problem that didn't exist

The US-65.3 commit message says: *"Without this PR, FileChange parts degrade silently to no-ops (file paths are collected but no Patch text is produced)."*

But opencode's `session.diff` event already carries patch text via `FileDiff.Info`. The "no-ops" were happening because the adapter wasn't reading the native event's diff data — not because `/workspace` wasn't a git repo. The fix should have been in the adapter's event translation, not in the workspace init.

### 6.3 `/workspace` is architecturally a container, not a repo

Per the user: *"workspace is intended to be a place where we clone other repos to."* Users clone repos to `/workspace/myproject`. Each cloned repo has its own `.git` and its own history. opencode detects these per-project and does the right thing natively. The top-level `/workspace` should NOT be a git repo — it's a parent directory, like `$HOME`.

---

## 7. Recommended fix

### 7.1 Immediate (this pod — already applied)

1. ✅ Deleted the 4.1 GB snapshot gitdir (`rm -rf /workspace/.local/opencode/snapshot/global/<hash>`)
2. ✅ Added `/workspace/.gitignore` with `/.local/` to prevent immediate recurrence
3. Result: workspace went from 4.3 GB to 225 MB; opencode stable at 8ms statusz

**Warning:** This is temporary. The next pod restart will re-run `git init` (the init script is unchanged). The `.gitignore` persists on the PVC but the snapshot system will re-activate.

### 7.2 Short-term (code fix — recommended approach)

**Remove the `git init` from `buildWorkspaceDirsInit`** (revert PR #715). Restore the init script to:
```
mkdir -p /pvc/workspace /pvc/home /pvc/tmp
```

This:
- Disables opencode's snapshot system at the container level (`vcs` stays undefined for the global project → `Snapshot.enabled()` returns false)
- Lets opencode detect VCS per-project when users clone actual repos (the correct behavior)
- Keeps opencode's native `session.diff` / `Vcs.diff` working for real repos
- Makes `filediff.Producer` a harmless no-op (it already degrades gracefully — `adapter.go:359`: `if a.differ == nil { return nil }`)
- Should be paired with removing or no-op'ing the `filediff.Producer` wiring, since it adds no value

### 7.3 Alternative short-term (if filediff must be kept)

If there's a reason to keep `/workspace` as a git repo (there shouldn't be — see §5), then:

1. Add `/.local/` to `/workspace/.gitignore` in the init script
2. Set `"snapshot": false` in the opencode agent-config to disable the snapshot system entirely

But this is strictly worse than §7.2 because it keeps a meaningless git repo and requires ongoing maintenance of the exclusion.

### 7.4 Long-term (defensive)

1. Add a startup check in agentd that detects if `/workspace/.local/opencode/snapshot/` exceeds a threshold (e.g., 2x worktree size) and logs a warning or auto-cleans
2. Consider contributing a self-exclusion fix upstream to opencode (exclude `$XDG_DATA_HOME` from the snapshot `git add --all` pathspec) — this would protect all opencode users, not just LLMSafeSpaces
3. Add an integration test that boots a workspace pod, runs an agent turn, and asserts the snapshot dir stays bounded

---

## 8. Actions taken in this session

| Action | Status |
|---|---|
| Identified snapshot bloat on pod d3e35405 (4.1 GB for 129 MB workspace) | ✅ Done |
| Proved 61,832 of 61,833 indexed files are `.local/` self-references | ✅ Done |
| Proved no `/workspace/.gitignore` exists | ✅ Done |
| Traced `git init` to US-65.3 (commit `9775e3d2c`, PR #715) | ✅ Done |
| Verified snapshot code unchanged across all opencode 1.18.x releases | ✅ Done |
| Verified `filediff.Producer` adds no value over opencode's native `session.diff` | ✅ Done |
| Cleaned up bloat on pod d3e35405 (4.3 GB → 225 MB) | ✅ Done |
| Added temporary `.gitignore` on pod d3e35405 | ✅ Done |
| Confirmed same bloat on second active workspace (a2846397: 1.3 GB for 120 KB) | ✅ Done |
| Code fix (remove `git init`) | ❌ Not yet implemented |
| Cleanup on pod a2846397 | ❌ Not yet done |
| Upstream opencode issue for self-exclusion | ❌ Not yet filed |

---

## 9. Open questions

1. **Should we remove `filediff.Producer` entirely, or leave it as a no-op?** It adds no value (§5), but removing it touches the adapter (`pkg/agent/opencode/adapter.go:340-350`), the adapter options (`WithFileDiffProducer`), and the agentd wiring. Leaving it as a no-op is lower risk but leaves dead code.

2. **Do we need per-turn commits?** The US-65.3 commit message mentions "per-turn commits (so each turn's diff has a base) are a separate follow-up — agentd commits at session.idle." This was never implemented. If `filediff` is removed, this question is moot. If kept, it needs a separate design.

3. **Should we set `"snapshot": false` in the opencode agent-config regardless?** Even without the `git init`, if a user manually runs `git init` in `/workspace`, the snapshot system would activate. Setting `"snapshot": false` would be a belt-and-suspenders defense, but it disables the revert/undo feature for all users — including those working in legitimate per-project repos where the snapshot system works correctly.

---

## 10. References

- **PR #715** (commit `9775e3d2c`) — the `git init` change: `controller/internal/workspace/pod_builder.go:635-642`
- **opencode snapshot source** — `packages/opencode/src/snapshot/index.ts` (blob `4da9bc3ca86467bddaf19b69e570c9051da0842a`, identical v1.18.9–v1.18.18)
- **opencode issue #18072** — upstream report of snapshot `git add` running indefinitely
- **opencode config docs** — `snapshot: false` option (config.mdx:630-641)
- **Design 0049 §10** — the open risk US-65.3 was supposed to resolve
- **filediff.Producer** — `pkg/agent/opencode/filediff/filediff.go`
- **Adapter filediff wiring** — `pkg/agent/opencode/adapter.go:79-86, 338-350, 354-379`
- **Worklog 0744** — documents the Epic 65 hotfix marathon; this worklog identifies the root cause that 0744's fixes couldn't address
