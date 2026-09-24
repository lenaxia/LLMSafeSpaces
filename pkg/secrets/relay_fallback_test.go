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
