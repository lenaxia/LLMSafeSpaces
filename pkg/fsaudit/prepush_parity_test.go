// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package fsaudit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// prepushHookPath is the runtime-image artifact under test: the git
// template-dir pre-push hook that gives every NEW clone in a workspace
// the #1808 all-NUL-blob guard.
var prepushHookPath = filepath.Join("..", "..", "runtimes", "base", "git-templates", "hooks", "pre-push")

// dockerfilePath locates the Dockerfile for content wiring pins.
var dockerfilePath = filepath.Join("..", "..", "runtimes", "base", "Dockerfile")

// PIN (artifact posture): the hook ships in-tree, is valid shell
// syntax, and is executable — a non-executable template hook is
// silently inert (git skips it), which is exactly the failure mode
// this lane exists to prevent.
func TestPrePushHook_ArtifactPresentAndValidShell(t *testing.T) {
	fi, err := os.Stat(prepushHookPath)
	if err != nil {
		t.Fatalf("pre-push hook missing from the runtime tree: %v", err)
	}
	if fi.Mode()&0o111 == 0 {
		t.Fatalf("pre-push hook must be executable (mode %s), or git silently ignores it", fi.Mode())
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH (syntax check)")
	}
	if out, err := exec.Command(bash, "-n", prepushHookPath).CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, out)
	}
}

// PIN (kill-switch + fail-open markers present): the hook must carry
// the documented env kill-switch (LLMSAFESPACE_PREPUSH_AUDIT=0) and a
// fail-open posture marker — the never-worse doctrine is the review
// bar for this surface.
func TestPrePushHook_DoctrineMarkers(t *testing.T) {
	raw, err := os.ReadFile(prepushHookPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, marker := range []string{
		"LLMSAFESPACE_PREPUSH_AUDIT",
		"fail open", // doctrine comment + behavior
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("hook missing %q (doctrine regression)", marker)
		}
	}
}

// PIN (template wiring): the Dockerfile installs the hook into git's
// template dir under hooks/ so `git init`/`git clone` activate it —
// dropping the file in-tree without the image wiring would be inert.
func TestPrePushHook_DockerfileTemplateWiring(t *testing.T) {
	raw, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, "git-templates") {
		t.Fatal("Dockerfile does not reference git-templates (hook not installed into the image)")
	}
	if !strings.Contains(body, "/usr/share/git-core/templates") {
		t.Fatal("Dockerfile does not target git's template dir /usr/share/git-core/templates")
	}
}

// --- behavioral pins (real git where available) ---------------------------
//
// pushFixture builds a repo + bare remote with the hook installed the
// way the runtime image installs it (template dir), and returns a push
// func.
func pushFixture(t *testing.T) (push func(commitMsg string, deadFile bool) error) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH (behavioral hook pins)")
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	remote := filepath.Join(dir, "remote.git")

	templates := filepath.Join(dir, "templates", "hooks")
	if err := os.MkdirAll(templates, 0o755); err != nil {
		t.Fatal(err)
	}
	// Install exactly as the image does (copy, keep exec bit).
	hookRaw, err := os.ReadFile(prepushHookPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templates, "pre-push"), hookRaw, 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(git, args...)
		return cmd.CombinedOutput()
	}
	gitEnv := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "no-global.cfg"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME="+dir,
	)
	gitIn := func(cwd string, args ...string) ([]byte, error) {
		cmd := exec.Command(git, args...)
		cmd.Dir = cwd
		cmd.Env = gitEnv
		return cmd.CombinedOutput()
	}
	_ = run // quiet linter

	if out, err := gitIn(dir, "init", "--bare", remote); err != nil {
		t.Fatalf("bare remote init: %v\n%s", err, out)
	}
	if out, err := gitIn(dir, "-c", "init.templateDir="+filepath.Join(dir, "templates"), "init", work); err != nil {
		t.Fatalf("work init: %v\n%s", err, out)
	}
	// Template-dir activation: the hook must now be live in the clone.
	if _, err := os.Stat(filepath.Join(work, ".git", "hooks", "pre-push")); err != nil {
		t.Fatalf("init.templateDir did not activate the hook: %v", err)
	}
	if out, err := gitIn(work, "config", "user.email", "t@example.invalid"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := gitIn(work, "config", "user.name", "t"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}

	return func(commitMsg string, deadFile bool) error {
		if deadFile {
			if err := os.WriteFile(filepath.Join(work, "managed.yaml"), make([]byte, 4096), 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(filepath.Join(work, "managed.yaml"), []byte("# pin v1.2.3\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if out, err := gitIn(work, "add", "-A"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if out, err := gitIn(work, "commit", "-m", commitMsg); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		out, err := gitIn(work, "push", "file://"+remote, "HEAD:refs/heads/main")
		if err != nil {
			t.Logf("push output:\n%s", out)
		}
		return err
	}
}

// PIN (fail-closed on detection — the #1808 incident shape): a push
// whose new range contains an all-NUL blob MUST be rejected by the
// hook. This is the class that reached the remote in the incident.
func TestPrePushHook_BlocksAllNulBlobPush(t *testing.T) {
	push := pushFixture(t)
	if err := push("first clean commit", false); err != nil {
		t.Fatalf("clean seed push must succeed: %v", err)
	}
	if err := push("second commit", true); err == nil {
		t.Fatal("push containing an all-NUL blob must FAIL (fail-closed on positive detection)")
	}
}

// PIN (never block clean work): an ordinary push passes untouched.
func TestPrePushHook_CleanPushPasses(t *testing.T) {
	push := pushFixture(t)
	if err := push("only clean content", false); err != nil {
		t.Fatalf("clean push must pass: %v", err)
	}
}

// PIN (kill-switch): LLMSAFESPACE_PREPUSH_AUDIT=0 disables the guard —
// the documented escape hatch for users with legitimate all-NUL files.
// Direct invocation with the env set and a would-be-dead range on
// stdin: the hook must exit 0 immediately, without invoking git at all.
func TestPrePushHook_KillSwitchDisablesGuard(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	cmd := exec.Command(prepushHookPath)
	cmd.Env = append(os.Environ(), "LLMSAFESPACE_PREPUSH_AUDIT=0")
	cmd.Stdin = strings.NewReader("refs/heads/main " + strings.Repeat("a", 40) + " refs/heads/main " + strings.Repeat("0", 40) + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("kill-switched hook must exit 0, got %v (%s)", err, out)
	}
}

// PIN (fail-open on tooling failure): when git is UNAVAILABLE (the
// scanner), the hook must let the push proceed (exit 0) — a broken
// guard must never block legitimate work. Never-worse doctrine, both
// directions pinned. (A missing bash INTERPRETER is pre-script
// territory — exit 127 before any doctrine line runs — and outside the
// guard's contract; bash is a runtime-image base package.)
func TestPrePushHook_FailsOpenWhenGitUnavailable(t *testing.T) {
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not on PATH")
	}
	// PATH with bash ONLY: env resolves the shebang, the script runs,
	// and its `command -v git` fail-open line is what executes.
	minPath := t.TempDir()
	if err := os.Symlink(bashPath, filepath.Join(minPath, "bash")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(prepushHookPath)
	cmd.Env = []string{"PATH=" + minPath, "HOME=" + t.TempDir()}
	cmd.Stdin = strings.NewReader("refs/heads/main " + strings.Repeat("a", 40) + " refs/heads/main " + strings.Repeat("0", 40) + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("hook must fail OPEN when git is unavailable, got %v (%s)", err, out)
	}
}
