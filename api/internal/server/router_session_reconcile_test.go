// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// #1340: the reconcile pass is piggybacked on the sidebar's session
// list (router.go, beside BackfillSessionParents). This test pins THE
// WIRING: a GET /workspaces/:id/sessions through the production router
// triggers the convergence pass (the harness list call runs). Deleting
// the piggyback call fails this test — the r1 review's
// "zero tests would fail" gap.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
)

// reconcileSpyAdapter lists ONE session and records the call.
type reconcileSpyAdapter struct {
	nullTestAdapter
	mu      sync.Mutex
	calls   int
	present []string
}

// CountMessages overrides the nil-embedded Adapter so the #1481 count
// rebuild (which walks PRESENT sessions) doesn't nil-panic this wiring
// spy — the router piggyback drives the full reconciliation.
func (a *reconcileSpyAdapter) CountMessages(_ context.Context, _, _, _ string) (int, error) {
	return 0, nil
}

func (a *reconcileSpyAdapter) ListSessions(_ context.Context, _, _ string) ([]session.Session, error) {
	a.mu.Lock()
	a.calls++
	present := a.present
	a.mu.Unlock()
	out := make([]session.Session, 0, len(present))
	for _, id := range present {
		out = append(out, session.Session{ID: id})
	}
	return out, nil
}

func (a *reconcileSpyAdapter) listCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// recordingIndex satisfies the session-index surface the reconcile
// path reads (rows) and writes (deletes).
type recordingIndex struct {
	mu      sync.Mutex
	rows    map[string][]types.SessionListItem
	deleted map[string]bool
}

func newRecordingIndex() *recordingIndex {
	return &recordingIndex{rows: map[string][]types.SessionListItem{}, deleted: map[string]bool{}}
}

func (r *recordingIndex) seed(workspaceID, sessionID string, count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[workspaceID] = append(r.rows[workspaceID], types.SessionListItem{ID: sessionID, Title: sessionID, MessageCount: count})
}

func (r *recordingIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (r *recordingIndex) ListByWorkspace(_ context.Context, w string) ([]types.SessionListItem, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]types.SessionListItem{}, r.rows[w]...), nil
}
func (r *recordingIndex) DeleteByWorkspace(_ context.Context, _ string) error { return nil }
func (r *recordingIndex) DeleteSession(_ context.Context, w, s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleted[w+"/"+s] = true
	return nil
}
func (r *recordingIndex) UpsertTitle(_ context.Context, _, _, _ string) error             { return nil }
func (r *recordingIndex) UpsertParent(_ context.Context, _, _, _ string) error            { return nil }
func (r *recordingIndex) UpsertContextUsed(_ context.Context, _, _ string, _ int64) error { return nil }
func (r *recordingIndex) RebuildMessageCount(_ context.Context, _, _ string, _ int) error {
	return nil
}

func (r *recordingIndex) UpdateLastSeen(_ context.Context, _, _ string) error { return nil }
func (r *recordingIndex) Start() error                                        { return nil }
func (r *recordingIndex) Stop() error                                         { return nil }

func TestRouterSessionList_TriggersReconcilePass(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	ws := &imocks.MockWorkspaceService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	ws.On("ResolveWorkspace", mock.Anything, mock.Anything).
		Return(&types.WorkspaceMetadata{ID: "ws-1", UserID: "test-user"}, nil).Maybe()
	ws.On("CheckOwnership", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
			return
		}
		c.Set("userID", "test-user")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	k8sMock := k8smocks.NewMockKubernetesClient()
	k8sMock.On("LlmsafespacesV1").Return(nil, nil).Maybe()
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset()).Maybe()

	spy := &reconcileSpyAdapter{present: []string{"ses_alive"}}
	proxyHandler, err := handlers.NewProxyHandler(k8sMock, log, "default", nil, spy)
	require.NoError(t, err)
	// Pre-set the parent-backfill gate: otherwise BackfillSessionParents
	// ALSO calls adapter.ListSessions on this route and satisfies a
	// call-count assertion with the WRONG path — the r2 mutation finding
	// (the piggyback could be deleted and the old pin still passed).
	proxyHandler.SetParentBackfilledForTest("ws-1")
	idx := newRecordingIndex()
	// A count-zero ghost absent from the harness + prior miss=1 → this
	// pass REAPS (asserting the deletion is the wiring proof: router →
	// handler → wsstate → PlanReconciliation → sessionindex delete).
	idx.seed("ws-1", "ses_ghost", 0)
	idx.seed("ws-1", "ses_alive", 3)
	proxyHandler.SetSessionIndex(idx)
	proxyHandler.SeedReconcileMissesForTest("ws-1", map[string]int{"ses_ghost": 1})
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		[]types.SessionListItem{
			{ID: "ses_ghost", Title: "Ghost", MessageCount: 0},
			{ID: "ses_alive", Title: "Alive", MessageCount: 3},
		}, nil)

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	router := NewRouter(svc, log, proxyHandler, RouterConfig{Debug: false})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	assert.Eventually(t, func() bool { return spy.listCalls() >= 1 }, 3*time.Second, 50*time.Millisecond,
		"the sidebar list must trigger the harness list diff (the router piggyback)")
	assert.Eventually(t, func() bool {
		idx.mu.Lock()
		defer idx.mu.Unlock()
		return idx.deleted["ws-1/ses_ghost"]
	}, 3*time.Second, 50*time.Millisecond,
		"the piggybacked pass must reap the count-zero ghost (deleting router.go's ReconcileSessionIndex call fails this)")
}
