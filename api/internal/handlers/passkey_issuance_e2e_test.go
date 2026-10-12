// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/passkey"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// e2eCredStore is an in-memory passkey.Store for the issuance-gate e2e. The
// ceremony path (challenge generation, go-webauthn attestation/assertion
// verification, session-store) is REAL — this fake only persists credentials,
// exactly like the service-tier memStore in the passkey package's own tests.
type e2eCredStore struct {
	mu      sync.Mutex
	creds   []passkey.Credential
	codes   []passkey.RecoveryCode
	usedIDs map[string]bool
}

func newE2ECredStore() *e2eCredStore {
	return &e2eCredStore{usedIDs: map[string]bool{}}
}

func (s *e2eCredStore) ListCredentials(_ context.Context, _ string) ([]passkey.Credential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]passkey.Credential(nil), s.creds...), nil
}

func (s *e2eCredStore) GetCredentialByCredentialID(_ context.Context, _ []byte) (*passkey.Credential, error) {
	return nil, nil
}

func (s *e2eCredStore) CreateCredential(_ context.Context, c *passkey.Credential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = append(s.creds, *c)
	return nil
}

func (s *e2eCredStore) CreateCredentialAndRecoveryCodes(_ context.Context, c *passkey.Credential, hashes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds = append(s.creds, *c)
	for _, h := range hashes {
		s.codes = append(s.codes, passkey.RecoveryCode{CodeHash: h})
	}
	return nil
}

func (s *e2eCredStore) CreateRecoveryCodes(_ context.Context, _ string, hashes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range hashes {
		s.codes = append(s.codes, passkey.RecoveryCode{CodeHash: h})
	}
	return nil
}

func (s *e2eCredStore) ListAvailableRecoveryCodes(_ context.Context, _ string) ([]passkey.RecoveryCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []passkey.RecoveryCode
	for _, c := range s.codes {
		if c.UsedAt == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *e2eCredStore) ConsumeRecoveryCode(_ context.Context, _ string, codeHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.codes {
		if s.codes[i].CodeHash == codeHash && s.codes[i].UsedAt == nil {
			now := time.Now()
			s.codes[i].UsedAt = &now
			return nil
		}
	}
	return errE2ENoRecoveryCode
}

// errE2ENoRecoveryCode mirrors the not-found error the real store returns;
// the recovery path is not exercised by this e2e (unit tier covers Recover).
var errE2ENoRecoveryCode = &passkeyStoreError{"no matching recovery code"}

type passkeyStoreError struct{ msg string }

func (e *passkeyStoreError) Error() string { return e.msg }

func (s *e2eCredStore) UpdateCredentialAfterLogin(_ context.Context, id uuid.UUID, signCount uint32, lastUsed time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.creds {
		if s.creds[i].ID == id {
			s.creds[i].SignCount = signCount
			s.creds[i].LastUsedAt = &lastUsed
			return nil
		}
	}
	return nil
}

func (s *e2eCredStore) DeleteCredential(_ context.Context, _ string, _ uuid.UUID) error { return nil }

func (s *e2eCredStore) CountCredentials(_ context.Context, _ string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.creds), nil
}

// TestE2E_PasskeyIssuanceGates_ThroughRealCeremony is the #1652 item-1
// integration tier the review required: a FULL passkey ceremony (real
// go-webauthn crypto, real session/challenge store, real
// handlers.PasskeyHandler mounted at its production route paths) asserting
// that the account-state gates hold at the issuance source — the rejected
// user passes WebAuthn verification (the credential is genuinely theirs) and
// still receives NO token and NO session cookie.
func TestE2E_PasskeyIssuanceGates_ThroughRealCeremony(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer redisClient.Close()

	credStore := newE2ECredStore()
	users := &fakePasskeyUsers{users: map[string]*types.User{}}
	svc, err := passkey.New(passkey.ServiceConfig{
		RPID:      "localhost",
		RPName:    "Test",
		RPOrigins: []string{"https://localhost"},
		Store:     credStore,
		Users:     users,
		Sessions:  passkey.NewCacheSessionStore(redisClient),
	})
	require.NoError(t, err)

	auth := &fakePasskeyAuth{token: "e2e-jwt"}
	h := NewPasskeyHandler(svc, auth, users, time.Hour, "lsp_session", "")
	r := gin.New()
	r.POST("/passkey/register/begin", h.RegisterBegin)
	r.POST("/passkey/register/finish", h.RegisterFinish)
	r.POST("/passkey/login/begin", h.LoginBegin)
	r.POST("/passkey/login/finish", h.LoginFinish)

	doJSON := func(t *testing.T, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// === Phase 1: register a real credential through the real handler ===
	const email = "gates-e2e@test.com"
	regBegin := doJSON(t, "/passkey/register/begin", map[string]string{"email": email, "name": "Gates E2E"})
	require.Equal(t, http.StatusOK, regBegin.Code, "register/begin: %s", regBegin.Body.String())

	var regBeginResp passkey.BeginRegistrationOptions
	require.NoError(t, json.Unmarshal(regBegin.Body.Bytes(), &regBeginResp))
	challenge, ok := regBeginResp.Options["challenge"].(string)
	require.True(t, ok, "options must carry a challenge")
	require.NotEmpty(t, regBeginResp.SessionToken)

	device := passkey.NewTestAuthenticator()
	attResp, err := device.GenerateRegistrationResponse(challenge)
	require.NoError(t, err)

	regFinish := doJSON(t, "/passkey/register/finish", map[string]any{
		"sessionToken": regBeginResp.SessionToken,
		"email":        email,
		"name":         "Gates E2E",
		"response":     attResp,
	})
	require.Equal(t, http.StatusOK, regFinish.Code, "register/finish: %s", regFinish.Body.String())

	// Dev mode (no email verifier wired): the created user is healthy.
	created, _ := users.GetUserByEmail(context.Background(), email)
	require.NotNil(t, created)
	require.True(t, created.Active && created.EmailVerified && created.Status == types.UserStatusActive,
		"dev-mode registration must produce a healthy user, got %+v", created)

	// loginOnce runs a full login ceremony (fresh challenge + assertion) and
	// returns the LoginFinish recorder.
	loginOnce := func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		begin := doJSON(t, "/passkey/login/begin", map[string]string{"email": email})
		require.Equal(t, http.StatusOK, begin.Code, "login/begin: %s", begin.Body.String())
		var beginResp passkey.BeginLoginOptions
		require.NoError(t, json.Unmarshal(begin.Body.Bytes(), &beginResp))
		loginChallenge, ok := beginResp.Options["challenge"].(string)
		require.True(t, ok)
		assertion, err := device.GenerateAssertionResponse(loginChallenge)
		require.NoError(t, err)
		return doJSON(t, "/passkey/login/finish", map[string]any{
			"sessionToken": beginResp.SessionToken,
			"email":        email,
			"response":     assertion,
		})
	}

	// === Phase 2: healthy user — token + cookie both issued ===
	happy := loginOnce(t)
	require.Equal(t, http.StatusOK, happy.Code, "healthy login: %s", happy.Body.String())
	var happyBody map[string]any
	require.NoError(t, json.Unmarshal(happy.Body.Bytes(), &happyBody))
	assert.Equal(t, "e2e-jwt", happyBody["token"])
	assert.Contains(t, happy.Header().Get("Set-Cookie"), "lsp_session=e2e-jwt",
		"healthy login must set the session cookie")

	// === Phase 3: the gates — real assertion succeeds, issuance must not run ===
	cases := []struct {
		name       string
		mutate     func(u *types.User)
		wantStatus int
	}{
		{"suspended", func(u *types.User) { u.Status = types.UserStatusSuspended }, http.StatusUnauthorized},
		{"inactive", func(u *types.User) { u.Active = false }, http.StatusUnauthorized},
		{"unverified email", func(u *types.User) { u.EmailVerified = false }, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := users.GetUserByEmail(context.Background(), email)
			require.NotNil(t, u)
			tc.mutate(u)

			rec := loginOnce(t)
			assert.Equal(t, tc.wantStatus, rec.Code, "body: %s", rec.Body.String())

			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Empty(t, body["token"], "no token may be issued")
			assert.NotContains(t, rec.Header().Get("Set-Cookie"), "lsp_session=",
				"no session cookie may be set for a rejected account")
			// Restore health for the next case.
			u.Status = types.UserStatusActive
			u.Active = true
			u.EmailVerified = true
		})
	}
}
