// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
)

// SecretInjector decrypts and serializes the secrets bound to a workspace,
// using the calling user's session DEK to decrypt user-owned credentials
// and user_secrets entries (ssh-key, env-secret, secret-file, etc.).
//
// Used by handlers authenticated via JWT — the bind-time live-push and
// the explicit POST /api/v1/workspaces/:id/reload-secrets endpoint. The
// sessionID is mandatory for these handlers; without it user-DEK content
// cannot be decrypted.
//
// Implementations: *SecretService.
type SecretInjector interface {
	InjectSecrets(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, workspaceID string) ([]byte, error)
}

// SessionlessSecretInjector returns the subset of workspace secrets that
// can be decrypted without a user session — admin and org provider
// credentials, encrypted with server-KEK material (US-50.2 RootKeyProvider).
//
// Used by callers without a user session:
//
//   - Pod bootstrap (Epic 35 US-35.3): the init container POSTs the API
//     with a projected SA token; there is no JWT and no DEK in flight.
//     NOTE: pod-bootstrap now prefers PodBootstrapSecretInjector when
//     available (design/0045). SessionlessSecretInjector remains the
//     fallback contract for callers that cannot request user-DEK on
//     the caller's behalf.
//
//   - API-key authenticated handlers (e.g. SDK calls without a JWT): the
//     handler cannot decrypt user-DEK content, so the SessionlessSecretInjector
//     gives it the server-KEK subset only. The user-DEK content is delivered
//     later by a JWT-authenticated reload (the existing two-phase pattern
//     documented in commit 4b48a4e7).
//
// User-owned credentials and user_secrets entries are intentionally
// omitted; they are emitted as audit events ("secret_skipped_no_session")
// so operators have observability into what was deferred. Implementations
// MUST audit every skipped binding to preserve the existing observability
// contract documented in pkg/secrets/secret_service.go (M-5 fix).
//
// Implementations: *SecretService.
type SessionlessSecretInjector interface {
	InjectSessionlessSecrets(ctx context.Context, userID, workspaceID string) ([]byte, error)
}

// PodBootstrapSecretInjector is the pod-bootstrap-path variant that
// attempts a best-effort user-DEK unwrap via KeyService.GetDEKForUser
// (which walks jwt_sessions rows and the enumerator's retained signing
// keys) and, on success, decrypts user-DEK bindings alongside
// server-KEK bindings — as if a JWT-authenticated request had been made.
//
// On DEK-unavailable (no active jwt_sessions, or none unwrappable),
// implementations MUST degrade to SessionlessSecretInjector semantics:
// user-DEK bindings audited and skipped, server-KEK-only payload
// returned. The pod boots with the reduced set and the auto-push flow
// will still deliver user-DEK when the user next logs in.
//
// Trust model: callers (pod-bootstrap handler) authenticate the workspace
// SA token via TokenReview and verify the request is on behalf of
// workspace X. The workspace CRD lists X's owner as the principal whose
// DEK is fetched. This is not privilege escalation — the pod would
// receive these same secrets via reload-secrets anyway. See
// design/0045_2026-07-06_boot-time-user-dek-delivery.md § Threat model.
//
// Implementations: *SecretService.
type PodBootstrapSecretInjector interface {
	InjectSecretsForPodBootstrap(ctx context.Context, userID, workspaceID string) ([]byte, error)
}

// Compile-time assertions that *SecretService satisfies both interfaces.
var (
	_ SecretInjector             = (*SecretService)(nil)
	_ SessionlessSecretInjector  = (*SecretService)(nil)
	_ PodBootstrapSecretInjector = (*SecretService)(nil)
)

// InjectedSecret is a single secret entry in the secrets.json file
// that the init container reads to materialize secrets.
type InjectedSecret struct {
	Type      SecretType      `json:"type"`
	Name      string          `json:"name"`
	Metadata  json.RawMessage `json:"metadata"`
	Plaintext string          `json:"plaintext"`
}

// InjectSecrets implements SecretInjector. See interface godoc.
//
// Workspace ownership is enforced by WorkspaceAccessMiddleware on
// POST /:id/reload-secrets (design 0041 D5) and is inherently true for
// the bind-time push path inside the SecretsHandler (the caller is the
// workspace owner). This method does not re-check ownership — the
// HTTP layer must.
//
// ARCHITECTURAL NOTE — credential-class delivery semantics:
//
// Admin (owner_type='admin') and org (owner_type='org') credentials use
// server-side KEKs derived in pkg/secrets/root_key.go. They can be
// decrypted regardless of session, and are always included.
//
// User credentials (owner_type='user') and user_secrets entries (ssh-key,
// env-secret, etc.) are encrypted with the user's DEK, which requires
// an active authenticated session. When sessionID identifies a session
// without a cached DEK (expired, evicted, or never unlocked), the
// affected entries are skipped with an audit event and the workspace
// falls back to lower-priority server-KEK entries.
//
// Callers without any session at all (init container, API-key auth)
// MUST use SessionlessSecretInjector instead of passing an empty
// sessionID here — that path was previously the source of bug class
// "buildNonLLMSecrets propagates GetDEK error" (this worklog,
// 2026-06-24 production incident).
func (s *SecretService) InjectSecrets(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, workspaceID string) ([]byte, error) {
	credStore, err := s.requireCredentialStore()
	if err != nil {
		return nil, err
	}

	bindings, err := credStore.GetWorkspaceCredentials(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get workspace credentials: %w", err)
	}

	providerData := s.loadLLMCredentials(ctx, bindings, userID, workspaceID, sessionID, matchedSigningKey)

	nonLLM, err := s.loadNonLLMSecrets(ctx, userID, sessionID, matchedSigningKey, workspaceID)
	if err != nil {
		return nil, err
	}

	return buildSecretsJSON(providerData, nonLLM)
}

// InjectSessionlessSecrets implements SessionlessSecretInjector. See
// interface godoc.
//
// Returns server-KEK-decryptable credentials only. User-DEK bindings are
// not just skipped — they are audited via "secret_skipped_no_session"
// events so operators have signal that a workspace's user-owned content
// is awaiting a JWT-authenticated delivery via reload-secrets. Without
// auditing, an operator inspecting "why is the agent not seeing my SSH
// key after a pod restart" has no breadcrumb at all.
func (s *SecretService) InjectSessionlessSecrets(ctx context.Context, userID, workspaceID string) ([]byte, error) {
	credStore, err := s.requireCredentialStore()
	if err != nil {
		return nil, err
	}

	bindings, err := credStore.GetWorkspaceCredentials(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get workspace credentials: %w", err)
	}

	// LLM loop: loadServerKEKCredentials emits "credential_skipped_no_session"
	// for each user binding (cleaner messaging than loadLLMCredentials's
	// "credential_decrypt_failed" with error="DEK not available" — the
	// latter is technically accurate but operator-confusing).
	providerData := s.loadServerKEKCredentials(ctx, bindings, userID, workspaceID)

	// Non-LLM loop: loadNonLLMSecrets is called with sessionID="". The
	// degrade path inside that function (post-PR-#407 review pass 2)
	// emits a "secret_skipped_no_session" audit per relevant
	// user_secret and returns nil. No separate audit pass needed.
	nonLLM, err := s.loadNonLLMSecrets(ctx, userID, "", nil, workspaceID)
	if err != nil {
		return nil, err
	}

	return buildSecretsJSON(providerData, nonLLM)
}

// InjectSecretsForPodBootstrap implements PodBootstrapSecretInjector.
// See interface godoc and design/0045_2026-07-06_boot-time-user-dek-delivery.md.
//
// Attempts a best-effort user-DEK unwrap via KeyService.GetDEKForUser. On
// success, delegates to InjectSecrets so user-DEK bindings decrypt
// through the normal (dek, jti) → decryptBinding path. GetDEKForUser
// writes the unwrapped DEK back to Redis under the returned jti, so the
// downstream GetDEK(jti) call in decryptBinding hits the cache — one
// unwrap per request, not per binding.
//
// On DEK-unavailable (no active jwt_sessions row for this user, none
// unwrappable with retained signing keys, or KeyService not wired at
// all), falls back to InjectSessionlessSecrets. The pod boots with
// server-KEK-only secrets; the auto-push flow (secretautopush) will
// deliver user-DEK secrets when the user next logs in.
//
// Errors from GetDEKForUser other than ErrDEKUnavailable are treated as
// "unavailable" and degrade the same way: a transient DB failure at pod
// boot must not fail the boot; auto-push will retry once the API
// recovers. The specific error is not logged here because
// GetDEKForUser's callers (secretautopush.run, this method) both treat
// it uniformly — the KeyService's own logs already record the failure.
func (s *SecretService) InjectSecretsForPodBootstrap(ctx context.Context, userID, workspaceID string) ([]byte, error) {
	// s.keys is set at construction (NewSecretService); tests may pass
	// nil to isolate the secret store from key wiring. When nil, we
	// cannot fetch a DEK — degrade to the sessionless path.
	if s.keys == nil {
		return s.InjectSessionlessSecrets(ctx, userID, workspaceID)
	}

	// Best-effort unwrap. Any failure (ErrDEKUnavailable, list-error,
	// unwrap-error) collapses to "no session" via the sessionless
	// fallback. This preserves the "init container never blocks pod
	// boot" invariant from Epic 35.
	_, jti, err := s.keys.GetDEKForUser(ctx, userID)
	if err != nil || jti == "" {
		return s.InjectSessionlessSecrets(ctx, userID, workspaceID)
	}

	// InjectSecrets threads sessionID=jti and matchedSigningKey=nil.
	// The downstream GetDEK call (via decryptBinding) reads the DEK
	// from Redis cache under jti — populated as a side effect of
	// GetDEKForUser succeeding above. This is the same pattern
	// secretautopush.run uses (service.go:238-250).
	return s.InjectSecrets(ctx, userID, jti, nil, workspaceID)
}

// requireCredentialStore casts the configured store to CredentialStore
// (the interface the multi-source credential path needs). All production
// store types implement this; if the cast fails, a wrapper was added
// without implementing it and we want to return an explicit error rather
// than silently fall through to a partial path (H-3 fix).
func (s *SecretService) requireCredentialStore() (CredentialStore, error) {
	credStore, ok := s.store.(CredentialStore)
	if !ok {
		return nil, fmt.Errorf("store does not implement CredentialStore: ensure all store wrappers implement CredentialStore")
	}
	return credStore, nil
}

// loadLLMCredentials runs the multi-source LLM-credential loop. Each
// binding is decrypted using the appropriate KEK (admin/org via
// RootKeyProvider, user via session DEK). Failures are audited with
// "credential_decrypt_failed" and the loop continues so a lower-priority
// binding can take over (e.g. the admin free-tier covers a user whose
// session DEK is missing).
//
// The (admin/org/user) decryption matrix is identical across the
// session and sessionless paths; what differs is whether sessionID is
// non-empty. When empty, every user-bound binding fails the GetDEK call,
// audits, and continues — no error propagates.
func (s *SecretService) loadLLMCredentials(ctx context.Context, bindings []CredentialBinding, userID, workspaceID, sessionID string, matchedSigningKey []byte) []LLMProviderData {
	adminDecrypt := decryptFnFor(s.adminProvider)
	orgDecrypt := decryptFnFor(s.orgProvider)

	// Dedup by Slug — Slug is the per-owner unique identity (Epic 55).
	// Kind is NOT unique per owner (two openai_compatible LiteLLM
	// endpoints with different slugs both materialize as separate
	// providers in agent-config.json).
	seen := make(map[string]bool)
	var out []LLMProviderData
	for _, b := range bindings {
		if seen[b.Slug] {
			continue
		}
		pd, err := s.decryptBinding(ctx, b, sessionID, matchedSigningKey, adminDecrypt, orgDecrypt)
		if err != nil {
			// Don't set seen — allow fallback to lower-priority binding.
			s.audit(ctx, userID, "credential_decrypt_failed", nil, &workspaceID,
				map[string]string{"credentialID": b.ID, "slug": b.Slug, "kind": b.Kind, "ownerType": b.OwnerType, "error": err.Error()})
			continue
		}
		s.applyModelAllowlist(&pd, b)
		seen[b.Slug] = true
		out = append(out, pd)
	}
	return out
}

// loadServerKEKCredentials is loadLLMCredentials with all user-owned
// bindings audited and skipped explicitly. It emits "credential_skipped_no_session"
// rather than calling decryptBinding (which would emit
// "credential_decrypt_failed" for every user binding — semantically
// misleading because there is no decrypt failure, just no session).
func (s *SecretService) loadServerKEKCredentials(ctx context.Context, bindings []CredentialBinding, userID, workspaceID string) []LLMProviderData {
	adminDecrypt := decryptFnFor(s.adminProvider)
	orgDecrypt := decryptFnFor(s.orgProvider)

	seen := make(map[string]bool)
	var out []LLMProviderData
	for _, b := range bindings {
		if seen[b.Slug] {
			continue
		}
		if b.OwnerType == "user" {
			s.audit(ctx, userID, "credential_skipped_no_session", nil, &workspaceID,
				map[string]string{"credentialID": b.ID, "slug": b.Slug, "kind": b.Kind, "ownerType": b.OwnerType})
			continue
		}
		// Sessionless decryption: admin/org only. Pass empty sessionID
		// and nil matchedSigningKey; decryptBinding never reaches the
		// user branch for these owner_types.
		pd, err := s.decryptBinding(ctx, b, "", nil, adminDecrypt, orgDecrypt)
		if err != nil {
			s.audit(ctx, userID, "credential_decrypt_failed", nil, &workspaceID,
				map[string]string{"credentialID": b.ID, "slug": b.Slug, "kind": b.Kind, "ownerType": b.OwnerType, "error": err.Error()})
			continue
		}
		s.applyModelAllowlist(&pd, b)
		seen[b.Slug] = true
		out = append(out, pd)
	}
	return out
}

// applyModelAllowlist filters pd.Models against the credential's
// per-binding allowlist. Extracted from the original loop verbatim
// (no behavior change) so the two LLM loops can share it.
func (s *SecretService) applyModelAllowlist(pd *LLMProviderData, b CredentialBinding) {
	if len(b.ModelAllowlist) == 0 {
		return
	}
	allowed := make(map[string]bool, len(b.ModelAllowlist))
	for _, id := range b.ModelAllowlist {
		// Skip obviously invalid model IDs. The allowlist is stored as
		// a DB array and can accumulate stale entries (e.g. the literal
		// "default" from a mis-formed create request). An invalid ID
		// passed to FormatOpenCodeConfig produces a provider entry
		// with no valid models, causing opencode to treat the provider
		// as unconfigured and return 0 providers.
		if id == "" || id == "default" {
			continue
		}
		allowed[id] = true
	}
	var filtered []LLMModelConfig
	for _, m := range pd.Models {
		if allowed[m.ID] {
			if m.ContextLimit == 0 {
				m.ContextLimit = b.ModelContextLimits[m.ID]
			}
			if m.OutputLimit == 0 {
				m.OutputLimit = b.ModelOutputLimits[m.ID]
			}
			filtered = append(filtered, m)
		}
	}
	// If pd.Models is empty (credentials don't carry a model list) but
	// the allowlist has valid IDs, synthesize LLMModelConfig entries so
	// the provider is rendered with an explicit model allowlist.
	if len(filtered) == 0 && len(allowed) > 0 {
		filtered = make([]LLMModelConfig, 0, len(allowed))
		for _, id := range b.ModelAllowlist {
			if allowed[id] {
				filtered = append(filtered, LLMModelConfig{
					ID:           id,
					ContextLimit: b.ModelContextLimits[id],
					OutputLimit:  b.ModelOutputLimits[id],
				})
			}
		}
	}
	pd.Models = filtered
}

// decryptFn is a provider-bound decryption closure (US-50.2). It is nil when
// the corresponding provider was not wired, so decryptBinding can skip that
// credential class cleanly instead of panicking.
type decryptFn func(ctx context.Context, ciphertext []byte) ([]byte, error)

// decryptFnFor adapts a RootKeyProvider to a decryptFn. Returns nil when the
// provider is nil so callers can treat "not configured" uniformly.
func decryptFnFor(p RootKeyProvider) decryptFn {
	if p == nil {
		return nil
	}
	return p.Decrypt
}

func (s *SecretService) decryptBinding(ctx context.Context, b CredentialBinding, sessionID string, matchedSigningKey []byte, adminDecrypt, orgDecrypt decryptFn) (LLMProviderData, error) {
	var plaintext []byte
	switch b.OwnerType {
	case "user":
		dek, err := s.keys.GetDEK(ctx, sessionID, matchedSigningKey)
		if err != nil {
			return LLMProviderData{}, fmt.Errorf("get user DEK: %w", err)
		}
		plaintext, err = DecryptSecret(dek, b.Ciphertext)
		if err != nil {
			return LLMProviderData{}, err
		}
	case "admin":
		if adminDecrypt == nil {
			return LLMProviderData{}, fmt.Errorf("admin RootKeyProvider not configured")
		}
		pt, err := adminDecrypt(ctx, b.Ciphertext)
		if err != nil {
			return LLMProviderData{}, err
		}
		plaintext = pt
	case "org":
		if orgDecrypt == nil {
			return LLMProviderData{}, fmt.Errorf("org RootKeyProvider not configured")
		}
		pt, err := orgDecrypt(ctx, b.Ciphertext)
		if err != nil {
			return LLMProviderData{}, err
		}
		plaintext = pt
	default:
		return LLMProviderData{}, fmt.Errorf("unsupported owner_type %q", b.OwnerType)
	}
	var pd LLMProviderData
	if err := json.Unmarshal(plaintext, &pd); err != nil {
		return LLMProviderData{}, fmt.Errorf("unmarshal LLMProviderData: %w", err)
	}
	return pd, nil
}

// loadNonLLMSecrets returns user_secrets entries that are NOT
// llm-provider type — ssh-key, env-secret, secret-file, git-credential,
// and the legacy api-key (sunset 2026-12-19 but still loaded for
// existing creds).
//
// All non-LLM user_secrets are user-DEK encrypted, so decryption
// requires the caller's session DEK. Three cases the loop must
// handle without propagating an error:
//
//  1. No session at all (sessionID==""). Bootstrap path — every
//     user-DEK entry is skipped with an audit and the LLM/server-KEK
//     content is delivered.
//
//  2. Pseudo-session without a DEK (sessionID=="apikey:hash" or
//     sessionID set to a JWT jti whose DEK has expired/evicted).
//     API-key auth path and stale-JWT path. Same outcome as (1):
//     skip-with-audit, deliver everything else.
//
//  3. Real session with a real DEK. JWT-authenticated path —
//     decrypt and emit each entry; per-entry decrypt failures
//     audit-and-continue so a single corrupted ciphertext does
//     not poison delivery of the others.
//
// Pre-PR-#407 review-pass-2: the function used to hard-error in cases
// (1) and (2), making `loadServerKEKCredentials` necessary as a
// parallel implementation and forcing pushSecretsToAgent to branch on
// sessionID — which was dead code because API-key auth sets a
// non-empty pseudo-sessionID. The current implementation degrades
// gracefully so a single code path covers every caller; the
// SessionlessSecretInjector interface is kept for type-system
// expressiveness (callers who semantically have no session declare
// that intent at the API surface). InjectSessionlessSecrets is
// functionally equivalent to calling InjectSecrets with an empty
// sessionID — both flow through this graceful-degrade path — but
// they differ in LLM-loop audit messaging: the sessionless path emits
// "credential_skipped_no_session" via loadServerKEKCredentials,
// whereas InjectSecrets emits "credential_decrypt_failed" with a
// "DEK not available" error string. The user-visible outcome is the
// same; the audit trail is cleaner for the explicit-no-session caller.
func (s *SecretService) loadNonLLMSecrets(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, workspaceID string) ([]InjectedSecret, error) {
	bound, err := s.store.GetBindings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	var relevant []*UserSecret
	for _, secret := range bound {
		if secret.UserID == userID && secret.Type != SecretTypeLLMProvider {
			relevant = append(relevant, secret)
		}
	}
	if len(relevant) == 0 {
		return nil, nil
	}
	dek, err := s.keys.GetDEK(ctx, sessionID, matchedSigningKey)
	if err != nil {
		// No DEK available. Audit each user-DEK entry as skipped and
		// return the empty slice without error — this is the case (1)
		// and case (2) graceful degrade described in the godoc above.
		// The next reload-secrets push from a JWT-authenticated session
		// will retry decrypt and either succeed or fail loudly with
		// per-entry audits in the case (3) path below.
		for _, secret := range relevant {
			sid := secret.ID
			s.audit(ctx, userID, "secret_skipped_no_session", &sid, &workspaceID,
				map[string]string{"name": secret.Name, "type": string(secret.Type), "reason": err.Error()})
		}
		return nil, nil
	}
	var out []InjectedSecret
	for _, secret := range relevant {
		plaintext, err := DecryptSecret(dek, secret.Ciphertext)
		if err != nil {
			sid := secret.ID
			s.audit(ctx, userID, "secret_decrypt_failed", &sid, &workspaceID,
				map[string]string{"name": secret.Name, "type": string(secret.Type), "error": err.Error()})
			continue
		}
		out = append(out, InjectedSecret{
			Type:      secret.Type,
			Name:      secret.Name,
			Metadata:  secret.Metadata,
			Plaintext: string(plaintext),
		})
	}
	return out, nil
}

func buildSecretsJSON(providerData []LLMProviderData, nonLLM []InjectedSecret) ([]byte, error) {
	out := make([]InjectedSecret, 0, len(providerData)+len(nonLLM))
	for _, pd := range providerData {
		plaintext, err := json.Marshal(pd) //nolint:gosec // marshaling for secrets.json injection, not API response
		if err != nil {
			return nil, err
		}
		out = append(out, InjectedSecret{
			Type: SecretTypeLLMProvider,
			// Name on the InjectedSecret is the slug — this becomes the
			// provider-map key in agent-config.json (Epic 55). Before
			// Epic 55 this was the SDK kind ("custom", "opencode"),
			// causing identity collisions when two credentials shared
			// the same SDK. opencode persists this as `providerID` on
			// session records.
			Name:      pd.Slug,
			Plaintext: string(plaintext),
		})
	}
	out = append(out, nonLLM...)
	return json.Marshal(out)
}
