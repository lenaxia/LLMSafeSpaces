// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProvisionServerKEKKeys_DelegatesToKeyService verifies the auth-layer
// server-KEK provisioning wrapper delegates to the key service (Epic 58).
func TestProvisionServerKEKKeys_DelegatesToKeyService(t *testing.T) {
	svc, _, _ := newTestService(t)
	ks := &fakeKeyService{}
	svc.SetKeyService(ks)

	require.NoError(t, svc.ProvisionServerKEKKeys(context.Background(), "user-sso-1"))
	assert.Equal(t, []string{"user-sso-1"}, ks.serverKEKInitCalls, "InitializeUserKeysServerKEK must be called once for the user")
}

// TestProvisionServerKEKKeys_NoKeyService_Errors fails closed when no key
// service is wired rather than silently no-op'ing (a silent no-op would leave
// an SSO user with no DEK and no signal).
func TestProvisionServerKEKKeys_NoKeyService_Errors(t *testing.T) {
	svc, _, _ := newTestService(t)
	// keyService intentionally NOT wired.
	err := svc.ProvisionServerKEKKeys(context.Background(), "user-1")
	require.Error(t, err)
}

// TestIssueTokenAndUnlockDEK_ProvisionsWhenNoKeys is the SSO/passkey login
// completion: a user with no keys is provisioned a server-KEK DEK, then the new
// session is unlocked against it. Both calls must fire, and the token returned.
func TestIssueTokenAndUnlockDEK_ProvisionsWhenNoKeys(t *testing.T) {
	svc, _, _ := newTestService(t)
	ks := &fakeKeyService{hasKeysFn: func(_ context.Context, _ string) (bool, error) {
		return false, nil // new SSO user: no keys yet
	}}
	svc.SetKeyService(ks)

	tok, err := svc.IssueTokenAndUnlockDEK(context.Background(), "user-sso-1", time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, tok, "a valid session token must be returned")
	require.Len(t, ks.serverKEKInitCalls, 1, "server-KEK provisioning must run when the user has no keys")
	assert.Equal(t, "user-sso-1", ks.serverKEKInitCalls[0])
	require.Len(t, ks.unlockCalls, 1, "the new session must be unlocked")
	assert.Equal(t, "user-sso-1", ks.unlockCalls[0].UserID)
	assert.Equal(t, time.Hour, ks.unlockCalls[0].TTL)
}

// TestIssueTokenAndUnlockDEK_SkipsProvisionWhenKeysExist guards the
// idempotency/backfill path: an SSO user who already has a server-KEK DEK is
// NOT re-provisioned (which would discard their existing DEK + secrets).
func TestIssueTokenAndUnlockDEK_SkipsProvisionWhenKeysExist(t *testing.T) {
	svc, _, _ := newTestService(t)
	ks := &fakeKeyService{hasKeysFn: func(_ context.Context, _ string) (bool, error) {
		return true, nil // already provisioned
	}}
	svc.SetKeyService(ks)

	tok, err := svc.IssueTokenAndUnlockDEK(context.Background(), "user-sso-2", time.Hour)
	require.NoError(t, err)
	assert.NotEmpty(t, tok)
	assert.Empty(t, ks.serverKEKInitCalls, "must NOT re-provision a user who already has keys")
	require.Len(t, ks.unlockCalls, 1, "session must still be unlocked")
}

// TestIssueTokenAndUnlockDEK_ProvisionFailureStillReturnsToken documents the
// degradation contract: a provisioning failure must NOT fail the whole login
// (the token is still valid for non-secret operations); the user retries on
// next login. Mirrors Login's best-effort unlock-warning semantics.
func TestIssueTokenAndUnlockDEK_ProvisionFailureStillReturnsToken(t *testing.T) {
	svc, _, _ := newTestService(t)
	ks := &fakeKeyService{
		hasKeysFn:        func(_ context.Context, _ string) (bool, error) { return false, nil },
		serverKEKInitErr: assertAnError(t),
	}
	svc.SetKeyService(ks)

	tok, err := svc.IssueTokenAndUnlockDEK(context.Background(), "user-sso-3", time.Hour)
	require.NoError(t, err, "provisioning failure must not fail login")
	assert.NotEmpty(t, tok)
	assert.Empty(t, ks.unlockCalls, "unlock must be skipped when provisioning failed (no keys to unlock)")
}

// assertAnError returns a non-nil error for fake injection.
func assertAnError(t *testing.T) error {
	t.Helper()
	return errSentinel
}

var errSentinel = errSentinelErr{}

type errSentinelErr struct{}

func (errSentinelErr) Error() string { return "provisioning failed (injected)" }
