// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// InjectedSecret is a single secret entry in the legacy secrets.json /
// reload-secrets wire shape that the mixed fleet of pods already
// consumes. New server-side code builds a Batch and renders it with
// LegacyBatchJSON; this type is the W15 mixed-fleet pin.
type InjectedSecret struct {
	Type      SecretType      `json:"type"`
	Name      string          `json:"name"`
	Metadata  json.RawMessage `json:"metadata"`
	Plaintext string          `json:"plaintext"`
}

// Degrade reason codes (machine-readable, epic #1158 law 5: loud or
// absent). Only "owner has no DEK-encrypted content bound" is quiet in
// content terms; every other non-delivery carries one of these.
const (
	// DegradeDEKUnwrapFailed: the owner's user_keys row exists but could
	// not be unwrapped (or the key service is unwired). User-owned
	// entries are skipped-with-audit; server-KEK entries still deliver.
	DegradeDEKUnwrapFailed = "dek_unwrap_failed"

	// DegradeOwnerNoKeys: the owner has no user_keys record, so no
	// DEK-encrypted content of theirs exists — the batch is complete in
	// content terms and the reason records why user rows were skipped.
	DegradeOwnerNoKeys = "owner_no_keys"
)

// BuildDegrade is the loud-degrade companion of a built batch: a
// machine-readable reason the batch lacks user-DEK entries. Never
// silent (I10) — the accompanying audit events name the workspace and
// the underlying error.
type BuildDegrade struct {
	Reason string
}

// WorkspaceBatchBuilder is the one batch builder contract (design 0052
// §4.1, US-70.2): session identity decides whether a caller may invoke
// delivery, never what decrypts. Implementations: *SecretService.
type WorkspaceBatchBuilder interface {
	BuildWorkspaceBatch(ctx context.Context, ownerUserID, workspaceID string) (*Batch, *BuildDegrade, error)
}

// Compile-time assertion that *SecretService satisfies the builder contract.
var _ WorkspaceBatchBuilder = (*SecretService)(nil)

// BuildWorkspaceBatch builds the complete secret batch for a workspace:
// every effective credential binding, user secret binding, and bound MCP
// server the workspace's owner has — the "one builder, one truth"
// contract (I2). Session identity is absent from construction: user
// entries decrypt via GetDEKServerSide (K5), so suspend/resume, expired
// sessions, and background pushes all produce the SAME batch for the
// same row state.
//
// Structure (design 0057 R1, two-tier):
//
//  1. rows → []ManifestEntry — built from ROWS, pre-decrypt: id,
//     version, type, name, stored metadata. The manifest tier never
//     decrypts anything, so any replica computes the identical hash.
//     LLM credentials dedup by slug keeping the first row in binding
//     priority order (the intended winner); user_secrets rows filter to
//     the workspace owner's non-llm-provider entries; MCP rows are the
//     bound+enabled servers.
//  2. manifest hash → EnsureRevision — the DB row mints the monotonic
//     seq; replicas never mint seqs.
//  3. decrypt tier — the (admin/org/user) decrypt matrix with today's
//     per-entry audit-and-continue semantics: a failed credential falls
//     back to the next binding of the same slug; a failed user_secret or
//     MCP server is skipped with its audit event.
//
// THE REVISION DESCRIBES THE INTENDED SET: per-entry decrypt failures do
// NOT reshape the manifest — the failed entry stays in the manifest hash
// as the loud-degrade contract, and the batch omits only its value. The
// revision converges when the row heals (its version bumps).
//
// Degrade (I10): on DEK-tier failure the returned batch carries the
// server-KEK entries plus a BuildDegrade reason (dek_unwrap_failed /
// owner_no_keys); user rows are skipped with the *_skipped_no_session
// audit vocabulary. Never a silent partial.
//
// Workspace ownership is enforced by the callers' HTTP layer or token
// review; this method does not re-check it.
func (s *SecretService) BuildWorkspaceBatch(ctx context.Context, ownerUserID, workspaceID string) (*Batch, *BuildDegrade, error) {
	bindings, relevantSecrets, servers, err := s.loadWorkspaceRows(ctx, ownerUserID, workspaceID)
	if err != nil {
		return nil, nil, err
	}

	revStore, err := s.requireRevisionStore()
	if err != nil {
		return nil, nil, err
	}

	manifestHash := ManifestHash(ownerUserID, manifestFromRows(bindings, relevantSecrets, servers))
	seq, err := revStore.EnsureRevision(ctx, workspaceID, manifestHash)
	if err != nil {
		return nil, nil, fmt.Errorf("ensure workspace revision: %w", err)
	}

	dek, degrade := s.workspaceDEK(ctx, ownerUserID, workspaceID, bindings, relevantSecrets, servers)

	entries := make([]BatchEntry, 0, len(bindings)+len(relevantSecrets)+len(servers))
	entries = append(entries, s.buildCredentialEntries(ctx, ownerUserID, workspaceID, bindings, dek)...)
	entries = append(entries, s.buildUserSecretEntries(ctx, ownerUserID, workspaceID, relevantSecrets, dek)...)
	entries = append(entries, s.buildMCPEntries(ctx, ownerUserID, workspaceID, servers, dek)...)

	batch := &Batch{Entries: entries}
	batch.Revision = BatchRevision{
		Seq:          seq,
		ManifestHash: manifestHash,
		BatchHash:    BatchHash(*batch),
	}
	return batch, degrade, nil
}

// loadWorkspaceRows loads the raw rows the batch tiers derive from:
// credential bindings in priority order, the owner's non-llm-provider
// bound user secrets, and the bound+enabled MCP servers. Shared by
// BuildWorkspaceBatch (rows → manifest → decrypt) and ManifestFor (rows
// → manifest, stop).
func (s *SecretService) loadWorkspaceRows(ctx context.Context, ownerUserID, workspaceID string) ([]CredentialBinding, []*UserSecret, []MCPServerBindingRow, error) {
	credStore, err := s.requireCredentialStore()
	if err != nil {
		return nil, nil, nil, err
	}

	bindings, err := credStore.GetWorkspaceCredentials(ctx, workspaceID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get workspace credentials: %w", err)
	}
	bound, err := s.store.GetBindings(ctx, workspaceID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get workspace secret bindings: %w", err)
	}
	servers, err := credStore.GetWorkspaceMCPServers(ctx, workspaceID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get workspace mcp servers: %w", err)
	}

	var relevantSecrets []*UserSecret
	for _, secret := range bound {
		if secret.UserID == ownerUserID && secret.Type != SecretTypeLLMProvider {
			relevantSecrets = append(relevantSecrets, secret)
		}
	}
	return bindings, relevantSecrets, servers, nil
}

// ManifestFor computes the workspace's CURRENT manifest hash from rows
// alone — the decrypt-free tier. This is the seam the conditional
// pod-bootstrap 304 decision runs on (US-70.2): comparing the client's
// last manifest hash against this value never touches a ciphertext, the
// DEK key store, or the admin/org root providers. It also does NOT mint
// a revision: an unchanged manifest means the stored row already holds
// the matching seq, and a changed manifest mints its seq in the 200
// path's build.
func (s *SecretService) ManifestFor(ctx context.Context, ownerUserID, workspaceID string) (string, error) {
	bindings, relevantSecrets, servers, err := s.loadWorkspaceRows(ctx, ownerUserID, workspaceID)
	if err != nil {
		return "", err
	}
	return ManifestHash(ownerUserID, manifestFromRows(bindings, relevantSecrets, servers)), nil
}

// CurrentRevision returns the workspace's stored revision row (see
// RevisionStore). Exposed on the service so the conditional
// pod-bootstrap path can stamp the 304 ETag with the row's seq without
// holding the store directly.
func (s *SecretService) CurrentRevision(ctx context.Context, workspaceID string) (int64, string, bool, error) {
	revStore, err := s.requireRevisionStore()
	if err != nil {
		return 0, "", false, err
	}
	return revStore.CurrentRevision(ctx, workspaceID)
}

// manifestFromRows builds the decrypt-free tier from ROWS: the intended
// set as the store sees it, before any ciphertext is touched.
func manifestFromRows(bindings []CredentialBinding, secrets []*UserSecret, servers []MCPServerBindingRow) []ManifestEntry {
	entries := make([]ManifestEntry, 0, len(bindings)+len(secrets)+len(servers))

	seenSlug := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		if seenSlug[b.Slug] {
			continue
		}
		seenSlug[b.Slug] = true
		entries = append(entries, ManifestEntry{
			SecretID: b.ID,
			Version:  b.Version,
			Type:     SecretTypeLLMProvider,
			Name:     b.Slug,
		})
	}
	for _, sec := range secrets {
		entries = append(entries, ManifestEntry{
			SecretID: sec.ID,
			Version:  sec.Version,
			Type:     sec.Type,
			Name:     sec.Name,
			Metadata: sec.Metadata,
		})
	}
	for _, srv := range servers {
		entries = append(entries, ManifestEntry{
			SecretID: srv.ServerID,
			Version:  srv.Version,
			Type:     SecretTypeMcpServer,
			Name:     srv.Name,
			Metadata: mcpServerMetadata(srv),
		})
	}
	return entries
}

// workspaceDEK resolves the owner's DEK server-side, lazily: a
// workspace with no user-owned rows never touches user_keys (an owner
// without keys and without user rows is not a degrade). On failure it
// returns nil DEK plus the loud BuildDegrade; callers then skip user
// rows with the *_skipped_no_session audits.
func (s *SecretService) workspaceDEK(ctx context.Context, ownerUserID, workspaceID string, bindings []CredentialBinding, secrets []*UserSecret, servers []MCPServerBindingRow) ([]byte, *BuildDegrade) {
	needsDEK := len(secrets) > 0
	if !needsDEK {
		for _, b := range bindings {
			if b.OwnerType == "user" {
				needsDEK = true
				break
			}
		}
	}
	if !needsDEK {
		for _, srv := range servers {
			if srv.OwnerType == "user" {
				needsDEK = true
				break
			}
		}
	}
	if !needsDEK {
		return nil, nil
	}

	if s.keys == nil {
		s.audit(ctx, ownerUserID, "pod_bootstrap_dek_failed", nil, &workspaceID,
			map[string]string{"error": "key service not configured"})
		return nil, &BuildDegrade{Reason: DegradeDEKUnwrapFailed}
	}

	dek, _, err := s.keys.GetDEKServerSide(ctx, ownerUserID)
	if err == nil {
		return dek, nil
	}

	// ErrDEKUnavailable alone means "owner owns no DEK-encrypted
	// secrets" — expected, not audited. Everything else gets an audit
	// row tying the degrade to this workspace with the underlying error
	// string (the 2026-08-28 v0.25.1 incident lesson: silence was the
	// diagnosis cost).
	if !errors.Is(err, ErrDEKUnavailable) {
		s.audit(ctx, ownerUserID, "pod_bootstrap_dek_failed", nil, &workspaceID,
			map[string]string{"error": err.Error()})
		return nil, &BuildDegrade{Reason: DegradeDEKUnwrapFailed}
	}
	return nil, &BuildDegrade{Reason: DegradeOwnerNoKeys}
}

// buildCredentialEntries runs the multi-source LLM-credential loop with
// the (admin/org/user) decrypt matrix and slug dedup: the first binding
// of a slug that decrypts wins; failures audit
// "credential_decrypt_failed" and fall through so a lower-priority
// binding can take over. With dek==nil every user binding is skipped
// with "credential_skipped_no_session".
func (s *SecretService) buildCredentialEntries(ctx context.Context, ownerUserID, workspaceID string, bindings []CredentialBinding, dek []byte) []BatchEntry {
	adminDecrypt := decryptFnFor(s.adminProvider)
	orgDecrypt := decryptFnFor(s.orgProvider)

	seen := make(map[string]bool)
	var out []BatchEntry
	for _, b := range bindings {
		if seen[b.Slug] {
			continue
		}
		if b.OwnerType == "user" && dek == nil {
			// Cleaner messaging than a decrypt failure: there is no decrypt
			// attempt, just no DEK (the sessionless vocabulary, kept).
			s.audit(ctx, ownerUserID, "credential_skipped_no_session", nil, &workspaceID,
				map[string]string{"credentialID": b.ID, "slug": b.Slug, "kind": b.Kind, "ownerType": b.OwnerType})
			continue
		}
		pd, err := s.decryptBindingWithDEK(ctx, b, dek, adminDecrypt, orgDecrypt)
		if err != nil {
			// Don't set seen — allow fallback to lower-priority binding.
			s.audit(ctx, ownerUserID, "credential_decrypt_failed", nil, &workspaceID,
				map[string]string{"credentialID": b.ID, "slug": b.Slug, "kind": b.Kind, "ownerType": b.OwnerType, "error": err.Error()})
			continue
		}
		s.applyModelAllowlist(&pd, b)
		seen[b.Slug] = true
		plaintext, merr := json.Marshal(pd) //nolint:gosec // marshaling for secrets delivery, not API response
		if merr != nil {
			s.audit(ctx, ownerUserID, "credential_decrypt_failed", nil, &workspaceID,
				map[string]string{"credentialID": b.ID, "slug": b.Slug, "kind": b.Kind, "ownerType": b.OwnerType, "error": merr.Error()})
			continue
		}
		out = append(out, BatchEntry{
			SecretID: b.ID,
			Version:  b.Version,
			Type:     SecretTypeLLMProvider,
			// Name is the slug — the provider-map key in agent-config.json
			// (Epic 55); opencode persists it as providerID on sessions.
			Name:  b.Slug,
			Value: string(plaintext),
		})
	}
	return out
}

// buildUserSecretEntries decrypts the owner's non-llm-provider
// user_secrets. Per-entry failures audit "secret_decrypt_failed" and
// continue so one corrupted ciphertext does not poison the rest. With
// dek==nil every entry is skipped with "secret_skipped_no_session".
func (s *SecretService) buildUserSecretEntries(ctx context.Context, ownerUserID, workspaceID string, secrets []*UserSecret, dek []byte) []BatchEntry {
	if len(secrets) == 0 {
		return nil
	}
	if dek == nil {
		for _, secret := range secrets {
			sid := secret.ID
			s.audit(ctx, ownerUserID, "secret_skipped_no_session", &sid, &workspaceID,
				map[string]string{"name": secret.Name, "type": string(secret.Type), "reason": "dek unavailable"})
		}
		return nil
	}
	var out []BatchEntry
	for _, secret := range secrets {
		plaintext, err := DecryptSecret(dek, secret.Ciphertext)
		if err != nil {
			sid := secret.ID
			s.audit(ctx, ownerUserID, "secret_decrypt_failed", &sid, &workspaceID,
				map[string]string{"name": secret.Name, "type": string(secret.Type), "error": err.Error()})
			continue
		}
		out = append(out, BatchEntry{
			SecretID: secret.ID,
			Version:  secret.Version,
			Type:     secret.Type,
			Name:     secret.Name,
			Metadata: secret.Metadata,
			Value:    string(plaintext),
		})
	}
	return out
}

// buildMCPEntries decrypts the bound+enabled MCP servers. Failures for
// one server are audited and skipped — a single corrupted ciphertext
// does not block delivery of the others (D4: additive composition).
// With dek==nil user-scope servers are skipped with
// "mcp_skipped_no_session" (D13).
func (s *SecretService) buildMCPEntries(ctx context.Context, ownerUserID, workspaceID string, servers []MCPServerBindingRow, dek []byte) []BatchEntry {
	if len(servers) == 0 {
		return nil
	}
	adminDecrypt := decryptFnFor(s.adminProvider)
	orgDecrypt := decryptFnFor(s.orgProvider)

	var out []BatchEntry
	for _, srv := range servers {
		var plaintext []byte
		switch srv.OwnerType {
		case "admin":
			if adminDecrypt == nil {
				s.audit(ctx, ownerUserID, "mcp_decrypt_failed", &srv.ServerID, &workspaceID,
					map[string]string{"name": srv.Name, "ownerType": "admin", "error": "admin provider not configured"})
				continue
			}
			pt, err := adminDecrypt(ctx, srv.Ciphertext)
			if err != nil {
				s.audit(ctx, ownerUserID, "mcp_decrypt_failed", &srv.ServerID, &workspaceID,
					map[string]string{"name": srv.Name, "ownerType": "admin", "error": err.Error()})
				continue
			}
			plaintext = pt
		case "org":
			if orgDecrypt == nil {
				s.audit(ctx, ownerUserID, "mcp_decrypt_failed", &srv.ServerID, &workspaceID,
					map[string]string{"name": srv.Name, "ownerType": "org", "error": "org provider not configured"})
				continue
			}
			pt, err := orgDecrypt(ctx, srv.Ciphertext)
			if err != nil {
				s.audit(ctx, ownerUserID, "mcp_decrypt_failed", &srv.ServerID, &workspaceID,
					map[string]string{"name": srv.Name, "ownerType": "org", "error": err.Error()})
				continue
			}
			plaintext = pt
		case "user":
			if dek == nil {
				s.audit(ctx, ownerUserID, "mcp_skipped_no_session", &srv.ServerID, &workspaceID,
					map[string]string{"name": srv.Name, "ownerType": "user"})
				continue
			}
			pt, err := DecryptSecret(dek, srv.Ciphertext)
			if err != nil {
				s.audit(ctx, ownerUserID, "mcp_decrypt_failed", &srv.ServerID, &workspaceID,
					map[string]string{"name": srv.Name, "ownerType": "user", "error": err.Error()})
				continue
			}
			plaintext = pt
		default:
			continue
		}
		out = append(out, BatchEntry{
			SecretID: srv.ServerID,
			Version:  srv.Version,
			Type:     SecretTypeMcpServer,
			Name:     srv.Name,
			Metadata: mcpServerMetadata(srv),
			Value:    string(plaintext),
		})
	}
	return out
}

// mcpServerMetadata renders the server's stored transport/url/command/
// args/timeout fields — stored content, never derived.
func mcpServerMetadata(srv MCPServerBindingRow) json.RawMessage {
	meta := map[string]any{
		"transport": srv.Transport,
		"url":       srv.URL,
		"command":   srv.Command,
		"args":      srv.Args,
	}
	if srv.TimeoutMs != nil && *srv.TimeoutMs > 0 {
		meta["timeoutMs"] = *srv.TimeoutMs
	}
	metaJSON, _ := json.Marshal(meta)
	return metaJSON
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

func (s *SecretService) decryptBindingWithDEK(ctx context.Context, b CredentialBinding, dek []byte, adminDecrypt, orgDecrypt decryptFn) (LLMProviderData, error) {
	var plaintext []byte
	switch b.OwnerType {
	case "user":
		if dek == nil {
			return LLMProviderData{}, fmt.Errorf("get user DEK: %w", ErrDEKUnavailable)
		}
		pt, err := DecryptSecret(dek, b.Ciphertext)
		if err != nil {
			return LLMProviderData{}, err
		}
		plaintext = pt
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

// applyModelAllowlist filters pd.Models against the credential's
// per-binding allowlist. Extracted verbatim from the original loop (no
// behavior change) so credential entries keep Epic-55/30 semantics.
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

// requireRevisionStore casts the configured store to RevisionStore —
// the per-workspace seq mint. Same explicit-failure contract as
// requireCredentialStore: a store that cannot mint revisions must fail
// loudly, never let the builder fabricate a seq locally.
func (s *SecretService) requireRevisionStore() (RevisionStore, error) {
	revStore, ok := s.store.(RevisionStore)
	if !ok {
		return nil, fmt.Errorf("store does not implement RevisionStore: ensure all store wrappers implement RevisionStore")
	}
	return revStore, nil
}
