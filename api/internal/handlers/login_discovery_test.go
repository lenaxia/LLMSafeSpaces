// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- fakes ---

type lookupUserStore struct {
	users map[string]*types.User // keyed by lowercase email
	err   error                  // when set, GetUserByEmail returns this error
}

func newLookupUserStore() *lookupUserStore {
	return &lookupUserStore{users: map[string]*types.User{}}
}

func (s *lookupUserStore) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.users[strings.ToLower(strings.TrimSpace(email))], nil
}

type lookupOrgStore struct {
	orgs       map[string]*types.Organization // keyed by orgID
	userOrgID  string                         // value returned by GetUserOrgID
	userOrgErr error                          // when set, GetUserOrgID returns this error
	getOrgErr  error                          // when set, GetOrg returns this error
}

func newLookupOrgStore() *lookupOrgStore {
	return &lookupOrgStore{orgs: map[string]*types.Organization{}}
}

func (s *lookupOrgStore) GetUserOrgID(_ context.Context, _ string) (string, error) {
	return s.userOrgID, s.userOrgErr
}

func (s *lookupOrgStore) GetOrg(_ context.Context, orgID string) (*types.Organization, error) {
	if s.getOrgErr != nil {
		return nil, s.getOrgErr
	}
	return s.orgs[orgID], nil
}

type lookupCaptureLogger struct {
	errs []lookupLogEntry
}

type lookupLogEntry struct {
	msg string
	err error
}

func (l *lookupCaptureLogger) Error(msg string, err error, _ ...any) {
	l.errs = append(l.errs, lookupLogEntry{msg: msg, err: err})
}

func (l *lookupCaptureLogger) Warn(string, ...any) {}

func setupLoginLookupRouter(h *LoginDiscoveryHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/auth/lookup", h.Lookup)
	return r
}

// --- response-shape helpers ---

// requireLookupOK asserts status 200 and returns the parsed redirectUrl.
// All non-validation branches must hit this regardless of found/not-found.
func requireLookupOK(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	require.Equalf(t, http.StatusOK, w.Code, "status=%d body=%s", w.Code, w.Body.String())
	var resp struct {
		RedirectURL string `json:"redirectUrl"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body=%s", w.Body.String())
	require.NotEmpty(t, resp.RedirectURL, "redirectUrl must never be empty on 200")
	return resp.RedirectURL
}

// --- tests ---

func TestLoginLookup_FoundWithOrg_ReturnsSubdomainURL(t *testing.T) {
	users := newLookupUserStore()
	users.users["alice@acme.com"] = &types.User{ID: "user-1", Email: "alice@acme.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgID = "org-1"
	orgs.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "acme", Name: "Acme"}

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"alice@acme.com"}`)
	url := requireLookupOK(t, w)
	assert.Equal(t, "https://acme.app.example.com", url)
}

func TestLoginLookup_FoundNoOrg_ReturnsNotFoundRedirect(t *testing.T) {
	// User exists but has no membership — a personal-only user.
	users := newLookupUserStore()
	users.users["solo@example.com"] = &types.User{ID: "user-2", Email: "solo@example.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgID = "" // GetUserOrgID returns "" when no row

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"solo@example.com"}`)
	url := requireLookupOK(t, w)
	assert.Contains(t, url, "lookup=not_found")
}

func TestLoginLookup_NotFound_ReturnsNotFoundRedirect(t *testing.T) {
	users := newLookupUserStore() // empty — no users
	orgs := newLookupOrgStore()

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"nobody@example.com"}`)
	url := requireLookupOK(t, w)
	assert.Contains(t, url, "lookup=not_found")
}

func TestLoginLookup_GetUserByEmailDBError_ReturnsNotFoundRedirect(t *testing.T) {
	// The enumeration-safe contract: DB error must NOT surface as 500.
	// Follows the password_reset.go:119 precedent exactly.
	users := newLookupUserStore()
	users.err = errors.New("db connection lost")
	orgs := newLookupOrgStore()

	log := &lookupCaptureLogger{}
	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", log)
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"anyone@example.com"}`)
	url := requireLookupOK(t, w)
	assert.Contains(t, url, "lookup=not_found")
	require.Len(t, log.errs, 1, "DB error must be logged exactly once")
	assert.Contains(t, log.errs[0].msg, "user lookup")
}

func TestLoginLookup_GetUserOrgIDError_ReturnsNotFoundRedirect(t *testing.T) {
	// Second-query DB error must also mask — never 500.
	users := newLookupUserStore()
	users.users["alice@acme.com"] = &types.User{ID: "user-1", Email: "alice@acme.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgErr = errors.New("org_memberships table unreachable")

	log := &lookupCaptureLogger{}
	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", log)
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"alice@acme.com"}`)
	url := requireLookupOK(t, w)
	assert.Contains(t, url, "lookup=not_found")
	require.Len(t, log.errs, 1)
	assert.Contains(t, log.errs[0].msg, "org lookup")
}

func TestLoginLookup_GetOrgError_ReturnsNotFoundRedirect(t *testing.T) {
	// OrgID resolves but the org fetch fails — must mask.
	users := newLookupUserStore()
	users.users["alice@acme.com"] = &types.User{ID: "user-1", Email: "alice@acme.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgID = "org-1"
	orgs.getOrgErr = errors.New("organizations table unreachable")

	log := &lookupCaptureLogger{}
	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", log)
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"alice@acme.com"}`)
	url := requireLookupOK(t, w)
	assert.Contains(t, url, "lookup=not_found")
	require.Len(t, log.errs, 1)
	assert.Contains(t, log.errs[0].msg, "org fetch")
}

func TestLoginLookup_NoBaseDomain_ReturnsDirectSSOStartURL(t *testing.T) {
	// When subdomain routing is disabled (baseDomain == ""), the helper falls
	// back to the direct SSO start URL — which works today regardless of chart
	// config. A found user must NEVER be told "not found".
	users := newLookupUserStore()
	users.users["alice@acme.com"] = &types.User{ID: "user-1", Email: "alice@acme.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgID = "org-1"
	orgs.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "acme", Name: "Acme"}

	h := NewLoginDiscoveryHandler(users, orgs, "", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"alice@acme.com"}`)
	url := requireLookupOK(t, w)
	assert.Equal(t, "/api/v1/auth/sso/acme/start", url)
}

func TestLoginLookup_OrgDeleted_ReturnsNotFoundRedirect(t *testing.T) {
	// Membership row exists but the org was soft-deleted (GetOrg returns nil).
	// Cannot redirect to a deleted org's subdomain.
	users := newLookupUserStore()
	users.users["alice@old.com"] = &types.User{ID: "user-1", Email: "alice@old.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgID = "org-1"
	orgs.orgs["org-1"] = nil // GetOrg returns nil — org deleted

	log := &lookupCaptureLogger{}
	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", log)
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"alice@old.com"}`)
	url := requireLookupOK(t, w)
	assert.Contains(t, url, "lookup=not_found")
}

func TestLoginLookup_InvalidEmail_Returns400(t *testing.T) {
	users := newLookupUserStore()
	orgs := newLookupOrgStore()

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	// Not a valid email — binding rejects before any DB call.
	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"not-an-email"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginLookup_EmptyBody_Returns400(t *testing.T) {
	users := newLookupUserStore()
	orgs := newLookupOrgStore()

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginLookup_MalformedJSON_Returns400(t *testing.T) {
	users := newLookupUserStore()
	orgs := newLookupOrgStore()

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestLoginLookup_NormalizesEmail(t *testing.T) {
	// Input is uppercased; the resolver must normalize before lookup
	// (matches password_reset.go:117 convention). Note: gin's `email`
	// validator rejects leading/trailing whitespace, so we only test the
	// case-normalization path (the realistic mixed-case scenario).
	users := newLookupUserStore()
	users.users["alice@acme.com"] = &types.User{ID: "user-1", Email: "alice@acme.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgID = "org-1"
	orgs.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "acme", Name: "Acme"}

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"ALICE@ACME.COM"}`)
	url := requireLookupOK(t, w)
	assert.Equal(t, "https://acme.app.example.com", url)
}

func TestLoginLookup_ResponseShape_Uniform(t *testing.T) {
	// The load-bearing enumeration-safety property: every non-validation
	// branch returns the same JSON shape { redirectUrl: string }.
	// The string value differs but the shape must not.
	users := newLookupUserStore()
	users.users["alice@acme.com"] = &types.User{ID: "user-1", Email: "alice@acme.com"}

	orgs := newLookupOrgStore()
	orgs.userOrgID = "org-1"
	orgs.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "acme", Name: "Acme"}

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	cases := []struct {
		name  string
		email string
	}{
		{"found", "alice@acme.com"},
		{"not_found", "nobody@example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doRequest(r, "POST", "/api/v1/auth/lookup", `{"email":"`+tc.email+`"}`)
			require.Equal(t, http.StatusOK, w.Code)
			// Decode into a map to assert shape-only (single string field).
			var m map[string]interface{}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &m))
			require.Len(t, m, 1, "response must have exactly one field")
			require.Contains(t, m, "redirectUrl")
			_, ok := m["redirectUrl"].(string)
			require.True(t, ok, "redirectUrl must be a string")
		})
	}
}

func TestLoginLookup_BodySizeLimit_RejectsLargeBody(t *testing.T) {
	users := newLookupUserStore()
	orgs := newLookupOrgStore()

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)

	// Build a body over 1 MiB — must be rejected before any DB work.
	big := strings.Repeat("a", (1<<20)+1)
	body := `{"email":"` + big + `"}`

	w := doRequest(r, "POST", "/api/v1/auth/lookup", body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- subdomainFor unit tests (covers the helper directly) ---

func TestSubdomainFor_WithBase_ReturnsSubdomainURL(t *testing.T) {
	assert.Equal(t, "https://acme.app.example.com", subdomainFor("acme", "app.example.com"))
}

func TestSubdomainFor_WithLeadingDotBase_Normalizes(t *testing.T) {
	// Defensive: if an operator sets baseDomain=".app.example.com" the helper
	// must not produce "acme..app.example.com".
	assert.Equal(t, "https://acme.app.example.com", subdomainFor("acme", ".app.example.com"))
}

func TestSubdomainFor_EmptyBase_ReturnsDirectSSOStartURL(t *testing.T) {
	// Forward-compat fallback: when subdomain routing is disabled, redirect
	// to the direct SSO start URL — never tell a found user "not found".
	assert.Equal(t, "/api/v1/auth/sso/acme/start", subdomainFor("acme", ""))
}

// --- benchmark: timing measurement for the no-pad rationale ---
//
// Per US-54.1 acceptance criteria and DECISIONS.md D54-2: the "no timing pad"
// decision rests on the claim that the additional indexed lookup in the
// found branch (GetUserOrgID + GetOrg) is fast relative to network jitter.
// These benchmarks measure the resolver's two branches under the in-memory
// fake store — the absolute lower bound (real Postgres adds ~0.5-2ms/round-trip).
// The implementation worklog MUST record real Postgres p50/p99 numbers; these
// fakes only confirm the resolver itself adds negligible overhead.

func BenchmarkLoginLookup_Found(b *testing.B) {
	users := newLookupUserStore()
	users.users["alice@acme.com"] = &types.User{ID: "user-1", Email: "alice@acme.com"}
	orgs := newLookupOrgStore()
	orgs.userOrgID = "org-1"
	orgs.orgs["org-1"] = &types.Organization{ID: "org-1", Slug: "acme", Name: "Acme"}

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)
	body := []byte(`{"email":"alice@acme.com"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/api/v1/auth/lookup", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status=%d", w.Code)
		}
	}
}

func BenchmarkLoginLookup_NotFound(b *testing.B) {
	users := newLookupUserStore() // empty
	orgs := newLookupOrgStore()

	h := NewLoginDiscoveryHandler(users, orgs, "app.example.com", &lookupCaptureLogger{})
	r := setupLoginLookupRouter(h)
	body := []byte(`{"email":"nobody@example.com"}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/api/v1/auth/lookup", bytes.NewBuffer(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status=%d", w.Code)
		}
	}
}
