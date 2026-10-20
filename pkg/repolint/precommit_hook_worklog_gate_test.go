// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #1778 pins: the pre-commit worklog sentinel gate (.githooks/pre-commit,
// run_worklog_prefix_gate) must exempt merge commits that carry main's
// repolint-bot-numbered worklogs (1143_…, 1144_… — new to the branch via
// `git merge main`, already numbered on main, renaming would corrupt the
// bot's numbering), while still firing for genuinely unnumbered NEW
// worklogs on regular commits.
//
// Root cause of the false positive (#1772's event-loss recovery merge):
// the gate's merge detection tested `[ -f .git/MERGE_HEAD ]`, which is
// unreachable in a linked worktree — there `.git` is a FILE pointing at
// <main>/.git/worktrees/<name>, so the exemption never fired in the exact
// (worktree-lane) topology where merges are performed. These tests run
// the REAL hook (via core.hooksPath) inside a scratch repo + linked
// worktree, so a regression in either direction fails here.

// hookScratch is a throwaway git repository wired to run this checkout's
// real .githooks/pre-commit. Stub Make targets (repolint, gitleaks) stand
// in for the gates the sentinel check does not exercise, so a blocked or
// passing commit is attributable to the worklog gate alone.
type hookScratch struct {
	main     string // scratch repo root (branch main)
	worktree string // linked worktree (branch lane) — the lane topology
}

func newHookScratch(t *testing.T) *hookScratch {
	t.Helper()
	if testing.Short() {
		t.Skip("pre-commit hook integration skipped under -short (shells out to git/make)")
	}

	root := repoRoot(t)
	base := t.TempDir()
	s := &hookScratch{
		main:     filepath.Join(base, "repo"),
		worktree: filepath.Join(base, "wt"),
	}
	if err := os.MkdirAll(s.main, 0o755); err != nil {
		t.Fatal(err)
	}

	git(t, s.main, "init", "-q", "-b", "main")
	git(t, s.main, "config", "user.email", "pin@invalid")
	git(t, s.main, "config", "user.name", "pin")
	git(t, s.main, "config", "commit.gpgsign", "false")
	git(t, s.main, "config", "core.hooksPath", filepath.Join(root, ".githooks"))

	writeHookFile(t, filepath.Join(s.main, "Makefile"), "repolint:\n\t@true\n\ngitleaks:\n\t@true\n")
	git(t, s.main, "add", "Makefile")
	git(t, s.main, "commit", "-qm", "base")

	// The lane: a linked worktree with its own sentinel worklog, the
	// standard multi-agent topology (one shared checkout, per-lane
	// worktrees).
	git(t, s.main, "worktree", "add", s.worktree, "-b", "lane")
	writeHookFile(t, filepath.Join(s.worktree, "worklogs", "NNNN_2026-10-20_lane-work.md"), "# lane\n")
	git(t, s.worktree, "add", ".")
	git(t, s.worktree, "commit", "-qm", "lane work with sentinel worklog")

	// Main moves on: the post-merge bot numbers two worklogs. Their
	// numbers are canonical on main — a merging lane must not be forced
	// to rename them (the #1778 false positive). The real bot commits
	// from CI where local hooks are not installed (--no-verify mirrors
	// that; the pin is about the LANE's merge, not the bot's commit).
	for _, w := range []string{
		"1143_2026-10-19_1759-pvc-root-allows.md",
		"1144_2026-10-19_1758-build-cache-emptydir.md",
	} {
		writeHookFile(t, filepath.Join(s.main, "worklogs", w), "# "+w+"\n")
	}
	git(t, s.main, "add", ".")
	git(t, s.main, "commit", "--no-verify", "-qm", "chore(repolint): assign worklog numbers")

	return s
}

// commit attempts `git commit -m msg` in dir WITHOUT failing the test on
// error, so tests can assert on the outcome (blocked vs passed) and print
// the gate's message.
func commit(t *testing.T, dir, msg string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", "commit", "-m", msg)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestPreCommitHook_MergeCommitWithBotNumberedWorklogsPasses is the #1778
// exemption pin: a union merge concluded in a linked worktree (MERGE_HEAD
// present), carrying main's bot-numbered worklogs as additions new to the
// branch, must pass pre-commit WITHOUT --no-verify.
func TestPreCommitHook_MergeCommitWithBotNumberedWorklogsPasses(t *testing.T) {
	s := newHookScratch(t)

	// `git merge main` auto-commits clean merges without running
	// pre-commit; the recovery-merge shape (#1772) concludes manually
	// via --no-commit, which is where the hook (and the false positive)
	// runs. MERGE_HEAD is live for the whole window.
	git(t, s.worktree, "merge", "main", "--no-commit")

	// Sanity: we really are mid-merge in a linked worktree (.git is a
	// file there — the condition the old `[ -f .git/MERGE_HEAD ]` check
	// missed).
	mh := exec.Command("git", "rev-parse", "-q", "--verify", "MERGE_HEAD")
	mh.Dir = s.worktree
	if out, err := mh.CombinedOutput(); err != nil {
		t.Fatalf("precondition: merge not in conclusion: %v\n%s", err, out)
	}
	if fi, err := os.Stat(filepath.Join(s.worktree, ".git")); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("precondition: %s/.git should be a regular file in a linked worktree", s.worktree)
	}

	out, err := commit(t, s.worktree, "Merge main into lane (union)")
	if err != nil {
		t.Fatalf("pre-commit blocked a union merge carrying main's bot-numbered worklogs (#1778 false positive):\n%s", out)
	}

	// It must actually be a merge commit (two parents), not a silent bypass.
	p2 := exec.Command("git", "rev-parse", "--verify", "-q", "HEAD^2")
	p2.Dir = s.worktree
	if out, err := p2.CombinedOutput(); err != nil {
		t.Fatalf("commit did not conclude the merge (no second parent): %v\n%s", err, out)
	}
}

// TestPreCommitHook_RegularCommitWithUnnumberedWorklogStillFires is the
// still-fires leg: a genuinely unnumbered NEW worklog on a regular commit
// (no merge in conclusion) must still be blocked — also inside a linked
// worktree, proving the exemption did not turn the gate off there.
func TestPreCommitHook_RegularCommitWithUnnumberedWorklogStillFires(t *testing.T) {
	s := newHookScratch(t)

	writeHookFile(t, filepath.Join(s.worktree, "worklogs", "event-loss-recovery-notes.md"), "# notes\n")
	git(t, s.worktree, "add", ".")

	out, err := commit(t, s.worktree, "add unnumbered worklog")
	if err == nil {
		t.Fatalf("worklog sentinel gate did NOT fire for an unnumbered new worklog on a regular commit:\n%s", out)
	}
	if !strings.Contains(out, "must use the NNNN_ sentinel prefix") {
		t.Fatalf("regular commit blocked by an unexpected gate (want the worklog sentinel):\n%s", out)
	}
}

// TestPreCommitHook_RegularCommitWithAuthorPickedNumberStillFires pins the
// gate's second failure mode: an author picking their own number (here
// shaped exactly like a bot-numbered file) on a regular commit must still
// be blocked — the exemption is for merge conclusions only, so the
// sentinel scheme's anti-race guarantee on authoring commits is intact.
func TestPreCommitHook_RegularCommitWithAuthorPickedNumberStillFires(t *testing.T) {
	s := newHookScratch(t)

	writeHookFile(t, filepath.Join(s.worktree, "worklogs", "1234_2026-10-20_i-picked-this.md"), "# picked\n")
	git(t, s.worktree, "add", ".")

	out, err := commit(t, s.worktree, "add author-numbered worklog")
	if err == nil {
		t.Fatalf("worklog sentinel gate did NOT fire for an author-picked worklog number on a regular commit:\n%s", out)
	}
	if !strings.Contains(out, "must use the NNNN_ sentinel prefix") {
		t.Fatalf("regular commit blocked by an unexpected gate (want the worklog sentinel):\n%s", out)
	}
}

// TestPreCommitHook_RegularCommitWithSentinelPasses is the control: the
// NNNN_ sentinel form on a regular commit still passes the whole hook
// (doc-only commit → stubbed remaining gates) — the fix must not break
// the intended authoring flow.
func TestPreCommitHook_RegularCommitWithSentinelPasses(t *testing.T) {
	s := newHookScratch(t)

	writeHookFile(t, filepath.Join(s.worktree, "worklogs", "NNNN_2026-10-20_second-entry.md"), "# two\n")
	git(t, s.worktree, "add", ".")

	if out, err := commit(t, s.worktree, "add sentinel worklog"); err != nil {
		t.Fatalf("pre-commit blocked a correct NNNN_ sentinel worklog:\n%s", out)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

func writeHookFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
