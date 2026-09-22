// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLegacyScrubTracker (r1 missing-test): nil before run; runOnce is
// exactly-once even under concurrent invocation; a scrub error is
// recorded loudly in the report.
func TestLegacyScrubTracker(t *testing.T) {
	root := t.TempDir()
	authPath := authPathUnder(root)
	require.NoError(t, mkdirAllFor(authPath))
	require.NoError(t, writeJSONFile(authPath, map[string]any{
		"p": map[string]any{"type": "api", "key": "sk-X"},
	}))

	tr := newLegacyScrubTracker(root)
	assert.Nil(t, tr.snapshot(), "nil before the first Present observation")

	// Concurrent invocation: every racer calls runOnce; the sync.Once
	// must collapse them to exactly one scrub (the comment's claim, now
	// actually exercised).
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); tr.runOnce() }()
	}
	wg.Wait()
	rep := tr.snapshot()
	require.NotNil(t, rep)
	assert.Equal(t, 1, rep.AuthKeysRemoved)
	assert.NotZero(t, rep.RanAt)
	assert.Empty(t, rep.Error)
}

func TestLegacyScrubTracker_RecordsError(t *testing.T) {
	root := t.TempDir()
	authPath := authPathUnder(root)
	require.NoError(t, mkdirAllFor(authPath))
	require.NoError(t, writeRaw(authPath, `{not-json`))

	tr := newLegacyScrubTracker(root)
	tr.runOnce()
	rep := tr.snapshot()
	require.NotNil(t, rep)
	assert.Contains(t, rep.Error, "scrub legacy auth.json")
}
