// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Tests for design 0053 §5.4: pi overlay delivery pin resolution.
//
// Identical contract to the agentd/opencode pin resolution, against the
// pi annotation keys and the llmsafespaces-pi-pins ConfigMap:
//
//   - annotation extraction from the index (happy + missing keys)
//   - cache write on success, cache fallback on fetch failure,
//     stale-cache rejection (different digest)
//   - flag override precedence and the ErrPiPinsUnavailable sentinel on
//     every outage dead-end
//   - config validation: an EMPTY image is fatal (no baked fallback
//     ever existed for pi); one-sided pairs, non-hex, and floating tags
//     invalid.

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
	piPinImage = "ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	piPinAMD64 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	piPinARM64 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
)

func goodPiAnnotations() map[string]string {
	return map[string]string{
		piAnnotationKeyAMD64: piPinAMD64,
		piAnnotationKeyARM64: piPinARM64,
		annotationVersion:    "1.5.2",
	}
}

func newPiPinResolver(t *testing.T, objs ...client.Object) *cachedPinResolver {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &cachedPinResolver{Client: c, Namespace: "llmsafespaces", source: piPinSource}
}

func TestPiPinResolver_ExtractsAnnotations(t *testing.T) {
	r := newPiPinResolver(t)
	r.fetch = fakeIndexFetcher(goodPiAnnotations(), nil)
	pins, err := r.Resolve(context.Background(), piPinImage)
	require.NoError(t, err)
	require.Equal(t, piPinAMD64, pins.SHA256AMD64)
	require.Equal(t, piPinARM64, pins.SHA256ARM64)
}

func TestPiPinResolver_MissingAnnotationsFails(t *testing.T) {
	r := newPiPinResolver(t)
	r.fetch = fakeIndexFetcher(map[string]string{}, nil)
	_, err := r.Resolve(context.Background(), piPinImage)
	require.Error(t, err, "an index without pi pin annotations is a broken pipeline — fail closed, never launch unverifiable pods")
	require.Contains(t, err.Error(), "annotation")
}

func TestPiPinResolver_SuccessWritesCache(t *testing.T) {
	r := newPiPinResolver(t)
	r.fetch = fakeIndexFetcher(goodPiAnnotations(), nil)
	_, err := r.Resolve(context.Background(), piPinImage)
	require.NoError(t, err)

	cm := &corev1.ConfigMap{}
	require.NoError(t, r.Get(context.Background(),
		types.NamespacedName{Name: PiPinsConfigMapName, Namespace: "llmsafespaces"}, cm))
	require.Equal(t, piPinImage, cm.Data["image"])
	require.Equal(t, piPinAMD64, cm.Data["sha256-amd64"])
	require.Equal(t, piPinARM64, cm.Data["sha256-arm64"])
}

func TestPiPinResolver_FetchFailureFallsBackToCache(t *testing.T) {
	r := newPiPinResolver(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: PiPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        piPinImage,
			"sha256-amd64": piPinAMD64,
			"sha256-arm64": piPinARM64,
		},
	})
	r.fetch = fakeIndexFetcher(nil, ErrPiPinsUnavailable)
	pins, err := r.Resolve(context.Background(), piPinImage)
	require.NoError(t, err, "a registry outage at controller boot must not brick startup when a cached pin for the SAME digest exists")
	require.Equal(t, piPinAMD64, pins.SHA256AMD64)
}

func TestPiPinResolver_StaleCacheRejected(t *testing.T) {
	r := newPiPinResolver(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: PiPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        "ghcr.io/lenaxia/llmsafespaces/pi:1.5.2@sha256:" + strings.Repeat("1", 64),
			"sha256-amd64": piPinAMD64,
			"sha256-arm64": piPinARM64,
		},
	})
	r.fetch = fakeIndexFetcher(nil, ErrPiPinsUnavailable)
	_, err := r.Resolve(context.Background(), piPinImage)
	require.Error(t, err, "a cached pin for a DIFFERENT digest must never satisfy a new pin — that is the desync this design exists to prevent")
}

// TestPiPinResolver_CachesNeverCrossArtifacts: the opencode cache must
// not satisfy a pi resolution (and vice versa) — per-artifact ConfigMaps
// are the separation contract.
func TestPiPinResolver_CachesNeverCrossArtifacts(t *testing.T) {
	r := newPiPinResolver(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        piPinImage,
			"sha256-amd64": piPinAMD64,
			"sha256-arm64": piPinARM64,
		},
	})
	r.fetch = fakeIndexFetcher(nil, errors.New("connection refused"))
	_, err := r.Resolve(context.Background(), piPinImage)
	require.ErrorIs(t, err, ErrPiPinsUnavailable,
		"an opencode-pins cache entry must never satisfy pi resolution")
}

// TestPiPinResolver_ErrorsCarrySentinel proves the outage paths wrap
// ErrPiPinsUnavailable so main.go's errors.Is hint fires.
func TestPiPinResolver_ErrorsCarrySentinel(t *testing.T) {
	// No cache at all → sentinel.
	r := newPiPinResolver(t)
	r.fetch = fakeIndexFetcher(nil, errors.New("connection refused"))
	_, err := r.Resolve(context.Background(), piPinImage)
	require.ErrorIs(t, err, ErrPiPinsUnavailable, "first-boot outage must carry the sentinel for the manual-pin hint")

	// RBAC-denied cache read → sentinel (the wrap names the ConfigMap).
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	c3 := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "configmaps"}, key.Name, errors.New("denied"))
		},
	}).Build()
	r3 := &cachedPinResolver{Client: c3, Namespace: "llmsafespaces", source: piPinSource,
		fetch: fakeIndexFetcher(nil, errors.New("connection refused"))}
	_, err = r3.Resolve(context.Background(), piPinImage)
	require.ErrorIs(t, err, ErrPiPinsUnavailable, "RBAC-denied cache read must carry the sentinel")
	require.Contains(t, err.Error(), PiPinsConfigMapName, "the RBAC hint must name the ConfigMap")
}

// --- ResolvePiPinsWithCache: the production boot path -----------------------

func TestResolvePiPinsWithCache_FullFlagsShortCircuit(t *testing.T) {
	// Manual pin: no kubeconfig access, no registry access — flags
	// returned verbatim after hex validation.
	pins, err := ResolvePiPinsWithCache(context.Background(), piPinImage,
		"e"+strings.Repeat("e", 63), "f"+strings.Repeat("f", 63))
	require.NoError(t, err)
	require.Equal(t, "e"+strings.Repeat("e", 63), pins.SHA256AMD64)
	require.Equal(t, "f"+strings.Repeat("f", 63), pins.SHA256ARM64)
}

func TestResolvePiPinsWithCache_FullFlagsInvalidHexRejected(t *testing.T) {
	_, err := ResolvePiPinsWithCache(context.Background(), piPinImage, "nothex", "f"+strings.Repeat("f", 63))
	require.Error(t, err, "validation upstream notwithstanding, the short-circuit must not return garbage verbatim")
}

func TestResolvePiPinsFromCluster_ConfigErrorSurfaces(t *testing.T) {
	_, err := resolvePinsFromCluster(context.Background(),
		func() (*rest.Config, error) { return nil, errors.New("no kubeconfig") },
		"", fakeIndexFetcher(goodPiAnnotations(), nil), piPinSource, piPinImage)
	require.ErrorContains(t, err, "kubeconfig")
	require.ErrorContains(t, err, "pi pins", "the artifact name must appear so operators fix the right pin")
}

// --- config validation ------------------------------------------------------

func TestValidatePiDeliveryConfig(t *testing.T) {
	cases := []struct {
		name    string
		image   string
		amd64   string
		arm64   string
		wantErr string
	}{
		// The one DELIBERATE divergence from the opencode table: pi has
		// no baked fallback, so the empty form was never an opt-in.
		{"empty image is fatal (no fallback mode)", "", "", "", "mandatory"},
		{"fully configured", testPiImage, testPiSHAAMD64, testPiSHAARM64, ""},
		{"image only (renovate form)", testPiImage, "", "", ""},
		{"hashes only", "", testPiSHAAMD64, testPiSHAARM64, "mandatory"},
		{"partial hash override", testPiImage, testPiSHAAMD64, "", "BOTH"},
		{"partial hash override (mirrored)", testPiImage, "", testPiSHAARM64, "BOTH"},
		{"short hash", testPiImage, "abc", testPiSHAARM64, "64 hex"},
		{"non-hex hash", testPiImage, strings.Repeat("z", 64), testPiSHAARM64, "64 hex"},
		{"tag not digest", "ghcr.io/x/pi:v1.5.2", testPiSHAAMD64, testPiSHAARM64, "@sha256:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePiDeliveryConfig(tc.image, tc.amd64, tc.arm64)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestValidatePiDelivery_ErrorMessagesNamePi pins the guard's error
// attribution: an operator who misconfigures one artifact must not be
// sent chasing another.
func TestValidatePiDelivery_ErrorMessagesNamePi(t *testing.T) {
	err := validatePiDeliveryConfig("", testPiSHAAMD64, testPiSHAARM64)
	require.ErrorContains(t, err, "pi delivery")
	require.ErrorContains(t, err, "--pi-image")
}
