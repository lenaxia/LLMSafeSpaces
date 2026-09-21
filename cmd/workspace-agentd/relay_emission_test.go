// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// relay_emission_test.go — US-72.4's agentd-side emission contracts:
//
//   - the token-not-key CANARY: a relay batch materialized through the
//     REAL Materializer and formatted through the REAL pinned formatter
//     (FormatOpenCodeConfig via Materializer.FormatProviders — zero
//     formatter changes by construction) puts the TOKEN into the
//     provider files and NEVER the raw provider key;
//   - healthz/readyz/statusz surface the cached relay liveness slice
//     (degrade codes + warning line, never gating Healthy/Ready);
//   - EnrichProviders resolves /models through the router when the
//     baseURL is the router (the staged-catalog discovery path, §4.5).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	agentdsecrets "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// TestRelayEmission_TokenNotKeyCanary_FormatterPin: the formatted
// provider config contains the token bytes and never the raw key —
// through the unchanged Materializer → FormatProviders →
// FormatOpenCodeConfig chain (the formatter pin: the token lands in
// provider files exactly like a key would, format.go:71 verbatim).
// The batch is ALL-relay (the post-flip posture): the raw key the
// canary plants is the credential the router would hold, and it must
// appear nowhere in the emitted files.
func TestRelayEmission_TokenNotKeyCanary_FormatterPin(t *testing.T) {
	dir := t.TempDir()
	tokenPD, err := json.Marshal(sec.LLMProviderData{
		Kind: "openai", Slug: "openai", APIKey: fakeRelayToken,
		BaseURL: "http://router.invalid/w/ws-can/openai/v1",
		Models:  []sec.LLMModelConfig{{ID: "gpt-4o"}},
	})
	require.NoError(t, err)
	batch := sec.Batch{
		Entries: []sec.BatchEntry{{
			SecretID: "c1", Version: 1, Type: sec.SecretTypeLLMProvider, Name: "openai",
			Value: string(tokenPD),
			Metadata: mustJSONStr(t, map[string]string{
				sec.RelayMetadataKey:          "true",
				sec.RelayMetadataExpiresAtKey: futureRFC3339(t, time.Hour),
				sec.RelayMetadataRevisionKey:  "rCAN0001",
			}),
		}},
		Revision: sec.BatchRevision{Seq: 9, ManifestHash: "mh-can"},
	}
	raw, err := json.Marshal(batch)
	require.NoError(t, err)
	batchPath := filepath.Join(dir, "secrets.json")
	require.NoError(t, os.WriteFile(batchPath, raw, 0o600))

	bf, err := agentdsecrets.LoadBatchFile(batchPath)
	require.NoError(t, err)
	require.Len(t, bf.Secrets, 1)

	m := &agentdsecrets.Materializer{FS: agentdsecrets.RealFS(), Revision: bf.Revision, Paths: agentdsecrets.Paths{
		Home:            dir,
		SecretsBaseDir:  filepath.Join(dir, "rt", "secrets"),
		SSHDir:          filepath.Join(dir, "rt", "ssh"),
		AgentConfigPath: filepath.Join(dir, "agent-config.json"),
		SecretsEnvPath:  filepath.Join(dir, "secrets-env"),
		GitCredsPath:    filepath.Join(dir, "rt", "git-credentials"),
		StagingDir:      filepath.Join(dir, "rt", "staged-secret-files"),
	}}
	_, err = m.Materialize(bf.Secrets)
	require.NoError(t, err)

	formatted, err := m.FormatProviders(opencode.FormatOpenCodeConfig)
	require.NoError(t, err)
	require.NotEmpty(t, formatted)

	assert.Contains(t, string(formatted), fakeRelayToken, "the token is delivered verbatim (S1 carries the token by design, D3)")
	assert.NotContains(t, string(formatted), fakeRawKey, "CANARY: zero raw-key bytes in the formatted provider config")
	assert.Contains(t, string(formatted), "http://router.invalid/w/ws-can/openai/v1", "the router baseURL is wired")

	// The auth-store merge leg (#1296) takes StagedProviders — assert the
	// in-memory staged slice carries the token and never the raw key.
	for _, pd := range m.StagedProviders() {
		assert.Equal(t, fakeRelayToken, pd.APIKey)
		assert.NotEqual(t, fakeRawKey, pd.APIKey)
	}
}

// TestRelayEmission_HealthzSurfacesRelayDegrade: healthz carries the
// cached relay slice + the degraded:<code> warning (the US-70.1
// controller relay path), and NEVER flips Healthy on a relay degrade.
func TestRelayEmission_HealthzSurfacesRelayDegrade(t *testing.T) {
	relaySnap := func() *agentd.RelayHealth {
		return &agentd.RelayHealth{
			Present: true, Reachable: false, DegradedReason: "relay_unreachable",
			RouterURL: "http://router.invalid", LastProbeAt: time.Now().Unix(),
		}
	}
	handler := healthzHandler(time.Now(), "", nil, nil, relaySnap)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Healthy, "a relay degrade must not cascade to a liveness-probe kill (§4.8)")
	require.NotNil(t, resp.Relay)
	assert.Equal(t, "relay_unreachable", resp.Relay.DegradedReason)
	assert.False(t, resp.Relay.Reachable)
	assert.Contains(t, resp.Warnings, "degraded:relay_unreachable")
}

// TestRelayEmission_ReadyzSurfacesButNeverGates: readyz reports the
// degrade code observably while Ready stays governed by the existing
// semantics only.
func TestRelayEmission_ReadyzSurfacesButNeverGates(t *testing.T) {
	deps := serverDeps{
		healthCache: func() *healthzCache {
			c := newHealthzCache()
			c.snapshot.Store(&healthzCacheSnapshot{Initialized: true, Healthy: true})
			return c
		}(),
		cache: &providerCache{},
		gr:    newGateRecorder(time.Now(), agentdGateDurationSeconds, testLogger()),
		relayLiveness: func() *relayLivenessMonitor {
			m := newRelayLivenessMonitor(filepath.Join(t.TempDir(), "none.json"), nil)
			m.store(agentd.RelayHealth{Present: true, Reachable: false, DegradedReason: "token_expired"})
			return m
		}(),
	}
	handler := buildReadyzHandler(deps, func() bool { return true })

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/readyz", nil))
	require.Equal(t, http.StatusOK, rec.Code, "a relay degrade never drops readiness")

	var resp agentd.ReadyzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Ready)
	require.NotNil(t, resp.Relay)
	assert.Equal(t, "token_expired", resp.Relay.DegradedReason)
}

// TestRelayEmission_StatuszMirrorsRelaySlice: the deep-status scrape
// mirrors the slice so a relay degrade is visible between healthz
// scrapes.
func TestRelayEmission_StatuszMirrorsRelaySlice(t *testing.T) {
	client := &OpenCodeClient{password: "pw", client: &http.Client{Timeout: time.Second}}
	handler := buildStatuszHandler(client, &providerCache{}, newSessionStatusTracker(), newMemoryPressureMonitor(),
		time.Now(), "", defaultSysMetrics(), nil, nil,
		func() *agentd.RelayHealth {
			return &agentd.RelayHealth{Present: true, Reachable: true, AppliedRevision: "rST0001"}
		})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/statusz", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp agentd.StatuszResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Relay)
	assert.True(t, resp.Relay.Present)
	assert.True(t, resp.Relay.Reachable)
	assert.Equal(t, "rST0001", resp.Relay.AppliedRevision)
}

// TestRelayEmission_EnricherResolvesModelsThroughRouter: EnrichProviders
// hits {routerBaseURL}/models with the scoped token when the baseURL is
// the router (§4.5 — custom-endpoint model discovery keeps working and
// stays key-free pod-side: the router serves the staged catalog).
func TestRelayEmission_EnricherResolvesModelsThroughRouter(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]string{{"id": "gpt-4o"}, {"id": "gpt-4o-mini"}},
		})
	}))
	defer srv.Close()

	providers := []sec.LLMProviderData{{
		Kind: "openai", Slug: "openai",
		APIKey:  fakeRelayToken, // the token rides the apiKey slot verbatim
		BaseURL: srv.URL + "/w/ws-enr/openai/v1",
		// Models empty: the enricher's trigger condition.
	}}

	enrich := enrichProviderModels(context.Background(), t.TempDir(), &http.Client{Timeout: 5 * time.Second})
	out := enrich(providers)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 2)
	assert.Equal(t, "gpt-4o", out[0].Models[0].ID)
	assert.Equal(t, "/w/ws-enr/openai/v1/models", gotPath, "the router's /w/.../v1/models path (§4.5 shape)")
	assert.Equal(t, "Bearer "+fakeRelayToken, gotAuth, "the scoped token is the credential — no raw key exists pod-side")
}

// TestRelayEmission_NilMonitorIsSilent: a nil monitor (tests / partial
// wiring) keeps every surface empty — the flag-off pod contract.
func TestRelayEmission_NilMonitorIsSilent(t *testing.T) {
	assert.Nil(t, relayLivenessSnapshotFor(nil)())
	handler := healthzHandler(time.Now(), "", nil, nil, relayLivenessSnapshotFor(nil))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/healthz", nil))
	var resp agentd.HealthzResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp.Relay)
	assert.Empty(t, resp.Warnings)
}
