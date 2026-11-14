// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package scripts_test

// pi_bump_gate_test.go — the S6 acceptance harness for
// scripts/pi-bump-gate.sh: the gate must FAIL a pinned-version bump
// that ships without a fixture refresh, and PASS a no-op (or
// fixture-accompanied, or first-landing) change. These tests execute
// the REAL script against synthetic repos, the same stance as
// renumber_pr_num_test.go. The story's acceptance criterion ("bumping
// the pinned version with an unrefreshed fixture FAILS CI; a no-op
// bump passes clean") is pinned HERE, not by inspection of ci.yml.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateRepo stages a synthetic repo and returns a runner that executes
// the real gate script between two refs inside it.
type gateRepo struct {
	t   *testing.T
	dir string
}

func newGateRepo(t *testing.T) *gateRepo {
	t.Helper()
	dir := t.TempDir()
	g := &gateRepo{t: t, dir: dir}
	g.git("init", "-q", "-b", "main")
	return g
}

func (g *gateRepo) git(args ...string) {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (g *gateRepo) write(path, content string) {
	g.t.Helper()
	full := filepath.Join(g.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
}

func (g *gateRepo) run(bash, script, base, head string) (string, error) {
	g.t.Helper()
	cmd := exec.Command(bash, script, base, head)
	cmd.Dir = g.dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runGate stages the canonical repo shape (pin 1.5.2 + one fixture at
// base), applies the head mutations, and runs the gate.
func runGate(t *testing.T, mutate func(g *gateRepo)) (string, error) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script, err := filepath.Abs("pi-bump-gate.sh")
	if err != nil {
		t.Fatal(err)
	}
	g := newGateRepo(t)
	g.write("runtimes/pi/Dockerfile", "ARG PI_VERSION=1.5.2\n")
	g.write("pkg/agent/pi/testdata/events_stream.jsonl", `{"type":"agent_start"}`+"\n")
	g.git("add", "-A")
	g.git("commit", "-qm", "base")
	g.git("checkout", "-qb", "head")
	mutate(g)
	// The head ref must exist as a commit for the three-dot diff even
	// when nothing changed.
	g.git("add", "-A")
	g.git("commit", "-qm", "head", "--allow-empty")
	return g.run(bash, script, "main", "head")
}

func TestPiBumpGate_BumpWithoutFixtureFails(t *testing.T) {
	out, err := runGate(t, func(g *gateRepo) {
		g.write("runtimes/pi/Dockerfile", "ARG PI_VERSION=1.6.0\nARG PI_SOURCE_SHA256=deadbeef\n")
	})
	if err == nil {
		t.Fatalf("gate must FAIL a bump without fixture refresh; output:\n%s", out)
	}
	if !strings.Contains(out, "PI_VERSION bumped 1.5.2 -> 1.6.0") || !strings.Contains(out, "REFRESH.md") {
		t.Fatalf("gate error must name the bump and the runbook; output:\n%s", out)
	}
}

func TestPiBumpGate_BumpWithFixturePasses(t *testing.T) {
	out, err := runGate(t, func(g *gateRepo) {
		g.write("runtimes/pi/Dockerfile", "ARG PI_VERSION=1.6.0\nARG PI_SOURCE_SHA256=deadbeef\n")
		g.write("pkg/agent/pi/testdata/events_stream.jsonl", `{"type":"agent_start"}`+"\n"+`{"type":"agent_settled"}`+"\n")
	})
	if err != nil {
		t.Fatalf("gate must pass a bump WITH fixture refresh; output:\n%s", out)
	}
	if !strings.Contains(out, "pi fixture-freshness gate: ok") {
		t.Fatalf("gate should report ok; output:\n%s", out)
	}
}

func TestPiBumpGate_NoOpPasses(t *testing.T) {
	// The no-op bump: the pin did not move — the workflow's
	// "pinned >= latest, nothing to do" case lands nothing, and an
	// empty-diff head must pass clean.
	out, err := runGate(t, func(g *gateRepo) {})
	if err != nil {
		t.Fatalf("gate must pass a no-op change; output:\n%s", out)
	}
}

func TestPiBumpGate_ShaOnlyRotationPasses(t *testing.T) {
	// Rotating PI_SOURCE_SHA256 alone (version unchanged): the gate
	// compares the VERSION — a sha-only rotation is not a wire change
	// and must not demand fixtures.
	out, err := runGate(t, func(g *gateRepo) {
		g.write("runtimes/pi/Dockerfile", "ARG PI_VERSION=1.5.2\nARG PI_SOURCE_SHA256=rotated\n")
	})
	if err != nil {
		t.Fatalf("gate must pass a sha-only rotation; output:\n%s", out)
	}
}

func TestPiBumpGate_PinFirstLandingPasses(t *testing.T) {
	// Base without runtimes/pi/Dockerfile (the pre-S5-merge world):
	// vacuous pass, documented in the script.
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	script, err := filepath.Abs("pi-bump-gate.sh")
	if err != nil {
		t.Fatal(err)
	}
	g := newGateRepo(t)
	g.write("pkg/agent/pi/testdata/events_stream.jsonl", `{"type":"agent_start"}`+"\n")
	g.git("add", "-A")
	g.git("commit", "-qm", "base: no overlay yet")
	g.git("checkout", "-qb", "head")
	g.write("runtimes/pi/Dockerfile", "ARG PI_VERSION=1.5.2\n")
	g.write("pkg/agent/pi/testdata/events_stream.jsonl", `{"type":"agent_start"}`+"\n"+`{"type":"agent_settled"}`+"\n")
	g.git("add", "-A")
	g.git("commit", "-qm", "head: pin first lands WITH fixtures")
	out, err := g.run(bash, script, "main", "head")
	if err != nil {
		t.Fatalf("gate must pass when the pin first lands; output:\n%s", out)
	}
}

// TestBumpWorkflows_NoWedgeClass pins the review-r0 wedge fix
// structurally: both bump workflows must (a) FORCE-push their
// bot-owned branch (the plain push wedged the opencode twin for 5
// consecutive weekly runs — stranded branch + non-fast-forward
// rejections made the gh pr edit re-run path unreachable), and
// (b) bootstrap their PR label (gh pr create --label exits non-zero
// on a nonexistent label — the wedge's first step). Hermetic by
// design: it reads the workflow files, so the class cannot silently
// return in either twin.
func TestBumpWorkflows_NoWedgeClass(t *testing.T) {
	for _, wf := range []struct{ file, branch, label string }{
		{"../.github/workflows/pi-version-bump.yml", "chore/pi-bump-", "pi-bump"},
		{"../.github/workflows/opencode-version-bump.yml", "chore/opencode-bump-", "opencode-bump"},
	} {
		raw, err := os.ReadFile(wf.file)
		if err != nil {
			t.Fatalf("read %s: %v", wf.file, err)
		}
		src := string(raw)
		if !strings.Contains(src, "git push --force origin \"$BRANCH\"") {
			t.Errorf("%s: bot branch must be force-pushed (the bot owns the branch; a plain push wedges re-runs on a moved main — review r0)", wf.file)
		}
		if strings.Contains(src, "git push origin \"$BRANCH\"\n") {
			t.Errorf("%s: bare non-force push of the bot branch present (the 5-week wedge class)", wf.file)
		}
		if !strings.Contains(src, "gh label create "+wf.label) {
			t.Errorf("%s: must bootstrap label %q before gh pr create --label (nonexistent label exits non-zero and strands the branch)", wf.file, wf.label)
		}
		if !strings.Contains(src, "--label "+wf.label) {
			t.Errorf("%s: expected to attach label %q to its bump PRs", wf.file, wf.label)
		}
	}
}
