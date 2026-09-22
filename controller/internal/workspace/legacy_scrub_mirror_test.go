// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// US-72.6 (design 0058 §8): the controller mirror of the one-time
// legacy-key scrub report — statusz's LegacyScrub slice becomes the
// LegacyKeysScrubbed condition, and the EVENT fires exactly once (on
// first observation of a report that removed something or errored);
// re-observations of the static report never re-emit (idempotent).

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/record"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func countEvents(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, reason) {
			n++
		}
	}
	return n
}

// TestCheckAgentHealth_MirrorsLegacyScrub: a report with removals sets
// the condition True with the counts in the message, and emits the
// LegacyKeysScrubbed event EXACTLY once across repeated observations of
// the same static report.
func TestCheckAgentHealth_MirrorsLegacyScrub(t *testing.T) {
	report := &agentd.LegacyScrubHealth{
		RanAt:             1758518400,
		AuthKeysRemoved:   2,
		ConfigKeysRemoved: 1,
	}
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy:     true,
		LegacyScrub: report,
	})
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec

	r.checkAgentHealth(context.Background(), ws)

	cond := conditionOf(ws, v1.WorkspaceConditionLegacyKeysScrubbed)
	require.NotNil(t, cond, "the condition must exist after the first observation")
	assert.Equal(t, "True", cond.Status)
	assert.Contains(t, cond.Message, "auth=2")
	assert.Contains(t, cond.Message, "config=1")

	first := eventsFrom(rec)
	n1 := countEvents(first, "LegacyKeysScrubbed")
	assert.Equal(t, 1, n1, "the event fires on first observation")

	// Re-observe the SAME static report: no re-emit (eventsFrom DRAINS
	// the channel — the second call sees only NEW events; zero new is
	// the idempotency proof), condition stable.
	r.checkAgentHealth(context.Background(), ws)
	second := eventsFrom(rec)
	assert.Equal(t, 0, countEvents(second, "LegacyKeysScrubbed"),
		"the static report never re-emits (idempotent)")
}

// TestCheckAgentHealth_LegacyScrubCleanIsQuiet: an AlreadyClean report
// (removed nothing) sets the condition True/quiet and emits NO event —
// the steady state must not be noisy.
func TestCheckAgentHealth_LegacyScrubCleanIsQuiet(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy: true,
		LegacyScrub: &agentd.LegacyScrubHealth{
			RanAt: 1758518400,
		},
	})
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec

	r.checkAgentHealth(context.Background(), ws)

	cond := conditionOf(ws, v1.WorkspaceConditionLegacyKeysScrubbed)
	require.NotNil(t, cond)
	assert.Equal(t, "True", cond.Status)
	assert.Contains(t, cond.Message, "clean")
	assert.Equal(t, 0, countEvents(eventsFrom(rec), "LegacyKeysScrubbed"),
		"a clean scrub is quiet")
}

// TestCheckAgentHealth_LegacyScrubErrorSurfaces: a scrub error sets the
// condition False with the error string and emits the event once.
func TestCheckAgentHealth_LegacyScrubErrorSurfaces(t *testing.T) {
	r, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{
		Healthy: true,
		LegacyScrub: &agentd.LegacyScrubHealth{
			RanAt: 1758518400,
			Error: "scrub legacy auth.json: unmarshal: unexpected EOF",
		},
	})
	rec := record.NewFakeRecorder(32)
	r.Recorder = rec

	r.checkAgentHealth(context.Background(), ws)

	cond := conditionOf(ws, v1.WorkspaceConditionLegacyKeysScrubbed)
	require.NotNil(t, cond)
	assert.Equal(t, "False", cond.Status)
	assert.Contains(t, cond.Message, "unexpected EOF")
	assert.Equal(t, 1, countEvents(eventsFrom(rec), "LegacyKeysScrubbed"))
}

// TestCheckAgentHealth_NoLegacyScrubSlice: a flag-off pod (nil slice)
// never sets the condition.
func TestCheckAgentHealth_NoLegacyScrubSlice(t *testing.T) {
	_, ws, _ := setupSpawnEnvHealthTest(t, agentd.HealthzResponse{Healthy: true})

	cond := conditionOf(ws, v1.WorkspaceConditionLegacyKeysScrubbed)
	assert.Nil(t, cond, "no slice, no condition — flag-off pods are untouched")
}
