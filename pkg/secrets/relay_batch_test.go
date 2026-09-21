// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// relay_batch_test.go — US-72.4 (design 0058 §4.4/§4.5): the one
// builder's relay-only token emission. Under the deployment flag (a
// non-nil RelayTokenSource), llm-provider entries carry
// apiKey = handoff token and baseURL = router URL; the raw provider key
// never enters the token path. The handoff revision participates in the
// manifest tier so a ~TTL/2 token renewal rotates the manifest hash and
// the existing conditional-pull/resync machinery delivers the fresh
// token without a pod restart.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRelayTokenSource is the test RelayTokenSource: returns a canned
// handoff, records every call.
type fakeRelayTokenSource struct {
	handoff *RelayHandoff
	err     error
	calls   int
}

func (f *fakeRelayTokenSource) RelayHandoff(_ context.Context, _ string) (*RelayHandoff, error) {
	f.calls++
	return f.handoff, f.err
}

func testHandoff(revision string, providers ...RelayHandoffProvider) *RelayHandoff {
	return &RelayHandoff{
		Revision:  revision,
		RouterURL: "http://llm-relay-router.llm-relay.svc.cluster.local",
		Providers: providers,
	}
}

// installRelayEnv wires a workspace with one stageable provider (an
// admin openai credential) and one non-stageable one (bedrock — SDK
// auth, US-72.3 D5) plus one env secret, and installs the relay source.
func installRelayEnv(t *testing.T, src RelayTokenSource) (*SecretService, *builderEnv, *fakeRelayTokenSource) {
	t.Helper()
	svc, env, _ := setupBuilder(t)
	bedrock := CredentialBinding{
		ID: "cred-bedrock", OwnerType: "admin", OwnerID: "_platform", Kind: "bedrock", Slug: "bedrock-row-slug",
		Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{
			Kind: "bedrock", Slug: "aws-bedrock", APIKey: "bedrock-raw-key",
			Models: []LLMModelConfig{{ID: "us.anthropic.claude-3-7-sonnet", ContextLimit: 200000}},
		}),
		Version: 7, SourceType: "auto",
	}
	// Give the stageable credential a model list + binding allowlist so
	// token-path model parity is assertable.
	env.adminCred.ModelAllowlist = []string{"gpt-4o", "gpt-4o-mini"}
	env.adminCred.ModelContextLimits = map[string]int{"gpt-4o": 128000}
	env.creds = &mockCredentialStore{
		bindings: []CredentialBinding{env.adminCred, bedrock},
	}
	svc.store = env.store()
	_, err := svc.SetBindings(context.Background(), "user-1", "ws-1", []string{})
	require.NoError(t, err)
	var fake *fakeRelayTokenSource
	if src == nil {
		fake = &fakeRelayTokenSource{}
	} else {
		fake, _ = src.(*fakeRelayTokenSource)
	}
	svc.SetRelayTokenSource(src)
	return svc, env, fake
}

func stageableHandoff(revision, token string) *RelayHandoff {
	return testHandoff(revision, RelayHandoffProvider{
		ProviderSlug:   "openai",
		Kind:           "openai",
		Token:          token,
		RouterPath:     "/w/ws-1/openai/v1",
		BaseURL:        "https://api.openai.com/v1",
		ModelAllowlist: []string{"gpt-4o", "gpt-4o-mini"},
		KeyID:          "hpke-g1",
		ExpiresAt:      time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	})
}

// TestRelayBatch_FlagOff_BitIdentical: a nil source (flag off) never
// consults the handoff and emits the legacy raw-key batch. The
// regression pin for the "flag OFF = bit-identical behavior" invariant.
func TestRelayBatch_FlagOff_BitIdentical(t *testing.T) {
	svc, env, _ := installRelayEnv(t, nil)
	legacy, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade)

	llm, ok := findEntry(legacy, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`, "flag off keeps the raw key")
	assert.Nil(t, llm.Metadata, "flag off emits no relay metadata")

	// Installing a source, then removing it, must reproduce the same
	// value-bearing bytes (only the revision stamp may differ — the
	// manifest tier is rows-only with the source nil).
	fake := &fakeRelayTokenSource{handoff: stageableHandoff("rAAAA1111", "lrt_fake_token")}
	svc.SetRelayTokenSource(fake)
	flagged, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.True(t, fake.calls > 0)
	_ = env
	_ = flagged
	svc.SetRelayTokenSource(nil)
	restored, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, legacy.Entries, restored.Entries, "flag off after a flag-on build is byte-identical to legacy")
	assert.NotEqual(t, legacy.Entries, flagged.Entries, "sanity: the flag-on build differed")
}

// TestRelayBatch_FlagOn_EmitsTokenNotKey: with the handoff staged, the
// provider entry carries the token and the router baseURL; the raw key
// appears nowhere in the batch (the builder-side canary leg of the
// rogue-agent sweep).
func TestRelayBatch_FlagOn_EmitsTokenNotKey(t *testing.T) {
	const fakeToken = "lrt_fakeAAAA0123456789"
	fake := &fakeRelayTokenSource{handoff: stageableHandoff("rBBBB2222", fakeToken)}
	svc, _, _ := installRelayEnv(t, fake)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.Nil(t, degrade, "a fully staged handoff must not degrade")

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Contains(t, llm.Value, `"apiKey":"`+fakeToken+`"`, "token rides the apiKey field verbatim (the formatter pin)")
	assert.Contains(t, llm.Value, `"baseURL":"http://llm-relay-router.llm-relay.svc.cluster.local/w/ws-1/openai/v1"`,
		"baseURL is RouterURL + RouterPath (design §4.5)")
	assert.NotContains(t, llm.Value, "admin-key", "the raw provider key never enters the token-path entry")

	var meta map[string]string
	require.NoError(t, json.Unmarshal(llm.Metadata, &meta))
	assert.Equal(t, "true", meta["relay"])
	assert.Contains(t, meta["relayExpiresAt"], "T", "expiry honored pod-side (token_expired degrade)")
	assert.Equal(t, "rBBBB2222", meta["relayRevision"], "revision reported pod-side (the lineage conjunct)")

	// Canary over the WHOLE serialized batch: the openai raw key is
	// absent from every emitted byte. The bedrock credential (not
	// relay-frontable, US-72.3 D5 mixed-fleet) keeps the raw path.
	raw, err := json.Marshal(batch)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "admin-key")
	bedrock, ok := findEntry(batch, SecretTypeLLMProvider, "aws-bedrock")
	require.True(t, ok, "non-stageable kind keeps the raw-key mixed-fleet path")
	assert.Contains(t, bedrock.Value, `"apiKey":"bedrock-raw-key"`)
	assert.Nil(t, bedrock.Metadata)
}

// TestRelayBatch_TokenModelsMatchRawPathParity: apart from apiKey and
// baseURL the token entry equals the flag-off entry — model lists and
// context limits survive the swap (behavior parity for the formatter).
func TestRelayBatch_TokenModelsMatchRawPathParity(t *testing.T) {
	svc, _, _ := installRelayEnv(t, nil)
	legacyBatch, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	legacy, _ := findEntry(legacyBatch, SecretTypeLLMProvider, "openai")

	fake := &fakeRelayTokenSource{handoff: stageableHandoff("rCCCC3333", "lrt_fake_parity")}
	svc.SetRelayTokenSource(fake)
	tokenBatch, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	token, ok := findEntry(tokenBatch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)

	var rawPD, tokPD LLMProviderData
	require.NoError(t, json.Unmarshal([]byte(legacy.Value), &rawPD))
	require.NoError(t, json.Unmarshal([]byte(token.Value), &tokPD))
	assert.Equal(t, rawPD.Models, tokPD.Models, "model list + context limits are byte-identical to the raw path")
	assert.Equal(t, rawPD.Slug, tokPD.Slug)
	assert.Equal(t, rawPD.Kind, tokPD.Kind)
	assert.NotEqual(t, rawPD.APIKey, tokPD.APIKey)
	assert.NotEqual(t, rawPD.BaseURL, tokPD.BaseURL)
}

// TestRelayBatch_HandoffMissing_NoTokenBatch_NoRawFallback: a missing
// handoff Secret under flag-on is staging-not-ready: NO llm-provider
// entries (never a raw-key fallback), a machine-readable degrade, an
// audit row, and the non-provider classes still delivered.
func TestRelayBatch_HandoffMissing_NoTokenBatch_NoRawFallback(t *testing.T) {
	svc, env, fake := installRelayEnv(t, &fakeRelayTokenSource{})
	resetAudit(env.secrets)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade, "staging-not-ready must degrade loudly")
	assert.Equal(t, DegradeRelayStagingNotReady, degrade.Reason)

	for _, e := range batch.Entries {
		assert.NotEqual(t, SecretTypeLLMProvider, e.Type,
			"no token batch and NO raw-key fallback while staging is not ready")
	}
	raw, err := json.Marshal(batch)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "admin-key")
	assert.NotContains(t, string(raw), "bedrock-raw-key")
	assert.Contains(t, auditActions(env.secrets), "relay_staging_not_ready")
	assert.True(t, fake.calls > 0)
}

// TestRelayBatch_HandoffSourceError_TreatedAsNotReady: a transport
// error from the source is the same loud not-ready condition (fail
// closed), never a raw fallback.
func TestRelayBatch_HandoffSourceError_TreatedAsNotReady(t *testing.T) {
	svc, env, _ := installRelayEnv(t, &fakeRelayTokenSource{err: assert.AnError})
	resetAudit(env.secrets)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade)
	assert.Equal(t, DegradeRelayStagingNotReady, degrade.Reason)
	for _, e := range batch.Entries {
		assert.NotEqual(t, SecretTypeLLMProvider, e.Type)
	}
}

// TestRelayBatch_EmptyTokenEntry_SkippedLoudly: a handoff entry with an
// empty token (corruption — the controller never writes one) must not
// produce a keyless provider entry nor fall back to raw.
func TestRelayBatch_EmptyTokenEntry_SkippedLoudly(t *testing.T) {
	h := stageableHandoff("rDDDD4444", "")
	fake := &fakeRelayTokenSource{handoff: h}
	svc, env, _ := installRelayEnv(t, fake)
	resetAudit(env.secrets)

	batch, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	_, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	assert.False(t, ok, "an empty staged token emits no provider entry")
	raw, err := json.Marshal(batch)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "admin-key")
	assert.Contains(t, auditActions(env.secrets), "relay_token_missing")
}

// TestRelayBatch_ManifestTier_RevisionChangeRotatesHash: the handoff
// revision participates in the manifest tier (US-70.2 invariant, design
// §4.4) — a renewed token (new revision) changes ManifestFor so the
// conditional pull resyncs without a pod restart; flag-off manifests
// never consult the source.
func TestRelayBatch_ManifestTier_RevisionChangeRotatesHash(t *testing.T) {
	svc, _, fake := installRelayEnv(t, &fakeRelayTokenSource{handoff: stageableHandoff("rEEEE5555", "lrt_fake_rev_a")})

	h1, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	fake.handoff = stageableHandoff("rFFFF6666", "lrt_fake_rev_b")
	h2, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.NotEqual(t, h1, h2, "a revision change (token renewal at ~TTL/2) must rotate the manifest hash")

	fake.handoff = nil
	h3, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.NotEqual(t, h1, h3, "handoff absent is a different intended set than handoff present")

	// Flag off: the manifest is rows-only and the source is never
	// consulted (bit-identical legacy tier).
	svc.SetRelayTokenSource(nil)
	h4, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	callsAfterFlagOff := fake.calls
	h5, err := svc.ManifestFor(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, h4, h5)
	assert.Equal(t, callsAfterFlagOff, fake.calls, "flag-off manifest never reads the handoff")
	assert.NotEqual(t, h1, h4, "sanity: flag-on manifest differs from rows-only")
}

// TestRelayBatch_ManifestHashWithRelayRevision_Shape: the relay line is
// deterministic and additive — same entries + revision ⇒ same hash;
// different revision ⇒ different hash; empty revision ⇒ identical to
// the legacy ManifestHash (the tier degrades cleanly).
func TestRelayBatch_ManifestHashWithRelayRevision_Shape(t *testing.T) {
	entries := []ManifestEntry{
		{SecretID: "s1", Version: 1, Type: SecretTypeLLMProvider, Name: "openai"},
		{SecretID: "s2", Version: 2, Type: SecretTypeEnvSecret, Name: "db"},
	}
	a1 := ManifestHashWithRelayRevision("user-1", entries, "rAAA1111")
	a2 := ManifestHashWithRelayRevision("user-1", []ManifestEntry{
		{SecretID: "s2", Version: 2, Type: SecretTypeEnvSecret, Name: "db"},
		{SecretID: "s1", Version: 1, Type: SecretTypeLLMProvider, Name: "openai"},
	}, "rAAA1111")
	assert.Equal(t, a1, a2, "entry order never affects the manifest hash (I6)")

	assert.NotEqual(t, a1, ManifestHashWithRelayRevision("user-1", entries, "rBBB2222"))
	assert.NotEqual(t, a1, ManifestHash("user-1", entries))
	assert.Equal(t, ManifestHash("user-1", entries), ManifestHashWithRelayRevision("user-1", entries, ""),
		"an empty relay revision degrades to the legacy tier")
}

// TestRelayBatch_BuildMintsNewSeqOnRevisionChange: end to end through
// EnsureRevision — a renewed token mints a fresh seq (the resync
// trigger), same rows.
func TestRelayBatch_BuildMintsNewSeqOnRevisionChange(t *testing.T) {
	fake := &fakeRelayTokenSource{handoff: stageableHandoff("rGGGG7777", "lrt_fake_seq_a")}
	svc, env, _ := installRelayEnv(t, fake)
	b1, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	fake.handoff = stageableHandoff("rHHHH8888", "lrt_fake_seq_b")
	b2, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)

	assert.Greater(t, b2.Revision.Seq, b1.Revision.Seq, "revision-only change (same rows) mints a new seq")
	assert.Equal(t, b1.Entries[0].SecretID, b2.Entries[0].SecretID)
	_ = env
}

// TestRelayBatch_FlagOffKeepsLegacyRowSlugDedup: the legacy path dedups
// on the binding ROW slug only — two rows with different row-slugs but
// the same decrypted pd slug BOTH emit (flag off is byte-identical,
// duplicates included). The relay path dedups on the decrypted slug (the
// staged set's key), so the duplicate collapses there.
func TestRelayBatch_FlagOffKeepsLegacyRowSlugDedup(t *testing.T) {
	svc, env, _ := setupBuilder(t)
	dup := CredentialBinding{
		ID: "cred-admin-dup", OwnerType: "admin", OwnerID: "_platform", Kind: "openai", Slug: "openai-dup-row",
		Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "admin-key"}),
		Version:    3, SourceType: "auto",
	}
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred, dup}}
	svc.store = env.store()

	// Flag off: both rows emit (the historical behavior).
	legacy, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	count := 0
	for _, e := range legacy.Entries {
		if e.Type == SecretTypeLLMProvider && e.Name == "openai" {
			count++
		}
	}
	assert.Equal(t, 2, count, "flag off keeps the legacy row-slug-only dedup (byte-identical, duplicates included)")

	// Flag on with the slug staged: the duplicate collapses (the staged
	// set is keyed by the decrypted slug).
	fake := &fakeRelayTokenSource{handoff: stageableHandoff("rDUPE9999", "lrt_fake_dup")}
	svc.SetRelayTokenSource(fake)
	relayed, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	count = 0
	for _, e := range relayed.Entries {
		if e.Type == SecretTypeLLMProvider && e.Name == "openai" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}
