// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// nightly_dispatch_test.go — structural pins for the nightly e2e
// dispatch architecture: a platform cron trigger (the
// "nightly-e2e-dispatcher" routine in the ops workspace) dispatches
// e2e-nightly.yml on time at 06:00Z because GitHub's own schedule has
// fired 4-6h late (11-run sample); the GitHub schedule STAYS as the
// reliability backup. The concurrency group makes the two entry paths
// mutually exclusive (whichever starts latest wins); these pins hold
// the three legs of that contract so none can rot silently.

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2ENightlyConcurrencyGroup(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)

	assert.Contains(t, src, "concurrency:", "the nightly must carry a concurrency group (schedule backup + platform dispatch must never double-run)")
	assert.Contains(t, src, "group: e2e-nightly", "one shared group for BOTH entry paths (event-specific groups would not conflict)")
	assert.Contains(t, src, "cancel-in-progress: true", "whichever run starts latest wins; the other is cancelled instead of overlapping")
}

// TestE2ENightlyScheduleBackupRetained pins the reliability backup: the
// GitHub schedule leg of the contract must not be removed when the
// platform dispatch becomes the primary path.
func TestE2ENightlyScheduleBackupRetained(t *testing.T) {
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	src := string(raw)

	assert.Contains(t, src, `cron: "0 6 * * *"`, "the 06:00Z GitHub schedule stays as the reliability backup")
	assert.Contains(t, src, "workflow_dispatch:", "the dispatcher's entry path (workflow_dispatch) must remain")
}
