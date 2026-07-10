// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"errors"
	"net/http"

	pkgerrors "github.com/lenaxia/llmsafespaces/pkg/errors"
)

// ErrNotMyCiphertext is returned by a RootKeyProvider's Decrypt when the
// ciphertext's prefix does not match the provider's format. It is distinct
// from ErrDecryptionFailed (crypto.go), which means "the prefix matched
// but the key was wrong" — a genuine decrypt failure rather than a routing
// signal.
//
// Used by CompositeProvider (composite_provider.go) to dispatch Decrypt
// across multiple providers without false-positive error logs: a provider
// returning ErrNotMyCiphertext tells the composite "try the next one,"
// while ErrDecryptionFailed tells it "this row is mine but corrupt —
// stop." Without this distinction, a multi-provider decrypt would log a
// spurious failure for every provider that didn't match the prefix.
//
// Plain sentinel (not *StatusError) because this is an internal routing
// signal — it never reaches the HTTP layer.
var ErrNotMyCiphertext = errors.New("ciphertext prefix does not match this provider")

// Sentinel errors returned by the secrets package. Each carries its HTTP
// status code and user-facing message via StatusError, so the generic
// error handler (respondWithError in router.go) maps them automatically
// — no handler-level switch needed.
//
// errors.Is still works for sentinel checks (pointer identity via chain
// traversal). errors.As can extract the *StatusError for generic typed
// handling.
//
// Wrapping (`fmt.Errorf("...: %w", ErrSecretNotFound)`) is supported
// and recommended so the classification survives upstream formatting.
var (
	// ErrSecretNotFound is returned when a secret does not exist or
	// is not owned by the requesting user. Both cases are conflated
	// to avoid leaking workspace existence cross-user.
	ErrSecretNotFound = &pkgerrors.StatusError{
		Status:  http.StatusNotFound,
		Code:    "secret_not_found",
		Message: "secret not found",
	}

	// ErrDuplicateSecret is returned when CreateSecret would violate
	// the (user_id, name) uniqueness constraint.
	ErrDuplicateSecret = &pkgerrors.StatusError{
		Status:  http.StatusConflict,
		Code:    "duplicate_secret",
		Message: "secret with this name already exists",
	}

	// ErrDEKUnavailable is returned when the per-session DEK is not
	// in the cache (typically because the JWT's jti has expired or
	// the user has not logged in since the cache was flushed).
	ErrDEKUnavailable = &pkgerrors.StatusError{
		Status:  http.StatusForbidden,
		Code:    "dek_unavailable",
		Message: "encryption key not available; re-authenticate",
	}

	// ErrInvalidSecretType is returned when a CreateSecret request
	// names a type outside ValidSecretTypes.
	ErrInvalidSecretType = &pkgerrors.StatusError{
		Status:  http.StatusBadRequest,
		Code:    "invalid_secret_type",
		Message: "invalid secret type",
	}

	// ErrInvalidMetadata is returned when the metadata blob is
	// missing a required field for the secret type, fails JSON
	// validation, or contains an adversarial mount_path.
	ErrInvalidMetadata = &pkgerrors.StatusError{
		Status:  http.StatusBadRequest,
		Code:    "invalid_metadata",
		Message: "invalid secret metadata",
	}

	// ErrInvalidPassword is returned by RevealSecret when the
	// password reconfirmation step fails. The handler maps this to
	// a uniform 403 — the same status used for missing DEK — so
	// the response shape does not differentiate between "wrong
	// password" and "session expired", reducing what an attacker
	// who has stolen a JWT can learn.
	ErrInvalidPassword = &pkgerrors.StatusError{
		Status:  http.StatusForbidden,
		Code:    "invalid_password",
		Message: "access denied",
	}

	// ErrUserKeysMissing is returned when the user_keys row for the
	// caller does not exist (e.g. legacy account that pre-dates
	// Epic 10 key initialisation, or a half-failed Register).
	ErrUserKeysMissing = &pkgerrors.StatusError{
		Status:  http.StatusPreconditionFailed,
		Code:    "user_keys_missing",
		Message: "user key material not initialized; please re-login",
	}

	// ErrInvalidLLMProvider is returned when LLMProviderData validation
	// fails (missing provider, missing API key, etc.).
	ErrInvalidLLMProvider = &pkgerrors.StatusError{
		Status:  http.StatusBadRequest,
		Code:    "invalid_llm_provider",
		Message: "invalid LLM provider data",
	}

	// ErrWorkspaceNotOwned is returned by binding operations when
	// the caller does not own the target workspace. Both
	// "workspace doesn't exist" and "workspace owned by someone
	// else" map to this single sentinel so the response shape does
	// not leak workspace existence cross-user. Handlers map to 404.
	//
	// The message says only "workspace not found" — NOT "or not
	// owned by caller" — because a future caller that logs
	// err.Error() would otherwise leak the same distinction the
	// type system was designed to hide. Callers that need to
	// classify the failure mode use errors.Is.
	ErrWorkspaceNotOwned = &pkgerrors.StatusError{
		Status:  http.StatusNotFound,
		Code:    "workspace_not_found",
		Message: "workspace not found",
	}

	// ErrCiphertextDecryptFailed is returned when the DEK was
	// successfully obtained but the stored ciphertext cannot be
	// decrypted with it (AEAD authentication failure). This is
	// distinct from ErrDEKUnavailable: the key material is present,
	// but it does not match the ciphertext.
	//
	// Most common cause: the user's DEK was rotated or the user_keys
	// row was rewritten without re-encrypting the secrets that were
	// encrypted under the old DEK. Less common: ciphertext corruption,
	// schema version skew (key_version mismatch), or storage tampering.
	//
	// The DEK itself is fine — re-authenticating will not help.
	ErrCiphertextDecryptFailed = &pkgerrors.StatusError{
		Status: http.StatusConflict,
		Code:   "ciphertext_decrypt_failed",
		Message: "this secret cannot be decrypted with your current encryption key — " +
			"the ciphertext was likely encrypted with a previous key. " +
			"Re-create the secret to recover; if you have not changed your password, " +
			"contact an administrator and reference your audit log.",
	}
)
