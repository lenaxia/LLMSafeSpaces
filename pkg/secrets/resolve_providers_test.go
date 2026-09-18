// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// resolve_providers_test.go — US-72.3: SecretService.ResolveLLMProviders is
// the credential source behind the internal llm-providers endpoint the
// controller staging polls. It must resolve EXACTLY the provider set the
// batch builder would deliver (same rows, same dedup, same allowlist), with
// per-entry audit-and-continue on decrypt failure.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveLLMProviders_MatchesBatchProviderSet(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred, env.orgCred}}
	svc.store = env.store()

	providers, err := svc.ResolveLLMProviders(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, providers, 2)

	bySlug := map[string]LLMProviderData{}
	for _, p := range providers {
		bySlug[p.Slug] = p
	}
	admin := bySlug["openai"]
	assert.Equal(t, "admin-key", admin.APIKey)
	assert.Equal(t, "openai", admin.Kind)
	org := bySlug["custom"]
	assert.Equal(t, "org-key", org.APIKey)
	assert.Equal(t, "openai_compatible", org.Kind)

	// The staged set must equal the batch's llm-provider entries one-for-one
	// (slug + plaintext) — the invariant that makes staging transparent to
	// the (future) token batch swap.
	batch, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	batchProviders := map[string]string{} // slug -> value JSON
	for _, e := range batch.Entries {
		if e.Type == SecretTypeLLMProvider {
			batchProviders[e.Name] = e.Value
		}
	}
	require.Len(t, batchProviders, 2)
	for _, p := range providers {
		pdJSON, ok := batchProviders[p.Slug]
		require.True(t, ok, "provider %q staged but absent from batch", p.Slug)
		var batchPD LLMProviderData
		require.NoError(t, json.Unmarshal([]byte(pdJSON), &batchPD))
		assert.Equal(t, batchPD, p, "provider %q differs from the batch entry", p.Slug)
	}
}

func TestResolveLLMProviders_DedupBySlugFirstWinner(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	fallback := CredentialBinding{
		ID: "cred-admin-2", OwnerType: "admin", OwnerID: "_platform", Kind: "openai", Slug: "openai-row-2",
		Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "loser-key"}),
		Version:    1, SourceType: "auto",
	}
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred, fallback}}
	svc.store = env.store()

	providers, err := svc.ResolveLLMProviders(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, providers, 1)
	assert.Equal(t, "admin-key", providers[0].APIKey, "binding priority order: first row of a slug wins")
}

func TestResolveLLMProviders_DecryptFailureAuditsAndContinues(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	broken := CredentialBinding{
		ID: "cred-broken", OwnerType: "org", OwnerID: "org-9", Kind: "openai", Slug: "broken",
		Ciphertext: []byte("garbage-not-a-ciphertext"), Version: 1, SourceType: "auto",
	}
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred, broken}}
	svc.store = env.store()

	resetAudit(env.secrets)
	providers, err := svc.ResolveLLMProviders(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Len(t, providers, 1, "the healthy credential still resolves")
	assert.Equal(t, "openai", providers[0].Slug)

	assert.Contains(t, auditActions(env.secrets), "credential_decrypt_failed",
		"a failed decrypt must audit, not silently skip")
}

func TestResolveLLMProviders_RequireCredentialStore(t *testing.T) {
	svc, _, _ := setupBuilder(t)
	svc.store = nil

	_, err := svc.ResolveLLMProviders(context.Background(), "user-1", "ws-1")
	require.Error(t, err)
}
