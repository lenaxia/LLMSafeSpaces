// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Open-branch claims: the taken-set across OPEN PRs (#1838's soft layer).
//
// The numbering authority before #1838 was main's current max — it
// could not see numbers held by pushed-but-unmerged branches, so two
// branches assigning inside one bot-lag window could both mint the
// same number and land it via file-wise auto-merge (occurrences:
// worklog 1161x2, 1164x2, 1173x2, 1175x2; migration 41x2).
//
// OpenClaims is what the assignment pass consults IN ADDITION to the
// local tree and origin/main: numbers already claimed by OTHER open
// PRs' heads. Discovery (defaultScanOpenClaims) shells out to gh and
// degrades to nil — the pre-#1838 behavior — whenever gh, the network,
// or the repository context is unavailable, so hooks and the bot keep
// working offline exactly as before.

// OpenClaims maps a numbered-artifact version to the open PRs whose
// heads claim it. The zero value is an empty (not nil) claim set.
type OpenClaims struct {
	// Worklogs maps worklog version → claiming PR numbers.
	Worklogs map[int][]int
	// Migrations maps migration version → claiming PR numbers.
	Migrations map[int][]int
}

// WorklogClaimed reports whether any open PR claims worklog version v.
func (c *OpenClaims) WorklogClaimed(v int) bool {
	if c == nil {
		return false
	}
	return len(c.Worklogs[v]) > 0
}

// MigrationClaimed reports whether any open PR claims migration v.
func (c *OpenClaims) MigrationClaimed(v int) bool {
	if c == nil {
		return false
	}
	return len(c.Migrations[v]) > 0
}

// WorklogClaimSet returns the claimed worklog versions as a set. Nil
// receiver yields nil — the pre-#1838 no-claims behavior.
func (c *OpenClaims) WorklogClaimSet() map[int]bool {
	if c == nil {
		return nil
	}
	out := make(map[int]bool, len(c.Worklogs))
	for v := range c.Worklogs {
		out[v] = true
	}
	return out
}

// scanOpenClaims is the discovery seam: implementations return the
// open-PR taken-set for the repo rooted at root, or nil when discovery
// is unavailable. Tests override it to simulate sibling branches; the
// bot and hooks consume it through FixWorklogs.
var scanOpenClaims = defaultScanOpenClaims

// defaultScanOpenClaims discovers open-PR claims via the GitHub API.
// Any unavailability (no gh, no auth, no network, not a repo) returns
// nil — assignment then falls back to main-only knowledge, the
// documented degraded mode whose residual races the #1807 uniqueness
// invariant catches.
func defaultScanOpenClaims(root string) *OpenClaims {
	return scanOpenClaimsViaGitHub(root)
}

// ---------------------------------------------------------------------------
// GitHub-backed discovery. The exec seams below exist so tests can
// simulate gh responses and degradation without a network; the shapes
// mirror the existing scanWorklogGit pattern (shell out, degrade on
// any error, bound the runtime).
// ---------------------------------------------------------------------------

// openPRHead is one open PR's number and head commit SHA. The JSON
// tags match `gh pr list --json number,headRefOid` output — found the
// hard way in a live smoke: without the tags, encoding/json matches
// field names case-insensitively ("number" works, "headRefOid" does
// NOT map to HeadSHA) and every SHA arrives empty, silently blinding
// the whole scan.
type openPRHead struct {
	Number  int    `json:"number"`
	HeadSHA string `json:"headRefOid"`
}

// ghListOpenPRs lists open PRs (number + head SHA) for the repo at
// root. Returns an error when gh is missing/unauthenticated — callers
// degrade to nil claims.
var ghListOpenPRs = func(root string) ([]openPRHead, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--state", "open", "--limit", "200", "--json", "number,headRefOid")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []openPRHead
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list: %w", err)
	}
	return prs, nil
}

// ghPRTreePaths lists all file paths in the tree of commit sha via the
// git-trees API (recursive). One call per PR, no clone data needed.
var ghPRTreePaths = func(root, sha string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	//nolint:gosec // G204: the interpolated sha is gh's own headRefOid (JSON-decoded from gh pr list output), not user input
	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/{owner}/{repo}/git/trees/%s?recursive=1", sha),
		"--jq", ".tree[].path")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh api git/trees/%s: %w", sha, err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// gitHEADSHA returns the current HEAD SHA of the repo at root, or ""
// when unavailable. The PR whose head equals the local HEAD is the
// caller's own PR: its claims are already the local tree.
var gitHEADSHA = func(root string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// scanOpenClaimsViaGitHub builds the open-PR taken-set: for every open
// PR other than the local HEAD's own, the numbered worklog versions and
// migration versions its head tree carries. Returns nil on ANY error —
// discovery is best-effort by design; the caller must never fail
// numbering because the taken-set could not be fetched.
func scanOpenClaimsViaGitHub(root string) *OpenClaims {
	prs, err := ghListOpenPRs(root)
	if err != nil {
		return nil
	}
	self := gitHEADSHA(root)
	claims := &OpenClaims{
		Worklogs:   map[int][]int{},
		Migrations: map[int][]int{},
	}
	for _, pr := range prs {
		if pr.HeadSHA == "" {
			// Defensive: a PR without a resolvable head SHA carries no
			// claims we can read (and must not be treated as matching
			// an empty local HEAD below).
			continue
		}
		if self != "" && pr.HeadSHA == self {
			continue // own PR: claims are already local knowledge
		}
		paths, err := ghPRTreePaths(root, pr.HeadSHA)
		if err != nil {
			// One unreachable PR must not blind the whole scan;
			// skip it rather than degrade everything. A claim we
			// miss here is what the numbering-gate hard layer
			// catches at merge time.
			continue
		}
		claimsFromTreePaths(pr.Number, paths, claims)
	}
	return claims
}

// claimsFromTreePaths folds one PR's tree paths into claims. Pure: the
// parser is what tests pin (both artifact classes, sentinel files claim
// nothing, non-matching paths are ignored).
func claimsFromTreePaths(prNumber int, paths []string, into *OpenClaims) {
	for _, p := range paths {
		base := filepath.Base(p)
		switch {
		case strings.HasPrefix(p, "worklogs/"):
			if m := WorklogNumberPattern.FindStringSubmatch(base); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					into.Worklogs[v] = appendUnique(into.Worklogs[v], prNumber)
				}
			}
		case strings.HasPrefix(p, "api/migrations/"):
			if m := MigrationPattern.FindStringSubmatch(base); m != nil && m[3] == "up" {
				if v, err := strconv.Atoi(m[1]); err == nil {
					into.Migrations[v] = appendUnique(into.Migrations[v], prNumber)
				}
			}
		}
	}
}

// appendUnique appends n when not already present (a PR head appears
// once per tree listing, but the helper keeps the invariant explicit).
func appendUnique(list []int, n int) []int {
	for _, existing := range list {
		if existing == n {
			return list
		}
	}
	return append(list, n)
}
