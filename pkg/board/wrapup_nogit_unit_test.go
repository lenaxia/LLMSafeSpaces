// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoGitExecutionInBoardPkg — D5's engine-side pin: the engine NEVER
// runs git. The mint renders a bounded window (the RFC 3339 pair + the
// covered story list) into the charter; the REVIEWER computes the aggregate
// diff in the project's target workspace, where the repo lives. This pins
// that no non-test source file under pkg/board imports os/exec or invokes
// exec.Command (the charter's "git log …" instruction STRING is data the
// reviewer reads, not code the engine runs).
func TestNoGitExecutionInBoardPkg(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "locate the test's own source file")
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "list pkg/board sources")

	checked := 0
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		require.NoError(t, err)
		src := string(raw)
		require.NotContains(t, src, "os/exec",
			"%s: pkg/board never shells out — the reviewer computes the diff (D5)", ent.Name())
		require.NotContains(t, src, "exec.Command",
			"%s: no subprocess execution anywhere in the board engine (D5)", ent.Name())
		checked++
	}
	require.Greater(t, checked, 0, "the scan saw sources (guard against a silently-empty directory)")
}
