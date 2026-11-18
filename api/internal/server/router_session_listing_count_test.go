// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// #1880 direction 3 — the WIRING pins, updated for the listing-latency
// async-walk redesign: the sidebar's session list route DISPATCHES the
// harness count walk for the drift class (detached from the request —
// the tests pin the dispatch seam inline via SetCountWalkDispatchForTest
// so the walk attempt and the index convergence are deterministic). The
// WIRE serves the index value on this pass; the walked truth lands in
// the index through the rebuild seam for the next poll. Deleting the
// router's ScheduleHarnessMessageCounts piggyback reddens these tests
// even with every handler unit green (the
// TestRouterSessionList_TriggersReconcilePass precedent). The r2
// unhappy-path twin pins the failure contract at the wiring tier: a
// walk error must leave the INDEX value on the wire (never worse than
// the pre-fix listing), never a 5xx.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
)

// listingCountSpyAdapter walks drift sessions' counts and records the
// calls. walkErr (r2) simulates the harness error leg at the wiring
// tier.
type listingCountSpyAdapter struct {
	nullTestAdapter
	mu       sync.Mutex
	walked   []string
	countFor map[string]int
	walkErr  error
}

func (a *listingCountSpyAdapter) CountMessages(_ context.Context, _, _, sessionID string) (int, error) {
	a.mu.Lock()
	a.walked = append(a.walked, sessionID)
	a.mu.Unlock()
	if a.walkErr != nil {
		return 0, a.walkErr
	}
	return a.countFor[sessionID], nil
}

// GetSession: the #1850 reap by-ID probe override (the reconcile
// piggyback shares this route; the nil-embedded adapter would
// SIGSEGV the fire-and-forget goroutine — the CI crash the r1 review
// caught on the reconcile spy).
func (a *listingCountSpyAdapter) GetSession(_ context.Context, _, _, _ string) (*session.Session, error) {
	return nil, agent.ErrSessionNotFound
}

func (a *listingCountSpyAdapter) ListSessions(_ context.Context, _, _ string) ([]session.Session, error) {
	// The drift row is LIST-INVISIBLE — that is the entire premise
	// (#1452): alive by ID, absent from the list.
	return []session.Session{{ID: "ses_other"}}, nil
}

// listingCountRouter builds the production router around the spy
// adapter and the given session list (the shared #1880 wiring-tier
// fixture). Returns the engine and the recording index (the walk's
// convergence surface).
func listingCountRouter(t *testing.T, spy *listingCountSpyAdapter, list []types.SessionListItem) (*gin.Engine, *recordingIndex) {
	t.Helper()
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

	proxyHandler, err := handlers.NewProxyHandler(k8sMock, log, "default", nil, spy)
	require.NoError(t, err)
	proxyHandler.SetParentBackfilledForTest("ws-1")
	// Pin the walk dispatch inline: the wiring assertions need the
	// dispatched pass to have COMPLETED by the time the response is
	// recorded (production dispatches detached — the async contract is
	// pinned at the handler tier, proxy_session_listing_count_async_test).
	proxyHandler.SetCountWalkDispatchForTest(func(fn func()) { fn() })
	idx := newRecordingIndex()
	idx.seed("ws-1", "ses_other", 2)
	proxyHandler.SetSessionIndex(idx)
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(list, nil).Maybe()

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	return NewRouter(svc, log, proxyHandler, RouterConfig{Debug: false}), idx
}

func listingCountList() []types.SessionListItem {
	return []types.SessionListItem{
		{ID: "ses_drift", InternalID: "ses_drift", Title: "Drift", MessageCount: 0},
		{ID: "ses_other", InternalID: "ses_other", Title: "Other", MessageCount: 2},
	}
}

func listingCountGet(t *testing.T, router *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestRouterSessionList_SchedulesHarnessCountWalkForDriftRow(t *testing.T) {
	spy := &listingCountSpyAdapter{countFor: map[string]int{"ses_drift": 389}}
	router, idx := listingCountRouter(t, spy, listingCountList())

	rec := listingCountGet(t, router)
	require.Equal(t, http.StatusOK, rec.Code)

	spy.mu.Lock()
	walked := append([]string{}, spy.walked...)
	spy.mu.Unlock()
	require.Contains(t, walked, "ses_drift",
		"the listing route dispatched the drift row's harness count walk by ID")

	var body []struct {
		ID           string `json:"id"`
		MessageCount int    `json:"messageCount"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	found := false
	for _, row := range body {
		switch row.ID {
		case "ses_drift":
			found = true
			assert.Equal(t, 0, row.MessageCount,
				"the wire serves the index value this pass — the walk runs detached from the request")
		case "ses_other":
			assert.Equal(t, 2, row.MessageCount,
				"non-drift rows serve their index values unchanged")
		}
	}
	assert.True(t, found, "the drift row is in the response")

	idx.mu.Lock()
	rebuilt := idx.rebuilt["ws-1/ses_drift"]
	idx.mu.Unlock()
	assert.Equal(t, 389, rebuilt,
		"the dispatched walk persisted the walked truth through the rebuild seam — the NEXT listing serves it")
}

// TestRouterSessionList_WalkErrorServesIndexValue — the r2 wiring-tier
// unhappy pin: a harness count-walk error on the drift row must answer
// the INDEX value on the wire with a 200 (the overlay is best-effort —
// its failure must never degrade the listing), and the walk must have
// been attempted (the outcome is observable, not silent).
func TestRouterSessionList_WalkErrorServesIndexValue(t *testing.T) {
	spy := &listingCountSpyAdapter{
		countFor: map[string]int{"ses_drift": 389},
		walkErr:  agent.ErrNoRunningPod,
	}
	router, _ := listingCountRouter(t, spy, listingCountList())

	rec := listingCountGet(t, router)
	require.Equal(t, http.StatusOK, rec.Code,
		"a walk error never fails the listing — the overlay is best-effort")

	spy.mu.Lock()
	walked := append([]string{}, spy.walked...)
	spy.mu.Unlock()
	require.Contains(t, walked, "ses_drift",
		"the drift row's walk was attempted — the error outcome is observable, not a silent skip")

	var body []struct {
		ID           string `json:"id"`
		MessageCount int    `json:"messageCount"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	found := false
	for _, row := range body {
		if row.ID == "ses_drift" {
			found = true
			assert.Equal(t, 0, row.MessageCount,
				"the failed walk serves the index value on the wire — never worse than the pre-fix listing")
		}
	}
	assert.True(t, found, "the drift row is in the response")
}
