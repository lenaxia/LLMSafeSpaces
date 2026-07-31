// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package passkey

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// ChallengeTTL is how long a WebAuthn challenge is valid. 5 minutes is the
// consumer standard — long enough for a user to interact with the authenticator
// prompt, short enough that a leaked challenge is useless quickly.
const ChallengeTTL = 5 * time.Minute

// RecoveryCodeCount is the number of recovery codes generated at enrollment.
const RecoveryCodeCount = 10

// RecoveryCodeLen is the character length of each recovery code (before any
// formatting). 20 random characters from an unambiguous alphabet.
const RecoveryCodeLen = 20

// recoveryBcryptCost is the bcrypt cost for recovery-code hashing. 12 in
// production; overridable via the recoveryBcryptCost package var for tests.
var recoveryBcryptCost = 12

// recoveryCodeAlphabet excludes visually-ambiguous characters (0/O, 1/I/l).
const recoveryCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// UserLookup abstracts the user-lookup the ceremony needs: email→user at login
// begin, and user existence verification at registration. Implementations are
// the database service (real) or a fake (tests). Matches the existing
// database.Service.GetUserByEmail shape.
type UserLookup interface {
	GetUserByEmail(ctx context.Context, email string) (*types.User, error)
}

// SessionStore abstracts the WebAuthn challenge store. Challenges MUST be
// crypto-random, single-use (deleted on consume), short-TTL, and user-bound.
// The implementation is Redis-backed in production.
type SessionStore interface {
	SaveChallenge(ctx context.Context, token string, data []byte, ttl time.Duration) error
	// ConsumeChallenge atomically reads and deletes a challenge in a single
	// operation (e.g. Redis GETDEL). This closes the replay window that
	// separate Get+Delete calls would leave open under concurrent requests.
	// Returns (nil, nil) when the token has no stored challenge.
	ConsumeChallenge(ctx context.Context, token string) ([]byte, error)
}

// Service implements the WebAuthn registration and login ceremonies, wrapping
// go-webauthn for crypto verification. The private key never leaves the
// authenticator; this service only verifies public material (attestation at
// registration, assertion at login) and persists the resulting credential.
type Service struct {
	wan      *webauthn.WebAuthn
	store    Store
	users    UserLookup
	sessions SessionStore
	logger   Logger
}

// Logger is a minimal logger interface for non-fatal warnings (sign-count
// update failures, etc.). Implementations: *logger.Logger in production,
// nil in tests.
type Logger interface {
	Warn(msg string, args ...any)
}

// ServiceConfig holds the constructor-time deps for the ceremony service.
type ServiceConfig struct {
	RPID      string
	RPName    string
	RPOrigins []string
	Store     Store
	Users     UserLookup
	Sessions  SessionStore
	Logger    Logger
}

// New constructs the ceremony service. Returns an error when the WebAuthn RP
// config is invalid (empty RPID, no origins) so the caller can fail at boot
// rather than at first request.
func New(cfg ServiceConfig) (*Service, error) {
	if cfg.RPID == "" {
		return nil, fmt.Errorf("passkey: RPID is required")
	}
	if len(cfg.RPOrigins) == 0 {
		return nil, fmt.Errorf("passkey: at least one RPOrigin is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("passkey: Store is required")
	}
	if cfg.Users == nil {
		return nil, fmt.Errorf("passkey: UserLookup is required")
	}
	if cfg.Sessions == nil {
		return nil, fmt.Errorf("passkey: SessionStore is required")
	}
	wan, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: cfg.RPName,
		RPOrigins:     cfg.RPOrigins,
	})
	if err != nil {
		return nil, fmt.Errorf("passkey: construct webauthn: %w", err)
	}
	return &Service{wan: wan, store: cfg.Store, users: cfg.Users, sessions: cfg.Sessions, logger: cfg.Logger}, nil
}

// --- webauthn.User adapter ---

// webauthnUser adapts our stored Credential slice to the go-webauthn User
// interface. go-webauthn calls these methods during ceremony verification.
type webauthnUser struct {
	id          []byte
	name        string
	displayName string
	creds       []webauthn.Credential
}

func (u *webauthnUser) WebAuthnID() []byte                         { return u.id }
func (u *webauthnUser) WebAuthnName() string                       { return u.name }
func (u *webauthnUser) WebAuthnDisplayName() string                { return u.displayName }
func (u *webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// toWebAuthnCredential converts our stored Credential to go-webauthn's type.
func toWebAuthnCredential(c Credential) webauthn.Credential {
	return webauthn.Credential{
		ID:              c.CredentialID,
		PublicKey:       c.PublicKey,
		AttestationType: c.AttestationType,
		Transport:       toProtocolTransports(c.Transports),
		Authenticator: webauthn.Authenticator{
			SignCount: c.SignCount,
		},
	}
}

func toProtocolTransports(ts []string) []protocol.AuthenticatorTransport {
	out := make([]protocol.AuthenticatorTransport, len(ts))
	for i, t := range ts {
		out[i] = protocol.AuthenticatorTransport(t)
	}
	return out
}

// --- challenge token helpers ---

// newChallengeToken returns a crypto-random hex token used as the Redis key
// for a challenge session. The token is what the browser sends back at
// Finish* time so the server can look up the stored challenge.
func newChallengeToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate challenge token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// --- Registration ceremony ---

// BeginRegistrationOptions is returned to the browser. Options is the
// WebAuthn CredentialCreation dict fed to navigator.credentials.create().
// SessionToken is the opaque token the browser sends back at /finish.
type BeginRegistrationOptions struct {
	Options      map[string]any `json:"options"`
	SessionToken string         `json:"sessionToken"`
}

// BeginRegistration starts a passkey enrollment for a user who has no
// passkeys yet. Generates a challenge, persists it (single-use, TTL-bound),
// and returns the WebAuthn options + session token.
func (s *Service) BeginRegistration(ctx context.Context, userID, username string) (*BeginRegistrationOptions, error) {
	// Build the webauthn.User. For a brand-new user with no existing
	// passkeys, the credential list is empty — go-webauthn accepts that.
	user := &webauthnUser{
		id:          []byte(userID),
		name:        username,
		displayName: username,
		creds:       []webauthn.Credential{},
	}

	creation, session, err := s.wan.BeginRegistration(user)
	if err != nil {
		return nil, fmt.Errorf("begin registration: %w", err)
	}

	token, err := newChallengeToken()
	if err != nil {
		return nil, err
	}
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, fmt.Errorf("marshal session: %w", err)
	}
	if err := s.sessions.SaveChallenge(ctx, token, sessionData, ChallengeTTL); err != nil {
		return nil, fmt.Errorf("save challenge: %w", err)
	}

	opts, err := marshalResponse(creation.Response)
	if err != nil {
		return nil, err
	}
	return &BeginRegistrationOptions{Options: opts, SessionToken: token}, nil
}

// FinishRegistrationResult holds the verified credential + recovery codes
// generated at enrollment (one-time display). Neither is persisted by the
// service — the CALLER (HTTP handler) must create the user row FIRST (so the
// FK constraint on user_passkeys.user_id is satisfied), then atomically
// persist both via Store.CreateCredentialAndRecoveryCodes.
type FinishRegistrationResult struct {
	Credential         Credential
	RecoveryCodes      []string
	RecoveryCodeHashes []string
}

func (s *Service) FinishRegistration(ctx context.Context, sessionToken, username, name string, response map[string]any) (*FinishRegistrationResult, error) {
	// Consume the challenge BEFORE parsing. Single-use guarantee: once
	// submitted, consumed regardless of parse/verify outcome.
	sessionData, err := s.consumeChallenge(ctx, sessionToken)
	if err != nil {
		return nil, err
	}

	parsed, err := parseCreationFromMap(response)
	if err != nil {
		return nil, fmt.Errorf("parse attestation: %w", err)
	}

	userID := string(sessionData.UserID)
	waUser := &webauthnUser{
		id:          sessionData.UserID,
		name:        username,
		displayName: username,
		creds:       []webauthn.Credential{},
	}

	cred, err := s.wan.CreateCredential(waUser, *sessionData, parsed)
	if err != nil {
		return nil, fmt.Errorf("verify attestation: %w", err)
	}

	stored := Credential{
		ID:                uuid.New(),
		UserID:            userID,
		CredentialID:      cred.ID,
		PublicKey:         cred.PublicKey,
		AttestationType:   cred.AttestationType,
		AttestationFormat: cred.AttestationFormat,
		SignCount:         cred.Authenticator.SignCount,
		Transports:        fromProtocolTransports(cred.Transport),
		Name:              name,
		CreatedAt:         time.Now(),
	}

	codes, hashes, err := generateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, fmt.Errorf("generate recovery codes: %w", err)
	}

	return &FinishRegistrationResult{
		Credential:         stored,
		RecoveryCodes:      codes,
		RecoveryCodeHashes: hashes,
	}, nil
}

// CreateCredentialAndRecoveryCodes atomically persists a credential AND its
// recovery-code hashes via the Store. The handler calls this after creating
// the user row, so the FK constraint is satisfied. Exposed on the service so
// the handler doesn't reach into the Store directly.
func (s *Service) CreateCredentialAndRecoveryCodes(ctx context.Context, cred *Credential, hashes []string) error {
	return s.store.CreateCredentialAndRecoveryCodes(ctx, cred, hashes)
}

// ListUserCredentials returns all passkeys for a user (for the settings page).
func (s *Service) ListUserCredentials(ctx context.Context, userID string) ([]Credential, error) {
	return s.store.ListCredentials(ctx, userID)
}

// DeleteUserCredential removes a passkey. Refuses the last remaining one.
func (s *Service) DeleteUserCredential(ctx context.Context, userID string, credID uuid.UUID) error {
	return s.store.DeleteCredential(ctx, userID, credID)
}

// RegenerateRecoveryCodes generates a new set of recovery codes, replacing
// all existing unused codes. Returns the plaintext codes (one-time display).
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID string) ([]string, error) {
	codes, hashes, err := generateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, fmt.Errorf("generate recovery codes: %w", err)
	}
	if err := s.store.CreateRecoveryCodes(ctx, userID, hashes); err != nil {
		return nil, fmt.Errorf("store recovery codes: %w", err)
	}
	return codes, nil
}

// parseCreationFromMap parses a WebAuthn attestation response from a
// map[string]any (the browser's PublicKeyCredential JSON).
func parseCreationFromMap(m map[string]any) (*protocol.ParsedCredentialCreationData, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return protocol.ParseCredentialCreationResponseBody(bytes.NewReader(raw))
}

// parseAssertionFromMap parses a WebAuthn assertion response from a
// map[string]any (the browser's PublicKeyCredential JSON).
func parseAssertionFromMap(m map[string]any) (*protocol.ParsedCredentialAssertionData, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return protocol.ParseCredentialRequestResponseBody(bytes.NewReader(raw))
}

// --- Login ceremony ---

// BeginLoginOptions is returned to the browser. Options is the WebAuthn
// CredentialAssertion dict fed to navigator.credentials.get().
type BeginLoginOptions struct {
	Options      map[string]any `json:"options"`
	SessionToken string         `json:"sessionToken"`
}

// BeginLogin starts a passkey assertion for a user identified by email. The
// user must have at least one registered passkey. Generates a challenge
// scoped to the user's allowed credentials.
func (s *Service) BeginLogin(ctx context.Context, email string) (*BeginLoginOptions, string, error) {
	user, err := s.users.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, "", fmt.Errorf("lookup user: %w", err)
	}
	if user == nil {
		return nil, "", ErrUserNotFound
	}

	creds, err := s.store.ListCredentials(ctx, user.ID)
	if err != nil {
		return nil, "", fmt.Errorf("list credentials: %w", err)
	}
	if len(creds) == 0 {
		return nil, "", ErrNoPasskeyRegistered
	}

	waCreds := make([]webauthn.Credential, len(creds))
	for i, c := range creds {
		waCreds[i] = toWebAuthnCredential(c)
	}
	waUser := &webauthnUser{
		id:          []byte(user.ID),
		name:        user.Username,
		displayName: user.Username,
		creds:       waCreds,
	}

	assertion, session, err := s.wan.BeginLogin(waUser)
	if err != nil {
		return nil, "", fmt.Errorf("begin login: %w", err)
	}

	token, err := newChallengeToken()
	if err != nil {
		return nil, "", err
	}
	sessionData, err := json.Marshal(session)
	if err != nil {
		return nil, "", fmt.Errorf("marshal session: %w", err)
	}
	if err := s.sessions.SaveChallenge(ctx, token, sessionData, ChallengeTTL); err != nil {
		return nil, "", fmt.Errorf("save challenge: %w", err)
	}

	opts, err := marshalResponse(assertion.Response)
	if err != nil {
		return nil, "", err
	}
	return &BeginLoginOptions{Options: opts, SessionToken: token}, user.ID, nil
}

// FinishLogin verifies the authenticator's assertion against the stored
// challenge and returns the user ID. The caller (HTTP handler) issues the
// session token + unlocks the DEK. Updates the sign count (cloned-
// authenticator detection) after a successful assertion.
func (s *Service) FinishLogin(ctx context.Context, sessionToken, email string, response map[string]any) (string, error) {
	user, err := s.users.GetUserByEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("lookup user: %w", err)
	}
	if user == nil {
		return "", ErrUserNotFound
	}

	// Consume the challenge BEFORE parsing the response. The single-use
	// guarantee means: once the browser submits a FinishLogin request, the
	// challenge is consumed regardless of whether verification succeeds. This
	// prevents replay even on parse/verify failure.
	sessionData, err := s.consumeChallenge(ctx, sessionToken)
	if err != nil {
		return "", err
	}

	// Parse after consuming — so a malformed response still burns the challenge.
	parsed, err := parseAssertionFromMap(response)
	if err != nil {
		return "", fmt.Errorf("parse assertion: %w", err)
	}

	creds, err := s.store.ListCredentials(ctx, user.ID)
	if err != nil {
		return "", fmt.Errorf("list credentials: %w", err)
	}
	waCreds := make([]webauthn.Credential, len(creds))
	for i, c := range creds {
		waCreds[i] = toWebAuthnCredential(c)
	}
	waUser := &webauthnUser{
		id:          []byte(user.ID),
		name:        user.Username,
		displayName: user.Username,
		creds:       waCreds,
	}

	cred, err := s.wan.ValidateLogin(waUser, *sessionData, parsed)
	if err != nil {
		return "", fmt.Errorf("verify assertion: %w", err)
	}

	var credID uuid.UUID
	for _, c := range creds {
		if equalBytes(c.CredentialID, cred.ID) {
			credID = c.ID
			break
		}
	}
	if credID != (uuid.UUID{}) {
		if err := s.store.UpdateCredentialAfterLogin(ctx, credID, cred.Authenticator.SignCount, time.Now()); err != nil {
			if s.logger != nil {
				s.logger.Warn("passkey: sign-count update failed (cloned-auth detection degraded)",
					"credential_id", credID.String(), "error", err.Error())
			}
		}
	}

	return user.ID, nil
}

// --- recovery code consumption ---

// ConsumeRecoveryCode validates a recovery code against the stored hashes.
// Returns the user ID on success. A matched code is marked used (single-use).
// Constant-time comparison is via bcrypt — the same protection as password
// verification. The caller forces the user to enroll a new passkey after a
// recovery-code login.
func (s *Service) ConsumeRecoveryCode(ctx context.Context, email, code string) (string, error) {
	user, err := s.users.GetUserByEmail(ctx, email)
	if err != nil {
		return "", fmt.Errorf("lookup user: %w", err)
	}
	if user == nil {
		return "", ErrUserNotFound
	}

	codes, err := s.store.ListAvailableRecoveryCodes(ctx, user.ID)
	if err != nil {
		return "", fmt.Errorf("list recovery codes: %w", err)
	}
	for _, rc := range codes {
		if bcrypt.CompareHashAndPassword([]byte(rc.CodeHash), []byte(code)) == nil {
			if err := s.store.ConsumeRecoveryCode(ctx, user.ID, rc.CodeHash); err != nil {
				return "", fmt.Errorf("consume recovery code: %w", err)
			}
			return user.ID, nil
		}
	}
	return "", ErrRecoveryCodeNotFound
}

// --- internal helpers ---

// consumeChallenge retrieves and deletes a challenge (single-use). Returns
// an error if the challenge is missing, expired, or malformed.
func (s *Service) consumeChallenge(ctx context.Context, token string) (*webauthn.SessionData, error) {
	data, err := s.sessions.ConsumeChallenge(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("consume challenge: %w", err)
	}
	if data == nil {
		return nil, ErrChallengeExpired
	}

	var session webauthn.SessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("unmarshal challenge: %w", err)
	}
	return &session, nil
}

func fromProtocolTransports(ts []protocol.AuthenticatorTransport) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// generateRecoveryCodes generates n random recovery codes from an unambiguous
// alphabet. Returns the plaintext codes (one-time display) and their bcrypt
// hashes (for storage).
func generateRecoveryCodes(n int) (codes, hashes []string, err error) {
	codes = make([]string, n)
	hashes = make([]string, n)
	for i := 0; i < n; i++ {
		code, err := randomRecoveryCode()
		if err != nil {
			return nil, nil, err
		}
		codes[i] = code
		hash, err := bcrypt.GenerateFromPassword([]byte(code), recoveryBcryptCost)
		if err != nil {
			return nil, nil, fmt.Errorf("hash recovery code: %w", err)
		}
		hashes[i] = string(hash)
	}
	return codes, hashes, nil
}

func randomRecoveryCode() (string, error) {
	code := make([]byte, RecoveryCodeLen)
	for i := range code {
		idx, err := randInt(len(recoveryCodeAlphabet))
		if err != nil {
			return "", err
		}
		code[i] = recoveryCodeAlphabet[idx]
	}
	return string(code), nil
}

// randInt returns a crypto-random int in [0, max) without modulo bias, via
// rejection sampling. The bias from int(b) % max is small (256 % 31 = 8 →
// ~12.5% for the first 8), but rejection sampling is the correct approach for
// security-sensitive random selection.
func randInt(max int) (int, error) {
	// Rejection sampling: reject values in the incomplete final bucket.
	// The largest multiple of max that fits in a byte:
	limit := 256 - (256 % max)
	for {
		b := make([]byte, 1)
		if _, err := rand.Read(b); err != nil {
			return 0, err
		}
		if int(b[0]) < limit {
			return int(b[0]) % max, nil
		}
	}
}

// marshalResponse converts a go-webauthn protocol struct to a map[string]any
// suitable for JSON-serializing to the browser. Returns an error on marshal
// or unmarshal failure rather than silently returning nil (Rule 6: explicit
// error handling).
func marshalResponse(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal webauthn response: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("unmarshal webauthn response: %w", err)
	}
	return m, nil
}
