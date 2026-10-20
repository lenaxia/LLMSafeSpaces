//go:build integration

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationHarness_ChildHonorsSyntheticDataHome — a BEHAVIORAL
// pin on the harness env construction (salvage of #1764 over the
// merged #1762 base): the booted child must honor the harness's
// synthetic XDG_DATA_HOME, whatever the runner inherited. This catches
// any future regression that stops delivering the overrides (a
// reordered strip/append, a dropped key). It is NOT the red-first
// proof of the strip: on the pinned 1.18.15 the child honors appended
// overrides under BOTH the strip and the append-only shapes (the build
// resolves duplicate env entries last-match — see the harness comment);
// the strip is deterministic-env hygiene, not an observed-leak fix.
// Observable proof: the child's structured log lands under the XDG
// data home it actually honored.
func TestIntegrationHarness_ChildHonorsSyntheticDataHome(t *testing.T) {
	srv := StartIntegrationServer(t, 0, legacyIntegrationConfig)
	t.Cleanup(func() { srv.cancel() })

	// The child writes opencode.log under <XDG_DATA_HOME>/opencode/log
	// within seconds of boot. Bounded wait, then assert the log exists
	// under the HARNESS's data dir — proving the synthetic value won.
	logDir := filepath.Join(srv.dataDir, "opencode", "log")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(logDir); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child's structured log never appeared under the harness data dir %s — the child did not honor the synthetic XDG_DATA_HOME (an inherited value won by first-match)", logDir)
		}
		time.Sleep(200 * time.Millisecond)
	}

	entries, err := os.ReadDir(logDir)
	require.NoError(t, err)
	assert.NotEmpty(t, entries, "the log directory must contain the child's log")
}
