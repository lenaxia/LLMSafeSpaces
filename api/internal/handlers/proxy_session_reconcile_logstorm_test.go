// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// proxy_session_reconcile_logstorm_test.go — the per-row reconcile Warn
// emission is aggregated to ONE warn per pass per class.
//
// Production incident (2026-11-09): after an API restart, the first
// listing request per workspace fired a reconcile pass that emitted one
// Warn line PER kept row (kept_for_operator) and one PER list-invisible
// count-zero row (count_skip_not_present) — hundreds of lines per
// workspace per 30s cadence, the log/memory noise of the boot window.
// Per-row OUTCOME METRICS are unchanged (they carry the diagnosis); only
// the log volume is aggregated.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sessionindex "github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// reconcileWarnCapture records Warn calls so log-volume assertions are possible.
type reconcileWarnCapture struct {
	testLogger
	warns []string
}

func (l *reconcileWarnCapture) Warn(msg string, kv ...interface{}) {
	var b strings.Builder
	b.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		fmt.Fprintf(&b, "|%v=%v", kv[i], kv[i+1])
	}
	l.warns = append(l.warns, b.String())
}

var _ pkginterfaces.LoggerInterface = (*reconcileWarnCapture)(nil)

// captureReconcileHandler wires the ghost-scenario handler with a
// capturing logger: harness lists NO sessions, counts walkable.
func captureReconcileHandler(t *testing.T) (*ProxyHandler, *reconcileWarnCapture, *mockSessionIndex) {
	t.Helper()
	idx := newMockSessionIndex()
	h := newProxyHandlerForAdapterTest(t)
	h.sessionIndex = idx
	h.adapter = &mockAdapter{
		listSessionsFn: func(_ context.Context, _, _ string) ([]session.Session, error) {
			return nil, nil
		},
		countMessagesFn: func(_ context.Context, _, _, _ string) (int, error) {
			return 0, nil
		},
	}
	cl := &reconcileWarnCapture{}
	h.logger = cl
	return h, cl, idx
}

// seedGhostRows appends n history-bearing rows (absent at harness, aged
// past the spawn grace) directly into the index mock.
func seedGhostRows(t *testing.T, idx *mockSessionIndex, workspaceID string, n int) {
	t.Helper()
	aged := time.Now().UTC().Add(-2 * time.Hour)
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ses_ghost_%02d", i)
		idx.rows[workspaceID] = append(idx.rows[workspaceID], types.SessionListItem{
			ID: id, Title: id, InternalID: id, MessageCount: 10 + i, LastMessageAt: &aged,
		})
	}
}

func TestSessionIndexReconcile_LogStormAggregated(t *testing.T) {
	h, cl, idx := captureReconcileHandler(t)
	const n = 4
	seedGhostRows(t, idx, "ws-1", n)

	before := testutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator"))
	// kept_for_operator is a THRESHOLD outcome: two consecutive absences
	// (reconcileMissThreshold) put a history-bearing row in the operator
	// population. Drive both passes.
	runReconcile(t, h, "ws-1")
	runReconcile(t, h, "ws-1")

	after := testutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("kept_for_operator"))
	assert.Equal(t, before+float64(n), after, "per-row outcome metrics unchanged")

	var stormLines []string
	for _, w := range cl.warns {
		if strings.Contains(w, "history-bearing rows absent at harness") {
			stormLines = append(stormLines, w)
		}
	}
	require.Len(t, stormLines, 1, "one aggregated warn per pass, got %d: %v", len(stormLines), cl.warns)
	assert.Contains(t, stormLines[0], "count=4", "aggregated warn carries the row count")
}

func TestSessionIndexReconcile_LogStormNoRowsNoWarn(t *testing.T) {
	h, cl, _ := captureReconcileHandler(t)
	runReconcile(t, h, "ws-1")
	for _, w := range cl.warns {
		assert.NotContains(t, w, "history-bearing rows absent at harness")
	}
}

func TestSessionIndexReconcile_CountSkipStormAggregated(t *testing.T) {
	h, cl, idx := captureReconcileHandler(t)
	// Count-zero rows absent from the harness list: the #1452
	// list-invisible class — each used to emit its own Warn.
	const n = 3
	aged := time.Now().UTC().Add(-2 * time.Hour)
	idx.mu.Lock()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("ses_invisible_%02d", i)
		idx.rows["ws-1"] = append(idx.rows["ws-1"], types.SessionListItem{
			ID: id, Title: id, InternalID: id, MessageCount: 0, LastMessageAt: nil,
			CreatedAt: &aged,
		})
	}
	idx.mu.Unlock()

	before := testutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_skip_not_present"))
	runReconcile(t, h, "ws-1")

	after := testutil.ToFloat64(sessionindex.ReconcileOutcomeForTest("count_skip_not_present"))
	assert.Equal(t, before+float64(n), after, "per-row outcome metrics unchanged")

	var stormLines []string
	for _, w := range cl.warns {
		if strings.Contains(w, "list-invisible count-zero row") {
			stormLines = append(stormLines, w)
		}
	}
	require.Len(t, stormLines, 1, "one aggregated warn per pass, got %d: %v", len(stormLines), cl.warns)
	assert.Contains(t, stormLines[0], "count=3")
}
