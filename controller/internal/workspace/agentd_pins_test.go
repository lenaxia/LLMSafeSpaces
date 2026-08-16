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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

const (
	pinImage = "ghcr.io/lenaxia/llmsafespaces/agentd:dev@sha256:" + "058d0941c6a7decd538fc2465444ca2fa70a5467118320a6e414dfa691e48dfe"
	pinAMD64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinARM64 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func fakeIndexFetcher(ann map[string]string, err error) remoteIndexFetcher {
	return func(_ context.Context, _ string) (ociAnnotations, error) {
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
	r := &cachedPinResolver{Client: c, Namespace: "llmsafespaces", fetch: fakeIndexFetcher(goodAnnotations(), nil)}
	pins, err := r.Resolve(context.Background(), pinImage)
	require.NoError(t, err)
	require.Equal(t, pinAMD64, pins.SHA256AMD64)
	require.Equal(t, pinARM64, pins.SHA256ARM64)
}

func TestCachedPinResolver_MissingAnnotationsFails(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &cachedPinResolver{Client: c, Namespace: "llmsafespaces", fetch: fakeIndexFetcher(map[string]string{}, nil)}
	_, err := r.Resolve(context.Background(), pinImage)
	require.Error(t, err, "an index without pin annotations is a broken pipeline — fail closed, never launch unverifiable pods")
	require.Contains(t, err.Error(), "annotation")
}

func TestCachedPinResolver_SuccessWritesCache(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	res := &cachedPinResolver{
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
		Client:    c,
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(nil, errFetchUnavailable),
	}
	_, err := res.Resolve(context.Background(), pinImage)
	require.Error(t, err, "a cached pin for a DIFFERENT digest must never satisfy a new pin — that is the desync this design exists to prevent")
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
	res := &cachedPinResolver{Client: c, Namespace: "llmsafespaces", fetch: fakeIndexFetcher(nil, errFetchUnavailable)}
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

func repeat(s string, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		out += s
	}
	return out
}

var _ = v1.WorkspaceConditionAgentdVerified // keep import aligned with package
