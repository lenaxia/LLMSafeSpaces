// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/validation"
)

// SecretService provides encrypted secret CRUD operations.
type SecretService struct {
	keys          *KeyService
	store         SecretStore
	adminProvider RootKeyProvider // US-50.2: wraps deriveServerKey("provider-credentials")
	orgProvider   RootKeyProvider // US-50.2: wraps deriveServerKey("org-credentials")
}

// NewSecretService creates a new SecretService.
//
// As a side-effect we register the SecretStore on the KeyService so
// (Historical: secret re-encryption was used by the now-removed RotateKeyWithPassword, Bug 9 in
// worklog 0085). The two services share a store anyway; this just makes
// the linkage explicit at construction time.
func NewSecretService(keys *KeyService, store SecretStore) *SecretService {
	if keys != nil {
		keys.SetSecretStore(store)
	}
	return &SecretService{keys: keys, store: store}
}

// SetAdminProvider installs the RootKeyProvider for admin (owner_type='admin')
// provider credentials. When non-nil, the injector methods (InjectSecrets and
// InjectSessionlessSecrets) decrypt admin bindings through it; when nil, admin
// bindings are skipped with an audit event.
func (s *SecretService) SetAdminProvider(p RootKeyProvider) {
	s.adminProvider = p
}

// SetOrgProvider installs the RootKeyProvider for org (owner_type='org') provider
// credentials. When non-nil, the injector methods (InjectSecrets and
// InjectSessionlessSecrets) decrypt org bindings through it; when nil, org
// bindings are skipped with an audit event.
func (s *SecretService) SetOrgProvider(p RootKeyProvider) {
	s.orgProvider = p
}

// CreateSecret encrypts and stores a new secret.
func (s *SecretService) CreateSecret(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, req CreateSecretRequest) (*SecretResponse, error) {
	if !ValidSecretTypes[req.Type] {
		return nil, fmt.Errorf("%w: %s (valid: %s)",
			ErrInvalidSecretType, req.Type, formatSecretTypes(ValidSecretTypesList()))
	}

	if req.Type == SecretTypeAPIKey && isAPIKeySunset() {
		return nil, fmt.Errorf("%w: api-key secret type was removed on %s; migrate to llm-provider (for LLM APIs) or env-secret (for other APIs). See docs/migration/api-key-to-llm-provider.md",
			ErrInvalidSecretType, APIKeySunsetDate)
	}

	if err := validation.ValidateSecretName(req.Name); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidMetadata, err)
	}

	if err := validateMetadata(req.Type, req.Metadata); err != nil {
		return nil, err
	}

	if err := validateValue(req.Type, req.Value); err != nil {
		return nil, err
	}

	dek, err := s.keys.GetDEK(ctx, sessionID, matchedSigningKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDEKUnavailable, err)
	}

	ciphertext, err := EncryptSecret(dek, []byte(req.Value))
	if err != nil {
		return nil, fmt.Errorf("encrypt secret: %w", err)
	}

	// Get current key version
	record, err := s.keys.store.GetUserKey(ctx, userID)
	if err != nil || record == nil {
		return nil, ErrUserKeysMissing
	}

	metadata := req.Metadata
	if metadata == nil {
		metadata = json.RawMessage("{}")
	}

	secret := &UserSecret{
		UserID:        userID,
		Name:          req.Name,
		Type:          req.Type,
		Ciphertext:    ciphertext,
		KeyVersion:    record.KeyVersion,
		Metadata:      metadata,
		GlobalDefault: req.GlobalDefault,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}

	if err := s.store.CreateSecret(ctx, secret); err != nil {
		return nil, fmt.Errorf("store secret: %w", err)
	}

	s.audit(ctx, userID, "create", &secret.ID, nil, map[string]string{"name": req.Name, "type": string(req.Type)})

	return &SecretResponse{
		ID:            secret.ID,
		Name:          secret.Name,
		Type:          secret.Type,
		Metadata:      secret.Metadata,
		GlobalDefault: secret.GlobalDefault,
		CreatedAt:     secret.CreatedAt,
		UpdatedAt:     secret.UpdatedAt,
	}, nil
}

// GetSecretByName returns secret metadata by name (never the value).
func (s *SecretService) GetSecretByName(ctx context.Context, userID, name string) (*SecretResponse, error) {
	secret, err := s.store.GetSecretByName(ctx, userID, name)
	if err != nil {
		return nil, err
	}
	if secret == nil {
		return nil, nil
	}
	return &SecretResponse{
		ID:            secret.ID,
		Name:          secret.Name,
		Type:          secret.Type,
		Metadata:      secret.Metadata,
		GlobalDefault: secret.GlobalDefault,
		CreatedAt:     secret.CreatedAt,
		UpdatedAt:     secret.UpdatedAt,
	}, nil
}

// GetSecret returns secret metadata (never the value).
func (s *SecretService) GetSecret(ctx context.Context, userID, secretID string) (*SecretResponse, error) {
	secret, err := s.store.GetSecret(ctx, userID, secretID)
	if err != nil {
		return nil, err
	}
	if secret == nil {
		return nil, ErrSecretNotFound
	}
	return &SecretResponse{
		ID:            secret.ID,
		Name:          secret.Name,
		Type:          secret.Type,
		Metadata:      secret.Metadata,
		GlobalDefault: secret.GlobalDefault,
		CreatedAt:     secret.CreatedAt,
		UpdatedAt:     secret.UpdatedAt,
	}, nil
}

// ListSecrets returns all secret metadata for a user (never values).
func (s *SecretService) ListSecrets(ctx context.Context, userID string) ([]*SecretResponse, error) {
	secrets, err := s.store.ListSecrets(ctx, userID)
	if err != nil {
		return nil, err
	}
	result := make([]*SecretResponse, len(secrets))
	for i, sec := range secrets {
		result[i] = &SecretResponse{
			ID:            sec.ID,
			Name:          sec.Name,
			Type:          sec.Type,
			Metadata:      sec.Metadata,
			GlobalDefault: sec.GlobalDefault,
			CreatedAt:     sec.CreatedAt,
			UpdatedAt:     sec.UpdatedAt,
		}
	}
	return result, nil
}

// UpdateSecret re-encrypts and updates a secret's value.
func (s *SecretService) UpdateSecret(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, secretID string, req UpdateSecretRequest) error {
	secret, err := s.store.GetSecret(ctx, userID, secretID)
	if err != nil {
		return err
	}
	if secret == nil {
		return ErrSecretNotFound
	}

	dek, err := s.keys.GetDEK(ctx, sessionID, matchedSigningKey)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDEKUnavailable, err)
	}

	ciphertext, err := EncryptSecret(dek, []byte(req.Value))
	if err != nil {
		return fmt.Errorf("encrypt secret: %w", err)
	}

	record, err := s.keys.store.GetUserKey(ctx, userID)
	if err != nil || record == nil {
		return ErrUserKeysMissing
	}

	secret.Ciphertext = ciphertext
	secret.KeyVersion = record.KeyVersion
	secret.UpdatedAt = time.Now()
	prevGlobalDefault := secret.GlobalDefault
	if req.Metadata != nil {
		if err := validateMetadata(secret.Type, req.Metadata); err != nil {
			return err
		}
		secret.Metadata = req.Metadata
	}
	if req.GlobalDefault != nil {
		secret.GlobalDefault = *req.GlobalDefault
	}
	if err := validateValue(secret.Type, req.Value); err != nil {
		return err
	}

	if err := s.store.UpdateSecret(ctx, secret); err != nil {
		return fmt.Errorf("update secret: %w", err)
	}

	auditMeta := map[string]string{"name": secret.Name}
	if req.GlobalDefault != nil && *req.GlobalDefault != prevGlobalDefault {
		auditMeta["globalDefault"] = strconv.FormatBool(secret.GlobalDefault)
	}
	s.audit(ctx, userID, "update", &secretID, nil, auditMeta)
	return nil
}

// DeleteSecret removes a secret and its bindings.
func (s *SecretService) DeleteSecret(ctx context.Context, userID, secretID string) error {
	secret, err := s.store.GetSecret(ctx, userID, secretID)
	if err != nil {
		return err
	}
	if secret == nil {
		return ErrSecretNotFound
	}

	if err := s.store.DeleteSecret(ctx, userID, secretID); err != nil {
		return fmt.Errorf("delete secret: %w", err)
	}

	s.audit(ctx, userID, "delete", &secretID, nil, map[string]string{"name": secret.Name, "type": string(secret.Type)})
	return nil
}

// DecryptSecretValue decrypts a secret's value (used for pod injection).
func (s *SecretService) DecryptSecretValue(ctx context.Context, userID, sessionID string, matchedSigningKey []byte, secretID string) ([]byte, error) {
	secret, err := s.store.GetSecret(ctx, userID, secretID)
	if err != nil {
		return nil, err
	}
	if secret == nil {
		return nil, ErrSecretNotFound
	}

	dek, err := s.keys.GetDEK(ctx, sessionID, matchedSigningKey)
	if err != nil {
		// DEK is unavailable from cache (session expired, user not logged in,
		// or Redis flushed). Audit so operators can correlate user reports.
		s.audit(ctx, userID, "secret_decrypt_failed", &secretID, nil,
			map[string]string{"name": secret.Name, "type": string(secret.Type), "reason": "dek_unavailable", "error": err.Error()})
		return nil, fmt.Errorf("%w: %v", ErrDEKUnavailable, err)
	}

	plaintext, err := DecryptSecret(dek, secret.Ciphertext)
	if err != nil {
		// AEAD authentication failed: DEK is present but does not match the
		// stored ciphertext. Most common cause is a DEK rotation or user_keys
		// rewrite without re-encrypting the existing secrets. Distinct from
		// ErrDEKUnavailable — re-authenticating will NOT fix this.
		s.audit(ctx, userID, "secret_decrypt_failed", &secretID, nil,
			map[string]string{"name": secret.Name, "type": string(secret.Type), "reason": "ciphertext_aead_failure", "key_version": fmt.Sprintf("%d", secret.KeyVersion), "error": err.Error()})
		return nil, fmt.Errorf("%w: secret %q (type=%s, key_version=%d): %v",
			ErrCiphertextDecryptFailed, secret.Name, secret.Type, secret.KeyVersion, err)
	}

	s.audit(ctx, userID, "read", &secretID, nil, map[string]string{"name": secret.Name})
	return plaintext, nil
}

// SetBindings sets which secrets are bound to a workspace. The caller
// must own every secret being bound; an unowned secret produces
// ErrSecretNotFound (mapped to 404 by the handler). Workspace ownership
// itself is enforced by WorkspaceAccessMiddleware on PUT /:id/bindings
// (design 0041 D5) — the service trusts that decision so it can also be
// called from background paths where the caller is implicitly authorized
// (e.g. workspace.Service.refreshEphemeralSecrets).
func (s *SecretService) SetBindings(ctx context.Context, userID, workspaceID string, secretIDs []string) (BindingsMutationResult, error) {
	// Verify all secrets belong to the user and accumulate for diff.
	var newSecrets []*UserSecret
	for _, sid := range secretIDs {
		secret, err := s.store.GetSecret(ctx, userID, sid)
		if err != nil {
			return BindingsMutationResult{}, err
		}
		if secret == nil {
			return BindingsMutationResult{}, fmt.Errorf("%w: %s", ErrSecretNotFound, sid)
		}
		newSecrets = append(newSecrets, secret)
	}

	// Get existing bindings for diff and audit.
	existing, getErr := s.store.GetBindings(ctx, workspaceID)
	if getErr != nil {
		existing = nil
	}

	if err := s.store.SetBindings(ctx, workspaceID, secretIDs); err != nil {
		return BindingsMutationResult{}, fmt.Errorf("set bindings: %w", err)
	}

	// Audit removed and added bindings.
	existingIDs := make(map[string]bool, len(existing))
	for _, sec := range existing {
		existingIDs[sec.ID] = true
	}
	newIDs := make(map[string]bool, len(newSecrets))
	for _, sec := range newSecrets {
		newIDs[sec.ID] = true
	}
	for _, sec := range existing {
		if !newIDs[sec.ID] {
			sid := sec.ID
			s.audit(ctx, userID, "unbind", &sid, &workspaceID, nil)
		}
	}
	for _, sec := range newSecrets {
		if !existingIDs[sec.ID] {
			sid := sec.ID
			s.audit(ctx, userID, "bind", &sid, &workspaceID, nil)
		}
	}

	if getErr != nil {
		return BindingsMutationResult{LLMProviderAffected: true}, nil
	}
	return computeBindingsDiff(existing, newSecrets), nil
}

// AddBindings adds secretIDs to a workspace's binding set without
// removing any existing bindings. The store-level implementation
// takes a workspace-scoped advisory lock so concurrent SetBindings /
// AddBindings calls cannot lose updates (worklog 0094 pass-2 finding
// O1). Each secret's ownership is verified before the binding is
// recorded; an unowned secret produces ErrSecretNotFound.
//
// Used by SetWorkspaceEnv to merge newly-created env-secrets into
// the workspace bindings without the Get-then-Set window the previous
// implementation suffered from. Workspace ownership is enforced by
// WorkspaceAccessMiddleware on PUT /:id/env (design 0041 D5).
func (s *SecretService) AddBindings(ctx context.Context, userID, workspaceID string, secretIDs []string) (BindingsMutationResult, error) {
	if len(secretIDs) == 0 {
		return BindingsMutationResult{}, nil
	}
	var newSecrets []*UserSecret
	for _, sid := range secretIDs {
		secret, err := s.store.GetSecret(ctx, userID, sid)
		if err != nil {
			return BindingsMutationResult{}, err
		}
		if secret == nil {
			return BindingsMutationResult{}, fmt.Errorf("%w: %s", ErrSecretNotFound, sid)
		}
		newSecrets = append(newSecrets, secret)
	}
	if err := s.store.AddBindings(ctx, workspaceID, secretIDs); err != nil {
		return BindingsMutationResult{}, fmt.Errorf("add bindings: %w", err)
	}
	for _, sec := range newSecrets {
		sid := sec.ID
		s.audit(ctx, userID, "bind", &sid, &workspaceID, nil)
	}
	return computeBindingsDiff(nil, newSecrets), nil
}

// GetBindings returns secrets bound to a workspace. Workspace ownership is
// enforced by WorkspaceAccessMiddleware on GET /:id/bindings (design 0041 D5);
// the service trusts that decision and does not re-fetch the workspace.
func (s *SecretService) GetBindings(ctx context.Context, userID, workspaceID string) (*BindingsResponse, error) {
	secrets, err := s.store.GetBindings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	bindings := make([]BoundSecret, len(secrets))
	for i, sec := range secrets {
		bindings[i] = BoundSecret{
			SecretID: sec.ID,
			Name:     sec.Name,
			Type:     sec.Type,
		}
	}
	return &BindingsResponse{Bindings: bindings}, nil
}

// GetBindingsForSecret returns workspace IDs that a secret is bound to.
//
// Ownership-failure modes (secret not found, secret owned by someone
// else) are conflated to a uniform empty result so the response shape
// does not leak existence cross-tenant. Genuine system errors (DB
// outage on the lookup) propagate so the handler can return 5xx
// instead of a misleading empty 200.
func (s *SecretService) GetBindingsForSecret(ctx context.Context, userID, secretID string) ([]string, error) {
	secret, err := s.store.GetSecret(ctx, userID, secretID)
	if err != nil {
		return nil, fmt.Errorf("get secret for ownership check: %w", err)
	}
	if secret == nil {
		return nil, nil
	}
	return s.store.GetBindingsForSecret(ctx, secretID)
}

// QueryAudit returns audit log entries for the current user.
func (s *SecretService) QueryAudit(ctx context.Context, userID string, query AuditQuery) ([]*AuditEntry, error) {
	return s.store.QueryAudit(ctx, userID, query)
}

// SeedGlobalDefaultSecrets binds all secrets with global_default=true owned
// by userID to the given workspace. Called by the workspace service on
// workspace creation as a best-effort operation (failure is logged but does
// not roll back the workspace). Uses the service-level AddBindings so each
// auto-bind is recorded in the audit log (matching every user-initiated
// bind path) and is idempotent if called more than once for the same
// workspace (ON CONFLICT DO NOTHING at the store layer).
func (s *SecretService) SeedGlobalDefaultSecrets(ctx context.Context, workspaceID, userID string) error {
	defaults, err := s.store.ListGlobalDefaultSecrets(ctx, userID)
	if err != nil {
		return fmt.Errorf("list global default secrets: %w", err)
	}
	if len(defaults) == 0 {
		return nil
	}
	ids := make([]string, len(defaults))
	for i, d := range defaults {
		ids[i] = d.ID
	}
	// Service-level AddBindings verifies ownership (already pre-filtered by
	// ListGlobalDefaultSecrets) and emits a "bind" audit entry per secret,
	// preserving the audit-trail contract every other bind path satisfies.
	_, err = s.AddBindings(ctx, userID, workspaceID, ids)
	return err
}

// auditWorkspaceIDMaxLen matches the secret_audit_log.workspace_id
// column width (VARCHAR(36)). Adversarial input — e.g. a forged
// workspaceID 200 characters long — must be truncated before the
// audit row reaches Postgres or the INSERT fails with "value too
// long" and the security event is silently dropped (validator pass-5
// finding N-1). Truncation is preferable to outright rejection: the
// failed-auth event itself is the signal we care about; a slightly
// truncated workspaceID is still useful forensically.
const auditWorkspaceIDMaxLen = 36

func (s *SecretService) audit(ctx context.Context, userID, action string, secretID, workspaceID *string, meta map[string]string) {
	entry := &AuditEntry{
		UserID:    userID,
		Action:    action,
		Timestamp: time.Now(),
	}
	if secretID != nil {
		entry.SecretID = secretID
	}
	// Local copy of meta so we never mutate the caller's map. Even
	// though every current caller passes nil or a fresh literal,
	// the contract should not silently mutate caller state
	// (validator pass-6 finding NEW-2). Cheap: most maps are <=3
	// entries.
	auditMeta := make(map[string]string, len(meta)+1)
	for k, v := range meta {
		auditMeta[k] = v
	}
	if workspaceID != nil {
		// Truncate to schema width so an adversarial caller posting
		// a 200-char workspaceID does not silently DoS the audit
		// pipeline. We rune-slice (not byte-slice) so a multibyte
		// boundary cannot produce invalid UTF-8 that Postgres
		// would reject as 'invalid byte sequence for encoding
		// "UTF8"' — that would silently drop the audit row, which
		// is the exact failure mode this truncation is meant to
		// prevent (validator pass-6 finding NEW-1).
		ws := *workspaceID
		if rs := []rune(ws); len(rs) > auditWorkspaceIDMaxLen {
			ws = string(rs[:auditWorkspaceIDMaxLen])
			auditMeta["workspaceID_truncated"] = "true"
		}
		entry.WorkspaceID = &ws
	}
	if len(auditMeta) > 0 {
		entry.Metadata, _ = json.Marshal(auditMeta) //nolint:errcheck // marshaling map[string]string cannot fail
	}
	// Fire-and-forget audit logging (async in production, sync in tests)
	_ = s.store.LogAudit(ctx, entry) //nolint:errcheck // audit log is best-effort; failure doesn't affect the secret operation
}

// validateValue validates type-specific constraints on the plaintext secret
// value before encryption. Errors wrap ErrInvalidMetadata so callers map them
// to 400 responses via handleSecretError.
func validateValue(secretType SecretType, value string) error {
	if secretType != SecretTypeLLMProvider {
		return nil
	}
	// llm-provider value must be JSON-encoded LLMProviderData with required fields.
	var d LLMProviderData
	if err := json.Unmarshal([]byte(value), &d); err != nil {
		return fmt.Errorf("%w: llm-provider value must be JSON (got: %v)", ErrInvalidMetadata, err)
	}
	if err := d.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidMetadata, err)
	}
	return nil
}

// validateMetadata validates type-specific metadata requirements.
// Errors wrap ErrInvalidMetadata so callers can use errors.Is to map
// any failure to a 400 response (handlers/secrets.go::handleSecretError).
func validateMetadata(secretType SecretType, metadata json.RawMessage) error {
	if metadata == nil || string(metadata) == "{}" || string(metadata) == "null" {
		// Metadata is optional for most types, but required for some
		switch secretType {
		case SecretTypeSSHKey:
			return fmt.Errorf("%w: ssh-key requires metadata with key_type field", ErrInvalidMetadata)
		case SecretTypeSecretFile:
			return fmt.Errorf("%w: secret-file requires metadata with mount_path field", ErrInvalidMetadata)
		case SecretTypeEnvSecret:
			return fmt.Errorf("%w: env-secret requires metadata with var_name field", ErrInvalidMetadata)
		}
		return nil
	}

	var m map[string]interface{}
	if err := json.Unmarshal(metadata, &m); err != nil {
		return fmt.Errorf("%w: invalid metadata JSON: %v", ErrInvalidMetadata, err)
	}

	switch secretType {
	case SecretTypeSSHKey:
		if _, ok := m["key_type"]; !ok {
			return fmt.Errorf("%w: ssh-key metadata requires key_type field", ErrInvalidMetadata)
		}
	case SecretTypeSecretFile:
		mp, ok := m["mount_path"]
		if !ok {
			return fmt.Errorf("%w: secret-file metadata requires mount_path field", ErrInvalidMetadata)
		}
		mpStr, _ := mp.(string)
		if err := validateMountPath(mpStr); err != nil {
			return err
		}
	case SecretTypeEnvSecret:
		if _, ok := m["var_name"]; !ok {
			return fmt.Errorf("%w: env-secret metadata requires var_name field", ErrInvalidMetadata)
		}
	}
	return nil
}

// validateMountPath enforces the same path-traversal rules as the
// in-pod materializer's resolveMountPath, applied at the API layer as
// defense-in-depth (Bug 13 in worklog 0085). Rejects empty paths,
// absolute paths, the bare base directory ("."), and any relative path
// that resolves outside its (notional) base directory after Clean.
//
// All failures wrap ErrInvalidMetadata so callers can map them to a
// 400 response without substring matching.
func validateMountPath(mp string) error {
	if strings.TrimSpace(mp) == "" {
		return fmt.Errorf("%w: mount_path is empty", ErrInvalidMetadata)
	}
	if filepath.IsAbs(mp) {
		return fmt.Errorf("%w: mount_path %q must be relative to the secrets base directory", ErrInvalidMetadata, mp)
	}
	cleaned := filepath.Clean(mp)
	if cleaned == "." {
		return fmt.Errorf("%w: mount_path may not name the base directory itself", ErrInvalidMetadata)
	}
	// Notional base must be deep enough that filepath.Rel can produce a
	// "../" prefix when an input escapes; using "/" loses that signal
	// because filepath.Clean strips leading "..". The concrete base in
	// production is /home/sandbox/.secrets but only the depth matters
	// for this check.
	const base = "/llmsafespaces/notional/secrets"
	candidate := filepath.Clean(filepath.Join(base, mp))
	rel, err := filepath.Rel(base, candidate)
	if err != nil {
		return fmt.Errorf("%w: invalid mount_path %q: %v", ErrInvalidMetadata, mp, err)
	}
	if rel == "." || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("%w: mount_path %q escapes secrets base directory", ErrInvalidMetadata, mp)
	}
	return nil
}
