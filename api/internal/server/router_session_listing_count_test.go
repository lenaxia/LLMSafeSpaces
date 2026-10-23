// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

// #1880 direction 3 — the WIRING pin: the sidebar's session list route
// serves the HARNESS count for the drift class. A GET through the
// production router must walk the harness count for a list-invisible
// count-zero row (the #1452 class the reconcile rebuild cannot reach)
// and carry it on the WIRE — deleting the router's ServeHarnessMessageCounts
// piggyback reddens this test even with every handler unit green (the
// TestRouterSessionList_TriggersReconcilePass precedent for the
// reconcile piggyback).

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

// listingCountSpyAdapter walks ONE drift session's count and records
// the call.
type listingCountSpyAdapter struct {
	nullTestAdapter
	mu       sync.Mutex
	walked   []string
	countFor map[string]int
}

func (a *listingCountSpyAdapter) CountMessages(_ context.Context, _, _, sessionID string) (int, error) {
	a.mu.Lock()
	a.walked = append(a.walked, sessionID)
	a.mu.Unlock()
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

func TestRouterSessionList_ServesHarnessCountForDriftRow(t *testing.T) {
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

	spy := &listingCountSpyAdapter{countFor: map[string]int{"ses_drift": 389}}
	proxyHandler, err := handlers.NewProxyHandler(k8sMock, log, "default", nil, spy)
	require.NoError(t, err)
	proxyHandler.SetParentBackfilledForTest("ws-1")
	idx := newRecordingIndex()
	idx.seed("ws-1", "ses_other", 2)
	proxyHandler.SetSessionIndex(idx)
	ws.On("ListWorkspaceSessions", mock.Anything, "test-user", "ws-1").Return(
		[]types.SessionListItem{
			{ID: "ses_drift", InternalID: "ses_drift", Title: "Drift", MessageCount: 0},
			{ID: "ses_other", InternalID: "ses_other", Title: "Other", MessageCount: 2},
		}, nil)

	svc := &mockServices{auth: auth, metrics: met, workspace: ws}
	router := NewRouter(svc, log, proxyHandler, RouterConfig{Debug: false})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/workspaces/ws-1/sessions", nil)
	req.Header.Set("Authorization", "Bearer token")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	spy.mu.Lock()
	walked := append([]string{}, spy.walked...)
	spy.mu.Unlock()
	require.Contains(t, walked, "ses_drift",
		"the listing route walked the drift row's harness count by ID")

	var body []struct {
		ID           string `json:"id"`
		MessageCount int    `json:"messageCount"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	found := false
	for _, row := range body {
		if row.ID == "ses_drift" {
			found = true
			assert.Equal(t, 389, row.MessageCount,
				"the WIRE carries the harness truth for the drift row")
		}
	}
	assert.True(t, found, "the drift row is in the response")
}
