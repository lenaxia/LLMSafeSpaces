// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// nightly_dispatch_test.go — structural pins for the nightly e2e
// dispatch architecture: a platform cron trigger (the
// "nightly-e2e-dispatcher" routine in the ops workspace) dispatches
// e2e-nightly.yml on time at 06:00Z because GitHub's own schedule has
// fired 4-6h late (11-run sample); the GitHub schedule STAYS as the
// reliability backup. The concurrency group makes the two entry paths
// mutually exclusive (whichever starts latest wins). The pins assert
// on the PARSED YAML document — substring checks would pass on a
// commented-out block and never prove parseability (r1 review F3).

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type nightlyWorkflowDoc struct {
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress bool   `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
}

func parseNightlyWorkflow(t *testing.T) nightlyWorkflowDoc {
	t.Helper()
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	var doc nightlyWorkflowDoc
	require.NoError(t, yaml.Unmarshal(raw, &doc),
		"e2e-nightly.yml must parse — a syntax break here fails every nightly entry path")
	return doc
}

// nightlyOnBlock returns the raw `on:` mapping — trigger legs are
// asserted by KEY PRESENCE, not value: `workflow_dispatch:` carries no
// value (decodes nil even when present).
func nightlyOnBlock(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../.github/workflows/e2e-nightly.yml")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	onBlock, ok := doc["on"].(map[string]any)
	require.True(t, ok, "the workflow must carry an `on:` trigger block")
	return onBlock
}

// TestE2ENightlyConcurrencyGroup pins the mutual exclusion: ONE shared
// group covering BOTH entry paths (event-specific groups would not
// conflict), latest start wins.
func TestE2ENightlyConcurrencyGroup(t *testing.T) {
	doc := parseNightlyWorkflow(t)
	assert.Equal(t, "e2e-nightly", doc.Concurrency.Group,
		"one shared group for schedule backup + platform dispatch — they must conflict")
	assert.True(t, doc.Concurrency.CancelInProgress,
		"whichever run starts latest wins; the other is canceled instead of overlapping")
}

// TestE2ENightlyScheduleBackupRetained pins the reliability backup and
// the dispatcher's entry path: neither leg of the contract can be
// silently dropped once the platform dispatch becomes primary.
func TestE2ENightlyScheduleBackupRetained(t *testing.T) {
	onBlock := nightlyOnBlock(t)
	_, hasDispatch := onBlock["workflow_dispatch"]
	assert.True(t, hasDispatch, "workflow_dispatch must remain — it is the dispatcher routine's entry path")

	schedule, ok := onBlock["schedule"].([]any)
	require.True(t, ok, "the GitHub schedule leg stays as the reliability backup")
	require.Len(t, schedule, 1, "exactly one schedule entry — the 06:00Z backup slot")
	entry, ok := schedule[0].(map[string]any)
	require.True(t, ok, "schedule entries must be cron mappings")
	assert.Equal(t, "0 6 * * *", entry["cron"], "the 06:00Z backup slot is the contract")
}
