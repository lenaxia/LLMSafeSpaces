// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// memCrossOrgAuditStore satisfies handlers.auditStore via structural typing.
// It returns a single fixed entry so the admin path can assert a real 200 body.
type memCrossOrgAuditStore struct{}

func (memCrossOrgAuditStore) ListOrgAudit(_ context.Context, _ string, limit, offset int) ([]*types.AuditEntry, *types.PaginationMetadata, error) {
	return []*types.AuditEntry{}, &types.PaginationMetadata{Total: 0, Limit: limit, Offset: offset}, nil
}

func (memCrossOrgAuditStore) ListAllAudit(_ context.Context, f types.AuditFilters) ([]*types.AuditEntry, *types.PaginationMetadata, error) {
	entries := []*types.AuditEntry{
		{ID: 1, ActorID: "user-1", Domain: "org", Action: "policy.set", OrgID: "org-1"},
	}
	return entries, &types.PaginationMetadata{Total: len(entries), Limit: f.Limit, Offset: f.Offset}, nil
}

// newAdminAuditRouter builds a real NewRouter with the AuditHandler wired and an
// auth middleware that stamps userRole=role. This proves the
// /api/v1/admin/audit route is registered through the live wiring path and is
// protected by AdminGuard.
func newAdminAuditRouter(t *testing.T, role string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()

	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "test-user")
		c.Set("userRole", role)
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	svc := &mockServices{auth: auth, metrics: met}
	cfg := RouterConfig{
		Debug:        false,
		AuditHandler: handlers.NewAuditHandler(memCrossOrgAuditStore{}),
	}
	return NewRouter(svc, log, nil, cfg)
}

// TestAdminAuditRoute_AdminGuard_BlocksNonAdmin is the integration counterpart
// to the unit-level AdminGuard test: it confirms the router mounts AdminGuard
// on /api/v1/admin/audit so a non-admin gets 404 BEFORE the handler runs.
func TestAdminAuditRoute_AdminGuard_BlocksNonAdmin(t *testing.T) {
	router := newAdminAuditRouter(t, "user")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, "non-admin must get 404, body=%s", rec.Body.String())
}

// TestAdminAuditRoute_AdminGuard_AllowsAdmin confirms an admin reaches the
// handler and gets 200 with the audit items.
func TestAdminAuditRoute_AdminGuard_AllowsAdmin(t *testing.T) {
	router := newAdminAuditRouter(t, "admin")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audit?domain=org", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "admin must get 200, body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "policy.set", "response must contain the audit item")
}

// --- #1449: org-scope automation CRUD through the LIVE wiring ---

// TestOrgAutomationRoutes_ProductionWire proves the org trigger + workflow
// routes are registered through NewRouter with the REAL OrgAdminGuard and
// resolve the resource segment (#1449: the :id/:triggerId shadowing 404'd
// every org-scope GET/PUT/DELETE through exactly this wiring).
func TestOrgAutomationRoutes_ProductionWire(t *testing.T) {
	gin.SetMode(gin.TestMode)
	log, err := apilogger.New(false, "error", "json")
	require.NoError(t, err)

	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	met.On("IncrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	met.On("DecrementActiveConnections", mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) {
		c.Set("userID", "test-user")
		c.Next()
	}))
	auth.On("GetUserID", mock.Anything).Return("test-user")

	trigStore := newFakeTriggerStore()
	wfStore := newFakeWorkflowStore()
	require.NoError(t, trigStore.CreateTrigger(context.Background(), &wf.TriggerRow{
		ID: "trig-org", OwnerType: types.WorkflowOwnerOrg, OwnerID: "org-7",
		Name: "wire-trigger", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{"expr":"0 3 1 * *","tz":"UTC"}`),
	}))
	require.NoError(t, wfStore.CreateWorkflow(context.Background(), &wf.WorkflowRow{
		ID: "wf-org", OwnerType: types.WorkflowOwnerOrg, OwnerID: "org-7",
		Name: "wire-workflow", Slug: "wire-workflow", Status: "draft",
	}))

	cfg := RouterConfig{
		OrgsHandler: handlers.NewOrgsHandler(&fakeOrgStore{
			memberRole: map[string]string{"test-user": "admin"},
			org:        &types.Organization{ID: "org-7", Name: "Wire", Slug: "wire", Status: types.OrgStatusActive, CreatedBy: "test-user"},
		}, nil),
		OrgTriggersHandler:  handlers.NewOrgTriggersHandler(trigStore, nil, nil),
		OrgWorkflowsHandler: handlers.NewOrgWorkflowsHandler(wfStore, nil),
	}
	svc := &mockServices{auth: auth, metrics: met}
	router := NewRouter(svc, log, nil, cfg)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/orgs/org-7/triggers/trig-org", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "org trigger GET through the live wire: body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "wire-trigger")

	req = httptest.NewRequest(http.MethodGet, "/api/v1/orgs/org-7/workflows/wf-org", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code, "org workflow GET through the live wire: body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "wire-workflow")

	// Foreign org id: fails closed (the wire fake's IsOrgAdmin is
	// org-agnostic, so the owner-scoped lookup is what rejects here — 404;
	// with an org-aware store the OrgAdminGuard's 403 fires first; either
	// way the resource is unreachable).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/orgs/org-OTHER/triggers/trig-org", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.NotEqual(t, http.StatusOK, rec.Code, "foreign-org request must fail closed (got %d)", rec.Code)
}
