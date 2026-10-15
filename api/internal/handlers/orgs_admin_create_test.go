// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// ssoJSONBlock is a complete, valid initial SSO configuration (Option B,
// design 0065 §4 / #1679) spliced into create payloads. Complete = valid
// discoveryUrl + clientId + clientSecret (the first-config rule: no stored
// secret exists to fall back on).
const ssoJSONBlock = `"sso":{"discoveryUrl":"https://idp.acme.example/.well-known/openid-configuration","clientId":"acme-idp-client","clientSecret":"idp-secret-1","claimedDomains":["acme.example"]}`

// withSSO splices the complete SSO block into a flat JSON object body.
func withSSO(body string) string {
	return strings.Replace(body, "}", ", "+ssoJSONBlock+"}", 1)
}

// fakeSSOProvisioner records ApplyConfigMutation calls so tests can assert
// the initial SSO config is persisted in the same step as org creation
// (Option B, #1679). err, when set, is returned to simulate persistence
// failure (the handler must roll the creation back).
type fakeSSOProvisioner struct {
	mu    sync.Mutex
	calls []fakeSSOProvisionCall
	err   error
}

type fakeSSOProvisionCall struct {
	OrgID string
	Req   types.UpsertSSOConfigRequest
}

func (f *fakeSSOProvisioner) ApplyConfigMutation(_ context.Context, orgID string, req types.UpsertSSOConfigRequest, _ []byte, _ []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeSSOProvisionCall{OrgID: orgID, Req: req})
	if f.err != nil {
		return nil, f.err
	}
	return []byte("encrypted-secret"), nil
}

func (f *fakeSSOProvisioner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// setupAdminCreateRouter builds a router that sets the given userID + role in
// the gin context (mimicking AuthMiddleware) and mounts only the POST /orgs
// route. The billing provider is wired so tests can assert it is never called,
// and a succeeding fake SSO provisioner is wired by default (Option B, #1679).
func setupAdminCreateRouter(t *testing.T, store *mockOrgStore, isPlatformAdmin bool, billing OrgBilling) (*gin.Engine, *fakeOrgBilling) {
	t.Helper()
	return setupAdminCreateRouterWithSSO(t, store, isPlatformAdmin, billing, &fakeSSOProvisioner{})
}

// setupAdminCreateRouterWithSSO is setupAdminCreateRouter with an injectable
// SSO provisioner: pass nil to simulate a deployment where the SSO service is
// not wired (creation must refuse with 503, not create a no-SSO org).
func setupAdminCreateRouterWithSSO(t *testing.T, store *mockOrgStore, isPlatformAdmin bool, billing OrgBilling, prov orgSSOProvisioner) (*gin.Engine, *fakeOrgBilling) {
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
	if prov != nil {
		handler.SetSSOProvisioner(prov)
	}

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
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com","planId":"enterprise"}`))
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

// TestCreateOrg_WithCompleteSSO_PersistsConfigInSameStep pins the Option B
// happy path (#1679, design 0065 §4): a creation request carrying a complete
// SSO block creates the org AND provisions its initial OIDC config in the
// same request — the provisioner is called exactly once, with the new org's
// ID and the request's SSO payload verbatim. No second step exists.
func TestCreateOrg_WithCompleteSSO_PersistsConfigInSameStep(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	prov := &fakeSSOProvisioner{}
	router, _ := setupAdminCreateRouterWithSSO(t, store, true, nil, prov)

	w := doRequest(router, "POST", "/api/v1/orgs",
		`{"name":"Acme","slug":"acme-withsso","ownerEmail":"owner@example.com","planId":"enterprise","sso":{"discoveryUrl":"https://idp.acme.example/.well-known/openid-configuration","clientId":"acme-idp-client","clientSecret":"idp-secret-1","claimedDomains":["acme.example"," acme.io "],"autoProvision":false,"groupRoleMapping":{"admins":"admin"}}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 with complete SSO config, got %d: %s", w.Code, w.Body.String())
	}
	var resp types.CreateOrgResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Status != types.OrgStatusActive {
		t.Errorf("expected status=active, got %q", resp.Status)
	}

	if prov.callCount() != 1 {
		t.Fatalf("provisioner must be called exactly once (same-step SSO), got %d calls", prov.callCount())
	}
	call := prov.calls[0]
	if call.OrgID != resp.ID {
		t.Errorf("provisioner must receive the NEW org's ID: call orgID=%q, created org ID=%q", call.OrgID, resp.ID)
	}
	if call.Req.DiscoveryURL != "https://idp.acme.example/.well-known/openid-configuration" {
		t.Errorf("discoveryUrl not passed through verbatim: %q", call.Req.DiscoveryURL)
	}
	if call.Req.ClientID != "acme-idp-client" {
		t.Errorf("clientId not passed through verbatim: %q", call.Req.ClientID)
	}
	if call.Req.ClientSecret != "idp-secret-1" {
		t.Errorf("clientSecret not passed through verbatim: %q", call.Req.ClientSecret)
	}
	if call.Req.AutoProvision == nil || *call.Req.AutoProvision {
		t.Errorf("autoProvision=false must be passed through (ApplyConfigMutation defaults to true when nil)")
	}
	if len(call.Req.GroupRoleMapping) != 1 || call.Req.GroupRoleMapping["admins"] != types.OrgRoleAdmin {
		t.Errorf("groupRoleMapping not passed through: %v", call.Req.GroupRoleMapping)
	}
	// Domain normalization happens inside ApplyConfigMutation (NormalizeDomains);
	// the handler passes the raw list through.
	if len(call.Req.ClaimedDomains) != 2 {
		t.Errorf("claimedDomains not passed through verbatim: %v", call.Req.ClaimedDomains)
	}
}

// TestCreateOrg_IncompleteSSO_Rejected400NoSideEffects pins the completeness
// gate: an SSO block that is present but incomplete/malformed is a 400 with
// `sso.<field>` detail keys, BEFORE any store call — no org row, no
// membership row, provisioner never invoked. This is the mutation-check
// surface for the Option B gate: weakening validateInitialSSOConfig (or
// removing the gate) makes these arms (and the flipped pin above) fail.
func TestCreateOrg_IncompleteSSO_Rejected400NoSideEffects(t *testing.T) {
	cases := []struct {
		name       string
		sso        string
		wantDetail string
	}{
		{"missing discoveryUrl", `"sso":{"clientId":"c","clientSecret":"s"}`, "sso.discoveryUrl"},
		{"malformed discoveryUrl", `"sso":{"discoveryUrl":"not-a-url","clientId":"c","clientSecret":"s"}`, "sso.discoveryUrl"},
		{"missing clientId", `"sso":{"discoveryUrl":"https://idp.example/.well-known/openid-configuration","clientSecret":"s"}`, "sso.clientId"},
		{"missing clientSecret", `"sso":{"discoveryUrl":"https://idp.example/.well-known/openid-configuration","clientId":"c"}`, "sso.clientSecret"},
		{"invalid group role mapping", `"sso":{"discoveryUrl":"https://idp.example/.well-known/openid-configuration","clientId":"c","clientSecret":"s","groupRoleMapping":{"devs":"owner"}}`, "sso.groupRoleMapping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockOrgStore()
			store.usersByEmail["owner@example.com"] = "owner-1"
			prov := &fakeSSOProvisioner{}
			router, _ := setupAdminCreateRouterWithSSO(t, store, true, nil, prov)

			w := doRequest(router, "POST", "/api/v1/orgs",
				`{"name":"Acme","slug":"acme-bad","ownerEmail":"owner@example.com",`+tc.sso+`}`)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
			}
			var body struct {
				Error   string            `json:"error"`
				Details map[string]string `json:"details"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, ok := body.Details[tc.wantDetail]; !ok {
				t.Errorf("expected details key %q, got details=%v", tc.wantDetail, body.Details)
			}
			if store.orgsLen() != 0 || store.membersLen() != 0 {
				t.Errorf("rejected request must leave zero side effects; orgs=%d members=%d", store.orgsLen(), store.membersLen())
			}
			if prov.callCount() != 0 {
				t.Errorf("provisioner must not be called for an invalid payload; called %d times", prov.callCount())
			}
		})
	}
}

// TestCreateOrg_SSOPersistFails_RollsBackOrgCreation pins the no-orphan
// invariant: when the SSO config cannot be persisted after the org row (and
// owner membership) exist, the creation is rolled back — membership removed
// (so the owner is not blocked by the single-org unique index), org
// soft-deleted — and the client gets a 500. No org survives without its SSO
// config.
func TestCreateOrg_SSOPersistFails_RollsBackOrgCreation(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	prov := &fakeSSOProvisioner{err: errors.New("persist sso config: connection refused")}
	router, _ := setupAdminCreateRouterWithSSO(t, store, true, nil, prov)

	w := doRequest(router, "POST", "/api/v1/orgs",
		withSSO(`{"name":"Acme","slug":"acme-roll","ownerEmail":"owner@example.com"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on SSO persist failure, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not created") {
		t.Errorf("error should state the org was not created, got: %s", w.Body.String())
	}
	if store.orgsLen() != 0 {
		t.Errorf("org row must be rolled back on SSO persist failure; %d orgs remain", store.orgsLen())
	}
	if store.membersLen() != 0 {
		t.Errorf("membership row must be rolled back on SSO persist failure; %d memberships remain", store.membersLen())
	}
	if prov.callCount() != 1 {
		t.Errorf("provisioner should have been attempted exactly once, got %d", prov.callCount())
	}
}

// TestCreateOrg_SSOPersistFails_ServerKeyMissing_503: the specific
// deployment-level failure (server KEK not configured) maps to 503 like the
// SSO handler's own mutation path, with the same rollback.
func TestCreateOrg_SSOPersistFails_ServerKeyMissing_503(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	prov := &fakeSSOProvisioner{err: errors.New("server key not configured")}
	router, _ := setupAdminCreateRouterWithSSO(t, store, true, nil, prov)

	w := doRequest(router, "POST", "/api/v1/orgs",
		withSSO(`{"name":"Acme","slug":"acme-nokey","ownerEmail":"owner@example.com"}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the server key is not configured, got %d: %s", w.Code, w.Body.String())
	}
	if store.orgsLen() != 0 || store.membersLen() != 0 {
		t.Errorf("503 must also roll back; orgs=%d members=%d", store.orgsLen(), store.membersLen())
	}
}

// alertCaptureLogger records Error-level emissions so tests can PIN the
// alert-grade rollback-failure logs (orchestrator ruling on #1679: rollback
// is best-effort and deliberately not transactional — when it fails, the
// orphan org must be logged loudly, never silently swallowed).
type alertCaptureLogger struct {
	mu     sync.Mutex
	errors []string
}

func (l *alertCaptureLogger) Warn(_ string, _ ...any) {}

func (l *alertCaptureLogger) Error(msg string, _ error, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errors = append(l.errors, msg)
}

func (l *alertCaptureLogger) errorCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.errors)
}

// TestCreateOrg_RollbackItselfFails_LogsAlertGrade pins the orphan-org
// observability contract: when the SSO persist fails AND the best-effort
// rollback ALSO fails, the request still reports the creation error (500),
// but the orphan must surface at Error level with the identifiers operators
// need (orgID, ownerID, cause) — the exact scenario the orchestrator's r1
// addition guards. Not transactional by ruling; loud by requirement.
func TestCreateOrg_RollbackItselfFails_LogsAlertGrade(t *testing.T) {
	cases := []struct {
		name           string
		inject         func(*mockOrgStore)
		wantOrphanOrgs int
		wantLogMsg     string
	}{
		{
			name:   "membership removal fails (owner stays blocked by D8 index)",
			inject: func(s *mockOrgStore) { s.removeMemberErr = errors.New("membership delete failed") },
			// SoftDeleteOrg still succeeds → org row is gone; the orphan is
			// the membership row (invisible to orgsLen, counted below).
			wantOrphanOrgs: 0,
			wantLogMsg:     "could not remove the owner membership",
		},
		{
			name:           "org soft-delete fails (no-SSO org row survives)",
			inject:         func(s *mockOrgStore) { s.softDeleteErr = errors.New("soft delete failed") },
			wantOrphanOrgs: 1,
			wantLogMsg:     "could not soft-delete the org",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newMockOrgStore()
			store.usersByEmail["owner@example.com"] = "owner-1"
			tc.inject(store)

			prov := &fakeSSOProvisioner{err: errors.New("persist sso config: connection refused")}
			handler := NewOrgsHandler(store, &mockOrgAuthService{userID: "admin-1"})
			handler.SetSSOProvisioner(prov)
			alerts := &alertCaptureLogger{}
			handler.SetLogger(alerts)
			router := newAdminTestRouter(true)
			router.POST("/api/v1/orgs", handler.Create)

			w := doRequest(router, "POST", "/api/v1/orgs",
				withSSO(`{"name":"Acme","slug":"acme-orphan","ownerEmail":"owner@example.com"}`))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("expected 500 (the creation error still surfaces), got %d: %s", w.Code, w.Body.String())
			}
			if alerts.errorCount() == 0 {
				t.Fatal("rollback failure MUST emit an Error-level (alert-grade) log — silent orphan violates the #1679 ruling")
			}
			found := false
			alerts.mu.Lock()
			for _, m := range alerts.errors {
				if strings.Contains(m, tc.wantLogMsg) {
					found = true
				}
			}
			alerts.mu.Unlock()
			if !found {
				t.Errorf("expected an alert containing %q, got %v", tc.wantLogMsg, alerts.errors)
			}
			if got := store.orgsLen(); got != tc.wantOrphanOrgs {
				t.Errorf("orphan-org bookkeeping: expected %d surviving org rows for this arm, got %d", tc.wantOrphanOrgs, got)
			}
		})
	}
}

// TestCreateOrg_RollbackSucceeds_NoAlertLog pins the quiet side of the same
// contract: a SUCCESSFUL rollback (the common case) emits no Error-level
// noise — alert-grade logging is reserved for operator action, so a noisy
// alert stream would hide the real orphans.
func TestCreateOrg_RollbackSucceeds_NoAlertLog(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	prov := &fakeSSOProvisioner{err: errors.New("persist sso config: connection refused")}
	handler := NewOrgsHandler(store, &mockOrgAuthService{userID: "admin-1"})
	handler.SetSSOProvisioner(prov)
	alerts := &alertCaptureLogger{}
	handler.SetLogger(alerts)
	router := newAdminTestRouter(true)
	router.POST("/api/v1/orgs", handler.Create)

	w := doRequest(router, "POST", "/api/v1/orgs",
		withSSO(`{"name":"Acme","slug":"acme-cleanrb","ownerEmail":"owner@example.com"}`))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	if alerts.errorCount() != 0 {
		t.Errorf("successful rollback must not log at Error level; got %d alerts: %v", alerts.errorCount(), alerts.errors)
	}
	if store.orgsLen() != 0 || store.membersLen() != 0 {
		t.Errorf("rollback must clean up; orgs=%d members=%d", store.orgsLen(), store.membersLen())
	}
}

// TestCreateOrg_SSOProvisionerUnwired_Returns503 pins the deployment-shape
// gate: a handler with NO SSO provisioner wired (SSO service unavailable —
// e.g. state-signing key derivation failed at boot) refuses creation with
// 503 rather than creating an org in the unsupported no-SSO state.
func TestCreateOrg_SSOProvisionerUnwired_Returns503(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, billing := setupAdminCreateRouterWithSSO(t, store, true, nil, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		withSSO(`{"name":"Acme","slug":"acme-unwired","ownerEmail":"owner@example.com"}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when the SSO provisioner is not wired, got %d: %s", w.Code, w.Body.String())
	}
	if store.orgsLen() != 0 || store.membersLen() != 0 {
		t.Errorf("no org may be created without the provisioner; orgs=%d members=%d", store.orgsLen(), store.membersLen())
	}
	if billing.checkoutCalls != 0 {
		t.Errorf("billing must not be invoked")
	}
}

func TestCreateOrg_Admin_DefaultPlanIsEnterprise(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		withSSO(`{"name":"Acme","slug":"acmedefault","ownerEmail":"owner@example.com"}`))
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
		withSSO(`{"name":"My Org","slug":"myorg","ownerEmail":"admin-1@example.com"}`))
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
		withSSO(`{"name":"Acme","slug":"acmenorm","ownerEmail":"Owner@Example.com"}`))
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

	// Payload intentionally has NO sso block: the platform-admin guard fires
	// before Option B SSO validation, so this also pins guard precedence.
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
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"nobody@example.com"}`))
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
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`))
	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for duplicate slug, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateOrg_Admin_LookupError_Returns500(t *testing.T) {
	store := newMockOrgStore()
	store.userByEmailErr = errors.New("db down")

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`))
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
		withSSO(`{"name":"Acme","slug":"AcMeCo","ownerEmail":"owner@example.com"}`))
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
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`))
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
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on GetUserOrgID error, got %d: %s", w.Code, w.Body.String())
	}
}

// --- Error-path branches in Create (reviewer round 1) ---

// CreateOrgWithAdmin succeeds, the initial SSO config is persisted (Option B,
// #1679), but UpdateOrgStatus fails → 500. The org is left pending_activation
// WITH its SSO config (the no-SSO-org invariant is unaffected — the org has
// OIDC; only billing activation failed). This documents the partial-state
// behavior for the status leg (a separate cleanup concern tracked outside
// Story 2; the pending-org cleaner reaps stale pending orgs).
func TestCreateOrg_Admin_UpdateOrgStatusFails_Returns500(t *testing.T) {
	store := newMockOrgStore()
	store.usersByEmail["owner@example.com"] = "owner-1"
	store.updateStatusErr = errors.New("update failed")

	router, _ := setupAdminCreateRouter(t, store, true, nil)

	w := doRequest(router, "POST", "/api/v1/orgs",
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`))
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
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`))
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
		withSSO(`{"name":"Acme","slug":"acme","ownerEmail":"owner@example.com"}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 on TOCTOU duplicate insert, got %d: %s", w.Code, w.Body.String())
	}
}
