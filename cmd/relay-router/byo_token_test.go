// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestByoTokenMintVerifyRoundtrip(t *testing.T) {
	m := newByoTokenMinter([]byte("router-signing-key-0123456789abcdef"))
	now := time.Unix(1780000000, 0)
	m.clock = func() time.Time { return now }
	payload := byoTokenPayload{
		WorkspaceID:    "ws-123",
		ProviderSlug:   "zai",
		BaseURL:        "https://ai.thekao.cloud/v1",
		ModelAllowlist: []string{"glm-4.7", "glm-4.7-air"},
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(24 * time.Hour).Unix(),
		KeyID:          "hpke-g1",
	}

	token, err := m.Mint(payload)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, "lrt_"))

	got, err := m.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestByoTokenMintValidation(t *testing.T) {
	m := newByoTokenMinter([]byte("k"))
	now := time.Now()
	tests := []struct {
		name    string
		payload byoTokenPayload
	}{
		{"missing workspace", byoTokenPayload{ProviderSlug: "s", BaseURL: "u", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}},
		{"missing provider", byoTokenPayload{WorkspaceID: "w", BaseURL: "u", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}},
		{"missing baseURL", byoTokenPayload{WorkspaceID: "w", ProviderSlug: "s", IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}},
		{"exp before iat", byoTokenPayload{WorkspaceID: "w", ProviderSlug: "s", BaseURL: "u", IssuedAt: now.Unix(), ExpiresAt: now.Unix()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Mint(tc.payload)
			require.ErrorIs(t, err, ErrMalformedToken)
		})
	}
}

func TestByoTokenVerifyForged(t *testing.T) {
	minter := newByoTokenMinter([]byte("legit-key"))
	forger := newByoTokenMinter([]byte("attacker-key"))
	now := time.Now()
	token, err := forger.Mint(byoTokenPayload{
		WorkspaceID: "ws-1", ProviderSlug: "s", BaseURL: "https://up.example",
		IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(), KeyID: "hpke-g1",
	})
	require.NoError(t, err)

	_, err = minter.Verify(token)
	require.ErrorIs(t, err, ErrTokenSignature)
}

func TestByoTokenVerifyTamperedPayload(t *testing.T) {
	m := newByoTokenMinter([]byte("legit-key"))
	now := time.Now()
	token, err := m.Mint(byoTokenPayload{
		WorkspaceID: "ws-1", ProviderSlug: "s", BaseURL: "https://up.example",
		ModelAllowlist: []string{"m1"}, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	// Flip one character of the payload segment (base64url alphabet dance
	// stays within the token charset).
	tampered := token[:8] + "A" + token[9:]
	if tampered == token {
		tampered = token[:8] + "B" + token[9:]
	}
	_, err = m.Verify(tampered)
	require.ErrorIs(t, err, ErrTokenSignature)
}

func TestByoTokenExpirySkewBound(t *testing.T) {
	m := newByoTokenMinter([]byte("k"))
	m.clock = func() time.Time { return time.Unix(1780000000, 0) }
	base := m.clock()

	token, err := m.Mint(byoTokenPayload{
		WorkspaceID: "w", ProviderSlug: "s", BaseURL: "u",
		IssuedAt: base.Unix(), ExpiresAt: base.Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	// Within skew tolerance (30s) past exp → still valid.
	m.clock = func() time.Time { return base.Add(time.Minute + 29*time.Second) }
	_, err = m.Verify(token)
	require.NoError(t, err)

	// Beyond tolerance → expired.
	m.clock = func() time.Time { return base.Add(time.Minute + 31*time.Second) }
	_, err = m.Verify(token)
	require.ErrorIs(t, err, ErrTokenExpired)

	// Negative direction: iat in the future beyond tolerance → rejected.
	m2 := newByoTokenMinter([]byte("k"))
	now := time.Unix(1780000000, 0)
	m2.clock = func() time.Time { return now }
	future, err := m2.Mint(byoTokenPayload{
		WorkspaceID: "w", ProviderSlug: "s", BaseURL: "u",
		IssuedAt: now.Add(5 * time.Minute).Unix(), ExpiresAt: now.Add(6 * time.Minute).Unix(),
	})
	require.NoError(t, err)
	_, err = m2.Verify(future)
	require.ErrorIs(t, err, errTokenNotYetValid)
}

func TestByoTokenMalformed(t *testing.T) {
	m := newByoTokenMinter([]byte("k"))
	for _, tc := range []string{"", "nope", "lrt_", "lrt_onlyonesegment", "lrt_!!!_???"} {
		_, err := m.Verify(tc)
		require.Error(t, err, "token %q", tc)
	}
}

func TestExtractBearerToken(t *testing.T) {
	token, ok := extractBearerToken("Bearer lrt_abc_def")
	require.True(t, ok)
	assert.Equal(t, "lrt_abc_def", token)

	_, ok = extractBearerToken("Basic zzz")
	assert.False(t, ok)
	_, ok = extractBearerToken("Bearer ")
	assert.False(t, ok)
}
