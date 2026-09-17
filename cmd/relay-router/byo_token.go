// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// byoTokenPrefix is the wire marker for relay tokens (design 0058 §4.4):
// lrt_<base64url(payload_json)>_<base64url(HMAC-SHA256(payload_json))>.
// Nothing secret rides in the payload — the HMAC is the integrity bound.
const byoTokenPrefix = "lrt_"

// byoTokenPayload is the scoped token body (design 0058 §4.4). Exactly one
// upstream (baseURL), one workspace, one provider, a model allowlist, and
// the staging keyID the envelope must resolve under.
type byoTokenPayload struct {
	WorkspaceID    string   `json:"workspaceID"`
	ProviderSlug   string   `json:"providerSlug"`
	BaseURL        string   `json:"baseURL"`
	ModelAllowlist []string `json:"modelAllowlist"`
	IssuedAt       int64    `json:"iat"`
	ExpiresAt      int64    `json:"exp"`
	KeyID          string   `json:"keyID"`
}

var (
	ErrMalformedToken   = errors.New("malformed relay token")
	ErrTokenSignature   = errors.New("relay token signature mismatch")
	ErrTokenExpired     = errors.New("relay token expired")
	errTokenNotYetValid = errors.New("relay token issued in the future")
)

// byoTokenMinter mints and verifies relay tokens with one HMAC-SHA256
// signing key. The router is the single holder: exactly one validator
// authority (the replicas share the key via a Secret), and the mint
// endpoint is the only external path to it.
type byoTokenMinter struct {
	signingKey    []byte
	clock         func() time.Time
	skewTolerance time.Duration
}

func newByoTokenMinter(signingKey []byte) *byoTokenMinter {
	return &byoTokenMinter{
		signingKey:    signingKey,
		clock:         time.Now,
		skewTolerance: 30 * time.Second,
	}
}

// Mint produces `lrt_<b64url(payload)>_<b64url(hmac)>` binding the scope.
func (m *byoTokenMinter) Mint(payload byoTokenPayload) (string, error) {
	if payload.WorkspaceID == "" || payload.ProviderSlug == "" || payload.BaseURL == "" {
		return "", fmt.Errorf("%w: workspace, provider and baseURL are required", ErrMalformedToken)
	}
	if payload.ExpiresAt <= payload.IssuedAt {
		return "", fmt.Errorf("%w: exp must be after iat", ErrMalformedToken)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, m.signingKey)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return byoTokenPrefix + encoded + "_" + sig, nil
}

// Verify checks the HMAC only — the staged-Secret-present conjunct (K3)
// belongs to the server, which owns the informer cache, not the token
// layer. Clock skew up to m.skewTolerance is tolerated on both ends
// (router replicas are the only validators; the skew domain is
// intra-cluster NTP).
func (m *byoTokenMinter) Verify(token string) (byoTokenPayload, error) {
	rest, ok := strings.CutPrefix(token, byoTokenPrefix)
	if !ok {
		return byoTokenPayload{}, ErrMalformedToken
	}
	encoded, sigB64, ok := strings.Cut(rest, "_")
	if !ok || encoded == "" || sigB64 == "" {
		return byoTokenPayload{}, ErrMalformedToken
	}
	mac := hmac.New(sha256.New, m.signingKey)
	mac.Write([]byte(encoded))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || !hmac.Equal(want, got) {
		return byoTokenPayload{}, ErrTokenSignature
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return byoTokenPayload{}, ErrMalformedToken
	}
	var payload byoTokenPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return byoTokenPayload{}, ErrMalformedToken
	}
	now := m.clock()
	if now.After(time.Unix(payload.ExpiresAt, 0).Add(m.skewTolerance)) {
		return byoTokenPayload{}, ErrTokenExpired
	}
	if time.Unix(payload.IssuedAt, 0).After(now.Add(m.skewTolerance)) {
		return byoTokenPayload{}, errTokenNotYetValid
	}
	return payload, nil
}

// extractBearerToken pulls the token from the Authorization header.
func extractBearerToken(header string) (string, bool) {
	rest, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", false
	}
	token := strings.TrimSpace(rest)
	if token == "" {
		return "", false
	}
	return token, true
}
