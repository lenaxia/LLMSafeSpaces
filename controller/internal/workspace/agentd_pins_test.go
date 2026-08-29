// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Tests for the #863 follow-up: single-coordinate agentd pin resolution.
//
// The Renovate-friendly contract: values.yaml pins ONLY
// repo:tag@sha256:<digest>; the per-arch binary sha256s ride on the
// image index as OCI annotations (stamped by merge-agentd, covered by
// the digest). The controller resolves digest → annotations at startup,
// with a ConfigMap cache for registry outages and manual flag overrides
// for break-glass. These tests lock in:
//
//   - annotation extraction from the index (happy + missing keys)
//   - cache write on success, cache fallback on fetch failure,
//     stale-cache rejection (different image)
//   - flag override precedence (explicit flags win over annotations)
//   - config validation: image-only is now VALID (resolution path);
//     hashes-without-image remains invalid; digest-pinned image enforced.

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
	pinImage = "ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:" + "058d0941c6a7decd538fc2465444ca2fa70a5467118320a6e414dfa691e48dfe"
	pinAMD64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinARM64 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func fakeIndexFetcher(ann map[string]string, err error) remoteIndexFetcher {
	return func(_ context.Context, _ overlayPinSource, _ string) (ociAnnotations, error) {
		if err != nil {
			return nil, err
		}
		return ann, nil
	}
}

func goodAnnotations() map[string]string {
	return map[string]string{
		annotationKeyAMD64: pinAMD64,
		annotationKeyARM64: pinARM64,
		annotationVersion:  "0.15.7",
	}
}

func TestCachedPinResolver_ExtractsAnnotations(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &cachedPinResolver{Client: c, Namespace: "llmsafespaces", source: agentdPinSource, fetch: fakeIndexFetcher(goodAnnotations(), nil)}
	pins, err := r.Resolve(context.Background(), pinImage)
	require.NoError(t, err)
	require.Equal(t, pinAMD64, pins.SHA256AMD64)
	require.Equal(t, pinARM64, pins.SHA256ARM64)
}

func TestCachedPinResolver_MissingAnnotationsFails(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &cachedPinResolver{Client: c, Namespace: "llmsafespaces", source: agentdPinSource, fetch: fakeIndexFetcher(map[string]string{}, nil)}
	_, err := r.Resolve(context.Background(), pinImage)
	require.Error(t, err, "an index without pin annotations is a broken pipeline — fail closed, never launch unverifiable pods")
	require.Contains(t, err.Error(), "annotation")
}

func TestCachedPinResolver_SuccessWritesCache(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	res := &cachedPinResolver{
		source:    agentdPinSource,
		Client:    c,
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(goodAnnotations(), nil),
	}
	pins, err := res.Resolve(context.Background(), pinImage)
	require.NoError(t, err)
	require.Equal(t, pinAMD64, pins.SHA256AMD64)

	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"}, cm))
	require.Equal(t, pinImage, cm.Data["image"])
	require.Equal(t, pinAMD64, cm.Data["sha256-amd64"])
	require.Equal(t, pinARM64, cm.Data["sha256-arm64"])
}

func TestCachedPinResolver_FetchFailureFallsBackToCache(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        pinImage,
			"sha256-amd64": pinAMD64,
			"sha256-arm64": pinARM64,
		},
	}).Build()
	res := &cachedPinResolver{
		source:    agentdPinSource,
		Client:    c,
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(nil, errFetchUnavailable),
	}
	pins, err := res.Resolve(context.Background(), pinImage)
	require.NoError(t, err, "a registry outage at controller boot must not brick startup when a cached pin for the SAME digest exists")
	require.Equal(t, pinAMD64, pins.SHA256AMD64)
}

func TestCachedPinResolver_StaleCacheRejected(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        "ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111",
			"sha256-amd64": pinAMD64,
			"sha256-arm64": pinARM64,
		},
	}).Build()
	res := &cachedPinResolver{
		source:    agentdPinSource,
		Client:    c,
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(nil, errFetchUnavailable),
	}
	_, err := res.Resolve(context.Background(), pinImage)
	require.Error(t, err, "a cached pin for a DIFFERENT digest must never satisfy a new pin — that is the desync this design exists to prevent")
}

// TestCachedPinResolver_ErrorsCarrySentinel proves the outage paths
// wrap ErrAgentdPinsUnavailable so main.go's errors.Is hint fires.
func TestCachedPinResolver_ErrorsCarrySentinel(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	// No cache at all → sentinel.
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	res := &cachedPinResolver{Client: c, Namespace: "llmsafespaces", source: agentdPinSource, fetch: fakeIndexFetcher(nil, errors.New("connection refused"))}
	_, err := res.Resolve(context.Background(), pinImage)
	require.ErrorIs(t, err, ErrAgentdPinsUnavailable, "first-boot outage must carry the sentinel for the manual-pin hint")

	// Malformed cache → sentinel.
	c3 := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        pinImage,
			"sha256-amd64": "garbage",
			"sha256-arm64": pinARM64,
		},
	}).Build()
	res3 := &cachedPinResolver{source: agentdPinSource, Client: c3, Namespace: "llmsafespaces", fetch: fakeIndexFetcher(nil, errors.New("connection refused"))}
	_, err = res3.Resolve(context.Background(), pinImage)
	require.ErrorIs(t, err, ErrAgentdPinsUnavailable, "malformed-cache refusal must carry the sentinel")

	// RBAC-denied cache read → sentinel (the wrap names the ConfigMap).
	c4 := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "configmaps"}, key.Name, errors.New("denied"))
		},
	}).Build()
	res4 := &cachedPinResolver{source: agentdPinSource, Client: c4, Namespace: "llmsafespaces", fetch: fakeIndexFetcher(nil, errors.New("connection refused"))}
	_, err = res4.Resolve(context.Background(), pinImage)
	require.ErrorIs(t, err, ErrAgentdPinsUnavailable, "RBAC-denied cache read must carry the sentinel")
	require.Contains(t, err.Error(), AgentdPinsConfigMapName, "the RBAC hint must name the ConfigMap")

	// Stale-digest cache → sentinel (desync refusal is also an outage dead-end).
	c2 := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        "ghcr.io/x/a:dev@sha256:" + strings.Repeat("9", 64),
			"sha256-amd64": pinAMD64,
			"sha256-arm64": pinARM64,
		},
	}).Build()
	res2 := &cachedPinResolver{source: agentdPinSource, Client: c2, Namespace: "llmsafespaces", fetch: fakeIndexFetcher(nil, errors.New("connection refused"))}
	_, err = res2.Resolve(context.Background(), pinImage)
	require.ErrorIs(t, err, ErrAgentdPinsUnavailable, "stale-cache refusal must carry the sentinel")
}

func TestCachedPinResolver_MalformedCacheRejected(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        pinImage,
			"sha256-amd64": "not-hex",
			"sha256-arm64": pinARM64,
		},
	}).Build()
	res := &cachedPinResolver{Client: c, Namespace: "llmsafespaces", source: agentdPinSource, fetch: fakeIndexFetcher(nil, errFetchUnavailable)}
	_, err := res.Resolve(context.Background(), pinImage)
	require.Error(t, err, "a malformed cache entry must not satisfy the pin")
	require.Contains(t, err.Error(), "malformed")
}

func TestValidateAgentdDeliveryConfig_ImageOnlyIsValid(t *testing.T) {
	// Renovate/Dependabot update ONE line: the tag+digest. Hashes come
	// from annotations at startup. Image-only must therefore be valid.
	require.NoError(t, validateAgentdDeliveryConfig(pinImage, "", ""))
	// Hashes without image still invalid (they are overrides).
	require.Error(t, validateAgentdDeliveryConfig("", pinAMD64, ""))
	// Full manual pin still valid.
	require.NoError(t, validateAgentdDeliveryConfig(pinImage, pinAMD64, pinARM64))
	// Partial manual pin invalid (all-or-nothing once any hash is given).
	require.Error(t, validateAgentdDeliveryConfig(pinImage, pinAMD64, ""))
}

// --- ResolvePinsWithCache: the production boot path -----------------------

func TestResolvePinsWithCache_FullFlagsShortCircuit(t *testing.T) {
	// Manual pin: no kubeconfig access, no registry access — flags
	// returned verbatim after hex validation.
	pins, err := ResolvePinsWithCache(context.Background(), pinImage,
		"c"+strings.Repeat("c", 63), "d"+strings.Repeat("d", 63))
	require.NoError(t, err)
	require.Equal(t, "c"+strings.Repeat("c", 63), pins.SHA256AMD64)
	require.Equal(t, "d"+strings.Repeat("d", 63), pins.SHA256ARM64)
}

func TestResolvePinsWithCache_FullFlagsInvalidHexRejected(t *testing.T) {
	_, err := ResolvePinsWithCache(context.Background(), pinImage, "nothex", "d"+strings.Repeat("d", 63))
	require.Error(t, err, "validation upstream notwithstanding, the short-circuit must not return garbage verbatim")
}

// The image-only CLUSTER path (ResolvePinsWithCache → resolver → cache
// write → ns fallback) is covered by agentd_pins_envtest_test.go — a
// prior fake-client test here duplicated ExtractsAnnotations without
// exercising the entrypoint (mutation-proven in review round 3).

func TestResolvePinsFromCluster_ConfigErrorSurfaces(t *testing.T) {
	// Ordering proof only (config errors before any namespace use).
	// The namespace fallback itself is envtest-covered
	// (agentd_pins_envtest_test.go) — a unit test here cannot prove it.
	_, err := resolvePinsFromCluster(context.Background(),
		func() (*rest.Config, error) { return nil, errors.New("no kubeconfig") },
		"", fakeIndexFetcher(goodAnnotations(), nil), agentdPinSource, pinImage)
	require.ErrorContains(t, err, "kubeconfig")
}

// --- sameDigest: digest identity, not ref identity ------------------------

func TestSameDigest(t *testing.T) {
	d1 := "sha256:" + strings.Repeat("1", 64)
	d2 := "sha256:" + strings.Repeat("2", 64)
	require.True(t, sameDigest("ghcr.io/x/a:dev@"+d1, "ghcr.io/x/a:v1.2.0@"+d1),
		"re-tagging the same digest is the same content")
	require.False(t, sameDigest("ghcr.io/x/a:dev@"+d1, "ghcr.io/x/a:dev@"+d2))
	require.False(t, sameDigest("ghcr.io/x/a:dev@"+d1, "ghcr.io/x/a:dev"),
		"a ref without a digest never matches")
	require.False(t, sameDigest("ghcr.io/x/a:dev", "ghcr.io/x/a:dev"))
}

// TestCachedPinResolver_RetaggedSameDigestServesDuringOutage is the
// motivating scenario for digest-keyed caching: the cache was written
// for :dev@X, the live ref is :v1.2.0@X (same digest, new tag — the
// post-Renovate-bump shape), the registry is unreachable. The cache
// must serve. Mutation-proof: reverting sameDigest to full-ref equality
// makes this fail.
func TestCachedPinResolver_RetaggedSameDigestServesDuringOutage(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        "ghcr.io/x/agentd:dev@sha256:" + strings.Repeat("1", 64),
			"sha256-amd64": pinAMD64,
			"sha256-arm64": pinARM64,
		},
	}).Build()
	res := &cachedPinResolver{
		source:    agentdPinSource,
		Client:    c,
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(nil, errFetchUnavailable),
	}
	pins, err := res.Resolve(context.Background(), "ghcr.io/x/agentd:v1.2.0@sha256:"+strings.Repeat("1", 64))
	require.NoError(t, err, "same digest under a new tag must serve from cache during an outage")
	require.Equal(t, pinAMD64, pins.SHA256AMD64)
}
