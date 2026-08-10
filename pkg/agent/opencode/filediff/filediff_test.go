// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package filediff

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// US-65.3 prototype — design/0049 §10 open question: can `git diff`
// produce the unified-diff text pkg/session.FileDiff requires?
//
// These tests build a real git repo in t.TempDir(), commit a baseline,
// modify files, and verify DiffFiles produces the expected patches.
// No mocks — git is the source of truth for unified-diff format.

// gitRepo is a test fixture: a directory initialized as a git repo
// with one initial commit containing the supplied files.
type gitRepo struct {
	t      *testing.T
	root   string
	hasGit bool
}

func newGitRepo(t *testing.T) *gitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available — skipping filediff integration test")
	}
	root := t.TempDir()

	// Configure a deterministic git identity so commits succeed without
	// inheriting host git config (CI environments often lack user.email).
	runGit(t, root, "init", "--initial-branch=main")
	runGit(t, root, "config", "user.email", "test@example.com")
	runGit(t, root, "config", "user.name", "Test")

	return &gitRepo{t: t, root: root, hasGit: true}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func (g *gitRepo) writeCommit(msg string, files map[string]string) {
	g.t.Helper()
	for path, content := range files {
		full := filepath.Join(g.root, path)
		require.NoError(g.t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(g.t, os.WriteFile(full, []byte(content), 0o644))
	}
	runGit(g.t, g.root, append([]string{"add", "."}, keys(files)...)...)
	runGit(g.t, g.root, "commit", "-m", msg)
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (g *gitRepo) modify(files map[string]string) {
	g.t.Helper()
	for path, content := range files {
		full := filepath.Join(g.root, path)
		require.NoError(g.t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(g.t, os.WriteFile(full, []byte(content), 0o644))
	}
	// NOTE: deliberately NOT committing — DiffFiles diffs against
	// HEAD so uncommitted changes are what we measure.
}

func TestDiffFiles_NoChanges_ReturnsEmpty(t *testing.T) {
	repo := newGitRepo(t)
	repo.writeCommit("baseline", map[string]string{
		"foo.txt": "hello\n",
	})

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"foo.txt"})
	require.NoError(t, err)
	assert.Empty(t, diffs, "no changes since HEAD → no diffs")
}

func TestDiffFiles_ModifiedFile_ProducesUnifiedDiff(t *testing.T) {
	repo := newGitRepo(t)
	repo.writeCommit("baseline", map[string]string{
		"foo.txt": "line1\nline2\nline3\n",
	})
	repo.modify(map[string]string{
		"foo.txt": "line1\nCHANGED\nline3\n",
	})

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"foo.txt"})
	require.NoError(t, err)
	require.Len(t, diffs, 1)

	d := diffs[0]
	assert.Equal(t, "foo.txt", d.Path)
	// The patch must be valid unified-diff: contain the markers and
	// the removed/added lines.
	assert.Contains(t, d.Patch, "diff --git a/foo.txt b/foo.txt")
	assert.Contains(t, d.Patch, "--- a/foo.txt")
	assert.Contains(t, d.Patch, "+++ b/foo.txt")
	assert.Contains(t, d.Patch, "-line2")
	assert.Contains(t, d.Patch, "+CHANGED")
	// Re-applying the patch with `git apply --check` must succeed —
	// verifies the patch is valid unified-diff.
	require.NoError(t, applyCheck(t, repo.root, d.Patch),
		"patch must be valid (git apply --check must succeed)")
}

func TestDiffFiles_MultipleFiles_ReturnsOneDiffPerChangedFile(t *testing.T) {
	repo := newGitRepo(t)
	repo.writeCommit("baseline", map[string]string{
		"a.txt": "aaa\n",
		"b.txt": "bbb\n",
		"c.txt": "ccc\n",
	})
	repo.modify(map[string]string{
		"a.txt": "AAA\n",
		"c.txt": "CCC\n",
		// b.txt unchanged
	})

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"a.txt", "b.txt", "c.txt"})
	require.NoError(t, err)
	require.Len(t, diffs, 2, "two changed files (a, c); b is unchanged → not in result")

	paths := map[string]bool{}
	for _, d := range diffs {
		paths[d.Path] = true
	}
	assert.True(t, paths["a.txt"])
	assert.True(t, paths["c.txt"])
	assert.False(t, paths["b.txt"], "unchanged file must not appear")
}

func TestDiffFiles_NewFileNotInHEAD_ProducesDiffWithEmptyOldSide(t *testing.T) {
	// A file added since HEAD (not yet committed) is a valid case:
	// opencode created a new file. `git diff HEAD -- new.txt` shows
	// the entire file content as additions.
	repo := newGitRepo(t)
	repo.writeCommit("baseline", map[string]string{
		"existing.txt": "old\n",
	})
	repo.modify(map[string]string{
		"new.txt": "fresh content\nline 2\n",
	})

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"new.txt"})
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	d := diffs[0]
	assert.Equal(t, "new.txt", d.Path)
	assert.Contains(t, d.Patch, "--- /dev/null")
	assert.Contains(t, d.Patch, "+++ b/new.txt")
	assert.Contains(t, d.Patch, "+fresh content")
}

func TestDiffFiles_DeletedFile_ProducesDiffWithEmptyNewSide(t *testing.T) {
	repo := newGitRepo(t)
	repo.writeCommit("baseline", map[string]string{
		"doomed.txt": "to be removed\n",
		"keep.txt":   "stay\n",
	})
	require.NoError(t, os.Remove(filepath.Join(repo.root, "doomed.txt")))

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"doomed.txt"})
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	d := diffs[0]
	assert.Equal(t, "doomed.txt", d.Path)
	assert.Contains(t, d.Patch, "--- a/doomed.txt")
	assert.Contains(t, d.Patch, "+++ /dev/null")
	assert.Contains(t, d.Patch, "-to be removed")
}

func TestDiffFiles_BinaryFile_HasNoHunkMarkers(t *testing.T) {
	repo := newGitRepo(t)
	repo.writeCommit("baseline", map[string]string{
		"bin.dat": "\x00\x01\x02 binary",
	})
	repo.modify(map[string]string{
		"bin.dat": "\x00\x03\x04 binary changed",
	})

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"bin.dat"})
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	d := diffs[0]
	assert.Equal(t, "bin.dat", d.Path)
	// Binary files emit "Binary files a/... and b/... differ" instead
	// of @@ hunks. Patch is non-empty (header + binary marker line)
	// but contains no @@ hunks.
	assert.NotContains(t, d.Patch, "@@", "binary file diff must not have hunk markers")
	assert.Contains(t, d.Patch, "Binary files")
}

func TestDiffFiles_SubdirectoryPath_NormalizedToRepoRelative(t *testing.T) {
	repo := newGitRepo(t)
	require.NoError(t, os.MkdirAll(filepath.Join(repo.root, "src", "pkg"), 0o755))
	repo.writeCommit("baseline", map[string]string{
		"src/pkg/foo.go": "package pkg\n",
	})
	repo.modify(map[string]string{
		"src/pkg/foo.go": "package pkg // modified\n",
	})

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"src/pkg/foo.go"})
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	d := diffs[0]
	assert.Equal(t, "src/pkg/foo.go", d.Path, "Path must be repo-relative")
	assert.Contains(t, d.Patch, "diff --git a/src/pkg/foo.go b/src/pkg/foo.go")
}

func TestDiffFiles_PathWithSpaces_ExtractedCorrectly(t *testing.T) {
	// Files with spaces in the name: git's default core.quotePath
	// quotes paths with "unusual" characters, but spaces are NOT
	// quoted — they appear raw in the diff header. Our parser must
	// handle the literal "a/My File.txt b/My File.txt" form.
	repo := newGitRepo(t)
	repo.writeCommit("baseline", map[string]string{
		"My File.txt": "v1\n",
	})
	repo.modify(map[string]string{
		"My File.txt": "v2\n",
	})

	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), []string{"My File.txt"})
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	assert.Equal(t, "My File.txt", diffs[0].Path)
}

func TestDiffFiles_EmptyFileList_NoGitInvocation(t *testing.T) {
	repo := newGitRepo(t)
	p, err := NewProducer(repo.root)
	require.NoError(t, err)

	diffs, err := p.DiffFiles(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, diffs)
}

func TestDiffFiles_GitMissing_ReturnsError(t *testing.T) {
	// Pointing at a non-repo directory must surface a clear error
	// (not a silent empty result that hides the misconfiguration).
	dir := t.TempDir()
	p, err := NewProducer(dir)
	require.NoError(t, err)

	_, err = p.DiffFiles(context.Background(), []string{"any.txt"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git diff HEAD failed")
}

func TestNewProducer_RejectsRelativePath(t *testing.T) {
	_, err := NewProducer("relative/path")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be absolute")
}

func TestNewProducer_AcceptsAbsolutePath(t *testing.T) {
	p, err := NewProducer(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, p)
}

// applyCheck runs `git apply --check` on the supplied patch to verify
// it is valid unified-diff that git can apply. The patch is applied
// against the BASELINE state (before the modifications that produced
// it), so we have to revert the modification first.
func applyCheck(t *testing.T, repoRoot, patch string) error {
	t.Helper()
	// Reset working tree to HEAD (drop the modifications that
	// produced the patch). The patch then describes the change from
	// HEAD to the modified state, and `git apply --check` verifies
	// it can reproduce that change cleanly.
	runGit(t, repoRoot, "reset", "--hard", "HEAD")

	cmd := exec.Command("git", "-C", repoRoot, "apply", "--check", "-")
	cmd.Stdin = strings.NewReader(patch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("git apply --check output: %s", out)
	}
	return err
}
