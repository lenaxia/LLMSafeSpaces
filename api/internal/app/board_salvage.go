// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_salvage.go — the salvage-prefer terminal's production probe
// (pkg/board/salvage.go's SalvageSource): the app-seam adapter that
// asks git/GitHub the three questions about a task's lane, in the
// workspace pod where the worktree lives.
//
// LANE RESOLUTION (the design's "workspace repo + the branch from the
// row's pointer when present, else the charter's lane convention"):
//
//   - The row's recorded pointer names the branch — the reliable leg
//     (every depose after the first carries it).
//   - Without a pointer (first sighting) the probe DISCOVERS: branches
//     are ordered by recency and probed for facts, topical matches
//     (the task's short-id words) first. The exhausted attempts were,
//     by definition, the workspace's most recent committers — the
//     latest lane holding facts is the honest v1 guess, stated as
//     such: a wrong first sighting costs one depose whose pointer the
//     inheritor is told to VERIFY, never a lost PR (the #2064 shape
//     the seam exists for).
//
// HONESTY (the liveness seam's discipline): the probe rides the SAME
// activator + node-execution seams as the spawner (EnsureActive +
// agentd's sessionless script node — non-agent types carry no harness
// identity, #1957's keyless shape), read-only (git inspection + one gh
// pr list; no writes, no markers). Failure at ANY leg — scope resolve,
// activation, dispatch, script error, budget miss — answers nil
// LOUDLY: the ordinary failed terminal, never a failed claim. The
// budget is the seam's contract (~2s): an ACTIVE pod resolves fast; a
// cold workspace that cannot activate in-budget degrades to no
// signals (loud) — tuning the constant is a deployment concern, not a
// correctness one (the loop bound keeps degradation from spinning).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardSalvageBudget clamps one SalvageFor call (the seam's ~2s
// hygiene, the boardObserveBudget precedent).
const boardSalvageBudget = 2 * time.Second

// newBoardSalvageSource builds the production SalvageSource over the
// spawner's own seams: the task-scope resolver (owner + target
// workspace), the pod activator, and the agentd node executor.
func newBoardSalvageSource(scope boardScopeResolver, activator spawnActivator, executor spawnDispatcher) board.SalvageSource {
	return &boardSalvageSource{scope: scope, activator: activator, executor: executor}
}

type boardSalvageSource struct {
	scope     boardScopeResolver
	activator spawnActivator
	executor  spawnDispatcher
}

// SalvageFor implements board.SalvageSource. Nil is "nothing
// salvageable" and "could not answer" alike — always loud on the
// latter, always the ordinary terminal engine-side.
func (b *boardSalvageSource) SalvageFor(ctx context.Context, t *board.Task) *board.SalvageSignals {
	if b == nil || b.scope == nil || b.activator == nil || b.executor == nil || t == nil || t.ID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, boardSalvageBudget)
	defer cancel()

	_, workspaceID, err := b.scope(ctx, t.ID)
	if err != nil {
		slog.WarnContext(ctx, "board: salvage probe could not resolve the task's workspace (no signals)",
			"taskId", t.ID, "error", err.Error())
		return nil
	}
	branch := ""
	if t.Salvage != nil {
		branch = t.Salvage.Branch
	}
	podIP, err := b.activator.EnsureActive(ctx, workspaceID, boardSalvageBudget)
	if err != nil {
		slog.WarnContext(ctx, "board: salvage probe could not reach the workspace pod in budget (no signals)",
			"taskId", t.ID, "workspaceID", workspaceID, "error", err.Error())
		return nil
	}
	resp, err := b.executor.Execute(ctx, workspaceID, podIP, boardSalvageProbeRequest(t.ShortID, branch))
	if err != nil {
		slog.WarnContext(ctx, "board: salvage probe dispatch failed (no signals)",
			"taskId", t.ID, "workspaceID", workspaceID, "error", err.Error())
		return nil
	}
	if resp.ErrorCode != "" {
		// #1878's class: agentd surfaces script failures as HTTP-200
		// error envelopes — a failure leg, never a silent empty.
		slog.WarnContext(ctx, "board: salvage probe script failed (no signals)",
			"taskId", t.ID, "errorCode", resp.ErrorCode, "detail", resp.Detail)
		return nil
	}
	return parseBoardSalvageOutput(resp.Output)
}

// boardSalvageProbeRequest builds the sessionless script-node dispatch:
// python, read-only, the lane keys rendered as COMPILE-TIME constants
// (the gate-script declSeed pattern — the script is self-contained and
// byte-pinnable; no input plumbing to abuse).
func boardSalvageProbeRequest(shortID, branch string) *apiwf.NodeExecRequest {
	spec, _ := json.Marshal(map[string]any{
		"language": "python",
		"handler":  boardSalvageScript(shortID, branch),
	})
	return &apiwf.NodeExecRequest{
		NodeID: "board-salvage-probe", NodeType: "script",
		Spec: spec, Timeout: "20s",
	}
}

// boardSalvageScript renders the probe: find the workspace's git repos
// (bounded, deduplicated by git-dir, hidden/vendor dirs skipped),
// resolve the lane (the pointer's branch when given, else recency-
// ordered discovery with topical preference), and answer the three
// questions — unpushed commits on the lane (rev-list --not --remotes),
// an open PR from the lane (gh pr list), uncommitted changes in the
// lane's worktree (status --porcelain).
func boardSalvageScript(shortID, branch string) string {
	shortIDJSON, _ := json.Marshal(shortID)
	branchJSON, _ := json.Marshal(branch)
	return fmt.Sprintf(`import json, os, subprocess

BRANCH = %s
SHORT_ID = %s

def _run(cmd, cwd=None, timeout=8):
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, cwd=cwd)
        return r.returncode, (r.stdout or "")
    except Exception:
        return 1, ""

def _git(repo, *args):
    code, out = _run(["git", "-C", repo] + list(args))
    return out.strip() if code == 0 else ""

def _repos():
    found, seen = [], set()
    try:
        entries = ["/workspace"] + [os.path.join("/workspace", d) for d in os.listdir("/workspace")]
    except Exception:
        entries = ["/workspace"]
    for d in entries:
        base = os.path.basename(d.rstrip("/"))
        if base.startswith(".") or base in ("node_modules",):
            continue
        if _git(d, "rev-parse", "--is-inside-work-tree") != "true":
            continue
        key = _git(d, "rev-parse", "--absolute-git-dir")
        if not key or key in seen:
            continue
        seen.add(key)
        found.append(d)
        if len(found) >= 8:
            break
    return found

def _worktrees(repo):
    wts, cur = {}, None
    for line in _git(repo, "worktree", "list", "--porcelain").splitlines():
        if line.startswith("worktree "):
            cur = line[len("worktree "):]
        elif line.startswith("branch ") and cur:
            wts[line[len("branch "):].replace("refs/heads/", "")] = cur
    return wts

def _open_pr(wt, branch):
    code, out = _run(["gh", "pr", "list", "--state", "open", "--head", branch,
                      "--limit", "1", "--json", "number"], cwd=wt, timeout=6)
    if code != 0:
        return ""
    try:
        rows = json.loads(out or "[]")
        if rows and rows[0].get("number"):
            return str(rows[0]["number"])
    except Exception:
        pass
    return ""

def _facts(repo, branch, wt):
    head = _git(repo, "rev-parse", "--short=12", branch)
    count = _git(repo, "rev-list", "--count", branch, "--not", "--remotes")
    try:
        unpushed = int(count or "0") > 0
    except Exception:
        unpushed = False
    uncommitted = False
    if wt:
        code, status = _run(["git", "-C", wt, "status", "--porcelain"])
        uncommitted = code == 0 and bool(status.strip())
    pr = _open_pr(repo, branch)
    if not (unpushed or pr or uncommitted):
        return None
    return {"branch": branch, "head": head, "openPr": pr, "uncommitted": uncommitted}

def _topical(name):
    tokens = [t for t in SHORT_ID.lower().split("-") if len(t) > 3]
    low = name.lower()
    return bool(tokens) and any(t in low for t in tokens)

def handler(input):
    for repo in _repos():
        wts = _worktrees(repo)
        if BRANCH:
            if BRANCH in wts:
                f = _facts(repo, BRANCH, wts[BRANCH])
            elif _git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/" + BRANCH):
                f = _facts(repo, BRANCH, None)
            else:
                f = None
            if f:
                return f
            continue
        order = _git(repo, "for-each-ref", "--sort=-committerdate",
                     "--format=%%(refname:short)", "refs/heads").splitlines()
        for b in [x for x in order if _topical(x)] + [x for x in order if not _topical(x)]:
            if not b or b in ("main", "master"):
                continue
            f = _facts(repo, b, wts.get(b))
            if f:
                return f
    return {}
`, string(branchJSON), string(shortIDJSON))
}

// parseBoardSalvageOutput maps the script's answer onto the seam's
// signals; the empty object (nothing found) and unparseable output are
// nil — absence and garbage both mean "no signals", the ordinary
// terminal.
func parseBoardSalvageOutput(raw json.RawMessage) *board.SalvageSignals {
	if len(raw) == 0 {
		return nil
	}
	var out struct {
		Branch      string `json:"branch"`
		Head        string `json:"head"`
		OpenPR      string `json:"openPr"`
		Uncommitted bool   `json:"uncommitted"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	sig := &board.SalvageSignals{Branch: out.Branch, Head: out.Head, OpenPR: out.OpenPR, Uncommitted: out.Uncommitted}
	if sig.Empty() {
		return nil
	}
	return sig
}
