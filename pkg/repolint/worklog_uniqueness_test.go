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

// #1795 pins (owner ruling, option c): the post-rewrite hook KEEPS its
// renumber-all semantics — distributed self-healing during repolint-bot
// lag — and the safety net for what that semantics can produce (two
// writers minting the same number across the lag window) is a repolint
// UNIQUENESS INVARIANT: duplicate worklog numbers redden CI. The old
// duplicate check was removed from runWorklogSentinels on the
// since-falsified assumption that "the post-merge bot assigns numbers
// atomically per-merge, so merge-time collisions cannot occur" — the
// 25ed1853 lag window (rebase onto main carrying four unnumbered
// sentinels) proved otherwise.

// buildRepolintBinary builds the real cmd/repolint binary into dest.
func buildRepolintBinary(t *testing.T, dest string) {
	t.Helper()
	if testing.Short() {
		t.Skip("repolint binary integration skipped under -short (shells out to go build)")
	}
	root := repoRoot(t)
	cmd := exec.Command("go", "build", "-o", dest, "./cmd/repolint")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build cmd/repolint: %v\n%s", err, out)
	}
}

// TestWorklogUniqueness_RedOnDuplicates — THE #1795 red pin: a tree
// with two worklogs claiming the same number must FAIL the repolint
// run (the state two lag-window writers can independently produce).
// Reproduces the invariant at the CLI level — the same surface CI's
// `make repolint` drives.
func TestWorklogUniqueness_RedOnDuplicates(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module pin.test/pin\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wl := filepath.Join(root, "worklogs")
	if err := os.MkdirAll(wl, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two files, one number — the duplicate class the invariant exists
	// for. Distinct dates/slugs so only the NUMBER collides.
	for _, name := range []string{
		"1150_2026-10-19_lane-a.md",
		"1150_2026-10-20_lane-b.md",
	} {
		if err := os.WriteFile(filepath.Join(wl, name), []byte("# pin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Healthy neighbors so the ONLY finding is the duplicate.
	for _, name := range []string{"1149_2026-10-18_prior.md", "1151_2026-10-21_next.md"} {
		if err := os.WriteFile(filepath.Join(wl, name), []byte("# pin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	bin := filepath.Join(t.TempDir(), "repolint")
	buildRepolintBinary(t, bin)

	cmd := exec.Command(bin)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	combined := string(out)
	if err == nil {
		t.Fatalf("repolint must FAIL on duplicate worklog numbers (ruling c uniqueness invariant) — exited 0\n%s", combined)
	}
	// The DISCRIMINATING assertion: pre-#1795 the duplicate passed
	// silently (the check had been removed); the uniqueness line naming
	// 1150 is the invariant firing — the scratch's other missing-dir
	// failures must not be what fails this test.
	if !strings.Contains(combined, "FAIL  worklogs uniqueness") || !strings.Contains(combined, "1150") {
		t.Errorf("the worklogs uniqueness check must fire and name the duplicated number 1150:\n%s", combined)
	}
}

// TestWorklogUniqueness_UppercaseSlugsCounted — the #1807 r6 gap pin:
// historical filenames with uppercase slugs (e.g. the live
// `0498_..._epic-54-US-54.1-...md` class) are invisible to
// WorklogPattern's lowercase-only slug class — the uniqueness check
// must count ANY numbered-worklog file (WorklogNumberPattern) or real
// duplicates hide in plain sight (two live pairs shipped on main
// before this pin: 0498 and 0500).
func TestWorklogUniqueness_UppercaseSlugsCounted(t *testing.T) {
	d := t.TempDir()
	files := []string{
		"0498_2026-06-22_epic-54-US-54.1-login-lookup-endpoint.md",
		"0498_2026-06-22_relay-leak-fix-implementation.md",
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(d, f), []byte("# pin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := SequenceCheck(SequenceConfig{Dir: d, Pattern: WorklogNumberPattern, AllowGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Duplicates) == 0 {
		t.Fatalf("the uppercase-slug duplicate pair must be counted — WorklogNumberPattern must see both 0498 files")
	}
}

// TestRepolintMain_RegistersWorklogUniqueness — the registration pin
// (the #1305 pattern): a check deleted from main() passes every
// package test; this fails instead.
func TestRepolintMain_RegistersWorklogUniqueness(t *testing.T) {
	src, err := os.ReadFile(repoRoot(t) + "/cmd/repolint/main.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, pin := range []string{
		"failures += runWorklogUniqueness(root)",
		"func runWorklogUniqueness(root string) int {",
	} {
		if !strings.Contains(string(src), pin) {
			t.Errorf("cmd/repolint/main.go lost its wiring: %q not found — the uniqueness invariant would be silently disabled", pin)
		}
	}
}

// TestPostRewrite_LagWindowRenumbersMainInheritedSentinels — the
// 25ed1853 scenario, end to end through the REAL hook: main carries
// unnumbered sentinels (repolint-bot lag), a lane rebase fires
// .githooks/post-rewrite, and the sanctioned renumber-all semantics
// numbers EVERY sentinel — main-inherited included — with no
// duplicates. This pins ruling c's chosen behavior; "fixing" the hook
// to skip main-inherited sentinels (option a) must fail here.
func TestPostRewrite_LagWindowRenumbersMainInheritedSentinels(t *testing.T) {
	if testing.Short() {
		t.Skip("post-rewrite hook integration skipped under -short (shells out to git)")
	}
	s := newHookScratch(t)

	// The hook's fix-worklogs pass resolves the repo root by walking up
	// for go.mod — the scratch must carry one (the real repo always
	// does). Committed on main before the lag commit so the rebased
	// lane tree includes it.
	writeHookFile(t, filepath.Join(s.main, "go.mod"), "module pin.test/pin\n\ngo 1.26\n")

	// The lag state on main: two unnumbered worklogs the bot hasn't
	// numbered yet (the 25ed1853 shape).
	writeHookFile(t, filepath.Join(s.main, "worklogs", "NNNN_2026-10-19_main-lane-one.md"), "# one\n")
	writeHookFile(t, filepath.Join(s.main, "worklogs", "NNNN_2026-10-20_main-lane-two.md"), "# two\n")
	git(t, s.main, "add", "-A")
	git(t, s.main, "commit", "-q", "--no-verify", "-m", "main: two unnumbered worklogs (bot lag)")

	// The lane: its own sentinel, committed before the rebase.
	writeHookFile(t, filepath.Join(s.worktree, "worklogs", "NNNN_2026-10-20_lane-three.md"), "# three\n")
	git(t, s.worktree, "add", "-A")
	git(t, s.worktree, "commit", "-q", "-m", "lane: own worklog")

	// A real repolint binary where the hook expects it, so the hook's
	// build-if-needed probe takes the prebuilt path.
	bin := filepath.Join(s.worktree, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	buildRepolintBinary(t, filepath.Join(bin, "repolint"))

	// The lag-window rebase: post-rewrite fires on the lane worktree.
	git(t, s.worktree, "rebase", "main")

	// Ruling c semantics: every sentinel numbered — including the two
	// main-inherited ones — and all numbers unique. The harness seeds
	// its own lane sentinel (lane-work) and two numbered fixtures
	// (1143/1144), so the tree is six files; the CLAIM is: zero
	// unnumbered remain, no number repeats, and both main-inherited
	// sentinels received numbers.
	entries, err := os.ReadDir(filepath.Join(s.worktree, "worklogs"))
	if err != nil {
		t.Fatal(err)
	}
	numbered := map[string]string{}
	for _, e := range entries {
		m := WorklogPattern.FindStringSubmatch(e.Name())
		if m == nil {
			t.Errorf("worklog %q still unnumbered after the lag-window rebase — renumber-all semantics broken", e.Name())
			continue
		}
		if prev, dup := numbered[m[1]]; dup {
			t.Errorf("duplicate number %s after renumber: %q and %q — the uniqueness invariant's exact trigger", m[1], prev, e.Name())
		}
		numbered[m[1]] = e.Name()
	}
	for _, inherited := range []string{"main-lane-one", "main-lane-two"} {
		found := false
		for _, name := range numbered {
			if strings.Contains(name, inherited) {
				found = true
			}
		}
		if !found {
			t.Errorf("main-inherited sentinel %q was NOT renumbered — the lag-window self-healing semantics changed", inherited)
		}
	}
	if len(numbered) < 4 {
		t.Errorf("expected the renumber to cover all sentinels (>=4 numbered), got %d: %v", len(numbered), numbered)
	}
}
