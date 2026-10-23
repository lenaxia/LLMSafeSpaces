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

// sha256ZeroSha is git's null object id in SHA-256 repos (64 zeros).
const sha256ZeroSha = "0000000000000000000000000000000000000000000000000000000000000000"

// PIN (the SHA-256 zero-sha arm, r3 F2 — revert of this arm was
// mutation-proven suite-invisible): a NEW-BRANCH push line whose
// remote sha is the SHA-256 null id must be recognized as
// new-branch (scan everything reachable from local sha). With a dead
// blob committed at HEAD, the hook must REFUSE — verified by OUTPUT
// CONTENT ("REFUSED"), never by a bare non-zero exit (a relative-path
// exec failure would satisfy that vacuously; the hook path is
// absolutized for the cmd.Dir execution).
func TestPrePushHook_Sha256NewBranchRangeScans(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH (SHA-256 arm pin)")
	}
	hookAbs, err := filepath.Abs(prepushHookPath)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	gitEnv := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "no-global.cfg"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME="+dir,
	)
	gitIn := func(args ...string) ([]byte, error) {
		cmd := exec.Command(gitPath, args...)
		cmd.Dir = work
		cmd.Env = gitEnv
		return cmd.CombinedOutput()
	}
	if out, err := exec.Command(gitPath, "init", work).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if out, err := gitIn("config", "user.email", "t@example.invalid"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := gitIn("config", "user.name", "t"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	// A dead blob AT HEAD: the new-branch range must reach it.
	if err := os.WriteFile(filepath.Join(work, "dead.yaml"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := gitIn("add", "-A"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := gitIn("commit", "-m", "dead"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	shaOut, err := gitIn("rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("%v\n%s", err, shaOut)
	}
	head := strings.TrimSpace(string(shaOut))

	cmd := exec.Command(hookAbs)
	cmd.Dir = work
	cmd.Env = gitEnv
	// local=HEAD, remote=SHA-256 null id → new-branch shape.
	cmd.Stdin = strings.NewReader("refs/heads/main " + head + " refs/heads/main " + sha256ZeroSha + "\n")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("SHA-256 new-branch range with a dead blob at HEAD must REFUSE (guard silently inert otherwise — r3 F2); got exit 0 (%s)", out)
	}
	if !strings.Contains(string(out), "REFUSED") {
		t.Fatalf("refusal must be the hook's verdict output, not an incidental exec failure: err=%v out=%s", err, out)
	}
}

// PIN (behavioral, SHA-256 object-format repo end-to-end — r3 F2's
// ideal): the whole guard works in a `git init --object-format=sha256`
// world. The DEAD-BLOB push is the FIRST push (new-branch shape:
// remote sha = the 64-zero SHA-256 null id) precisely so the zero-sha
// arm is LOAD-BEARING — with the arm removed, the invalid range makes
// the guard fail-open and the dead push SUCCEEDS (pin RED). The clean
// repo then proves first-push of clean content passes. Skipped
// gracefully where the local git predates --object-format.
func TestPrePushHook_Sha256ObjectFormatRepoPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH (SHA-256 repo pin)")
	}
	dir := t.TempDir()
	templates := filepath.Join(dir, "templates") // the template DIR (git copies its contents: hooks/, ...)
	if err := os.MkdirAll(filepath.Join(templates, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	hookRaw, err := os.ReadFile(prepushHookPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(templates, "hooks", "pre-push"), hookRaw, 0o755); err != nil {
		t.Fatal(err)
	}

	gitEnv := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+filepath.Join(dir, "no-global.cfg"),
		"GIT_CONFIG_SYSTEM=/dev/null",
		"HOME="+dir,
	)
	gitIn := func(cwd string, args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = gitEnv
		return cmd.CombinedOutput()
	}
	// sha256Repo builds a fresh clone-style work repo + EMPTY bare
	// remote (every push is the new-branch/Z64 shape). Skips (once)
	// when the local git lacks --object-format.
	supportsSha256 := false
	sha256Repo := func(t *testing.T) (work string, push func(content []byte) error) {
		t.Helper()
		name := t.Name()
		work = filepath.Join(dir, name+"-work")
		remote := filepath.Join(dir, name+"-remote.git")
		if !supportsSha256 {
			if out, err := gitIn(dir, "init", "--object-format=sha256", "--bare", remote); err != nil {
				t.Skipf("git lacks --object-format=sha256 (SHA-256 behavioral pin): %v\n%s", err, out)
			}
			supportsSha256 = true
		} else if out, err := gitIn(dir, "init", "--object-format=sha256", "--bare", remote); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if out, err := gitIn(dir, "-c", "init.templateDir="+templates, "init", "--object-format=sha256", work); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if _, err := os.Stat(filepath.Join(work, ".git", "hooks", "pre-push")); err != nil {
			t.Fatalf("init.templateDir did not activate the hook in the SHA-256 repo: %v", err)
		}
		if out, err := gitIn(work, "config", "user.email", "t@example.invalid"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		if out, err := gitIn(work, "config", "user.name", "t"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return work, func(content []byte) error {
			if err := os.WriteFile(filepath.Join(work, "managed.yaml"), content, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := gitIn(work, "add", "-A"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if out, err := gitIn(work, "commit", "-m", "c"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			out, err := gitIn(work, "push", "file://"+remote, "HEAD:refs/heads/main")
			if err != nil {
				t.Logf("push output:\n%s", out)
			}
			return err
		}
	}

	// The dead blob goes FIRST — the new-branch (Z64 remote) path is
	// the arm under pin.
	t.Run("dead first push refused", func(t *testing.T) {
		_, push := sha256Repo(t)
		if err := push(make([]byte, 4096)); err == nil {
			t.Fatal("dead-blob FIRST push in a SHA-256 repo must be REFUSED (the 64-zero arm is load-bearing here — without it the range is invalid and the guard fails open)")
		}
	})
	t.Run("clean first push passes", func(t *testing.T) {
		_, push := sha256Repo(t)
		if err := push([]byte("# pin v1.2.3\n")); err != nil {
			t.Fatalf("clean FIRST push in a SHA-256 repo must pass: %v", err)
		}
	})
}
