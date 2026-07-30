// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sso

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// fakeKeyManager records the calls made by the SSO service's issueSession path.
type fakeKeyManager struct {
	issueProvisionUserID string
	issueUnlockUserID    string
	issueUnlockTTL       time.Duration
	issueToken           string
	issueErr             error
	provisionUserID      string
	provisionErr         error
}

func (f *fakeKeyManager) ProvisionServerKEKKeys(_ context.Context, userID string) error {
	f.provisionUserID = userID
	return f.provisionErr
}

func (f *fakeKeyManager) IssueTokenAndUnlockDEK(_ context.Context, userID string, ttl time.Duration) (string, error) {
	f.issueUnlockUserID = userID
	f.issueUnlockTTL = ttl
	if f.issueErr != nil {
		return "", f.issueErr
	}
	return f.issueToken, nil
}

func newIssueSessionService(t *testing.T, issuer TokenIssuer, km UserKeyManager) *Service {
	t.Helper()
	orgs := &fakeOrgStore{}
	users := &fakeUserStore{}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	kp, err := secrets.NewStaticKeyProvider(key)
	require.NoError(t, err)
	svc, err := New(orgs, users, ServiceConfig{
		TokenIssuer: issuer,
		KeyManager:  km,
		KeyProvider: kp,
		StateKey:    []byte("test-state-hmac-key-0123456789ab"),
		TokenTTL:    time.Hour,
	})
	require.NoError(t, err)
	return svc
}

// TestIssueSession_WithKeyManager routes through the key manager (Epic 58):
// the issued token comes from IssueTokenAndUnlockDEK, NOT the plain issuer, and
// the manager receives the configured token TTL.
func TestIssueSession_WithKeyManager(t *testing.T) {
	issuer := &fakeIssuer{tok: "plain-issuer-token"} // must NOT be used
	km := &fakeKeyManager{issueToken: "unlocked-session-token"}
	svc := newIssueSessionService(t, issuer, km)

	tok, err := svc.issueSession(context.Background(), "user-sso-1")
	require.NoError(t, err)
	assert.Equal(t, "unlocked-session-token", tok, "token must come from the key manager")
	assert.Equal(t, "user-sso-1", km.issueUnlockUserID)
	assert.Equal(t, time.Hour, km.issueUnlockTTL, "manager must receive the service token TTL")
}

// TestIssueSession_FallsBackToIssuerWithoutManager guards the pre-epic / test
// fallback: with no key manager wired, issueSession uses TokenIssuer directly.
func TestIssueSession_FallsBackToIssuerWithoutManager(t *testing.T) {
	issuer := &fakeIssuer{tok: "plain-issuer-token"}
	svc := newIssueSessionService(t, issuer, nil)

	tok, err := svc.issueSession(context.Background(), "user-1")
	require.NoError(t, err)
	assert.Equal(t, "plain-issuer-token", tok)
}

// TestNew_RejectsNilStores keeps the constructor's invariants honest after the
// struct gained the keyManager field (regression guard on wiring).
func TestNew_KeyManagerOptional(t *testing.T) {
	orgs := &fakeOrgStore{}
	users := &fakeUserStore{}
	kp, err := secrets.NewStaticKeyProvider(make([]byte, 32))
	require.NoError(t, err)
	// KeyManager omitted — must still construct (optional dependency).
	svc, err := New(orgs, users, ServiceConfig{
		TokenIssuer: &fakeIssuer{tok: "t"},
		KeyProvider: kp,
		StateKey:    []byte("test-state-hmac-key-0123456789ab"),
	})
	require.NoError(t, err)
	assert.Nil(t, svc.keyManager, "keyManager must default to nil when omitted")
}
