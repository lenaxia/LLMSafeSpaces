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
	"github.com/lenaxia/llmsafespaces/api/internal/services/sso"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Design 0064 (#1669) phase 1 — router-tier pins for the orgContext field on
// GET /auth/config: present iff the request Host is <slug>.<baseDomain> for an
// SSO-configured org; omitted (never an error, never a shape change to the
// rest of the response) in every miss branch. Real wiring: a real SSOHandler
// over a real sso.Service backed by a combined fake store.

// orgContextFakeStore satisfies both the handlers ssoStore interface and the
// sso.Service orgStore interface (one store serves both — the production
// shape, where one PgOrgStore does).
type orgContextFakeStore struct {
	orgs    map[string]*types.Organization // by slug
	configs map[string]*types.OrgSSOConfig // by orgID
}

func newOrgContextFakeStore() *orgContextFakeStore {
	return &orgContextFakeStore{
		orgs:    map[string]*types.Organization{},
		configs: map[string]*types.OrgSSOConfig{},
	}
}

func (s *orgContextFakeStore) GetOrgBySlug(_ context.Context, slug string) (*types.Organization, error) {
	return s.orgs[slug], nil
}
func (s *orgContextFakeStore) GetSSOConfig(_ context.Context, orgID string) (*types.OrgSSOConfig, error) {
	return s.configs[orgID], nil
}
func (s *orgContextFakeStore) UpsertSSOConfig(_ context.Context, _ *types.OrgSSOConfig) error {
	return nil
}
func (s *orgContextFakeStore) DeleteSSOConfig(_ context.Context, _ string) error { return nil }
func (s *orgContextFakeStore) SetDomainVerified(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}
func (s *orgContextFakeStore) RotateVerificationToken(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (s *orgContextFakeStore) GetOrgMember(_ context.Context, _, _ string) (*types.OrgMember, error) {
	return nil, nil
}
func (s *orgContextFakeStore) CountOrgAdmins(_ context.Context, _ string) (int, error) {
	return 0, nil
}
func (s *orgContextFakeStore) AddOrgMember(_ context.Context, _, _ string, _ types.OrgRole) error {
	return nil
}
func (s *orgContextFakeStore) UpdateOrgMemberRole(_ context.Context, _, _ string, _ types.OrgRole) error {
	return nil
}
func (s *orgContextFakeStore) ListSSODomains(_ context.Context) ([]types.SSODomain, error) {
	return []types.SSODomain{}, nil
}
func (s *orgContextFakeStore) CountSSOConfigs(_ context.Context) (int, error) {
	return len(s.configs), nil
}
func (s *orgContextFakeStore) LogOrgEvent(_ context.Context, _, _, _, _ string, _ map[string]any) error {
	return nil
}

type orgContextFakeUsers struct{}

func (orgContextFakeUsers) GetUserByEmail(_ context.Context, _ string) (*types.User, error) {
	return nil, nil
}
func (orgContextFakeUsers) CreateUser(_ context.Context, _ *types.User) error { return nil }

type orgContextFakeIssuer struct{}

func (orgContextFakeIssuer) GenerateToken(string) (string, error) { return "jwt", nil }

func newOrgContextRouter(t *testing.T, store *orgContextFakeStore, baseDomain string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc, err := sso.New(store, orgContextFakeUsers{}, sso.ServiceConfig{
		TokenIssuer: orgContextFakeIssuer{},
	})
	require.NoError(t, err)
	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	auth.On("GetUserID", mock.Anything).Return("").Maybe()
	ssoHandler := handlers.NewSSOHandler(svc, store, auth, "lsp_session", "", "https://app.test.local", nil)

	apiLog, _ := apilogger.New(false, "error", "json")
	return NewRouter(&authMockServices{auth: auth, metrics: met, database: &imocks.MockDatabaseService{}, cache: &imocks.MockCacheService{}}, apiLog, nil, RouterConfig{
		InstanceSettings:       nil,
		SSOHandler:             ssoHandler,
		OrgSubdomainBaseDomain: baseDomain,
	})
}

func doGetConfig(t *testing.T, router *gin.Engine, host string) (int, types.AuthConfig) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	req.Host = host
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var cfg types.AuthConfig
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfg))
	}
	return w.Code, cfg
}

func TestAuthConfig_OrgContext_SubdomainHostSSOOrg(t *testing.T) {
	store := newOrgContextFakeStore()
	store.orgs["acme"] = &types.Organization{ID: "org-1", Name: "Acme Corp", Slug: "acme"}
	store.configs["org-1"] = &types.OrgSSOConfig{OrgID: "org-1"}
	router := newOrgContextRouter(t, store, "app.example.com")

	code, cfg := doGetConfig(t, router, "acme.app.example.com")
	require.Equal(t, http.StatusOK, code)
	require.NotNil(t, cfg.OrgContext)
	assert.Equal(t, "acme", cfg.OrgContext.Slug)
	assert.Equal(t, "Acme Corp", cfg.OrgContext.Name)
	// The rest of the response shape is unchanged.
	assert.Equal(t, "LLMSafeSpaces", cfg.InstanceName)
	assert.True(t, cfg.OIDCEnabled)
}

func TestAuthConfig_OrgContext_MissBranchesOmitField(t *testing.T) {
	store := newOrgContextFakeStore()
	store.orgs["acme"] = &types.Organization{ID: "org-1", Name: "Acme Corp", Slug: "acme"}
	store.configs["org-1"] = &types.OrgSSOConfig{OrgID: "org-1"}
	store.orgs["beta"] = &types.Organization{ID: "org-2", Name: "Beta Ltd", Slug: "beta"} // no SSO config
	router := newOrgContextRouter(t, store, "app.example.com")

	// Org without SSO and unknown slug are indistinguishable; non-matching
	// hosts and the exact base host omit too. Every branch is a 200.
	for _, host := range []string{
		"beta.app.example.com",   // org exists, no SSO
		"nope.app.example.com",   // unknown slug
		"app.example.com",        // exact base
		"a.b.app.example.com",    // deeper host
		"acme.other.example.com", // wrong base
	} {
		code, cfg := doGetConfig(t, router, host)
		assert.Equal(t, http.StatusOK, code, "host %s", host)
		assert.Nil(t, cfg.OrgContext, "host %s", host)
	}
}

func TestAuthConfig_OrgContext_RoutingDisabledOmitsField(t *testing.T) {
	store := newOrgContextFakeStore()
	store.orgs["acme"] = &types.Organization{ID: "org-1", Name: "Acme Corp", Slug: "acme"}
	store.configs["org-1"] = &types.OrgSSOConfig{OrgID: "org-1"}
	router := newOrgContextRouter(t, store, "") // baseDomain unset

	code, cfg := doGetConfig(t, router, "acme.app.example.com")
	require.Equal(t, http.StatusOK, code)
	assert.Nil(t, cfg.OrgContext)
}

func TestAuthConfig_OrgContext_NilSSOHandlerOmitsField(t *testing.T) {
	// No SSOHandler wired at all — orgContext must never surface.
	gin.SetMode(gin.TestMode)
	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	auth.On("GetUserID", mock.Anything).Return("").Maybe()
	apiLog, _ := apilogger.New(false, "error", "json")
	router := NewRouter(&authMockServices{auth: auth, metrics: met, database: &imocks.MockDatabaseService{}, cache: &imocks.MockCacheService{}}, apiLog, nil, RouterConfig{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	req.Host = "acme.app.example.com"
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, w.Body.String(), "orgContext")
}
