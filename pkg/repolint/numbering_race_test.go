// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// ---------------------------------------------------------------------------
// The #1838 lag-window race, simulated end-to-end at the package level.
//
// Setup per test: a shared "main" view (worklogs 1160..1180), two branch
// trees forked from it (A and B), each carrying one NNNN_ sentinel. Each
// branch runs the SAME assignment pass the post-merge bot and
// .githooks/post-rewrite use — FixWorklogs — "inside one bot-lag window",
// i.e. against the same base view of main, exactly as two lanes
// renumbering concurrently do. Then the trees are merged file-wise (the
// way git auto-merges distinct filenames) and the #1807 uniqueness gate
// (SequenceCheck over WorklogNumberPattern) renders the verdict.
//
// TestRenumberRace_NaiveAssignmentCollides is the CONTROL: it documents
// that claims-blind assignment (today's behavior whenever discovery is
// unavailable — offline clone, unauthenticated gh) still collides. It is
// expected to keep "passing" by asserting the duplicate EXISTS; it pins
// the degraded mode the uniqueness invariant is the net for.
//
// TestRenumberRace_ClaimedAwareAssignmentDisjoint is the #1838 root-fix
// assertion: once the assignment pass consults the open-branch
// taken-set (the second assigner discovers the first's claim), the
// merged tree must pass the uniqueness gate.
// ---------------------------------------------------------------------------

// raceTree is one branch's working copy: a copy of the shared main view
// plus whatever the branch adds.
type raceTree struct {
	dir string
}

// newRaceFixture builds the shared main view (worklogs 1160..1180, all
// numbered, unique) and returns two branch trees forked from it.
func newRaceFixture(t *testing.T, slugA, slugB string) (*raceTree, *raceTree) {
	t.Helper()
	base := t.TempDir()
	for i := 1160; i <= 1180; i++ {
		mustWrite(t, filepath.Join(base, numName(i, "2026-10-21", "mainline")), "")
	}
	a := &raceTree{dir: copyRaceDir(t, base)}
	b := &raceTree{dir: copyRaceDir(t, base)}
	mustWrite(t, filepath.Join(a.dir, "NNNN_2026-10-22_"+slugA+".md"), "")
	mustWrite(t, filepath.Join(b.dir, "NNNN_2026-10-22_"+slugB+".md"), "")
	return a, b
}

// number runs the assignment pass (the hook/bot entry point) in this
// tree and returns the number the sentinel received, plus its filename.
func (rt *raceTree) number(t *testing.T) (int, string) {
	t.Helper()
	renames, err := FixWorklogs(rt.dir)
	if err != nil {
		t.Fatalf("FixWorklogs: %v", err)
	}
	if len(renames) != 1 {
		t.Fatalf("expected exactly 1 rename (the sentinel), got %d: %v", len(renames), renames)
	}
	m := WorklogPattern.FindStringSubmatch(renames[0].To)
	if m == nil {
		t.Fatalf("rename target %q is not a numbered worklog", renames[0].To)
	}
	n := atoi(t, m[1])
	return n, renames[0].To
}

// mergeInto copies every file of src into dst (file-wise auto-merge:
// distinct filenames never conflict — the exact mechanism that landed
// duplicate 1161/1164 pairs and migration 41x2 on main).
func (rt *raceTree) mergeInto(t *testing.T, dst *raceTree) {
	t.Helper()
	entries, err := os.ReadDir(rt.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(rt.dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst.dir, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// raceVerdict runs the #1807 uniqueness gate over the merged tree.
func (rt *raceTree) raceVerdict(t *testing.T) SequenceReport {
	t.Helper()
	rep, err := SequenceCheck(SequenceConfig{
		Dir:       rt.dir,
		Pattern:   WorklogNumberPattern,
		AllowGaps: true,
	})
	if err != nil {
		t.Fatalf("SequenceCheck: %v", err)
	}
	return rep
}

func TestRenumberRace_NaiveAssignmentCollides(t *testing.T) {
	// CONTROL — documents the race. Both assigners are claims-blind
	// (discovery unavailable). Both must mint the SAME next-free number
	// and the merged tree must carry a duplicate — the exact failure
	// mode of occurrences 1161x2 / 1164x2. If this ever fails, the
	// naive path changed shape; revisit the degradation notes in
	// FixWorklogs.
	restoreClaimsSeam(t, func(string) *OpenClaims { return nil })

	a, b := newRaceFixture(t, "lane-alpha", "lane-beta")
	an, _ := a.number(t)
	bn, _ := b.number(t)
	if an != bn {
		t.Fatalf("control expected both claim-blind assigners to collide on the same number, got %d vs %d", an, bn)
	}

	a.mergeInto(t, b)
	rep := b.raceVerdict(t)
	if rep.OK() {
		t.Fatalf("control expected duplicate %d on merged tree; got (ok)", an)
	}
	if len(rep.Duplicates) != 1 || rep.Duplicates[0].Version != an {
		t.Fatalf("control expected exactly one duplicate at %d, got %+v", an, rep.Duplicates)
	}
}

func TestRenumberRace_ClaimedAwareAssignmentDisjoint(t *testing.T) {
	// THE #1838 ROOT FIX. Assignment is serialized by knowledge rather
	// than by luck: the first assigner takes next-free; the second
	// assigner's discovery (the open-branch taken-set) sees the first's
	// claim and skips it. The merged tree must be duplicate-free.
	restoreClaimsSeam(t, func(string) *OpenClaims { return nil })

	a, b := newRaceFixture(t, "lane-alpha", "lane-beta")

	// Branch A numbers first; no other open branch holds a number yet.
	an, _ := a.number(t)

	// Branch B numbers with discovery of A's claim — the state the
	// fixed hook/bot computes via ScanOpenClaims (here injected).
	restoreClaimsSeam(t, func(string) *OpenClaims {
		return &OpenClaims{
			Worklogs: map[int][]int{an: {1839}},
		}
	})
	bn, _ := b.number(t)

	if an == bn {
		t.Fatalf("claimed-aware assignment still collided: both took %d", an)
	}

	a.mergeInto(t, b)
	if rep := b.raceVerdict(t); !rep.OK() {
		t.Fatalf("merged tree must pass the uniqueness gate after claimed-aware assignment: %s", rep.String())
	}
}

func TestRenumberRace_ThreeBranchesOneWindow_Disjoint(t *testing.T) {
	// Stress the taken-set: three concurrent branches. The assigners
	// run in arbitrary order; each sees the claims of the branches that
	// already numbered. All three numbers must be pairwise disjoint and
	// the merged tree must pass the gate.
	restoreClaimsSeam(t, func(string) *OpenClaims { return nil })

	base := t.TempDir()
	for i := 1160; i <= 1180; i++ {
		mustWrite(t, filepath.Join(base, numName(i, "2026-10-21", "mainline")), "")
	}
	slugs := []string{"lane-one", "lane-two", "lane-three"}
	trees := make([]*raceTree, len(slugs))
	for i, s := range slugs {
		trees[i] = &raceTree{dir: copyRaceDir(t, base)}
		mustWrite(t, filepath.Join(trees[i].dir, "NNNN_2026-10-22_"+s+".md"), "")
	}

	claimed := map[int][]int{}
	seen := map[int]bool{}
	for _, tr := range trees {
		claimSnapshot := &OpenClaims{Worklogs: cloneClaims(claimed)}
		restoreClaimsSeam(t, func(string) *OpenClaims { return claimSnapshot })
		n, _ := tr.number(t)
		if seen[n] {
			t.Fatalf("duplicate assignment %d across three branches", n)
		}
		seen[n] = true
		claimed[n] = []int{1840}
	}

	for _, tr := range trees[:len(trees)-1] {
		tr.mergeInto(t, trees[len(trees)-1])
	}
	if rep := trees[len(trees)-1].raceVerdict(t); !rep.OK() {
		t.Fatalf("merged tree must pass the uniqueness gate: %s", rep.String())
	}
	got := make([]int, 0, len(seen))
	for n := range seen {
		got = append(got, n)
	}
	sort.Ints(got)
	// Contiguous above the shared max is the expected (not required)
	// shape — sentinels are assigned in claim-aware order.
	if len(got) == 3 && (got[0] != 1181 || got[1] != 1182 || got[2] != 1183) {
		t.Logf("note: assignments were %v (contiguity not asserted — only disjointness is the invariant)", got)
	}
}

// TestFixWorklogs_SeamArgumentIsRepoRoot pins the discovery seam's
// ARGUMENT (r1 review): FixWorklogs must pass the PARENT of the
// worklogs dir (the repo root gh needs) to scanOpenClaims. A regression
// passing `dir` instead compiles, runs, returns nil claims everywhere —
// and silently blinds discovery forever, the exact silent-blinding
// failure class the live smoke caught via the missing JSON tags.
func TestFixWorklogs_SeamArgumentIsRepoRoot(t *testing.T) {
	base := t.TempDir()
	wl := filepath.Join(base, "worklogs")
	if err := os.MkdirAll(wl, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(wl, "1180_2026-10-22_mainline.md"), "")
	mustWrite(t, filepath.Join(wl, "NNNN_2026-10-22_lane.md"), "")

	var got []string
	restoreClaimsSeam(t, func(root string) *OpenClaims {
		got = append(got, root)
		return nil
	})
	if _, err := FixWorklogs(wl); err != nil {
		t.Fatalf("FixWorklogs: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("FixWorklogs never consulted the claims seam — discovery silently disabled")
	}
	for _, root := range got {
		if root != base {
			t.Errorf("claims seam received %q; want the repo root %q (parent of the worklogs dir) — wrong argument silently blinds gh discovery", root, base)
		}
	}
}

// TestFixWorklogs_DuplicateResolutionSkipsClaimedTargets pins the
// duplicate-resolution call site with NON-nil claims (r1 review: every
// other test exercises it with nil). A renumber target that collides
// with an open sibling's claim must be skipped — otherwise the heal
// itself recreates the branch-vs-branch race one number up.
func TestFixWorklogs_DuplicateResolutionSkipsClaimedTargets(t *testing.T) {
	dir := t.TempDir()
	for i := 1160; i <= 1180; i++ {
		if i == 1175 {
			continue
		}
		mustWrite(t, filepath.Join(dir, numName(i, "2026-10-22", "mainline")), "")
	}
	// The duplicate: two files at 1175, no mainline knowledge — the
	// lexically-last local is the newcomer.
	mustWrite(t, filepath.Join(dir, "1175_2026-10-22_aaa-first.md"), "")
	mustWrite(t, filepath.Join(dir, "1175_2026-10-22_zzz-newcomer.md"), "")

	// A sibling open PR already claims 1181 (the naive next-free).
	claimed := map[int]bool{1181: true}

	renames, err := fixWorklogs(dir, nil, claimed)
	if err != nil {
		t.Fatalf("fixWorklogs: %v", err)
	}
	if len(renames) != 1 {
		t.Fatalf("expected 1 rename, got %d: %v", len(renames), renames)
	}
	if renames[0].From != "1175_2026-10-22_zzz-newcomer.md" {
		t.Fatalf("expected the lexically-last local to be renumbered, got %s", renames[0].From)
	}
	if renames[0].To != "1182_2026-10-22_zzz-newcomer.md" {
		t.Fatalf("renumber target must skip the sibling-claimed 1181 → 1182, got %s", renames[0].To)
	}
	rep, err := SequenceCheck(SequenceConfig{Dir: dir, Pattern: WorklogNumberPattern, AllowGaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("tree must be duplicate-free after the claim-aware heal: %s", rep.String())
	}
}

func TestMigrationRace_TwoPairsSameVersion_GateReddens(t *testing.T) {
	// Pin for the numbering-gate workflow's migration half (occurrence
	// 4: two 000041 pairs file-wise auto-merged onto one tree). The
	// gate is SequenceCheck — the same detection the merge-tree recheck
	// runs via `repolint -numbering-only`.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "000040_trigger_script_gate.up.sql"), "")
	mustWrite(t, filepath.Join(dir, "000040_trigger_script_gate.down.sql"), "")
	mustWrite(t, filepath.Join(dir, "000041_task_failed_state.up.sql"), "")
	mustWrite(t, filepath.Join(dir, "000041_task_failed_state.down.sql"), "")
	mustWrite(t, filepath.Join(dir, "000041_board_config.up.sql"), "")
	mustWrite(t, filepath.Join(dir, "000041_board_config.down.sql"), "")

	rep, err := SequenceCheck(SequenceConfig{
		Dir:           dir,
		Pattern:       MigrationPattern,
		RequirePaired: true,
	})
	if err != nil {
		t.Fatalf("SequenceCheck: %v", err)
	}
	if rep.OK() {
		t.Fatal("gate must redden a merge tree carrying two version-41 migration pairs")
	}
	if len(rep.Duplicates) != 1 || rep.Duplicates[0].Version != 41 {
		t.Fatalf("expected duplicate at version 41, got %+v", rep.Duplicates)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// restoreClaimsSeam overrides the open-claims discovery seam for the
// duration of the test and restores it on cleanup.
func restoreClaimsSeam(t *testing.T, f func(string) *OpenClaims) {
	t.Helper()
	orig := scanOpenClaims
	scanOpenClaims = f
	t.Cleanup(func() { scanOpenClaims = orig })
}

func copyRaceDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func numName(n int, date, slug string) string {
	return pad4(n) + "_" + date + "_" + slug + ".md"
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("atoi(%q): non-digit", s)
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func pad4(n int) string {
	s := ""
	for v := n; v > 0; v /= 10 {
		s = string(rune('0'+v%10)) + s
	}
	for len(s) < 4 {
		s = "0" + s
	}
	return s
}

func cloneClaims(m map[int][]int) map[int][]int {
	out := make(map[int][]int, len(m))
	for k, v := range m {
		out[k] = append([]int(nil), v...)
	}
	return out
}
