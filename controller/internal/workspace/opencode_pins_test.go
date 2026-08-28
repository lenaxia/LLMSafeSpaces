// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Tests for design 0053 §4.2: opencode overlay delivery pin resolution.
//
// Identical contract to the agentd pin resolution (#863 follow-up),
// against the opencode annotation keys and the
// llmsafespaces-opencode-pins ConfigMap:
//
//   - annotation extraction from the index (happy + missing keys)
//   - cache write on success, cache fallback on fetch failure,
//     stale-cache rejection (different digest)
//   - flag override precedence and the ErrOpencodePinsUnavailable
//     sentinel on every outage dead-end
//   - config validation: image-only valid; hashes-without-image,
//     one-sided pairs, non-hex, and floating tags invalid.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	rest "k8s.io/client-go/rest"
)

const (
	opencodePinImage = "ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	opencodePinAMD64 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	opencodePinARM64 = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

func goodOpencodeAnnotations() map[string]string {
	return map[string]string{
		opencodeAnnotationKeyAMD64: opencodePinAMD64,
		opencodeAnnotationKeyARM64: opencodePinARM64,
		annotationVersion:          "1.18.10",
	}
}

func newOpencodePinResolver(t *testing.T, objs ...client.Object) *cachedPinResolver {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &cachedPinResolver{Client: c, Namespace: "llmsafespaces", source: opencodePinSource}
}

func TestOpencodePinResolver_ExtractsAnnotations(t *testing.T) {
	r := newOpencodePinResolver(t)
	r.fetch = fakeIndexFetcher(goodOpencodeAnnotations(), nil)
	pins, err := r.Resolve(context.Background(), opencodePinImage)
	require.NoError(t, err)
	require.Equal(t, opencodePinAMD64, pins.SHA256AMD64)
	require.Equal(t, opencodePinARM64, pins.SHA256ARM64)
}

func TestOpencodePinResolver_MissingAnnotationsFails(t *testing.T) {
	r := newOpencodePinResolver(t)
	r.fetch = fakeIndexFetcher(map[string]string{}, nil)
	_, err := r.Resolve(context.Background(), opencodePinImage)
	require.Error(t, err, "an index without opencode pin annotations is a broken pipeline — fail closed, never launch unverifiable pods")
	require.Contains(t, err.Error(), "annotation")
}

func TestOpencodePinResolver_SuccessWritesCache(t *testing.T) {
	r := newOpencodePinResolver(t)
	r.fetch = fakeIndexFetcher(goodOpencodeAnnotations(), nil)
	_, err := r.Resolve(context.Background(), opencodePinImage)
	require.NoError(t, err)

	cm := &corev1.ConfigMap{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"}, cm))
	require.Equal(t, opencodePinImage, cm.Data["image"])
	require.Equal(t, opencodePinAMD64, cm.Data["sha256-amd64"])
	require.Equal(t, opencodePinARM64, cm.Data["sha256-arm64"])
}

func TestOpencodePinResolver_FetchFailureFallsBackToCache(t *testing.T) {
	r := newOpencodePinResolver(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        opencodePinImage,
			"sha256-amd64": opencodePinAMD64,
			"sha256-arm64": opencodePinARM64,
		},
	})
	r.fetch = fakeIndexFetcher(nil, errOpencodeFetchUnavailable)
	pins, err := r.Resolve(context.Background(), opencodePinImage)
	require.NoError(t, err, "a registry outage at controller boot must not brick startup when a cached pin for the SAME digest exists")
	require.Equal(t, opencodePinAMD64, pins.SHA256AMD64)
}

func TestOpencodePinResolver_StaleCacheRejected(t *testing.T) {
	r := newOpencodePinResolver(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        "ghcr.io/lenaxia/llmsafespaces/opencode:1.18.10@sha256:" + strings.Repeat("1", 64),
			"sha256-amd64": opencodePinAMD64,
			"sha256-arm64": opencodePinARM64,
		},
	})
	r.fetch = fakeIndexFetcher(nil, errOpencodeFetchUnavailable)
	_, err := r.Resolve(context.Background(), opencodePinImage)
	require.Error(t, err, "a cached pin for a DIFFERENT digest must never satisfy a new pin — that is the desync this design exists to prevent")
}

// TestOpencodePinResolver_ErrorsCarrySentinel proves the outage paths
// wrap ErrOpencodePinsUnavailable so main.go's errors.Is hint fires.
func TestOpencodePinResolver_ErrorsCarrySentinel(t *testing.T) {
	// No cache at all → sentinel.
	r := newOpencodePinResolver(t)
	r.fetch = fakeIndexFetcher(nil, errors.New("connection refused"))
	_, err := r.Resolve(context.Background(), opencodePinImage)
	require.ErrorIs(t, err, ErrOpencodePinsUnavailable, "first-boot outage must carry the sentinel for the manual-pin hint")

	// Malformed cache → sentinel.
	r2 := newOpencodePinResolver(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        opencodePinImage,
			"sha256-amd64": "garbage",
			"sha256-arm64": opencodePinARM64,
		},
	})
	r2.fetch = fakeIndexFetcher(nil, errors.New("connection refused"))
	_, err = r2.Resolve(context.Background(), opencodePinImage)
	require.ErrorIs(t, err, ErrOpencodePinsUnavailable, "malformed-cache refusal must carry the sentinel")
	require.Contains(t, err.Error(), "malformed")

	// RBAC-denied cache read → sentinel (the wrap names the ConfigMap).
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c3 := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "configmaps"}, key.Name, errors.New("denied"))
		},
	}).Build()
	r3 := &cachedPinResolver{Client: c3, Namespace: "llmsafespaces", source: opencodePinSource,
		fetch: fakeIndexFetcher(nil, errors.New("connection refused"))}
	_, err = r3.Resolve(context.Background(), opencodePinImage)
	require.ErrorIs(t, err, ErrOpencodePinsUnavailable, "RBAC-denied cache read must carry the sentinel")
	require.Contains(t, err.Error(), OpencodePinsConfigMapName, "the RBAC hint must name the ConfigMap")
}

func TestOpencodePinResolver_MalformedCacheRejected(t *testing.T) {
	r := newOpencodePinResolver(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        opencodePinImage,
			"sha256-amd64": "not-hex",
			"sha256-arm64": opencodePinARM64,
		},
	})
	r.fetch = fakeIndexFetcher(nil, errOpencodeFetchUnavailable)
	_, err := r.Resolve(context.Background(), opencodePinImage)
	require.Error(t, err, "a malformed cache entry must not satisfy the pin")
	require.Contains(t, err.Error(), "malformed")
}

// --- ResolveOpencodePinsWithCache: the production boot path ---------------

func TestResolveOpencodePinsWithCache_FullFlagsShortCircuit(t *testing.T) {
	// Manual pin: no kubeconfig access, no registry access — flags
	// returned verbatim after hex validation.
	pins, err := ResolveOpencodePinsWithCache(context.Background(), opencodePinImage,
		"c"+strings.Repeat("c", 63), "d"+strings.Repeat("d", 63))
	require.NoError(t, err)
	require.Equal(t, "c"+strings.Repeat("c", 63), pins.SHA256AMD64)
	require.Equal(t, "d"+strings.Repeat("d", 63), pins.SHA256ARM64)
}

func TestResolveOpencodePinsWithCache_FullFlagsInvalidHexRejected(t *testing.T) {
	_, err := ResolveOpencodePinsWithCache(context.Background(), opencodePinImage, "nothex", "d"+strings.Repeat("d", 63))
	require.Error(t, err, "validation upstream notwithstanding, the short-circuit must not return garbage verbatim")
}

func TestResolveOpencodePinsFromCluster_ConfigErrorSurfaces(t *testing.T) {
	// Ordering proof only (config errors before any namespace use).
	_, err := resolvePinsFromCluster(context.Background(),
		func() (*rest.Config, error) { return nil, errors.New("no kubeconfig") },
		"", fakeIndexFetcher(goodOpencodeAnnotations(), nil), opencodePinSource, opencodePinImage)
	require.ErrorContains(t, err, "kubeconfig")
	require.ErrorContains(t, err, "opencode pins", "the artifact name must appear so operators fix the right pin")
}

// --- config validation ------------------------------------------------------

func TestValidateOpencodeDeliveryConfig(t *testing.T) {
	cases := []struct {
		name    string
		image   string
		amd64   string
		arm64   string
		wantErr string
	}{
		{"all empty (default, inert)", "", "", "", ""},
		{"fully configured", testOpencodeImage, testOpencodeSHAAMD64, testOpencodeSHAARM64, ""},
		{"image only (renovate form)", testOpencodeImage, "", "", ""},
		{"hashes only", "", testOpencodeSHAAMD64, testOpencodeSHAARM64, "image"},
		{"partial hash override", testOpencodeImage, testOpencodeSHAAMD64, "", "BOTH"},
		{"partial hash override (mirrored)", testOpencodeImage, "", testOpencodeSHAARM64, "BOTH"},
		{"short hash", testOpencodeImage, "abc", testOpencodeSHAARM64, "64 hex"},
		{"non-hex hash", testOpencodeImage, strings.Repeat("z", 64), testOpencodeSHAARM64, "64 hex"},
		{"tag not digest", "ghcr.io/x/opencode:v1", testOpencodeSHAAMD64, testOpencodeSHAARM64, "@sha256:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOpencodeDeliveryConfig(tc.image, tc.amd64, tc.arm64)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidateOpencodeDelivery_ErrorMessagesNameOpencode pins the
// guard's error attribution: an operator who misconfigures one artifact
// must not be sent chasing the other.
func TestValidateOpencodeDelivery_ErrorMessagesNameOpencode(t *testing.T) {
	err := validateOpencodeDeliveryConfig("", testOpencodeSHAAMD64, testOpencodeSHAARM64)
	require.ErrorContains(t, err, "opencode delivery")
	require.ErrorContains(t, err, "--opencode-image")
}
