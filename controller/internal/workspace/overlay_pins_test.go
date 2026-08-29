// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Tests for the generalized overlay pin resolver (design 0053 §4.2).
//
// The #863 single-coordinate resolution (digest-pinned image + per-arch
// binary sha256s as OCI index annotations + ConfigMap outage cache) grew
// a second consumer — opencode — which is the README Rule 12 trigger to
// pay for the parameterized abstraction. These tests lock in that the
// two instances are fully isolated:
//
//   - each source resolves against its OWN annotation keys
//   - each source writes/reads its OWN ConfigMap cache
//   - one artifact's cache can never satisfy the other's resolution
//   - error strings name the artifact they failed on
//

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newPinTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// TestOverlayPinResolver_SourcesUseDistinctConfigMaps proves the two
// instances never share cache state: resolving via the agentd source
// writes only llmsafespaces-agentd-pins, resolving via the opencode
// source writes only llmsafespaces-opencode-pins.
func TestOverlayPinResolver_SourcesUseDistinctConfigMaps(t *testing.T) {
	agentdClient := newPinTestClient(t)
	resAgentd := &cachedPinResolver{
		Client:    agentdClient,
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(goodAnnotations(), nil),
		source:    agentdPinSource,
	}
	pins, err := resAgentd.Resolve(context.Background(), pinImage)
	require.NoError(t, err)
	require.Equal(t, pinAMD64, pins.SHA256AMD64)

	opencodeClient := newPinTestClient(t)
	resOpencode := &cachedPinResolver{
		Client:    opencodeClient,
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(goodOpencodeAnnotations(), nil),
		source:    opencodePinSource,
	}
	pins, err = resOpencode.Resolve(context.Background(), opencodePinImage)
	require.NoError(t, err)
	require.Equal(t, opencodePinAMD64, pins.SHA256AMD64)

	cm := &corev1.ConfigMap{}
	require.NoError(t, agentdClient.Get(context.Background(),
		types.NamespacedName{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"}, cm))
	require.Equal(t, pinAMD64, cm.Data["sha256-amd64"])

	require.NoError(t, opencodeClient.Get(context.Background(),
		types.NamespacedName{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"}, cm))
	require.Equal(t, opencodePinAMD64, cm.Data["sha256-amd64"])

	require.Error(t, agentdClient.Get(context.Background(),
		types.NamespacedName{Name: OpencodePinsConfigMapName, Namespace: "llmsafespaces"}, cm),
		"the agentd source must never write the opencode cache")
	require.Error(t, opencodeClient.Get(context.Background(),
		types.NamespacedName{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"}, cm),
		"the opencode source must never write the agentd cache")
}

// TestOverlayPinResolver_AnnotationKeysArePerSource: an index carrying
// only agentd keys must not satisfy the opencode source and vice versa —
// the annotation namespaces are the desync guard between artifacts.
func TestOverlayPinResolver_AnnotationKeysArePerSource(t *testing.T) {
	agentdKeysOnly := &cachedPinResolver{
		Client:    newPinTestClient(t),
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(goodAnnotations(), nil),
		source:    opencodePinSource,
	}
	_, err := agentdKeysOnly.Resolve(context.Background(), opencodePinImage)
	require.Error(t, err, "an index stamped with only agentd annotations must not satisfy opencode pin resolution")

	opencodeKeysOnly := &cachedPinResolver{
		Client:    newPinTestClient(t),
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(goodOpencodeAnnotations(), nil),
		source:    agentdPinSource,
	}
	_, err = opencodeKeysOnly.Resolve(context.Background(), pinImage)
	require.Error(t, err, "an index stamped with only opencode annotations must not satisfy agentd pin resolution")
}

// TestOverlayPinResolver_CachesDoNotCrossSatisfy: during an opencode
// registry outage, a valid AGENTD cache for the same digest must not
// satisfy opencode resolution — each artifact fails closed on its own
// cache only.
func TestOverlayPinResolver_CachesDoNotCrossSatisfy(t *testing.T) {
	agentdCache := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: AgentdPinsConfigMapName, Namespace: "llmsafespaces"},
		Data: map[string]string{
			"image":        pinImage,
			"sha256-amd64": pinAMD64,
			"sha256-arm64": pinARM64,
		},
	}
	res := &cachedPinResolver{
		Client:    newPinTestClient(t, agentdCache),
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(nil, errFetchUnavailable),
		source:    opencodePinSource,
	}
	_, err := res.Resolve(context.Background(), pinImage)
	require.ErrorIs(t, err, ErrOpencodePinsUnavailable,
		"the agentd pins cache must never satisfy an opencode resolution — the annotation namespaces and ConfigMaps are per-artifact")
}

// TestOverlayPinResolver_ErrorsNameTheArtifact: outage/annotation
// failures must name the artifact in their error text so operator
// triage points at the right pin.
func TestOverlayPinResolver_ErrorsNameTheArtifact(t *testing.T) {
	agentdRes := &cachedPinResolver{
		Client:    newPinTestClient(t),
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(map[string]string{}, nil),
		source:    agentdPinSource,
	}
	_, err := agentdRes.Resolve(context.Background(), pinImage)
	require.ErrorContains(t, err, "agentd pins")

	opencodeRes := &cachedPinResolver{
		Client:    newPinTestClient(t),
		Namespace: "llmsafespaces",
		fetch:     fakeIndexFetcher(map[string]string{}, nil),
		source:    opencodePinSource,
	}
	_, err = opencodeRes.Resolve(context.Background(), opencodePinImage)
	require.ErrorContains(t, err, "opencode pins")
}
