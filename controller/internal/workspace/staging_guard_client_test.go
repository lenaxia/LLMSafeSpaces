// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// staging_guard_client_test.go — the fail-loud startup guard (AC: flag on
// without a reachable/configured router refuses startup) and the two HTTP
// seams (credential source, router internal API).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

func TestValidateRelayStagingFlags(t *testing.T) {
	assert.NoError(t, ValidateRelayStagingFlags(false, "", "", ""))
	assert.NoError(t, ValidateRelayStagingFlags(true, "http://router", "http://api", "llm-relay"))
	for _, tc := range []struct {
		name, router, api, ns string
	}{
		{"no router URL", "", "http://api", "llm-relay"},
		{"no API URL (credential source)", "http://router", "", "llm-relay"},
		{"no namespace", "http://router", "http://api", ""},
	} {
		assert.Error(t, ValidateRelayStagingFlags(true, tc.router, tc.api, tc.ns), tc.name)
	}
}

func TestProbeRelayRouter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	require.NoError(t, ProbeRelayRouter(context.Background(), srv.URL))

	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()
	assert.Error(t, ProbeRelayRouter(context.Background(), deadURL), "unreachable router must fail the probe")

	unhealthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthy.Close()
	assert.Error(t, ProbeRelayRouter(context.Background(), unhealthy.URL), "non-200 healthz must fail the probe")
}

func guardClient(t *testing.T, objs ...runtime.Object) *WorkspaceReconciler {
	t.Helper()
	sch := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(sch).WithRuntimeObjects(objs...).Build()
	return &WorkspaceReconciler{Client: fc, Scheme: sch}
}

func TestValidateRelayStagingStartup_FailLoudMatrix(t *testing.T) {
	pubSec, _ := makePubSecret(t, 1)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: relayTestNamespace}}

	t.Run("healthy full setup passes and creates the mint key", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer srv.Close()
		r := guardClient(t, ns, pubSec)
		cfg, err := NewRelayStagingConfig(srv.URL, relayTestNamespace, time.Hour, &fakeProviderSource{}, &fakeRouterClient{}, &recordingRedactor{})
		require.NoError(t, err)
		require.NoError(t, ValidateRelayStagingStartup(context.Background(), cfg, r.Client))
		mk := &corev1.Secret{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: relayTestNamespace, Name: secrets.RelayMintKeyName}, mk))
		assert.NotEmpty(t, string(mk.Data[secrets.RelayMintKeyDataKey]))
	})

	t.Run("unreachable router refuses startup", func(t *testing.T) {
		dead := httptest.NewServer(http.NewServeMux())
		url := dead.URL
		dead.Close()
		r := guardClient(t, ns, pubSec)
		cfg, err := NewRelayStagingConfig(url, relayTestNamespace, time.Hour, &fakeProviderSource{}, &fakeRouterClient{}, &recordingRedactor{})
		require.NoError(t, err)
		err = ValidateRelayStagingStartup(context.Background(), cfg, r.Client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not reachable")
	})

	t.Run("namespace absence surfaces via the pub read (no cluster-scoped Namespace GET)", func(t *testing.T) {
		// The guard deliberately performs NO Namespace GET (cluster-scoped;
		// granted under no rbac.scope). A missing namespace surfaces as the
		// pub Secret read failing — pinned here so the access pattern
		// cannot quietly regress into needing cluster-scope RBAC.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer srv.Close()
		r := guardClient(t) // no namespace object, no pub Secret
		cfg, err := NewRelayStagingConfig(srv.URL, relayTestNamespace, time.Hour, &fakeProviderSource{}, &fakeRouterClient{}, &recordingRedactor{})
		require.NoError(t, err)
		err = ValidateRelayStagingStartup(context.Background(), cfg, r.Client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), secrets.RelayPubSecretName)
		assert.NotContains(t, err.Error(), "cluster-scoped", "the guard must never require namespace-object RBAC")
	})

	t.Run("missing pub Secret (router not bootstrapped) refuses startup", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer srv.Close()
		r := guardClient(t, ns)
		cfg, err := NewRelayStagingConfig(srv.URL, relayTestNamespace, time.Hour, &fakeProviderSource{}, &fakeRouterClient{}, &recordingRedactor{})
		require.NoError(t, err)
		err = ValidateRelayStagingStartup(context.Background(), cfg, r.Client)
		require.Error(t, err)
		assert.Contains(t, err.Error(), secrets.RelayPubSecretName)
	})

	t.Run("shape-invalid pub refuses startup", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		defer srv.Close()
		badPub := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayPubSecretName, Namespace: relayTestNamespace},
			Data:       map[string][]byte{secrets.RelayPubDataKey: []byte("not-json")},
		}
		r := guardClient(t, ns, badPub)
		cfg, err := NewRelayStagingConfig(srv.URL, relayTestNamespace, time.Hour, &fakeProviderSource{}, &fakeRouterClient{}, &recordingRedactor{})
		require.NoError(t, err)
		assert.ErrorContains(t, ValidateRelayStagingStartup(context.Background(), cfg, r.Client), "shape-invalid")
	})
}

func TestCachedLLMProviderSource_TTLAndAuth(t *testing.T) {
	var calls int
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotToken = r.Header.Get("X-Internal-Token")
		if r.URL.Path != "/api/v1/internal/workspaces/ws-9/llm-providers" || r.URL.Query().Get("ownerUserID") != "u1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"providers": []map[string]any{{"kind": "openai", "slug": "openai", "apiKey": "sk-1"}},
		})
	}))
	defer srv.Close()

	src := NewCachedLLMProviderSource(srv.URL, "sekret", time.Minute)
	p1, err := src.LLMProviders(context.Background(), "u1", "ws-9")
	require.NoError(t, err)
	require.Len(t, p1, 1)
	p2, err := src.LLMProviders(context.Background(), "u1", "ws-9")
	require.NoError(t, err)
	assert.Equal(t, 1, calls, "cache absorbs repeat fetches within TTL")
	assert.Equal(t, "sk-1", p2[0].APIKey)
	assert.Equal(t, "sekret", gotToken)
}

func TestCachedLLMProviderSource_NoStaleServe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"providers": []map[string]any{}})
	}))
	defer srv.Close()
	src := NewCachedLLMProviderSource(srv.URL, "t", time.Minute)
	_, err := src.LLMProviders(context.Background(), "u", "w")
	require.NoError(t, err)
	srv.Close()

	// A later fetch failure must surface as an error — never a stale
	// credential set (unlike org-status, which fail-opens).
	_, err = src.LLMProviders(context.Background(), "other-user", "w")
	assert.Error(t, err)
}

func TestHTTPRelayRouterClient_MintAndRotate(t *testing.T) {
	pubSec, _ := makePubSecret(t, 4)
	var mintBody map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/internal/v1/tokens":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&mintBody))
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "lrt-real"})
		case "/internal/v1/keys/rotate":
			_ = json.NewEncoder(w).Encode(map[string]any{"keyID": "hpke-g4", "generation": 4, "publicKey": pubSec.Data[secrets.RelayPubDataKey]})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	r := guardClient(t, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secrets.RelayMintKeyName, Namespace: relayTestNamespace},
		Data:       map[string][]byte{secrets.RelayMintKeyDataKey: []byte("mint-secret")},
	})
	c := NewHTTPRelayRouterClient(srv.URL, relayTestNamespace, r.Client)

	token, err := c.MintToken(context.Background(), RelayMintRequest{WorkspaceID: "w", ProviderSlug: "s", BaseURL: "https://x", TTLSec: 60, KeyID: "hpke-g4"})
	require.NoError(t, err)
	assert.Equal(t, "lrt-real", token)
	assert.Equal(t, "Bearer mint-secret", auth)
	assert.Equal(t, "hpke-g4", mintBody["keyID"])

	receipt, err := c.RotateKeys(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "hpke-g4", receipt.KeyID)
	assert.EqualValues(t, 4, receipt.Generation)
}
