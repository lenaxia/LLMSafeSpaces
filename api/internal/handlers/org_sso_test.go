// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/sso"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// --- mock store for SSO CRUD + discovery ---
//
// mockSSOStore implements BOTH the handler's ssoStore interface AND the
// sso.Service orgStore interface. The SAME instance is handed to the service
// and the handler so a Put (which writes through the service) is visible to a
// subsequent Get (which reads through the handler) — mirroring production,
// where one *PgOrgStore serves both.

type mockSSOStore struct {
	mu        sync.Mutex
	configs   map[string]*types.OrgSSOConfig
	slugToOrg map[string]*types.Organization
	members   map[string]map[string]*types.OrgMember
	domains   []types.SSODomain
	getErr    error
	deleteErr error
	auditLog  []string
}

func newMockSSOStore() *mockSSOStore {
	return &mockSSOStore{
		configs:   map[string]*types.OrgSSOConfig{},
		slugToOrg: map[string]*types.Organization{},
		members:   map[string]map[string]*types.OrgMember{},
	}
}
func (m *mockSSOStore) GetSSOConfig(_ context.Context, orgID string) (*types.OrgSSOConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.configs[orgID], nil
}
func (m *mockSSOStore) UpsertSSOConfig(_ context.Context, cfg *types.OrgSSOConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[cfg.OrgID] = cfg
	return nil
}
func (m *mockSSOStore) DeleteSSOConfig(_ context.Context, orgID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.configs, orgID)
	return nil
}
func (m *mockSSOStore) SetDomainVerified(_ context.Context, orgID, domain string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[orgID]
	if !ok || cfg == nil {
		return false, nil
	}
	claimed := false
	for _, d := range cfg.ClaimedDomains {
		if strings.EqualFold(d, domain) {
			claimed = true
			break
		}
	}
	if !claimed {
		return false, nil
	}
	for _, d := range cfg.VerifiedDomains {
		if strings.EqualFold(d, domain) {
			return false, nil
		}
	}
	cfg.VerifiedDomains = append(cfg.VerifiedDomains, domain)
	return true, nil
}
func (m *mockSSOStore) RotateVerificationToken(_ context.Context, orgID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cfg, ok := m.configs[orgID]
	if !ok || cfg == nil {
		return "", fmt.Errorf("no sso config for org %s", orgID)
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	cfg.VerificationToken = fmt.Sprintf("%x", b)
	return cfg.VerificationToken, nil
}
func (m *mockSSOStore) GetOrgBySlug(_ context.Context, slug string) (*types.Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.slugToOrg[strings.ToLower(slug)], nil
}
func (m *mockSSOStore) GetOrgMember(_ context.Context, orgID, userID string) (*types.OrgMember, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mm, ok := m.members[orgID]; ok {
		return mm[userID], nil
	}
	return nil, nil
}
func (m *mockSSOStore) AddOrgMember(_ context.Context, orgID, userID string, role types.OrgRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.members[orgID] == nil {
		m.members[orgID] = map[string]*types.OrgMember{}
	}
	m.members[orgID][userID] = &types.OrgMember{OrgID: orgID, UserID: userID, Role: role}
	return nil
}
func (m *mockSSOStore) CountOrgAdmins(_ context.Context, orgID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, mm := range m.members[orgID] {
		if mm.Role == types.OrgRoleAdmin {
			n++
		}
	}
	return n, nil
}
func (m *mockSSOStore) UpdateOrgMemberRole(_ context.Context, orgID, userID string, role types.OrgRole) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.members[orgID] != nil && m.members[orgID][userID] != nil {
		m.members[orgID][userID].Role = role
	}
	return nil
}
func (m *mockSSOStore) ListSSODomains(_ context.Context) ([]types.SSODomain, error) {
	return m.domains, nil
}
func (m *mockSSOStore) CountSSOConfigs(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.configs), nil
}
func (m *mockSSOStore) LogOrgEvent(_ context.Context, _, _, action, _ string, _ map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditLog = append(m.auditLog, action)
	return nil
}

// newSSOServiceForHandler builds a real sso.Service with a static KEK + state
// key so the handler exercises the real encryption/cookie code paths. It shares
// the same store the handler reads from.
func newSSOServiceForHandler(t *testing.T, store *mockSSOStore, users *mockSSOHandlerUserStore, redirectBase string) *sso.Service {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte('a' + i)
	}
	kp, err := secrets.NewStaticKeyProvider(key)
	require.NoError(t, err)
	stateKey := []byte("handler-test-state-key-1234567890")
	svc, err := sso.New(store, users, sso.ServiceConfig{
		TokenIssuer:     &stubIssuer{tok: "jwt-from-handler"},
		KeyProvider:     kp,
		StateKey:        stateKey,
		TokenTTL:        3600_000_000_000, // 1h in ns
		StateTTL:        10 * 1000_000_000,
		RedirectBaseURL: redirectBase,
	})
	require.NoError(t, err)
	return svc
}

type stubIssuer struct{ tok string }

func (s *stubIssuer) GenerateToken(string) (string, error) { return s.tok, nil }

type mockSSOHandlerUserStore struct{ users map[string]*types.User }

func newMockSSOHandlerUserStore() *mockSSOHandlerUserStore {
	return &mockSSOHandlerUserStore{users: map[string]*types.User{}}
}
func (m *mockSSOHandlerUserStore) GetUserByEmail(_ context.Context, email string) (*types.User, error) {
	return m.users[strings.ToLower(email)], nil
}
func (m *mockSSOHandlerUserStore) CreateUser(_ context.Context, u *types.User) error {
	m.users[strings.ToLower(u.Email)] = u
	return nil
}

// buildSSOHandler wires the handler + router. ONE store serves both the service
// and the handler so writes are visible to reads. Also returns the service so
// tests can inject a fake DNS resolver for domain-verification tests.
func buildSSOHandler(t *testing.T) (*SSOHandler, *mockSSOStore, *mockSSOHandlerUserStore, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	svc := newSSOServiceForHandler(t, store, users, "https://api.test.local")
	h := NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", "", "https://app.test.local", nil)

	r := gin.New()
	r.GET("/api/v1/orgs/:id/sso", h.Get)
	r.PUT("/api/v1/orgs/:id/sso", h.Put)
	r.DELETE("/api/v1/orgs/:id/sso", h.Delete)
	r.POST("/api/v1/orgs/:id/sso/domains/:domain/verify", h.VerifyDomain)
	r.POST("/api/v1/orgs/:id/sso/verification-token/rotate", h.RotateToken)
	r.GET("/api/v1/auth/sso/domains", h.Domains)
	r.GET("/api/v1/auth/sso/:orgSlug/start", h.Start)
	r.GET("/api/v1/auth/sso/:orgSlug/callback", h.Callback)
	return h, store, users, r
}

// --- CRUD tests ---

func TestSSOHandler_Get_NoConfigReturnsDefault(t *testing.T) {
	_, _, _, r := buildSSOHandler(t)

	w := doRequest(r, "GET", "/api/v1/orgs/org-1/sso", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp types.OrgSSOConfigResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.False(t, resp.HasSecret)
	require.True(t, resp.AutoProvision)
}

func TestSSOHandler_Get_ReturnsConfigWithoutSecret(t *testing.T) {
	_, store, _, r := buildSSOHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{
		OrgID: "org-1", DiscoveryURL: "https://idp", ClientID: "cid",
		ClientSecret: []byte("encrypted-blob"), AutoProvision: false,
		ClaimedDomains:   []string{"acme.com"},
		GroupRoleMapping: map[string]types.OrgRole{"a": types.OrgRoleAdmin},
	}

	w := doRequest(r, "GET", "/api/v1/orgs/org-1/sso", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp types.OrgSSOConfigResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.HasSecret)
	require.Equal(t, "cid", resp.ClientID)
	require.False(t, resp.AutoProvision)
	// Encrypted secret must NEVER appear in the response body.
	require.NotContains(t, w.Body.String(), "encrypted-blob")
}

func TestSSOHandler_Put_EncryptsSecretAndPersists(t *testing.T) {
	_, store, _, r := buildSSOHandler(t)

	body := `{"discoveryUrl":"https://idp.example/.well-known/openid-configuration","clientId":"cid","clientSecret":"plaintext-secret","claimedDomains":["@ACME.com"],"autoProvision":false,"groupRoleMapping":{"admins":"admin"}}`
	w := doRequest(r, "PUT", "/api/v1/orgs/org-1/sso", body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	stored, ok := store.configs["org-1"]
	require.True(t, ok)
	require.Equal(t, "cid", stored.ClientID)
	require.Equal(t, []string{"acme.com"}, stored.ClaimedDomains, "domains normalized to bare lowercase")
	require.False(t, stored.AutoProvision)
	require.NotEqual(t, "plaintext-secret", string(stored.ClientSecret), "secret must be encrypted at rest")
	require.NotEmpty(t, stored.ClientSecret)
	require.Len(t, store.auditLog, 1)
}

func TestSSOHandler_Put_MissingSecretOnFirstConfig_400(t *testing.T) {
	_, _, _, r := buildSSOHandler(t)
	body := `{"discoveryUrl":"https://idp","clientId":"cid"}`
	w := doRequest(r, "PUT", "/api/v1/orgs/org-1/sso", body)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSSOHandler_Put_PartialUpdateKeepsExistingSecret(t *testing.T) {
	_, store, _, r := buildSSOHandler(t)
	// First, persist a config WITH a secret.
	body1 := `{"discoveryUrl":"https://idp","clientId":"cid","clientSecret":"first-secret"}`
	w1 := doRequest(r, "PUT", "/api/v1/orgs/org-1/sso", body1)
	require.Equal(t, http.StatusOK, w1.Code)
	firstSecret := store.configs["org-1"].ClientSecret

	// Update WITHOUT a clientSecret → existing encrypted blob must be retained.
	body2 := `{"discoveryUrl":"https://idp","clientId":"cid-renamed","autoProvision":false}`
	w2 := doRequest(r, "PUT", "/api/v1/orgs/org-1/sso", body2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	require.Equal(t, firstSecret, store.configs["org-1"].ClientSecret, "existing secret retained on partial update")
	require.Equal(t, "cid-renamed", store.configs["org-1"].ClientID)
	require.False(t, store.configs["org-1"].AutoProvision)
}

func TestSSOHandler_Put_InvalidRole_400(t *testing.T) {
	_, _, _, r := buildSSOHandler(t)
	body := `{"discoveryUrl":"https://idp","clientId":"cid","clientSecret":"s","groupRoleMapping":{"x":"superuser"}}`
	w := doRequest(r, "PUT", "/api/v1/orgs/org-1/sso", body)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSSOHandler_Put_BadBody_400(t *testing.T) {
	_, _, _, r := buildSSOHandler(t)
	w := doRequest(r, "PUT", "/api/v1/orgs/org-1/sso", `{not json`)
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSSOHandler_Delete(t *testing.T) {
	_, store, _, r := buildSSOHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{OrgID: "org-1", ClientSecret: []byte("x")}
	w := doRequest(r, "DELETE", "/api/v1/orgs/org-1/sso", "")
	require.Equal(t, http.StatusNoContent, w.Code)
	_, ok := store.configs["org-1"]
	require.False(t, ok)
	require.Contains(t, store.auditLog, "sso.delete")
}

func TestSSOHandler_Domains(t *testing.T) {
	_, store, _, r := buildSSOHandler(t)
	store.domains = []types.SSODomain{{Domain: "@acme.com", OrgSlug: "acme", OrgName: "Acme"}}
	w := doRequest(r, "GET", "/api/v1/auth/sso/domains", "")
	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	require.Contains(t, body, "@acme.com")
	require.Contains(t, body, "Acme")
}

// --- start/callback flow tests (full integration through the handler) ---

// fakeIdP is duplicated here from the sso package test (different package) so
// the handler test can drive a real OIDC provider end-to-end.
type handlerFakeIdP struct {
	t        *testing.T
	server   *httptest.Server
	privKey  rsaKey
	clientID string
	tokenFn  func(string) (map[string]any, error)
	codes    map[string]string
	mu       sync.Mutex
}

type rsaKey struct {
	priv interface{}
	sign func(t *testing.T, claims map[string]any, iss, aud string) (string, error)
}

func newHandlerFakeIdP(t *testing.T, clientID string) *handlerFakeIdP {
	t.Helper()
	// Use an RSA key via stdlib + golang-jwt (already a dependency).
	priv := mustRSA(t)
	fp := &handlerFakeIdP{
		t: t, clientID: clientID, codes: map[string]string{},
		privKey: rsaKey{priv: priv, sign: func(tt *testing.T, claims map[string]any, iss, aud string) (string, error) {
			return signRS256(tt, priv, iss, aud, clientID, claims)
		}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer":"%s","authorization_endpoint":"%s/authorize","token_endpoint":"%s/token","jwks_uri":"%s/jwks","id_token_signing_alg_values_supported":["RS256"],"response_types_supported":["code"],"subject_types_supported":["public"]}`, fp.server.URL, fp.server.URL, fp.server.URL, fp.server.URL)
	})
	mux.HandleFunc("/jwks", fp.handleJWKS)
	mux.HandleFunc("/token", fp.handleToken)
	fp.server = httptest.NewServer(mux)
	return fp
}

func (f *handlerFakeIdP) issuer() string { return f.server.URL }
func (f *handlerFakeIdP) close()         { f.server.Close() }

func (f *handlerFakeIdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	body := jwksJSON(f.t, f.privKey.priv)
	_, _ = fmt.Fprint(w, body)
}

func (f *handlerFakeIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	code := r.FormValue("code")
	f.mu.Lock()
	f.codes[code] = r.FormValue("code_verifier")
	f.mu.Unlock()
	claims, err := f.tokenFn(code)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	idToken, err := f.privKey.sign(f.t, claims, f.server.URL, f.clientID)
	if err != nil {
		http.Error(w, "sign failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"access_token":"a","token_type":"Bearer","expires_in":3600,"id_token":%q}`, idToken)
}

func TestSSOHandler_Start_RedirectsAndSetsCookie(t *testing.T) {
	h, store, _, r := buildSSOHandler(t)
	store.slugToOrg["acme"] = &types.Organization{ID: "org-acme", Slug: "acme", Status: types.OrgStatusActive}
	blob, err := h.svc.EncryptClientSecret(context.Background(), "secret")
	require.NoError(t, err)
	store.configs["org-acme"] = &types.OrgSSOConfig{
		OrgID: "org-acme", DiscoveryURL: "https://placeholder", ClientID: "cid", ClientSecret: blob,
		AutoProvision: true,
	}

	// Use a real fake IdP so discovery succeeds.
	idp := newHandlerFakeIdP(t, "cid")
	defer idp.close()
	store.configs["org-acme"].DiscoveryURL = idp.issuer()

	w := doRequest(r, "GET", "/api/v1/auth/sso/acme/start", "")
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	loc := w.Header().Get("Location")
	require.NotEmpty(t, loc)
	u, err := url.Parse(loc)
	require.NoError(t, err)
	require.Equal(t, "/authorize", u.Path)
	require.Equal(t, "S256", u.Query().Get("code_challenge_method"))

	// State cookie set.
	var cookieHeader string
	for _, c := range w.Result().Cookies() {
		if c.Name == h.svc.CookieName() {
			cookieHeader = c.Value
		}
	}
	require.NotEmpty(t, cookieHeader, "state cookie must be set")
	require.Equal(t, u.Query().Get("state"), stateFromCookie(t, h, cookieHeader))
}

func TestSSOHandler_Start_NoConfig_RedirectsWithError(t *testing.T) {
	_, store, _, r := buildSSOHandler(t)
	store.slugToOrg["acme"] = &types.Organization{ID: "org-acme", Slug: "acme"}
	// No SSO config.
	w := doRequest(r, "GET", "/api/v1/auth/sso/acme/start", "")
	require.Equal(t, http.StatusFound, w.Code)
	loc := w.Header().Get("Location")
	require.Contains(t, loc, "https://app.test.local")
	require.Contains(t, loc, "sso=not_configured")
}

func TestSSOHandler_Callback_SuccessSetsSessionCookieAndRedirects(t *testing.T) {
	h, store, users, r := buildSSOHandler(t)
	idp := newHandlerFakeIdP(t, "cid")
	defer idp.close()
	store.slugToOrg["acme"] = &types.Organization{ID: "org-acme", Slug: "acme", Status: types.OrgStatusActive}
	blob, err := h.svc.EncryptClientSecret(context.Background(), "secret")
	require.NoError(t, err)
	store.configs["org-acme"] = &types.OrgSSOConfig{
		OrgID: "org-acme", DiscoveryURL: idp.issuer(), ClientID: "cid", ClientSecret: blob, AutoProvision: true,
	}
	idp.tokenFn = func(string) (map[string]any, error) {
		return map[string]any{"email": "zoe@acme.com"}, nil
	}

	// 1. Start to obtain the signed state cookie + the IdP state.
	wStart := doRequest(r, "GET", "/api/v1/auth/sso/acme/start", "")
	require.Equal(t, http.StatusFound, wStart.Code)
	startURL, _ := url.Parse(wStart.Header().Get("Location"))
	state := startURL.Query().Get("state")
	var cookieVal string
	for _, c := range wStart.Result().Cookies() {
		if c.Name == h.svc.CookieName() {
			cookieVal = c.Value
		}
	}
	require.NotEmpty(t, cookieVal)

	// 2. Drive the callback with code + state + the cookie.
	cbURL := "/api/v1/auth/sso/acme/callback?code=the-code&state=" + state
	req := httptest.NewRequest("GET", cbURL, nil)
	req.AddCookie(&http.Cookie{Name: h.svc.CookieName(), Value: cookieVal})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	loc := w.Header().Get("Location")
	require.True(t, strings.HasPrefix(loc, "https://app.test.local"), "redirects to frontend: %s", loc)
	require.Contains(t, loc, "sso=success")

	// Session JWT cookie set.
	var sessionCookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == "lsp_session" {
			sessionCookie = c.Value
		}
	}
	require.Equal(t, "jwt-from-handler", sessionCookie)

	// User auto-provisioned.
	_, ok := users.users["zoe@acme.com"]
	require.True(t, ok)
}

func TestSSOHandler_Callback_AutoProvisionOff_RedirectsWithError(t *testing.T) {
	h, store, _, r := buildSSOHandler(t)
	idp := newHandlerFakeIdP(t, "cid")
	defer idp.close()
	store.slugToOrg["acme"] = &types.Organization{ID: "org-acme", Slug: "acme", Status: types.OrgStatusActive}
	blob, _ := h.svc.EncryptClientSecret(context.Background(), "secret")
	store.configs["org-acme"] = &types.OrgSSOConfig{
		OrgID: "org-acme", DiscoveryURL: idp.issuer(), ClientID: "cid", ClientSecret: blob, AutoProvision: false,
	}
	idp.tokenFn = func(string) (map[string]any, error) { return map[string]any{"email": "nope@acme.com"}, nil }

	wStart := doRequest(r, "GET", "/api/v1/auth/sso/acme/start", "")
	startURL, _ := url.Parse(wStart.Header().Get("Location"))
	state := startURL.Query().Get("state")
	var cookieVal string
	for _, c := range wStart.Result().Cookies() {
		if c.Name == h.svc.CookieName() {
			cookieVal = c.Value
		}
	}

	req := httptest.NewRequest("GET", "/api/v1/auth/sso/acme/callback?code=c&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: h.svc.CookieName(), Value: cookieVal})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusFound, w.Code)
	require.Contains(t, w.Header().Get("Location"), "sso=provisioning_disabled")
}

// TestSSOHandler_Callback_UnverifiedEmail_RedirectsWithError is the E2E wiring
// test for the F8 email_unverified error arm: an unverified-email callback must
// flow Callback → errorReason → frontend redirect with sso=email_unverified.
// Mirrors the provisioning_disabled test above so every errorReason arm is
// locked end-to-end (README E2E Wiring Verification requirement).
func TestSSOHandler_Callback_UnverifiedEmail_RedirectsWithError(t *testing.T) {
	h, store, _, r := buildSSOHandler(t)
	idp := newHandlerFakeIdP(t, "cid")
	defer idp.close()
	store.slugToOrg["acme"] = &types.Organization{ID: "org-acme", Slug: "acme", Status: types.OrgStatusActive}
	blob, _ := h.svc.EncryptClientSecret(context.Background(), "secret")
	store.configs["org-acme"] = &types.OrgSSOConfig{
		OrgID: "org-acme", DiscoveryURL: idp.issuer(), ClientID: "cid", ClientSecret: blob, AutoProvision: true,
	}
	// email_verified=false must trigger the F8 gate BEFORE any account binding.
	idp.tokenFn = func(string) (map[string]any, error) {
		return map[string]any{"email": "attacker@acme.com", "name": "Attacker", "email_verified": false}, nil
	}

	wStart := doRequest(r, "GET", "/api/v1/auth/sso/acme/start", "")
	startURL, _ := url.Parse(wStart.Header().Get("Location"))
	state := startURL.Query().Get("state")
	var cookieVal string
	for _, c := range wStart.Result().Cookies() {
		if c.Name == h.svc.CookieName() {
			cookieVal = c.Value
		}
	}

	req := httptest.NewRequest("GET", "/api/v1/auth/sso/acme/callback?code=c&state="+state, nil)
	req.AddCookie(&http.Cookie{Name: h.svc.CookieName(), Value: cookieVal})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusFound, w.Code, "unverified email must redirect (not 500)")
	require.Contains(t, w.Header().Get("Location"), "sso=email_unverified",
		"the email_unverified errorReason arm must be wired through to the redirect URL (F8)")
}

// stateFromCookie decodes the signed state cookie to read back the embedded
// state, mirroring what the service stores. It re-derives via the service's
// verify path so the test asserts against the real value.
func stateFromCookie(t *testing.T, h *SSOHandler, cookieVal string) string {
	t.Helper()
	// The cookie format is "<payload-b64>.<sig-b64>"; we read the payload
	// directly (the service verifies HMAC, but here we only need the state).
	parts := strings.SplitN(cookieVal, ".", 2)
	require.Len(t, parts, 2)
	decoded, err := base64urlDecode(parts[0])
	require.NoError(t, err)
	var p struct {
		State string `json:"s"`
	}
	require.NoError(t, json.Unmarshal(decoded, &p))
	return p.State
}

// --- F11: redirect URL must not be derived from forwarded headers ---

// capturingSSOLogger records Warn calls so a test can assert the F11 warning
// fires when resolveCallbackURL refuses to trust X-Forwarded-* headers
// (RedirectBaseURL unset). Implements ssoLogger.
type capturingSSOLogger struct {
	mu    sync.Mutex
	warns []string
}

func (l *capturingSSOLogger) Warn(msg string, _ ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg)
}

// TestE2E_SSO_ResolveCallbackURL_FailsWhenRedirectBaseURLUnset proves the F11
// fix: when RedirectBaseURL is unset, resolveCallbackURL refuses to derive the
// callback URL from X-Forwarded-Proto + Host and instead returns
// ErrRedirectBaseURLNotSet. A regression that re-introduced header derivation
// would reopen the trust gap (an attacker at a misconfigured reverse proxy
// could steer the IdP redirect).
func TestE2E_SSO_ResolveCallbackURL_FailsWhenRedirectBaseURLUnset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	// RedirectBaseURL = "" → the unset path.
	svc := newSSOServiceForHandler(t, store, users, "")
	log := &capturingSSOLogger{}
	h := NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", "", "https://app.test.local", log)

	// Simulate a request with forwarded headers (the attacker-influenceable
	// surface F11 documents). The handler MUST NOT use them.
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/auth/sso/acme/start", nil)
	c.Request.Host = "evil.example.com"
	c.Request.Header.Set("X-Forwarded-Proto", "https")

	url, err := h.resolveCallbackURL(c, "acme")

	// The URL must be empty (no header-derived value) and the error must be
	// the sentinel so handlers can map it distinctly.
	require.Empty(t, url, "no callback URL must be derived from forwarded headers when RedirectBaseURL is unset")
	require.ErrorIs(t, err, sso.ErrRedirectBaseURLNotSet,
		"unset RedirectBaseURL must return ErrRedirectBaseURLNotSet, not a header-derived URL")

	// The warning must have fired — this is the operator signal.
	require.Len(t, log.warns, 1, "exactly one warning expected when refusing forwarded headers")
	require.Contains(t, log.warns[0], "redirect base URL is not configured",
		"warning must explain the missing config so operators can grep for it")
}

// TestE2E_SSO_ResolveCallbackURL_NoWarnWhenRedirectBaseURLSet proves the
// complement: when RedirectBaseURL IS set, the canonical URL is returned with
// no error and no warning (the trust gap is closed).
func TestE2E_SSO_ResolveCallbackURL_NoWarnWhenRedirectBaseURLSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	svc := newSSOServiceForHandler(t, store, users, "https://api.production.local")
	log := &capturingSSOLogger{}
	h := NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", "", "https://app.test.local", log)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("GET", "/api/v1/auth/sso/acme/start", nil)

	url, err := h.resolveCallbackURL(c, "acme")
	require.NoError(t, err)
	require.Contains(t, url, "https://api.production.local/api/v1/auth/sso/acme/callback")
	require.Empty(t, log.warns, "no warning when RedirectBaseURL is set (trust gap closed)")
}

// TestE2E_SSO_Start_UnsetRedirectBaseURL_RedirectsToFrontend verifies the
// Start handler redirects to the frontend with a config_error token instead
// of returning JSON or trusting X-Forwarded-* headers to derive the callback URL.
func TestE2E_SSO_Start_UnsetRedirectBaseURL_RedirectsToFrontend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	svc := newSSOServiceForHandler(t, store, users, "")
	h := NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", "", "https://app.test.local", &capturingSSOLogger{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "orgSlug", Value: "acme"}}
	c.Request = httptest.NewRequest("GET", "/api/v1/auth/sso/acme/start", nil)

	h.Start(c)

	require.Equal(t, http.StatusFound, w.Code, "misconfiguration must redirect to frontend with error token")
	loc := w.Header().Get("Location")
	require.Contains(t, loc, "https://app.test.local", "must redirect to the configured frontend")
	require.Contains(t, loc, "sso=config_error", "must carry the config_error token")
}

// TestE2E_SSO_Callback_UnsetRedirectBaseURL_RedirectsToFrontend verifies the
// Callback handler redirects to the frontend with a config_error reason instead
// of trusting headers to finish the token exchange. The browser is mid-flow
// (IdP has already redirected back), so a JSON error is not possible; a
// frontend redirect with an error token is the correct UX.
func TestE2E_SSO_Callback_UnsetRedirectBaseURL_RedirectsToFrontend(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	svc := newSSOServiceForHandler(t, store, users, "")
	h := NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", "", "https://app.test.local", &capturingSSOLogger{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "orgSlug", Value: "acme"}}
	c.Request = httptest.NewRequest("GET", "/api/v1/auth/sso/acme/callback?code=x&state=y", nil)

	h.Callback(c)

	require.Equal(t, http.StatusFound, w.Code)
	loc := w.Header().Get("Location")
	require.Contains(t, loc, "https://app.test.local", "must redirect to the configured frontend")
	require.Contains(t, loc, "sso=config_error", "must carry the config_error token so the frontend surfaces a failure")
}

// --- DNS domain verification handler tests (D17 Q-S2) ---

// buildVerifyHandler wires a handler with a fake DNS resolver so verify
// endpoint tests don't depend on real DNS. Returns the resolver so the test
// can preset TXT records.
func buildVerifyHandler(t *testing.T) (*SSOHandler, *mockSSOStore, *gin.Engine, *fakeVerifyDNSResolver) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	svc := newSSOServiceForHandler(t, store, users, "https://api.test.local")
	dns := &fakeVerifyDNSResolver{records: map[string][]string{}}
	svc.SetDNSResolver(dns)
	h := NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", "", "https://app.test.local", nil)
	r := gin.New()
	r.POST("/api/v1/orgs/:id/sso/domains/:domain/verify", h.VerifyDomain)
	r.POST("/api/v1/orgs/:id/sso/verification-token/rotate", h.RotateToken)
	r.GET("/api/v1/orgs/:id/sso", h.Get)
	return h, store, r, dns
}

// fakeVerifyDNSResolver mirrors sso.fakeDNSResolver but lives in the handlers
// test package (sso's internal fakes are not exported).
type fakeVerifyDNSResolver struct {
	records map[string][]string
	err     error
}

func (f *fakeVerifyDNSResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.records[name], nil
}

func TestSSOHandler_VerifyDomain_Success(t *testing.T) {
	_, store, r, dns := buildVerifyHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{
		OrgID:             "org-1",
		ClaimedDomains:    []string{"acme.com"},
		VerificationToken: "tok-abc",
	}
	dns.records["_llmsafespaces-verify.acme.com"] = []string{"tok-abc"}

	w := doRequest(r, "POST", "/api/v1/orgs/org-1/sso/domains/acme.com/verify", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp sso.VerifyDomainResult
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(t, resp.Verified)
	require.Equal(t, "acme.com", resp.Domain)
}

func TestSSOHandler_VerifyDomain_DNSNotMatching_422(t *testing.T) {
	_, store, r, dns := buildVerifyHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{
		OrgID:             "org-1",
		ClaimedDomains:    []string{"acme.com"},
		VerificationToken: "correct",
	}
	dns.records["_llmsafespaces-verify.acme.com"] = []string{"wrong"}

	w := doRequest(r, "POST", "/api/v1/orgs/org-1/sso/domains/acme.com/verify", "")
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestSSOHandler_VerifyDomain_NotClaimed_400(t *testing.T) {
	_, store, r, _ := buildVerifyHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{
		OrgID:             "org-1",
		ClaimedDomains:    []string{"acme.com"},
		VerificationToken: "tok",
	}

	w := doRequest(r, "POST", "/api/v1/orgs/org-1/sso/domains/other.com/verify", "")
	require.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSSOHandler_VerifyDomain_NoToken_409(t *testing.T) {
	_, store, r, _ := buildVerifyHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{
		OrgID:          "org-1",
		ClaimedDomains: []string{"acme.com"},
	}

	w := doRequest(r, "POST", "/api/v1/orgs/org-1/sso/domains/acme.com/verify", "")
	require.Equal(t, http.StatusConflict, w.Code)
}

func TestSSOHandler_VerifyDomain_NoSSOConfig_404(t *testing.T) {
	_, _, r, _ := buildVerifyHandler(t)

	w := doRequest(r, "POST", "/api/v1/orgs/ghost/sso/domains/acme.com/verify", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestSSOHandler_RotateToken_Success(t *testing.T) {
	_, store, r, _ := buildVerifyHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{OrgID: "org-1"}

	w := doRequest(r, "POST", "/api/v1/orgs/org-1/sso/verification-token/rotate", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp["verificationToken"], 32)
}

func TestSSOHandler_RotateToken_NoSSOConfig_404(t *testing.T) {
	_, _, r, _ := buildVerifyHandler(t)

	w := doRequest(r, "POST", "/api/v1/orgs/ghost/sso/verification-token/rotate", "")
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestSSOHandler_Get_ReturnsVerifiedDomainsAndToken(t *testing.T) {
	_, store, r, _ := buildVerifyHandler(t)
	store.configs["org-1"] = &types.OrgSSOConfig{
		OrgID:             "org-1",
		DiscoveryURL:      "https://idp",
		ClientID:          "cid",
		ClientSecret:      []byte("enc"),
		ClaimedDomains:    []string{"acme.com", "acme.io"},
		VerifiedDomains:   []string{"acme.com"},
		VerificationToken: "tok-xyz",
		AutoProvision:     true,
	}

	w := doRequest(r, "GET", "/api/v1/orgs/org-1/sso", "")
	require.Equal(t, http.StatusOK, w.Code)
	var resp types.OrgSSOConfigResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Equal(t, []string{"acme.com", "acme.io"}, resp.ClaimedDomains)
	require.Equal(t, []string{"acme.com"}, resp.VerifiedDomains)
	require.Equal(t, "tok-xyz", resp.VerificationToken)
}

// TestSSOHandler_Callback_CookieDomainSet verifies that when the SSO handler
// is constructed with a non-empty cookieDomain, the session cookie set on
// successful callback carries the Domain attribute. Without this, the cookie
// would be host-only and invisible to org subdomains — breaking Epic 54.
func TestSSOHandler_Callback_CookieDomainSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMockSSOStore()
	users := newMockSSOHandlerUserStore()
	svc := newSSOServiceForHandler(t, store, users, "https://api.test.local")
	// Construct with cookieDomain=".app.example.com" — simulating subdomain routing enabled.
	h := NewSSOHandler(svc, store, &mockOrgAuthService{userID: "admin-1"}, "lsp_session", ".app.example.com", "https://app.test.local", nil)

	idp := newHandlerFakeIdP(t, "cid")
	defer idp.close()
	store.slugToOrg["acme"] = &types.Organization{ID: "org-acme", Slug: "acme", Status: types.OrgStatusActive}
	blob, err := h.svc.EncryptClientSecret(context.Background(), "secret")
	require.NoError(t, err)
	store.configs["org-acme"] = &types.OrgSSOConfig{
		OrgID: "org-acme", DiscoveryURL: idp.issuer(), ClientID: "cid", ClientSecret: blob, AutoProvision: true,
	}
	idp.tokenFn = func(string) (map[string]any, error) {
		return map[string]any{"email": "zoe@acme.com"}, nil
	}

	r := gin.New()
	r.GET("/api/v1/auth/sso/:orgSlug/start", h.Start)
	r.GET("/api/v1/auth/sso/:orgSlug/callback", h.Callback)

	// 1. Start to get the state cookie.
	wStart := doRequest(r, "GET", "/api/v1/auth/sso/acme/start", "")
	require.Equal(t, http.StatusFound, wStart.Code)
	startURL, _ := url.Parse(wStart.Header().Get("Location"))
	state := startURL.Query().Get("state")
	var cookieVal string
	for _, c := range wStart.Result().Cookies() {
		if c.Name == h.svc.CookieName() {
			cookieVal = c.Value
		}
	}
	require.NotEmpty(t, cookieVal)

	// 2. Drive the callback.
	cbURL := "/api/v1/auth/sso/acme/callback?code=the-code&state=" + state
	req := httptest.NewRequest("GET", cbURL, nil)
	req.AddCookie(&http.Cookie{Name: h.svc.CookieName(), Value: cookieVal})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusFound, w.Code, w.Body.String())

	// The session cookie must carry Domain="app.example.com" (Go's cookie
	// parser strips the leading dot per RFC 6265 §4.1.2.3; the Set-Cookie
	// header itself contains ".app.example.com" which is what browsers see).
	var sessionCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "lsp_session" {
			sessionCookie = c
			break
		}
	}
	require.NotNil(t, sessionCookie, "session cookie must be set on callback")
	assert.Equal(t, "app.example.com", sessionCookie.Domain,
		"SSO callback cookie Domain must match the configured cookieDomain for subdomain routing")
}
