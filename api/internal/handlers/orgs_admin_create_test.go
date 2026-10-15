// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// setupAdminCreateRouter builds a router that sets the given userID + role in
// the gin context (mimicking AuthMiddleware) and mounts only the POST /orgs
// route. The billing provider is wired so tests can assert it is never called.
func setupAdminCreateRouter(t *testing.T, store *mockOrgStore, isPlatformAdmin bool, billing OrgBilling) (*gin.Engine, *fakeOrgBilling) {
	t.Helper()
	if billing == nil {
		billing = &fakeOrgBilling{}
	}
	fb, ok := billing.(*fakeOrgBilling)
	if !ok {
		t.Fatalf("expected *fakeOrgBilling, got %T", billing)
	}
	handler := NewOrgsHandler(store, &mockOrgAuthService{userID: "admin-1"})
	handler.SetBilling(fb, "https://app/success", "https://app/cancel", "https://app/portal")

	r := newAdminTestRouter(isPlatformAdmin)
	r.POST("/api/v1/orgs", handler.Create)
	return r, fb
}

// newAdminTestRouter is a thin helper kept here so the Story 2 tests do not
// depend on the billing-test router setup. It sets userID=admin-1 and the role.
func newAdminTestRouter(isPlatformAdmin bool) *gin.Engine {
	return newRoleRouter("admin-1", isPlatformAdmin)
}

// newRoleRouter builds a gin engine whose context mimics the AuthMiddleware
// claims: userID and userRole are populated for every request.
func newRoleRouter(userID string, isPlatformAdmin bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	role := "user"
	if isPlatformAdmin {
		role = "admin"
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("userRole", role)
		c.Next()
	})
	return r
}

// --- Happy paths ---

func TestCreateOrg_Admin_KnownEmail_CreatesActiveOrgWithResolvedOwner(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com","planId":"enterprise"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp types.CreateOrgResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != types.OrgStatusActive {
		t.Errorf("expected status=active, got %q", resp.Status)
	}
	if resp.PlanID != types.PlanEnterprise {
		t.Errorf("expected plan=enterprise, got %q", resp.PlanID)
	}
	if resp.SubscriptionStatus != types.SubscriptionActive {
		t.Errorf("expected subscription active, got %q", resp.SubscriptionStatus)
	}
	if resp.UserRole != types.OrgRole("") {
		t.Errorf("caller (admin-1) is not the owner, so UserRole should be empty, got %q", resp.UserRole)
	}

	store.mu.Lock()
	var created *types.Organization
	for _, o := range store.orgs {
		created = o
	}
	members := store.members
	store.mu.Unlock()
	if created == nil {
		t.Fatal("no org was created in the store")
	}
	if created.CreatedBy != "admin-1" {
		t.Errorf("CreatedBy should be the caller admin, got %q", created.CreatedBy)
	}

	mem, _ := store.GetOrgMember(context.Background(), created.ID, "owner-1")
	if mem == nil {
		t.Fatalf("expected owner-1 to be added as an admin member; members=%v", members[created.ID])
	}
	if mem.Role != types.OrgRoleAdmin {
		t.Errorf("expected resolved owner to have role=admin, got %q", mem.Role)
	}
}

// TestCreateOrg_RequiresSSOConfigAtCreation enforces the owner's 0065 §4
// ruling (Option B, issue #1679): "All orgs require OIDC. We don't support
// anything else." Org creation (POST /api/v1/orgs, platform-admin only)
// must reject any request that does not carry a COMPLETE initial SSO
// configuration in the same step — no two-step bootstrap, no waiting state.
// A request without an `sso` block gets a 400 and produces zero side
// effects: no org row, no membership row.
//
// This is the deliberately-flipped successor of
// TestCreateOrg_NoSSORequirementGapPin, which documented the pre-enforcement
// gap (creation succeeded with no SSO config anywhere and the org came up
// immediately active). The pin flipped when #1679 implemented Option B; the
// flip is recorded here so the history is citable.
func TestCreateOrg_RequiresSSOConfigAtCreation(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme-nosso","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("contract (design 0065 §4, Option B): org creation without an SSO config must be rejected; expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if store.orgsLen() != 0 {
		t.Errorf("no org may be created without a complete SSO config; store has %d orgs", store.orgsLen())
	}
	if store.membersLen() != 0 {
		t.Errorf("no org membership may be created without a complete SSO config; store has %d membership rows", store.membersLen())
	}
}

func TestCreateOrg_Admin_DefaultPlanIsEnterprise(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acmedefault","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var resp types.CreateOrgResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.PlanID != types.PlanEnterprise {
		t.Errorf("admin-created org without explicit plan should default to enterprise, got %q", resp.PlanID)
	}
}

func TestCreateOrg_Admin_SelfEmail_OwnerEqualsCreator(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["admin-1@example.com"] = "admin-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"My Org","slug":"myorg","ownerEmail":"admin-1@example.com"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	var created *types.Organization
	for _, o := range store.orgs {
		created = o
	}
	store.mu.Unlock()
	if created == nil {
		t.Fatal("no org created")
	}
	mem, _ := store.GetOrgMember(context.Background(), created.ID, "admin-1")
	if mem == nil || mem.Role != types.OrgRoleAdmin {
		t.Errorf("creator==owner should still be an admin member; mem=%v", mem)
	}

	var resp types.CreateOrgResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.UserRole != types.OrgRoleAdmin {
		t.Errorf("when caller==owner, UserRole should be admin, got %q", resp.UserRole)
	}
}

// Email matching must be case-insensitive to match how the auth service
// normalizes emails on register/login (auth.go stores lowercased emails). An
// admin who types "Owner@Example.com" must resolve the same user as
// "owner@example.com".
func TestCreateOrg_Admin_OwnerEmailNormalizedBeforeLookup(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acmenorm","ownerEmail":"Owner@Example.com"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 for mixed-case email, got %d: %s",
			w.Code, w.Body.String())
	}
	store.mu.Lock()
	var created *types.Organization
	for _, o := range store.orgs {
		created = o
	}
	store.mu.Unlock()
	if created == nil {
		t.Fatal("no org created")
	}
	mem, _ := store.GetOrgMember(context.Background(), created.ID, "owner-1")
	if mem == nil {
		t.Errorf("expected owner-1 to be resolved and added as admin member")
	}
}

// --- Unhappy paths ---

func TestCreateOrg_NonAdmin_Returns403(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, billing := setupAdminCreateRouter(t, store, false, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-admin, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] == "" {
		t.Errorf("expected a clear error message")
	}
	if store.orgsLen() != 0 {
		t.Errorf("no org should be created when rejecting non-admin")
	}
	if billing.checkoutCalls != 0 {
		t.Errorf("Stripe must not be invoked for rejected requests")
	}
}

func TestCreateOrg_Admin_UnknownEmail_Returns404(t *testing.T) {
	store := newMockOrgStore()

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"nobody@example.com"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown owner email, got %d: %s", w.Code, w.Body.String())
	}
	if store.orgsLen() != 0 {
		t.Errorf("no org should be created when owner is unknown")
	}
}

func TestCreateOrg_Admin_MissingOwnerEmail_Returns400(t *testing.T) {
	store := newMockOrgStore()
	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing ownerEmail, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateOrg_Admin_InvalidOwnerEmail_Returns400(t *testing.T) {
	store := newMockOrgStore()
	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"not-an-email"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid ownerEmail, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateOrg_Admin_DuplicateSlug_Returns409(t *testing.T) {
	store := newMockOrgStore()
	store.slugExists = true
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for duplicate slug, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateOrg_Admin_LookupError_Returns500(t *testing.T) {
	store := newMockOrgStore()
	store.userByEmailErr = errors.New("db down")

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on lookup failure, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Slug handling preserved ---

func TestCreateOrg_Admin_SlugLowercased(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"AcMeCo","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	var created *types.Organization
	for _, o := range store.orgs {
		created = o
	}
	store.mu.Unlock()
	if created == nil {
		t.Fatal("no org created")
	}
	if created.Slug != "acmeco" {
		t.Errorf("slug should be lowercased, got %q", created.Slug)
	}
}

// orgsLen is a small helper used by assertions.
func (m *mockOrgStore) orgsLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.orgs)
}

// membersLen counts total membership rows across orgs (the rollback/no-side-
// effect assertions care that NO row survives, not which org it belonged to).
func (m *mockOrgStore) membersLen() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, ms := range m.members {
		n += len(ms)
	}
	return n
}

// --- Single-org enforcement (D8): Create path must pre-check the owner ---

// A platform admin creating an org for an owner who is already in another org
// must get a clear 409 "owner is already a member of another organization",
// NOT a misleading "slug already in use" (the 23505 from the unique index on
// org_memberships(user_id) would be misclassified by isDuplicateErr without
// the pre-check). Reviewer round 1 finding C1.
func TestCreateOrg_Admin_OwnerAlreadyInAnotherOrg_Conflict(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"
	store.userOrgID["owner-1"] = "org-existing" // owner is already in another org

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "another organization") {
		t.Errorf("expected 'another organization' message, got: %s", w.Body.String())
	}
	if store.orgsLen() != 0 {
		t.Errorf("no org should be created when the owner is already in another org")
	}
}

func TestCreateOrg_Admin_GetUserOrgIDError_500(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"
	store.userOrgIDErr = errors.New("db down")

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on GetUserOrgID error, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Error-path branches in Create (reviewer round 1) ---

// CreateOrgWithAdmin succeeds but UpdateOrgStatus fails → 500. The org is left
// pending_activation with no billing customer. This documents the partial-state
// behavior (a separate cleanup concern tracked outside Story 2).
func TestCreateOrg_Admin_UpdateOrgStatusFails_Returns500(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"
	store.updateStatusErr = errors.New("update failed")

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when UpdateOrgStatus fails, got %d: %s", w.Code, w.Body.String())
	}
	store.mu.Lock()
	var created *types.Organization
	for _, o := range store.orgs {
		created = o
	}
	store.mu.Unlock()
	if created == nil {
		t.Fatal("org row should exist from CreateOrgWithAdmin even when activation fails")
	}
	if created.Status != types.OrgStatusPendingActivation {
		t.Errorf("org should remain pending_activation when UpdateOrgStatus fails, got %q", created.Status)
	}
}

// CreateOrgWithAdmin returns a generic (non-duplicate) error → 500.
func TestCreateOrg_Admin_CreateOrgGenericError_Returns500(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"
	store.createErr = errors.New("insert failed: connection refused")

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on generic create error, got %d: %s", w.Code, w.Body.String())
	}
	if store.orgsLen() != 0 {
		t.Errorf("no org row should persist when CreateOrgWithAdmin fails")
	}
}

// CreateOrgWithAdmin returns a unique-violation (TOCTOU between the
// GetOrgBySlug pre-check and the insert) → 409. The partial unique index on
// organizations(LOWER(slug)) WHERE deleted_at IS NULL raises SQLSTATE 23505.
func TestCreateOrg_Admin_CreateOrgDuplicateTOCTOU_Returns409(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"
	store.createErr = &pgconn.PgError{Code: "23505"}

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on TOCTOU duplicate insert, got %d: %s", w.Code, w.Body.String())
	}
}
