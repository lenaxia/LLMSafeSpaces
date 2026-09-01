// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package tests

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type mockOrgPlanReader struct {
	org *types.Organization
	err error
}

func (m *mockOrgPlanReader) GetOrg(_ context.Context, _ string) (*types.Organization, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.org, nil
}

func newFeatureGuardRouter(reader *mockOrgPlanReader, feature string) (*gin.Engine, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Next()
	})
	r.GET("/orgs/:id/feature", middleware.FeatureGuard(reader, feature), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r, httptest.NewRecorder()
}

func TestFeatureGuard_BusinessPlan_AllowsPolicies(t *testing.T) {
	reader := &mockOrgPlanReader{org: &types.Organization{ID: "org-1", PlanID: types.PlanBusiness}}
	r, w := newFeatureGuardRouter(reader, "policies")

	req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for business+policies, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFeatureGuard_FreePlan_BlocksPolicies(t *testing.T) {
	reader := &mockOrgPlanReader{org: &types.Organization{ID: "org-1", PlanID: types.PlanFree}}
	r, w := newFeatureGuardRouter(reader, "policies")

	req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 (upgrade required) for free+policies, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not included") {
		t.Errorf("expected helpful error message, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"feature":"policies"`) {
		t.Errorf("expected feature name in body, got %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"planId":"free"`) {
		t.Errorf("expected planId in body, got %s", w.Body.String())
	}
}

func TestFeatureGuard_FreePlan_BlocksAudit(t *testing.T) {
	reader := &mockOrgPlanReader{org: &types.Organization{ID: "org-1", PlanID: types.PlanFree}}
	r, w := newFeatureGuardRouter(reader, "audit")

	req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 for free+audit, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFeatureGuard_EnterprisePlan_AllowsAll(t *testing.T) {
	reader := &mockOrgPlanReader{org: &types.Organization{ID: "org-1", PlanID: types.PlanEnterprise}}
	for _, feature := range []string{"policies", "audit", "sso", "custom_credentials"} {
		r, w := newFeatureGuardRouter(reader, feature)
		req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("enterprise must allow feature %q, got %d: %s", feature, w.Code, w.Body.String())
		}
	}
}

func TestFeatureGuard_TeamPlan_BlocksPoliciesAndAudit(t *testing.T) {
	reader := &mockOrgPlanReader{org: &types.Organization{ID: "org-1", PlanID: types.PlanTeam}}
	for _, feature := range []string{"policies", "audit", "sso"} {
		r, w := newFeatureGuardRouter(reader, feature)
		req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusPaymentRequired {
			t.Errorf("team must block feature %q (402), got %d: %s", feature, w.Code, w.Body.String())
		}
	}
}

func TestFeatureGuard_OrgNotFound_Returns404(t *testing.T) {
	reader := &mockOrgPlanReader{org: nil}
	r, w := newFeatureGuardRouter(reader, "policies")

	req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when org not found, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFeatureGuard_DBError_Returns500(t *testing.T) {
	reader := &mockOrgPlanReader{err: errors.New("connection refused")}
	r, w := newFeatureGuardRouter(reader, "policies")

	req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on DB error, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFeatureGuard_UnknownFeature_DefaultsAllow(t *testing.T) {
	reader := &mockOrgPlanReader{org: &types.Organization{ID: "org-1", PlanID: types.PlanFree}}
	r, w := newFeatureGuardRouter(reader, "unknown_future_feature")

	req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown feature (fail-open for forward compat), got %d: %s", w.Code, w.Body.String())
	}
}

func TestFeatureGuard_FailsOpenOnBlankPlan_AllowsKnownFeatures(t *testing.T) {
	reader := &mockOrgPlanReader{org: &types.Organization{ID: "org-1", PlanID: ""}}
	r, w := newFeatureGuardRouter(reader, "policies")

	req, _ := http.NewRequest(http.MethodGet, "/orgs/org-1/feature", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("blank plan should fall back to Free (deny policies), got %d: %s", w.Code, w.Body.String())
	}
}
