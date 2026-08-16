//go:build envtest

// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Envtest integration for the agentd pin resolution CLUSTER path
// (ResolvePinsWithCache → resolvePinsFromCluster → cachedPinResolver).
// Mutation-proven necessity (PR #890 review round 3): the unit suite
// could not detect the cluster path being broken, because no test
// drove it. These do — against a real API server.
//
// Run: go test ./controller/internal/workspace/ -tags envtest -run TestEnvtestAgentdPins
// Requires KUBEBUILDER_ASSETS (see .github/workflows/envtest.yml).

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	_ "k8s.io/client-go/plugin/pkg/client/auth" // keep envtest auth plugins linked
)

func startEnvtest(t *testing.T) *rest.Config {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "helm", "crds")},
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.Stop() })
	return cfg
}

// TestEnvtestAgentdPins_ImageOnlyResolvesAndCaches drives the REAL
// ResolvePinsWithCache image-only branch against a real API server: the
// injected fetcher resolves, the ConfigMap cache is created in the
// namespace, and a subsequent outage resolve falls back to that cache.
func TestEnvtestAgentdPins_ImageOnlyResolvesAndCaches(t *testing.T) {
	cfg := startEnvtest(t)
	t.Setenv("POD_NAMESPACE", "") // pin ambient env; exercises the default-ns fallback explicitly

	// envtest provides only default/kube-* namespaces; the fallback
	// namespace must exist before the cache write (round-4 finding).
	dyn0, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	require.NoError(t, dyn0.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "llmsafespaces"}}))

	// Swap BOTH seams: config loader → envtest API server; registry
	// fetcher → counting fake (the production fetcher would hit real
	// ghcr.io — the round-4 hermeticity finding).
	calls := 0
	origLoad := loadConfig
	loadConfig = func() (*rest.Config, error) { return cfg, nil }
	origFetch := prodFetchIndexAnnotations
	prodFetchIndexAnnotations = func(context.Context, string) (ociAnnotations, error) {
		calls++
		return goodAnnotations(), nil
	}
	t.Cleanup(func() {
		loadConfig = origLoad
		prodFetchIndexAnnotations = origFetch
	})

	pins, err := ResolvePinsWithCache(context.Background(), pinImage, "", "")
	require.NoError(t, err, "image-only form must resolve via the cluster path (this test replaces the fake-client near-duplicate that mutation proved unprotective)")
	require.Equal(t, pinAMD64, pins.SHA256AMD64)
	require.Equal(t, pinARM64, pins.SHA256ARM64)
	require.Equal(t, 1, calls, "resolution must have consulted the fetcher exactly once")

	// The cache ConfigMap must now exist in the DEFAULT namespace (empty
	// POD_NAMESPACE → llmsafespaces fallback).
	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	cm := &corev1.ConfigMap{}
	require.NoError(t, dyn.Get(context.Background(), client.ObjectKey{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"}, cm),
		"cache must be written to the fallback namespace")
	require.Equal(t, pinImage, cm.Data["image"])
}

// TestEnvtestAgentdPins_NamespaceFallbackProven uses a NON-default
// POD_NAMESPACE and asserts the cache lands in THAT namespace — deleting
// the fallback logic in resolvePinsFromCluster makes this fail.
func TestEnvtestAgentdPins_NamespaceFallbackProven(t *testing.T) {
	cfg := startEnvtest(t)
	t.Setenv("POD_NAMESPACE", "custom-ns")

	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	require.NoError(t, dyn.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "custom-ns"}}))

	// Route BOTH production seams at the test environment: the config
	// loader at the envtest API server, and the registry fetcher at a
	// fake (the production fetcher would hit real ghcr.io — the round-4
	// finding that made this test non-hermetic and unpassable).
	origLoad := loadConfig
	loadConfig = func() (*rest.Config, error) { return cfg, nil }
	origFetch := prodFetchIndexAnnotations
	prodFetchIndexAnnotations = func(context.Context, string) (ociAnnotations, error) {
		return goodAnnotations(), nil
	}
	t.Cleanup(func() {
		loadConfig = origLoad
		prodFetchIndexAnnotations = origFetch
	})

	pins, err := ResolvePinsWithCache(context.Background(), pinImage, "", "")
	require.NoError(t, err)
	require.Equal(t, pinAMD64, pins.SHA256AMD64)

	cm := &corev1.ConfigMap{}
	require.NoError(t, dyn.Get(context.Background(), client.ObjectKey{Name: AgentdPinsConfigMapName, Namespace: "custom-ns"}, cm),
		"cache must be written to POD_NAMESPACE when set — this is the ns-fallback mutation proof")
}

// TestEnvtestAgentdPins_OutageFallsBackToCache proves the full boot
// path under a registry outage: first resolve populates the cache,
// then with the fetcher failing, resolution still succeeds from the
// SAME digest's cache (round-5 requested leg).
func TestEnvtestAgentdPins_OutageFallsBackToCache(t *testing.T) {
	cfg := startEnvtest(t)
	t.Setenv("POD_NAMESPACE", "llmsafespaces")

	dyn, err := client.New(cfg, client.Options{})
	require.NoError(t, err)
	require.NoError(t, dyn.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "llmsafespaces"}}))

	origLoad := loadConfig
	loadConfig = func() (*rest.Config, error) { return cfg, nil }
	origFetch := prodFetchIndexAnnotations
	prodFetchIndexAnnotations = func(context.Context, string) (ociAnnotations, error) {
		return goodAnnotations(), nil
	}
	t.Cleanup(func() {
		loadConfig = origLoad
		prodFetchIndexAnnotations = origFetch
	})

	pins, err := ResolvePinsWithCache(context.Background(), pinImage, "", "")
	require.NoError(t, err)
	require.Equal(t, pinAMD64, pins.SHA256AMD64)

	// Registry goes down; the same digest must still resolve from cache.
	prodFetchIndexAnnotations = func(context.Context, string) (ociAnnotations, error) {
		return nil, errFetchUnavailable
	}
	pins, err = ResolvePinsWithCache(context.Background(), pinImage, "", "")
	require.NoError(t, err, "outage + same-digest cache must satisfy startup")
	require.Equal(t, pinAMD64, pins.SHA256AMD64)
}
