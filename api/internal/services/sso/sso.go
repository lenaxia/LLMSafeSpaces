// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sso implements OIDC single sign-on for organizations (US-43.10, D17).
//
// It owns three concerns:
//  1. SSO config encryption — the IdP client secret is encrypted at rest with
//     the server KEK (D17-S4), always decryptable with no org DEK dependency.
//  2. The PKCE Authorization Code login flow (start + callback).
//  3. Auto-provisioning + group-claim → role mapping applied on every login
//     (D17-S1 + D17-S3) so IdP-driven role changes propagate on re-login.
//
// The state/verifier pair is carried in a short-lived HMAC-signed cookie (the
// API is stateless, so there is no server-side session store).
package sso

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// Sentinel errors surfaced to handlers. Handlers map these to HTTP codes.
var (
	ErrSSONotConfigured = errors.New("SSO not configured for this organization")
	ErrStateExpired     = errors.New("SSO session expired, please try again")
	ErrStateInvalid     = errors.New("invalid SSO state")
	ErrAutoProvisionOff = apierrors.NewForbiddenError("account provisioning is disabled; contact your administrator", nil)
	ErrUserSuspended    = apierrors.NewForbiddenError("account suspended", nil)
	// ErrEmailUnverified fires when the ID token's email claim is not verified
	// by the IdP. Per OIDC spec the `email` claim MUST NOT be trusted for
	// account-binding decisions (auto-provision or login-match) without
	// email_verified==true; otherwise a permissive IdP that lets a user
	// register victim@example.com unverified would let the attacker SSO into
	// the victim's existing account (US-43.10 / F8).
	ErrEmailUnverified = apierrors.NewForbiddenError("identity provider has not verified the email claim", nil)
	// ErrRedirectBaseURLNotSet fires when an SSO flow needs to build an
	// absolute callback URL but oidc.redirectBaseUrl is not configured.
	// Returned instead of deriving the URL from X-Forwarded-* / Host headers
	// (F11): those headers are attacker-influenceable at a misconfigured
	// reverse proxy, and the SSO callback URL is security-sensitive (it is
	// where the IdP redirects with the authorization code). The deployment
	// must state the canonical base URL explicitly.
	ErrRedirectBaseURLNotSet = errors.New("OIDC redirect base URL is not configured; set oidc.redirectBaseUrl")
)

// orgStore is the org-data subset the SSO service depends on. PgOrgStore
// satisfies it; tests provide a fake.
type orgStore interface {
	GetOrgBySlug(ctx context.Context, slug string) (*types.Organization, error)
	GetSSOConfig(ctx context.Context, orgID string) (*types.OrgSSOConfig, error)
	UpsertSSOConfig(ctx context.Context, config *types.OrgSSOConfig) error
	DeleteSSOConfig(ctx context.Context, orgID string) error
	SetDomainVerified(ctx context.Context, orgID, domain string) (bool, error)
	RotateVerificationToken(ctx context.Context, orgID string) (string, error)
	GetOrgMember(ctx context.Context, orgID, userID string) (*types.OrgMember, error)
	// CountOrgAdmins prevents SSO-driven demotion from orphaning an org (the
	// only admin being demoted to member). See ensureMembership.
	CountOrgAdmins(ctx context.Context, orgID string) (int, error)
	AddOrgMember(ctx context.Context, orgID, userID string, role types.OrgRole) error
	UpdateOrgMemberRole(ctx context.Context, orgID, userID string, role types.OrgRole) error
}

// dnsResolver is the DNS lookup subset the SSO domain-verification flow
// depends on. The production implementation uses net.LookupTXT; tests
// inject a fake to avoid real DNS dependencies.
type dnsResolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// netResolver wraps the package-level net.Resolver to satisfy dnsResolver.
type netResolver struct{}

func (netResolver) LookupTXT(ctx context.Context, name string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, name)
}

// ErrDomainNotClaimed is returned when VerifyDomain is called for a domain
// the org has not claimed. Surfaced to handlers as a 400.
var ErrDomainNotClaimed = errors.New("domain is not in claimed domains")

// ErrNoVerificationToken is returned when VerifyDomain is called but the org
// has no verification token configured. The org admin must rotate/generate
// one first via RotateVerificationToken. Surfaced to handlers as a 409.
var ErrNoVerificationToken = errors.New("no verification token configured; rotate one first")

// ErrDNSNotMatching is returned when the TXT record at the verification host
// does not contain the org's verification token. Surfaced to handlers as a 422.
var ErrDNSNotMatching = errors.New("DNS TXT record does not contain the verification token")

// userStore is the user-data subset (auto-provisioning + lookup).
type userStore interface {
	GetUserByEmail(ctx context.Context, email string) (*types.User, error)
	CreateUser(ctx context.Context, user *types.User) error
}

// TokenIssuer issues a session JWT for an authenticated user.
type TokenIssuer interface {
	GenerateToken(userID string) (string, error)
}

// UserKeyManager is the server-KEK DEK capability the auth.Service exposes to
// the SSO (Epic 58) and passkey (Epic 59) login flows. It provisions a
// server-KEK-wrapped DEK for users who have none, and issues a session token
// whose jti is bound to the unlocked DEK — the non-password analog of
// auth.Service.Login's unlock step. Optional on the SSO service: when nil,
// completeLogin falls back to TokenIssuer.GenerateToken (pre-epic behavior;
// SSO users then have no personal-secret encryption until a later login).
type UserKeyManager interface {
	ProvisionServerKEKKeys(ctx context.Context, userID string) error
	IssueTokenAndUnlockDEK(ctx context.Context, userID string, ttl time.Duration) (string, error)
}

// Service implements the OIDC SSO login flow and SSO-config encryption.
type Service struct {
	orgs         orgStore
	users        userStore
	dns          dnsResolver
	issuer       TokenIssuer
	keyManager   UserKeyManager
	keyProvider  secrets.RootKeyProvider
	stateKey     []byte
	tokenTTL     time.Duration
	stateTTL     time.Duration
	redirectBase string
	frontendURL  string
	cookieName   string
	logger       *logger.Logger
}

// ServiceConfig holds the non-store dependencies of the SSO service.
type ServiceConfig struct {
	TokenIssuer         TokenIssuer
	KeyManager          UserKeyManager
	KeyProvider         secrets.RootKeyProvider
	StateKey            []byte
	TokenTTL            time.Duration
	StateTTL            time.Duration
	RedirectBaseURL     string
	FrontendRedirectURL string
	StateCookieName     string
	Logger              *logger.Logger
}

// DefaultStateTTL is the PKCE/state cookie lifetime.
const DefaultStateTTL = 10 * time.Minute

// New creates an SSO service. keyProvider/stateKey may be nil in which case
// config-mutation and login are rejected at runtime (returned as errors); this
// keeps the service constructible in test setups that exercise only a subset.
func New(orgs orgStore, users userStore, cfg ServiceConfig) (*Service, error) {
	if orgs == nil {
		return nil, errors.New("sso: org store is required")
	}
	if users == nil {
		return nil, errors.New("sso: user store is required")
	}
	if cfg.TokenIssuer == nil {
		return nil, errors.New("sso: token issuer is required")
	}
	stateTTL := cfg.StateTTL
	if stateTTL <= 0 {
		stateTTL = DefaultStateTTL
	}
	cookieName := cfg.StateCookieName
	if cookieName == "" {
		cookieName = "lsp_sso_state"
	}
	return &Service{
		orgs:         orgs,
		users:        users,
		dns:          netResolver{},
		issuer:       cfg.TokenIssuer,
		keyManager:   cfg.KeyManager,
		keyProvider:  cfg.KeyProvider,
		stateKey:     cfg.StateKey,
		tokenTTL:     cfg.TokenTTL,
		stateTTL:     stateTTL,
		redirectBase: strings.TrimRight(cfg.RedirectBaseURL, "/"),
		frontendURL:  cfg.FrontendRedirectURL,
		cookieName:   cookieName,
		logger:       cfg.Logger,
	}, nil
}

// CookieName returns the configured PKCE/state cookie name.
func (s *Service) CookieName() string { return s.cookieName }

// SetDNSResolver overrides the DNS resolver. Production code never calls this
// (the default netResolver is set in New); tests inject a fake to avoid real
// DNS dependencies.
func (s *Service) SetDNSResolver(r dnsResolver) {
	if r != nil {
		s.dns = r
	}
}

// StateTTL returns the state cookie lifetime (for Set-Cookie Max-Age).
func (s *Service) StateTTL() time.Duration { return s.stateTTL }

// TokenTTL returns the session JWT lifetime (for the success cookie Max-Age).
func (s *Service) TokenTTL() time.Duration { return s.tokenTTL }

// FrontendRedirectURL returns the post-callback browser destination.
func (s *Service) FrontendRedirectURL() string { return s.frontendURL }

// EncryptClientSecret encrypts a plaintext IdP client secret with the server
// KEK (D17-S4). Returns the at-rest blob suitable for OrgSSOConfig.ClientSecret.
func (s *Service) EncryptClientSecret(ctx context.Context, plaintext string) ([]byte, error) {
	if s.keyProvider == nil {
		return nil, errors.New("server key not configured")
	}
	return s.keyProvider.Encrypt(ctx, []byte(plaintext))
}

// NormalizeDomains lowercases, strips a leading "@", and de-duplicates claimed
// domains so the GIN-indexed lookup (`$1 = ANY(claimed_domains)`) matches
// regardless of how the admin entered them.
func NormalizeDomains(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// ApplyConfigMutation validates and persists an SSO config upsert. clientSecret
// is the plaintext IdP secret; an empty value means "leave the existing secret
// unchanged" (the caller must pre-load the existing config and re-supply its
// encrypted blob). existingVerified is the org's current verified_domains —
// the service intersects it with the new claimed_domains so verifications are
// preserved for domains still claimed and dropped for removed domains (D17 Q-S2
// invariant: verified ⊆ claimed). Returns the encrypted blob actually stored.
func (s *Service) ApplyConfigMutation(ctx context.Context, orgID string, req types.UpsertSSOConfigRequest, existingEncrypted []byte, existingVerified []string) ([]byte, error) {
	if s.keyProvider == nil {
		return nil, errors.New("server key not configured")
	}
	var secretBlob []byte
	if req.ClientSecret != "" {
		enc, err := s.EncryptClientSecret(ctx, req.ClientSecret)
		if err != nil {
			return nil, fmt.Errorf("encrypt client secret: %w", err)
		}
		secretBlob = enc
	} else if len(existingEncrypted) > 0 {
		secretBlob = existingEncrypted
	} else {
		return nil, errors.New("client secret is required on first SSO configuration")
	}

	auto := true
	if req.AutoProvision != nil {
		auto = *req.AutoProvision
	}
	mapping := req.GroupRoleMapping
	if mapping == nil {
		mapping = map[string]types.OrgRole{}
	}
	for _, role := range mapping {
		if role != types.OrgRoleAdmin && role != types.OrgRoleMember {
			return nil, fmt.Errorf("invalid role %q in groupRoleMapping (allowed: admin, member)", role)
		}
	}

	claimed := NormalizeDomains(req.ClaimedDomains)
	// Preserve verifications only for domains still claimed. New domains start
	// unverified (the subset invariant verified ⊆ claimed is enforced here).
	verified := intersectDomains(existingVerified, claimed)

	cfg := &types.OrgSSOConfig{
		OrgID:            orgID,
		DiscoveryURL:     strings.TrimSpace(req.DiscoveryURL),
		ClientID:         strings.TrimSpace(req.ClientID),
		ClientSecret:     secretBlob,
		ClaimedDomains:   claimed,
		VerifiedDomains:  verified,
		AutoProvision:    auto,
		GroupRoleMapping: mapping,
	}
	if err := s.orgs.UpsertSSOConfig(ctx, cfg); err != nil {
		return nil, fmt.Errorf("persist sso config: %w", err)
	}
	return secretBlob, nil
}

// intersectDomains returns the elements of a that also appear in b, preserving
// the order of a. Used to compute verified ⊆ claimed on config mutation.
func intersectDomains(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		return []string{}
	}
	bset := make(map[string]struct{}, len(b))
	for _, d := range b {
		bset[strings.ToLower(strings.TrimSpace(d))] = struct{}{}
	}
	out := make([]string, 0, len(a))
	for _, d := range a {
		key := strings.ToLower(strings.TrimSpace(d))
		if _, ok := bset[key]; ok {
			out = append(out, d)
		}
	}
	return out
}

// VerifyDomainResult is the outcome of VerifyDomain.
type VerifyDomainResult struct {
	Domain   string
	Verified bool
}

// VerifyDomain checks the DNS TXT record at _llmsafespaces-verify.<domain>
// for the org's verification token and, on match, promotes the domain to
// verified. On-demand: the org admin triggers this after adding the TXT record.
// Returns ErrDomainNotClaimed if the domain is not in the org's claimed list,
// ErrNoVerificationToken if the org has no token (must rotate one first), or
// ErrDNSNotMatching if the TXT record doesn't contain the token.
func (s *Service) VerifyDomain(ctx context.Context, orgID, domain string) (*VerifyDomainResult, error) {
	domain = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(domain), "@")))
	if domain == "" {
		return nil, errors.New("domain is required")
	}
	cfg, err := s.orgs.GetSSOConfig(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("load sso config: %w", err)
	}
	if cfg == nil {
		return nil, ErrSSONotConfigured
	}
	// The domain must be claimed before it can be verified.
	claimed := false
	for _, d := range cfg.ClaimedDomains {
		if strings.EqualFold(d, domain) {
			claimed = true
			break
		}
	}
	if !claimed {
		return nil, ErrDomainNotClaimed
	}
	if cfg.VerificationToken == "" {
		return nil, ErrNoVerificationToken
	}
	// DNS lookup: TXT records at _llmsafespaces-verify.<domain>. The token is
	// the record value (not the record name).
	lookupName := "_llmsafespaces-verify." + domain
	records, err := s.dns.LookupTXT(ctx, lookupName)
	if err != nil {
		// NXDOMAIN (no TXT record published yet — the most common first-time
		// scenario) is treated as "no match" rather than a server error. Go's
		// net.LookupTXT returns *net.DNSError with IsNotFound=true for this.
		// Truly transient errors (timeout, server failure) surface as 503.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return &VerifyDomainResult{Domain: domain, Verified: false}, ErrDNSNotMatching
		}
		return nil, fmt.Errorf("dns lookup for %s: %w", lookupName, err)
	}
	matched := false
	for _, r := range records {
		if strings.TrimSpace(r) == cfg.VerificationToken {
			matched = true
			break
		}
	}
	if !matched {
		return &VerifyDomainResult{Domain: domain, Verified: false}, ErrDNSNotMatching
	}
	// SetDomainVerified is idempotent: a re-verify after rotation (same record)
	// is a no-op. The domain is now verified regardless.
	if _, err := s.orgs.SetDomainVerified(ctx, orgID, domain); err != nil {
		return nil, fmt.Errorf("promote domain: %w", err)
	}
	return &VerifyDomainResult{Domain: domain, Verified: true}, nil
}

// RotateToken replaces the org's verification token with a fresh random value
// and returns it. Used for both initial creation and rotation. Old tokens stop
// matching immediately — admins must update their DNS TXT record after rotation.
// Returns ErrSSONotConfigured if the org has no SSO config (so the handler can
// map it to 404 rather than 500).
func (s *Service) RotateToken(ctx context.Context, orgID string) (string, error) {
	cfg, err := s.orgs.GetSSOConfig(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("load sso config: %w", err)
	}
	if cfg == nil {
		return "", ErrSSONotConfigured
	}
	token, err := s.orgs.RotateVerificationToken(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("rotate verification token: %w", err)
	}
	return token, nil
}

// StartResult is the outcome of StartLogin: the IdP authorization URL to
// redirect the browser to, and the signed state cookie to set on the response.
type StartResult struct {
	AuthURL string
	Cookie  *SignedCookie
}

// SignedCookie carries the Set-Cookie value plus its Max-Age.
type SignedCookie struct {
	Name    string
	Value   string
	MaxAge  time.Duration
	Expires time.Time
}

// StartLogin begins the OIDC Authorization Code + PKCE flow for an org.
// redirectURL is the absolute callback URL registered with the IdP. The handler
// resolves it from OIDC.RedirectBaseURL and fails with ErrRedirectBaseURLNotSet
// when that is unset (F11: the handler never derives it from the request).
func (s *Service) StartLogin(ctx context.Context, orgSlug, redirectURL string) (*StartResult, error) {
	if s.stateKey == nil {
		return nil, errors.New("SSO state signing key not configured")
	}
	org, err := s.orgs.GetOrgBySlug(ctx, orgSlug)
	if err != nil {
		return nil, fmt.Errorf("lookup org by slug: %w", err)
	}
	if org == nil {
		return nil, ErrSSONotConfigured
	}
	cfg, err := s.orgs.GetSSOConfig(ctx, org.ID)
	if err != nil {
		return nil, fmt.Errorf("load sso config: %w", err)
	}
	if cfg == nil {
		return nil, ErrSSONotConfigured
	}

	clientSecret, err := s.decryptSecret(ctx, cfg.ClientSecret)
	if err != nil {
		return nil, err
	}

	provider, err := oidc.NewProvider(ctx, cfg.DiscoveryURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery failed: %w", err)
	}
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile", "groups"},
	}

	verifier, err := randomToken(32)
	if err != nil {
		return nil, fmt.Errorf("generate pkce verifier: %w", err)
	}
	state, err := randomToken(24)
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	authURL := oauthCfg.AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("code_challenge", codeChallenge(verifier)),
	)

	cookie, err := s.issueStateCookie(state, verifier, org.ID)
	if err != nil {
		return nil, err
	}
	return &StartResult{AuthURL: authURL, Cookie: cookie}, nil
}

// CallbackResult is the outcome of HandleCallback.
type CallbackResult struct {
	Token       string
	UserID      string
	Email       string
	CreatedUser bool
	Role        types.OrgRole
}

// HandleCallback completes the OIDC flow: verifies state, exchanges the code,
// verifies the ID token, resolves (or auto-provisions) the user, applies the
// group-claim → role mapping, ensures org membership, and issues a JWT.
// redirectURL must match the URL passed to StartLogin (the IdP-registered callback).
func (s *Service) HandleCallback(ctx context.Context, orgSlug, redirectURL, code, state, cookieValue string) (*CallbackResult, error) {
	if s.stateKey == nil {
		return nil, errors.New("SSO state signing key not configured")
	}
	payload, err := s.verifyStateCookie(cookieValue)
	if err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare([]byte(payload.State), []byte(state)) != 1 {
		return nil, ErrStateInvalid
	}
	// Bind the callback to the org the start flow bound. This prevents an
	// attacker from initiating SSO for org A and replaying the callback against
	// org B's endpoint (which would provision membership in the wrong org).
	org, err := s.orgs.GetOrgBySlug(ctx, orgSlug)
	if err != nil {
		return nil, fmt.Errorf("lookup org by slug: %w", err)
	}
	if org == nil || org.ID != payload.OrgID {
		return nil, ErrStateInvalid
	}
	cfg, err := s.orgs.GetSSOConfig(ctx, org.ID)
	if err != nil {
		return nil, fmt.Errorf("load sso config: %w", err)
	}
	if cfg == nil {
		return nil, ErrSSONotConfigured
	}

	clientSecret, err := s.decryptSecret(ctx, cfg.ClientSecret)
	if err != nil {
		return nil, err
	}
	provider, err := oidc.NewProvider(ctx, cfg.DiscoveryURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery failed: %w", err)
	}
	oauthCfg := &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
	}
	token, err := oauthCfg.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", payload.Verifier),
	)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}

	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		return nil, errors.New("oidc provider did not return an id_token")
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("id token verification failed: %w", err)
	}

	var claims oidcClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("decode id token claims: %w", err)
	}
	if claims.Email == "" {
		return nil, errors.New("id token missing email claim")
	}
	// F8 (US-43.10 / D17): the OIDC `email` claim is only authoritative for
	// account binding when the IdP has verified it. An absent or false
	// email_verified claim is treated as unverified (per spec) so a permissive
	// IdP cannot be used to take over an existing account by registering its
	// email without verification. Org admins are responsible for configuring an
	// IdP that emits a true email_verified claim.
	if !claims.EmailVerified {
		return nil, ErrEmailUnverified
	}

	user, created, err := s.resolveUser(ctx, claims.Email, claims.Name, cfg.AutoProvision)
	if err != nil {
		return nil, err
	}
	if user.Status == types.UserStatusSuspended {
		return nil, ErrUserSuspended
	}

	role := resolveRole(claims.effectiveGroups(), cfg.GroupRoleMapping)
	if err := s.ensureMembership(ctx, org.ID, user.ID, role); err != nil {
		return nil, fmt.Errorf("ensure membership: %w", err)
	}

	jwt, err := s.issueSession(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	return &CallbackResult{
		Token:       jwt,
		UserID:      user.ID,
		Email:       user.Email,
		CreatedUser: created,
		Role:        role,
	}, nil
}

// issueSession issues the post-SSO session token. When a UserKeyManager is
// wired (Epic 58), it also provisions a server-KEK DEK for users who have none
// and unlocks it for the new session — so an auto-provisioned SSO user can save
// personal secrets without ever setting a password. Falls back to the plain
// TokenIssuer when no manager is wired (pre-epic / test setups).
func (s *Service) issueSession(ctx context.Context, userID string) (string, error) {
	if s.keyManager != nil {
		return s.keyManager.IssueTokenAndUnlockDEK(ctx, userID, s.tokenTTL)
	}
	return s.issuer.GenerateToken(userID)
}

// resolveUser returns the existing user for the email, or auto-provisions a new
// one when permitted. A new SSO account gets a random unusable password — the
// user has no password to derive a DEK from, so per D17-S1 personal credential
// operations stay unavailable until they set one. Org workspaces still work
// (server-side injection).
func (s *Service) resolveUser(ctx context.Context, email, name string, autoProvision bool) (*types.User, bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	user, err := s.users.GetUserByEmail(ctx, email)
	if err != nil {
		return nil, false, fmt.Errorf("lookup user by email: %w", err)
	}
	if user != nil {
		return user, false, nil
	}
	if !autoProvision {
		return nil, false, ErrAutoProvisionOff
	}
	username := name
	if username == "" {
		username = strings.Split(email, "@")[0]
	}
	raw, err := randomToken(32)
	if err != nil {
		return nil, false, fmt.Errorf("generate provisional password: %w", err)
	}
	user = &types.User{
		ID:           uuid.NewString(),
		Username:     username,
		Email:        email,
		Active:       true,
		Role:         "user",
		Status:       types.UserStatusActive,
		PasswordHash: "$2a$12$" + raw, //nolint:gosec // not a real hash — random, never verifiable; blocks password login
	}
	if err := s.users.CreateUser(ctx, user); err != nil {
		return nil, false, fmt.Errorf("auto-provision user: %w", err)
	}
	return user, true, nil
}

// ensureMembership makes sure the user belongs to the org with the given role,
// creating or updating the membership row as needed (D17-S3 refresh on login).
// A demotion admin→member is skipped when it would leave the org with no admins,
// so an IdP group change can never orphan the org (cf. D19 last-admin rule).
func (s *Service) ensureMembership(ctx context.Context, orgID, userID string, role types.OrgRole) error {
	if role == "" {
		role = types.OrgRoleMember
	}
	member, err := s.orgs.GetOrgMember(ctx, orgID, userID)
	if err != nil {
		return err
	}
	if member == nil {
		return s.orgs.AddOrgMember(ctx, orgID, userID, role)
	}
	if member.Role == role {
		return nil
	}
	// Demoting the user: refuse if they are the only admin. Promotions
	// (member→admin) and same-role updates are always safe.
	if role != types.OrgRoleAdmin && member.Role == types.OrgRoleAdmin {
		count, err := s.orgs.CountOrgAdmins(ctx, orgID)
		if err != nil {
			return err
		}
		if count <= 1 {
			// Keep the user as admin to avoid orphaning the org. The IdP
			// group change is recorded but the safety guard wins. Logged so
			// operators can detect stale IdP group mappings.
			if s.logger != nil {
				s.logger.Warn("sso: skipped demotion of sole org admin to preserve manageability",
					"orgID", orgID, "userID", userID)
			}
			return nil
		}
	}
	return s.orgs.UpdateOrgMemberRole(ctx, orgID, userID, role)
}

func (s *Service) decryptSecret(ctx context.Context, blob []byte) (string, error) {
	if s.keyProvider == nil {
		return "", errors.New("server key not configured")
	}
	plain, err := s.keyProvider.Decrypt(ctx, blob)
	if err != nil {
		return "", fmt.Errorf("decrypt client secret: %w", err)
	}
	return string(plain), nil
}

// RedirectBaseURL returns the configured absolute base for SSO callback URLs.
// When empty, the handler refuses to build a callback URL and returns
// ErrRedirectBaseURLNotSet rather than deriving one from the request (F11).
func (s *Service) RedirectBaseURL() string { return s.redirectBase }

// --- HMAC-signed state cookie ---

type cookiePayload struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	OrgID    string `json:"o"`
	Exp      int64  `json:"e"`
}

func (s *Service) issueStateCookie(state, verifier, orgID string) (*SignedCookie, error) {
	exp := time.Now().Add(s.stateTTL)
	payload := cookiePayload{State: state, Verifier: verifier, OrgID: orgID, Exp: exp.Unix()}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode state cookie: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, s.stateKey)
	mac.Write([]byte(encoded))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return &SignedCookie{
		Name:    s.cookieName,
		Value:   encoded + "." + sig,
		MaxAge:  s.stateTTL,
		Expires: exp,
	}, nil
}

func (s *Service) verifyStateCookie(value string) (*cookiePayload, error) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return nil, ErrStateInvalid
	}
	encoded, sig := parts[0], parts[1]
	mac := hmac.New(sha256.New, s.stateKey)
	mac.Write([]byte(encoded))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expectedSig)) != 1 {
		return nil, ErrStateInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, ErrStateInvalid
	}
	var payload cookiePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, ErrStateInvalid
	}
	if time.Now().Unix() >= payload.Exp {
		return nil, ErrStateExpired
	}
	return &payload, nil
}

// --- helpers ---

// oidcClaims is the claim set extracted from the verified ID token. Groups
// supports both the OIDC `groups` claim and Azure AD's `memberOf` (array of
// strings) as a compatibility fallback.
type oidcClaims struct {
	Email         string   `json:"email"`
	EmailVerified bool     `json:"email_verified"`
	Name          string   `json:"name"`
	Groups        []string `json:"groups"`
	MemberOf      []string `json:"memberOf"`
}

// effectiveGroups returns groups ∪ memberOf for role-mapping evaluation.
func (c oidcClaims) effectiveGroups() []string {
	if len(c.MemberOf) == 0 {
		return c.Groups
	}
	merged := make([]string, 0, len(c.Groups)+len(c.MemberOf))
	merged = append(merged, c.Groups...)
	return append(merged, c.MemberOf...)
}

// resolveRole maps the IdP group claims to the highest-privilege org role
// present in group_role_mapping. "admin" outranks "member"; unmapped groups and
// an empty mapping both yield "member" (the safe default for an authenticated
// org member).
func resolveRole(groups []string, mapping map[string]types.OrgRole) types.OrgRole {
	best := types.OrgRoleMember
	for _, g := range groups {
		role, ok := mapping[g]
		if !ok {
			continue
		}
		if role == types.OrgRoleAdmin {
			return types.OrgRoleAdmin
		}
		best = role
	}
	return best
}

// codeChallenge computes the S256 PKCE code_challenge from a verifier.
func codeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// randomToken returns a URL-safe random string of approx 4*n/3 bytes.
func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
