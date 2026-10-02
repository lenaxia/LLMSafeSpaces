// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// relay_fallback_test.go — design 0061 §4 (M2): the migration-mode
// fail-open fallback + both counters. "Not ready at batch time" = the
// workspace's staged handoff Secret absent (or its token for a bound
// provider expired) at the builder's decision point, per-workspace
// per-provider: migration mode delivers the pre-flip RAW-key entry and
// increments relay_fallback_deliveries_total{workspace,provider_slug};
// strict mode preserves the fail-closed class-mute (the steady-state
// posture — what #1537's sweep asserts) and increments
// relay_degraded_batches_total{workspace,reason}. The fallback surfaces
// a NON-NIL BuildDegrade with Reason=DegradeRelayFallbackDelivery (the
// M4 seam contract — wt-1453's CredentialsStaged hook keys on it), and
// the reason JOINS IsRelayDegrade's set (the builder owns the
// vocabulary).

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetRelayCounters clears both M2 counters (package-global
// collectors — the absolute zero-assertions need per-test isolation).
func resetRelayCounters(t *testing.T) {
	t.Helper()
	relayFallbackDeliveries.DeletePartialMatch(prometheus.Labels{})
	relayDegradedBatches.DeletePartialMatch(prometheus.Labels{})
}

func fallbackDelta(t *testing.T, ws, slug string) float64 {
	t.Helper()
	v := testutil.ToFloat64(relayFallbackDeliveries.WithLabelValues(ws, slug))
	return v
}

func degradedDelta(t *testing.T, ws, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(relayDegradedBatches.WithLabelValues(ws, reason))
}

// MIGRATION + handoff absent: the raw entries DELIVER (the pre-flip
// bytes), the degrade is non-nil with the FALLBACK reason (the seam
// contract), the fallback counter fires per provider slug, and the
// audit rows name the fallback.
func TestRelayFallback_MigrationDeliversRawKeysAndCounts(t *testing.T) {
	resetRelayCounters(t)
	fake := &fakeRelayTokenSource{} // handoff nil: staging not ready
	svc, env, _ := installRelayEnv(t, fake)
	_ = fake
	svc.SetRelayDeliveryFallback(true) // migration mode

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade, "the fallback surfaces a NON-NIL degrade (the M4 seam contract)")
	assert.Equal(t, DegradeRelayFallbackDelivery, degrade.Reason)
	assert.True(t, IsRelayDegrade(degrade), "the fallback reason is relay-class (the M4 classifier picks it up)")

	// The stageable provider delivers RAW (the pre-flip bytes — the
	// availability half of the trade).
	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`,
		"migration mode delivers the RAW key when staging is not ready")
	assert.Nil(t, llm.Metadata, "no relay metadata on a raw fallback entry")

	// The counter fired for the stageable slug (bedrock's not-staged raw
	// path is the existing mixed-fleet class — counted too by the
	// design's definition: no usable staged token).
	assert.Greater(t, fallbackDelta(t, "ws-1", "openai"), 0.0,
		"relay_fallback_deliveries_total{ws-1,openai} incremented")
	assert.Greater(t, fallbackDelta(t, "ws-1", "aws-bedrock"), 0.0,
		"a not-staged raw emission under a not-ready handoff counts as a fallback delivery")

	// The audit names the fallback (the audit log lives on the env's
	// mock secret store — the builderTestStore wraps it).
	assert.Contains(t, auditActions(env.secrets), "relay_fallback_delivery",
		"the whole-handoff fallback audits relay_fallback_delivery")

	// The DEGRADED counter does NOT fire: a fallback batch delivered.
	assert.Equal(t, 0.0, degradedDelta(t, "ws-1", DegradeRelayFallbackDelivery))
	assert.Equal(t, 0.0, degradedDelta(t, "ws-1", DegradeRelayStagingNotReady))
}

// MIGRATION + expired token for one provider: THAT provider falls back
// raw + counts; the still-valid provider keeps its token (per-provider
// readiness, the design's granularity).
func TestRelayFallback_ExpiredTokenFallsBackPerProvider(t *testing.T) {
	resetRelayCounters(t)
	svc, _, _ := installRelayEnv(t, &fakeRelayTokenSource{})
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	handoff := testHandoff("rEXP0001",
		RelayHandoffProvider{ProviderSlug: "openai", Kind: "openai", Token: "lrt_expired", RouterPath: "/w/ws-1/openai/v1", ExpiresAt: expired},
		RelayHandoffProvider{ProviderSlug: "aws-bedrock", Kind: "bedrock", Token: "lrt_bedrock_ok", RouterPath: "/w/ws-1/aws-bedrock/v1", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	)
	svc.SetRelayTokenSource(&fakeRelayTokenSource{handoff: handoff})
	svc.SetRelayDeliveryFallback(true)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	// The handoff EXISTS (staging ran) — no class degrade: the M4
	// condition stays True; only the expired provider fell back.
	assert.Nil(t, degrade, "an expired token with a present handoff is per-provider, not class-level")

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`,
		"the EXPIRED provider falls back to the raw key (an expired token is not-ready at batch time)")
	assert.Nil(t, llm.Metadata)
	assert.Greater(t, fallbackDelta(t, "ws-1", "openai"), 0.0, "the expired fallback counts")

	bedrock, ok := findEntry(batch, SecretTypeLLMProvider, "aws-bedrock")
	require.True(t, ok)
	assert.Contains(t, bedrock.Value, "lrt_bedrock_ok",
		"the still-valid provider keeps its token")
	require.NotNil(t, bedrock.Metadata, "the token entry carries relay metadata")
}

// STRICT + handoff absent: the CURRENT fail-closed behavior, pinned —
// the class mutes, the degrade says not-ready, and the DEGRADED counter
// fires (the mode-independent AC2 detection: strict-mode staging death
// is loud without the fallback counter).
func TestRelayFallback_StrictFailClosedPreserved(t *testing.T) {
	resetRelayCounters(t)
	svc, _, _ := installRelayEnv(t, &fakeRelayTokenSource{}) // handoff nil
	// No SetRelayDeliveryFallback call: the zero value is STRICT (the
	// fail-closed default until the API installs migration).

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	require.NotNil(t, degrade)
	assert.Equal(t, DegradeRelayStagingNotReady, degrade.Reason)

	_, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	assert.False(t, ok, "strict mode mutes the class (the fail-closed posture, unchanged)")

	assert.Greater(t, degradedDelta(t, "ws-1", DegradeRelayStagingNotReady), 0.0,
		"relay_degraded_batches_total{ws-1,relay_staging_not_ready} incremented — strict-mode detection is mode-independent")
	assert.Equal(t, 0.0, fallbackDelta(t, "ws-1", "openai"), "no fallback delivery under strict")
}

// Handoff present + valid: unchanged token delivery, no counters (the
// flip-path steady state).
func TestRelayFallback_PresentHandoffUnchanged(t *testing.T) {
	resetRelayCounters(t)
	svc, _, _ := installRelayEnv(t, &fakeRelayTokenSource{handoff: stageableHandoff("rOK00001", "lrt_fresh")})
	svc.SetRelayDeliveryFallback(true)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Nil(t, degrade, "a ready handoff: no degrade in either mode")

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Contains(t, llm.Value, "lrt_fresh")
	assert.Equal(t, 0.0, fallbackDelta(t, "ws-1", "openai"), "no fallback counter on the token path")

	// Key Decision 3, PINNED (r1 finding 4): the bedrock binding (a
	// NOT-STAGED slug under a PRESENT handoff — the #1529 mixed-fleet
	// raw class) does NOT count as a fallback delivery: staging is
	// READY; only absent-handoff and expired-token emissions count.
	assert.Equal(t, 0.0, fallbackDelta(t, "ws-1", "aws-bedrock"),
		"a not-staged raw emission under a PRESENT handoff is NOT a fallback delivery (Key Decision 3)")
}

// The seam contract (wt-1453's M4): the fallback reason JOINS
// IsRelayDegrade's set — the CredentialsStaged hook picks it up
// unchanged.
func TestIsRelayDegrade_IncludesFallback(t *testing.T) {
	assert.True(t, IsRelayDegrade(&BuildDegrade{Reason: DegradeRelayFallbackDelivery}))
	assert.True(t, IsRelayDegrade(&BuildDegrade{Reason: DegradeRelayStagingNotReady}))
	assert.False(t, IsRelayDegrade(&BuildDegrade{Reason: "dek_unwrap_failed"}))
	assert.False(t, IsRelayDegrade(nil))
}

// STRICT + expired token: the token DELIVERS UNCHANGED (applyRelayHandoff
// skips the expiry check when !fallbackAllowed — the existing renewal
// path owns expiry; no behavior change under strict).
func TestRelayFallback_StrictExpiredDeliversToken(t *testing.T) {
	resetRelayCounters(t)
	expired := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	handoff := testHandoff("rEXPSTR1", RelayHandoffProvider{
		ProviderSlug: "openai", Kind: "openai", Token: "lrt_expired_strict", RouterPath: "/w/ws-1/openai/v1", ExpiresAt: expired,
	})
	svc, _, _ := installRelayEnv(t, &fakeRelayTokenSource{handoff: handoff})
	// strict: the zero value — NO SetRelayDeliveryFallback call.

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Nil(t, degrade, "strict expired: no class degrade (the handoff exists)")

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok)
	assert.Contains(t, llm.Value, "lrt_expired_strict",
		"STRICT delivers the (expired) token UNCHANGED — the renewal path owns expiry")
	assert.NotNil(t, llm.Metadata, "token metadata (with the expiry the agentd liveness reads)")
	assert.Equal(t, 0.0, fallbackDelta(t, "ws-1", "openai"), "no fallback delivery under strict")
}

// TestRelayFallback_StageableAbsentFromPresentHandoff_MigrationCounts:
// design 0061 §4's not-ready is PER-PROVIDER ("the handoff Secret ...
// carrying a token for a currently-bound llm-provider credential, is
// absent") — a FRONTABLE provider whose token is missing from an
// otherwise-present handoff (mint failure, staging lag, torn handoff)
// is not-ready for THAT credential, and its raw emission must join the
// COUNTED fallback class. Post-#1611 this is the live residual failure
// shape: the 5-day outage's mint path has never succeeded in
// production, and a mint outage leaves exactly this handoff — present,
// token missing — which the stall detector was blind to (relayNotStaged
// → relay_raw_emission, uncounted, invisible to the strict-flip
// criterion). The NON-frontable kind in the same batch stays the
// uncounted D5 mixed-fleet class (Key Decision 3's bedrock pin,
// unchanged).
func TestRelayFallback_StageableAbsentFromPresentHandoff_MigrationCounts(t *testing.T) {
	resetRelayCounters(t)
	// Empty-but-present handoff: staging ran, every token is missing
	// (the all-mints-failed shape; a controller pass writes exactly this).
	svc, env, _ := installRelayEnv(t, &fakeRelayTokenSource{handoff: testHandoff("rMISS001")})
	resetAudit(env.secrets)
	svc.SetRelayDeliveryFallback(true)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	// Per-provider fallbacks stay counter/audit-only — the expired-arm
	// pin's class decision (TestRelayFallback_ExpiredTokenFallsBackPerProvider):
	// no class-level degrade when the handoff is present.
	assert.Nil(t, degrade, "per-provider fallback is not a class degrade (the M2 ruling's shape)")

	llm, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	require.True(t, ok, "migration delivers the raw entry (the availability half)")
	assert.Contains(t, llm.Value, `"apiKey":"admin-key"`)
	assert.Nil(t, llm.Metadata, "no relay metadata on a raw fallback entry")
	assert.Greater(t, fallbackDelta(t, "ws-1", "openai"), 0.0,
		"the stageable-absent emission COUNTS — the stall detector must see the mint-failure class")
	assert.Contains(t, auditActions(env.secrets), "relay_fallback_delivery",
		"the emission audits as a fallback delivery, not as the D5 raw class")

	// The non-frontable kind in the same batch: unchanged D5 raw path —
	// audited relay_raw_emission, NEVER counted (Key Decision 3).
	bedrock, ok := findEntry(batch, SecretTypeLLMProvider, "aws-bedrock")
	require.True(t, ok, "the non-frontable kind keeps the raw mixed-fleet path")
	assert.Contains(t, bedrock.Value, "bedrock-raw-key")
	assert.Equal(t, 0.0, fallbackDelta(t, "ws-1", "aws-bedrock"),
		"a non-frontable raw emission is NOT a fallback delivery (Key Decision 3, unchanged)")
	assert.Contains(t, auditActions(env.secrets), "relay_raw_emission")
}

// TestRelayFallback_StageableAbsentFromPresentHandoff_StrictMutes: under
// STRICT a frontable provider missing from a present handoff must NOT
// deliver raw — the fail-open hole in the fail-closed mode (strict's
// contract: zero raw-key delivery for stageable providers). The mute is
// audited and counted by relay_degraded_batches_total (the
// mode-independent detector); the non-frontable kind keeps the D5 raw
// carve in both modes.
func TestRelayFallback_StageableAbsentFromPresentHandoff_StrictMutes(t *testing.T) {
	resetRelayCounters(t)
	svc, env, _ := installRelayEnv(t, &fakeRelayTokenSource{handoff: testHandoff("rMISS002")})
	resetAudit(env.secrets)
	// strict: the zero value — NO SetRelayDeliveryFallback call.

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Nil(t, degrade, "per-provider mute is not a class degrade (handoff present)")

	_, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	assert.False(t, ok, "STRICT never emits raw for a frontable provider — the fail-open hole closed")
	assert.Greater(t, degradedDelta(t, "ws-1", DegradeRelayStagingNotReady), 0.0,
		"the strict mute is mode-independently detected (relay_degraded_batches_total)")
	assert.Contains(t, auditActions(env.secrets), "credential_skipped_relay_not_ready",
		"the muted provider is named in the audit vocabulary")
	assert.Equal(t, 0.0, fallbackDelta(t, "ws-1", "openai"), "no fallback delivery under strict")

	// D5 strict carve: the non-frontable kind still rides raw.
	bedrock, ok := findEntry(batch, SecretTypeLLMProvider, "aws-bedrock")
	require.True(t, ok, "the non-frontable D5 carve survives strict mode")
	assert.Contains(t, bedrock.Value, "bedrock-raw-key")
	assert.Nil(t, bedrock.Metadata)
}

// TestRelayFrontableProvider_Matrix pins the shared predicate the
// controller's relayDesiredSet and the builder's per-provider fallback
// classification MUST agree on (one truth in pkg/secrets — the staged
// set and the counted set key identically by construction).
func TestRelayFrontableProvider_Matrix(t *testing.T) {
	assert.True(t, RelayFrontableProvider(LLMProviderData{Kind: "openai"}),
		"a stageable kind with a table default upstream is frontable")
	assert.True(t, RelayFrontableProvider(LLMProviderData{Kind: "openai_compatible", BaseURL: "https://up.example.com/v1"}),
		"a custom endpoint kind with an explicit BaseURL is frontable")
	assert.False(t, RelayFrontableProvider(LLMProviderData{Kind: "openai_compatible"}),
		"a custom endpoint kind WITHOUT a BaseURL is not frontable (the controller skips it — worklog D5)")
	assert.False(t, RelayFrontableProvider(LLMProviderData{Kind: "bedrock", BaseURL: "https://x.example.com"}),
		"a non-stageable kind is never frontable regardless of BaseURL")
	assert.False(t, RelayFrontableProvider(LLMProviderData{Kind: ""}), "no kind, no fronting")
}

// TestRelayFallback_StrictTwoFrontableAbsent_CountsBatchOnce: the strict
// per-provider mute fires relay_degraded_batches_total ONCE PER BATCH
// (the counter's unit — the class-level path's shape), no matter how
// many frontable providers the all-mints-failed handoff is missing.
// Review r1 finding 1's missing test: the guard was a dead store inside
// the binding loop (per-ENTRY counting, 2.0 here).
func TestRelayFallback_StrictTwoFrontableAbsent_CountsBatchOnce(t *testing.T) {
	resetRelayCounters(t)
	svc, env, _ := setupBuilder(t)
	anthropic := CredentialBinding{
		ID: "cred-anthropic", OwnerType: "admin", OwnerID: "_platform", Kind: "anthropic", Slug: "anthropic-row-slug",
		Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{
			Kind: "anthropic", Slug: "anthropic", APIKey: "anthropic-raw-key",
			Models: []LLMModelConfig{{ID: "claude-sonnet-4-5"}},
		}),
		Version: 2, SourceType: "auto",
	}
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred, anthropic}}
	svc.store = env.store()
	svc.SetRelayTokenSource(&fakeRelayTokenSource{handoff: testHandoff("rTWO0001")})
	resetAudit(env.secrets)

	batch, degrade, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Nil(t, degrade, "per-provider mute is not a class degrade")

	_, ok := findEntry(batch, SecretTypeLLMProvider, "openai")
	assert.False(t, ok)
	_, ok = findEntry(batch, SecretTypeLLMProvider, "anthropic")
	assert.False(t, ok, "both frontable providers muted under strict")
	assert.Equal(t, 1.0, degradedDelta(t, "ws-1", DegradeRelayStagingNotReady),
		"ONE batch-level increment regardless of muted-provider count (the counter's unit)")
}

// TestRelayFallback_StrictDuplicateRows_SingleMutePerSlug: the same
// decrypted slug reached via two binding rows (the legacy row-slug-only
// dedup shape) mutes EXACTLY once — one audit row, one batch counter
// tick. Review r1 finding 1's dedup bypass: the mute's continue skipped
// the pd-slug seen mark, double-firing audit and counter per row.
func TestRelayFallback_StrictDuplicateRows_SingleMutePerSlug(t *testing.T) {
	resetRelayCounters(t)
	svc, env, _ := setupBuilder(t)
	dup := CredentialBinding{
		ID: "cred-admin-dup", OwnerType: "admin", OwnerID: "_platform", Kind: "openai", Slug: "openai-dup-row",
		Ciphertext: adminCiphertext(t, env.adminKey, LLMProviderData{Kind: "openai", Slug: "openai", APIKey: "admin-key"}),
		Version:    3, SourceType: "auto",
	}
	env.creds = &mockCredentialStore{bindings: []CredentialBinding{env.adminCred, dup}}
	svc.store = env.store()
	svc.SetRelayTokenSource(&fakeRelayTokenSource{handoff: testHandoff("rDUP0001")})
	resetAudit(env.secrets)

	batch, _, err := svc.BuildWorkspaceBatch(context.Background(), "user-1", "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, func() int {
		n := 0
		for _, e := range batch.Entries {
			if e.Type == SecretTypeLLMProvider {
				n++
			}
		}
		return n
	}(), "no llm-provider entries under the strict mute")

	muteRows := 0
	env.secrets.mu.Lock()
	for _, a := range env.secrets.audit {
		if a.Action == "credential_skipped_relay_not_ready" {
			muteRows++
		}
	}
	env.secrets.mu.Unlock()
	assert.Equal(t, 1, muteRows, "exactly ONE mute audit for the duplicate-row slug")
	assert.Equal(t, 1.0, degradedDelta(t, "ws-1", DegradeRelayStagingNotReady), "one batch-level tick")
}
