// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package local_test

// nightly_dispatch_test.go — structural pins for the nightly e2e
// schedule contract: the 2:17am Pacific (09:17 UTC) off-peak
// odd-minute slot (the top-of-hour 06:00 UTC was a documented
// high-contention window where this
// workflow's scheduled runs fired 4-6h late — 11-run sample; the
// platform-trigger dispatch experiment that briefly ran here was
// decommissioned in favor of the schedule fix). The concurrency group
// stays as source-agnostic double-run protection (schedule, manual
// dispatch, any future dispatch source — whichever starts latest
// wins). The pins assert on the PARSED YAML document — substring
// checks would pass on a commented-out block and never prove
// parseability (#1485 r1 review F3).

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
		"one shared group across every entry path (schedule, manual dispatch) — they must conflict")
	assert.True(t, doc.Concurrency.CancelInProgress,
		"whichever run starts latest wins; the other is canceled instead of overlapping")
}

// TestE2ENightlyScheduleSlotRetained pins the schedule slot and the
// manual entry path: neither leg of the contract can be silently
// changed or dropped.
func TestE2ENightlyScheduleSlotRetained(t *testing.T) {
	onBlock := nightlyOnBlock(t)
	_, hasDispatch := onBlock["workflow_dispatch"]
	assert.True(t, hasDispatch, "workflow_dispatch must remain — the manual entry path")

	schedule, ok := onBlock["schedule"].([]any)
	require.True(t, ok, "the schedule leg is the nightly's primary trigger — it must stay")
	require.Len(t, schedule, 1, "exactly one schedule entry — the off-peak slot")
	entry, ok := schedule[0].(map[string]any)
	require.True(t, ok, "schedule entries must be cron mappings")
	assert.Equal(t, "17 9 * * *", entry["cron"], "the 2:17am Pacific (09:17 UTC) odd-minute off-peak slot is the contract (top-of-hour slots are high-contention)")
}
