// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package repolint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForbiddenPathsCheck_CleanTree(t *testing.T) {
	dir := t.TempDir()
	rep, err := ForbiddenPathsCheck(dir)
	require.NoError(t, err)
	assert.True(t, rep.OK(), "empty tree has no forbidden paths: %s", rep)
}

func TestForbiddenPathsCheck_DetectsResurrection(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"runtimes/go/Dockerfile", "runtimes/tests/run_tests.sh"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte("FROM scratch"), 0o644))
	}
	rep, err := ForbiddenPathsCheck(dir)
	require.NoError(t, err)
	assert.False(t, rep.OK(), "resurrected per-language runtimes must be flagged")
	assert.Contains(t, rep.Violations, "runtimes/go")
	assert.Contains(t, rep.Violations, "runtimes/tests")
	assert.Contains(t, rep.String(), "c9c68684", "report names the resurrection mechanism")
}

// The live-tree guard: the repository itself must stay clean. This was
// demonstrably red before the #854 deletion and green after (the point of
// the check — a future stale-branch resurrection fails pre-commit/CI).
func TestForbiddenPathsCheck_RepoTree(t *testing.T) {
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	rep, err := ForbiddenPathsCheck(root)
	require.NoError(t, err)
	assert.True(t, rep.OK(), "legacy per-language runtime paths must not exist in the repo: %s", rep)
}
