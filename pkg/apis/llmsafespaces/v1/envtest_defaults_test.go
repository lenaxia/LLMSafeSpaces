// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build envtest

// Package v1_test contains envtest integration tests that verify CRD defaults
// are applied by the real Kubernetes API server's admission webhook.
//
// This test file is behind a build tag so it does NOT run in the normal test
// suite (it requires downloading kube-apiserver + etcd binaries). The
// dedicated envtest GitHub Actions workflow runs these tests on every PR
// and nightly. See .github/workflows/envtest.yml.
//
// The fast unit-test seam (SetDefaults_* functions + defaults_test.go) runs
// in the normal suite and covers the same defaults — envtest verifies that
// the real admission webhook agrees with our Go defaulter.
package v1_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// TestEnvtest_WorkspaceDefaultsAppliedByAPIServer creates a Workspace via the
// real K8s API server (envtest) and verifies that the kubebuilder default
// annotations are applied by the admission webhook. This is the integration-
// level counterpart to defaults_test.go (M5-a).
//
// It specifically guards the PR #231 class of bug: if a +kubebuilder:default
// annotation exists in the CRD YAML but the admission controller doesn't
// apply it, this test fails while the unit test (which calls SetDefaults_
// directly) passes.
func TestEnvtest_WorkspaceDefaultsAppliedByAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set — run via setup-envtest or the envtest workflow")
	}

	scheme := runtime.NewScheme()
	require.NoError(t, v1.AddToScheme(scheme))

	// Start envtest with our CRDs installed.
	//
	// Path is relative to the test binary's working directory, which
	// is the package directory: pkg/apis/llmsafespaces/v1/. Four
	// levels deep, so four `..` to reach the repo root. The previous
	// "../../../" only reached pkg/, which made envtest silently
	// install zero CRDs (CRDDirectoryPaths doesn't error on missing
	// paths) — the subsequent k8sClient.Create then failed with the
	// misleading "no matches for llmsafespaces.dev/v1, Resource=".
	// The dedicated envtest CI workflow has been red on every PR
	// since this file was introduced (PR #274). Fixed.
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../../../helm/crds"},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	require.NoError(t, err, "envtest startup")
	defer func() { _ = testEnv.Stop() }()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Create a bare Workspace — no defaults set, just required fields.
	ws := &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-envtest-defaults",
			Namespace: "default",
		},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "user-envtest"},
			Runtime: "python:3.11",
			Storage: v1.WorkspaceStorageConfig{Size: "5Gi"},
		},
	}

	require.NoError(t, k8sClient.Create(ctx, ws), "create workspace")

	// Re-fetch and verify defaults were applied by the API server.
	fetched := &v1.Workspace{}
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: "ws-envtest-defaults", Namespace: "default"}, fetched))

	assert.Equal(t, "amd64", fetched.Spec.Architecture, "Architecture default must be applied by admission webhook")
	assert.Equal(t, "standard", fetched.Spec.SecurityLevel, "SecurityLevel default must be applied")
	assert.Equal(t, int32(5), fetched.Spec.MaxActiveSessions, "MaxActiveSessions default must be applied")
	assert.Equal(t, "ReadWriteOnce", fetched.Spec.Storage.AccessMode, "Storage.AccessMode default must be applied")

	// AutoSuspend is a pointer-to-struct with default: {} on its OpenAPI
	// schema (added in #281). The CI envtest workflow (envtest.yml) installs
	// setup-envtest and sets KUBEBUILDER_ASSETS, so this runs against a real
	// kube-apiserver — if default: {} does NOT materialize the nested object
	// (i.e. AutoSuspend stays nil), these assertions fail and surface the gap.
	// That is the intended behavior: this test is the end-to-end validation of
	// the apiserver-defaulting claim, not just the Go defaulter.
	require.NotNil(t, fetched.Spec.AutoSuspend, "AutoSuspend must be materialized by default: {} (apiserver defaulting)")
	assert.True(t, fetched.Spec.AutoSuspend.Enabled, "AutoSuspend.Enabled must default to true (PR #231 class)")
	assert.Equal(t, int64(86400), fetched.Spec.AutoSuspend.IdleTimeoutSeconds, "IdleTimeoutSeconds default must be applied")
}
