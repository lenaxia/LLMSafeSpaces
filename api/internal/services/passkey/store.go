// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package passkey implements WebAuthn / FIDO2 passkey registration and login
// (Epic 59) plus the one-time-use recovery-code fallback for passkey-only
// users. It wraps github.com/go-webauthn/webauthn for ceremony verification.
//
// Security posture:
//   - user_passkeys holds ONLY public material (credential id, public key,
//     sign count, transports). The private key never leaves the authenticator;
//     a compromise of this table leaks no secret material.
//   - Challenges are crypto-random, single-use (deleted on consume), short-TTL
//     (5 min), and bound to the user (registration) or discovered user (login).
//   - Recovery codes are bcrypt-hashed (cost 12) shared secrets — the one
//     phishable factor in an otherwise phishing-resistant system, accepted per
//     the design's recovery tradeoff.
//
// DEK integration: a passkey-only user has no password, so their DEK is wrapped
// by the master-KEK provider with dek_source='passkey' (Epic 58 machinery).
package passkey

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Credential is the stored WebAuthn credential record (a user_passkeys row).
type Credential struct {
	ID                uuid.UUID
	UserID            string
	CredentialID      []byte
	PublicKey         []byte
	AttestationType   string
	AttestationFormat string
	AAGUID            *uuid.UUID
	SignCount         uint32
	Transports        []string
	Name              string
	CreatedAt         time.Time
	LastUsedAt        *time.Time
}

// ToDTO converts a stored credential to its API transfer object (public fields
// only; CredentialID/PublicKey are not exposed over the API).
func (c *Credential) ToDTO() types.PasskeyCredential {
	return types.PasskeyCredential{
		ID:             c.ID,
		UserID:         c.UserID,
		CredentialID:   c.CredentialID,
		Name:           c.Name,
		CreatedAt:      c.CreatedAt,
		LastUsedAt:     c.LastUsedAt,
		AttestationFmt: c.AttestationFormat,
		AAGUID:         c.AAGUID,
	}
}

// RecoveryCode is a stored (hashed) recovery code row. The plaintext is shown
// to the user exactly once at enrollment; only code_hash persists.
type RecoveryCode struct {
	ID        uuid.UUID
	UserID    string
	CodeHash  string
	UsedAt    *time.Time
	CreatedAt time.Time
}

// Store abstracts persistence for user_passkeys and user_recovery_codes. The
// passkey service depends on this narrow interface so the WebAuthn ceremony
// logic is testable against an in-memory fake (no Postgres required).
type Store interface {
	// ListCredentials returns every passkey a user has registered. Used both to
	// build the go-webauthn User (WebAuthnCredentials) at login and to surface
	// the credential list in account settings.
	ListCredentials(ctx context.Context, userID string) ([]Credential, error)
	// GetCredentialByCredentialID looks up a credential by its WebAuthn
	// credential id (authenticator-generated, globally unique). Used during
	// login assertion to find the owning user when the authenticator is
	// discoverable.
	GetCredentialByCredentialID(ctx context.Context, credentialID []byte) (*Credential, error)
	// CreateCredential persists a newly-registered credential. Fails on a
	// duplicate credential_id (unique index) — an authenticator cannot be bound
	// to two accounts.
	CreateCredential(ctx context.Context, c *Credential) error
	// UpdateCredentialAfterLogin records the post-assertion sign count (cloned-
	// authenticator detection) and last-used timestamp.
	UpdateCredentialAfterLogin(ctx context.Context, id uuid.UUID, signCount uint32, lastUsedAt time.Time) error
	// DeleteCredential removes a credential. Refused when it is the user's last
	// one (passkey-only users must keep ≥1 credential or they are locked out).
	DeleteCredential(ctx context.Context, userID string, id uuid.UUID) error
	// CountCredentials returns the number of credentials a user has.
	CountCredentials(ctx context.Context, userID string) (int, error)

	// CreateRecoveryCodes stores a batch of freshly-generated recovery codes
	// (bcrypt-hashed by the caller). Replaces any existing unused codes for the
	// user (re-enrollment invalidates prior codes).
	CreateRecoveryCodes(ctx context.Context, userID string, hashes []string) error
	// ListAvailableRecoveryCodes returns the user's unused recovery-code rows.
	ListAvailableRecoveryCodes(ctx context.Context, userID string) ([]RecoveryCode, error)
	// ConsumeRecoveryCode marks a recovery code used (single-use). Returns
	// ErrRecoveryCodeNotFound when no unused code for the user matches.
	ConsumeRecoveryCode(ctx context.Context, userID string, codeHash string) error
}

// Sentinel errors. Plain errors.New (not StatusError) because these are internal
// to the service layer; the HTTP handlers map them to appropriate status codes.
var (
	ErrCredentialNotFound   = storeErr("passkey credential not found")
	ErrLastCredential       = storeErr("cannot delete the last remaining passkey")
	ErrRecoveryCodeNotFound = storeErr("recovery code not found or already used")
	ErrUserNotFound         = storeErr("user not found")
	ErrNoPasskeyRegistered  = storeErr("user has no registered passkeys")
	ErrChallengeExpired     = storeErr("passkey challenge expired or not found")
)

type storeErr string

func (e storeErr) Error() string { return string(e) }
