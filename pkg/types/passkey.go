// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"time"

	"github.com/google/uuid"
)

// PasskeyCredential is the API transfer object for a stored WebAuthn
// credential (user_passkeys row). It is public material only — the private key
// never leaves the authenticator.
type PasskeyCredential struct {
	ID             uuid.UUID  `json:"id"`
	UserID         string     `json:"-" db:"user_id"`
	CredentialID   []byte     `json:"-" db:"credential_id"`
	Name           string     `json:"name,omitempty" db:"name"`
	CreatedAt      time.Time  `json:"createdAt" db:"created_at"`
	LastUsedAt     *time.Time `json:"lastUsedAt,omitempty" db:"last_used_at"`
	AttestationFmt string     `json:"-" db:"attestation_format"`
	AAGUID         *uuid.UUID `json:"-" db:"aaguid"`
}

// PasskeyRegisterBeginRequest initiates a WebAuthn registration ceremony.
type PasskeyRegisterBeginRequest struct {
	// Email identifies the account being created/logged-in (Phase 2 default
	// signup is passkey; email is still required to bind the credential). For an
	// existing user adding a second passkey, the session already identifies them.
	Email string `json:"email" binding:"required,email"`
	Name  string `json:"name,omitempty"`
}

// PasskeyBeginResponse carries the WebAuthn challenge/options the browser feeds
// to navigator.credentials.create() (register) or .get() (login). The opaque
// PublicKeyCredentialCreation/Request JSON is forwarded verbatim from go-webauthn.
type PasskeyBeginResponse struct {
	Options map[string]any `json:"options"`
}

// PasskeyFinishRequest carries the authenticator's attestation (register) or
// assertion (login) response, forwarded to go-webauthn for verification.
type PasskeyFinishRequest struct {
	// CredentialCreationResponse / CredentialAssertionResponse as produced by
	// the browser, parsed server-side by go-webauthn's protocol package. Kept as
	// raw JSON so the server never reshapes WebAuthn protocol fields.
	Response map[string]any `json:"response"`
	// Name is an optional friendly label for a newly-registered credential
	// ("YubiKey 5C", "iPhone Face ID"). Registration finish only.
	Name string `json:"name,omitempty"`
}

// PasskeyLoginBeginRequest identifies the account for a username-first login
// ceremony. Discoverable-credential (no-username) login is a future
// enhancement; Phase 2 ships username-first to match Epic 54's login discovery.
type PasskeyLoginBeginRequest struct {
	Email string `json:"email" binding:"required,email"`
}

// PasskeyFinishResponse is returned after a successful ceremony. On register
// finish it returns the session token + recovery codes (one-time display). On
// login finish it returns the session token.
type PasskeyFinishResponse struct {
	Token         string   `json:"token"`
	User          User     `json:"user"`
	RecoveryCodes []string `json:"recoveryCodes,omitempty"`
}
