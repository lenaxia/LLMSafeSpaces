// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// runtime_reader_test.go — #1587 pins: the cluster-scoped lookups on the
// reconcile path (RuntimeEnvironment resolution in buildPod, StorageClass
// detection in pvcUsesWaitForFirstConsumer) must be served by the DIRECT
// reader, never the cached client.
//
// The defect (#1551 defect 1): a cached Get of an unwatched
// cluster-scoped type lazily starts an informer; when RBAC withholds the
// list/watch the reflector 403-loops forever, the informer never syncs,
// and cache.Get blocks unconditionally — one reconcile wedges a worker
// silently. These pins simulate the never-syncing cache with a Reader
// whose cluster-scoped Get/List never return and assert the reconcile
// path completes anyway (served by the direct reader) inside a bounded
// window — the wedge is red-by-timeout, never a hung suite.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// neverSyncWindow bounds every wedge assertion: a healthy direct-reader
// build completes in microseconds; the window only ever fires when a
// read is stuck on a never-syncing informer.
const neverSyncWindow = 5 * time.Second

// errNeverSyncedCacheSim marks a test invariant break: the blocked read
// was released by cleanup and delegated to the wrapped client — the
// wedge simulation leaked past its window.
var errNeverSyncedCacheSim = errors.New("never-syncing cache simulation: blocked read should not have completed")

// neverSyncingCacheClient wraps a client.Client and BLOCKS FOREVER on
// Get/List of the types its predicate selects — the executable form of
// an informer that never syncs (controller-runtime's cache.Get waits
// unconditionally on sync; there is no deadline and no error). Every
// other call delegates to the wrapped client, exactly like a cache
// whose namespaced informers sync fine while one cluster-scoped
// reflector 403-loops (#1551's signature).
type neverSyncingCacheClient struct {
	client.Client
	block   func(obj runtime.Object) bool
	release chan struct{}
}

func newNeverSyncingCacheClient(t *testing.T, wrapped client.Client, block func(runtime.Object) bool) *neverSyncingCacheClient {
	t.Helper()
	c := &neverSyncingCacheClient{Client: wrapped, block: block, release: make(chan struct{})}
	t.Cleanup(func() { close(c.release) })
	return c
}

func (c *neverSyncingCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if c.block(obj) {
		return c.hang(ctx)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *neverSyncingCacheClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if c.block(list) {
		return c.hang(ctx)
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *neverSyncingCacheClient) hang(ctx context.Context) error {
	select {
	case <-c.release:
		return errNeverSyncedCacheSim
	case <-ctx.Done():
		return ctx.Err()
	}
}

func blockRuntimeEnvironments(obj runtime.Object) bool {
	switch obj.(type) {
	case *v1.RuntimeEnvironment, *v1.RuntimeEnvironmentList:
		return true
	}
	return false
}

func blockStorageClasses(obj runtime.Object) bool {
	_, ok := obj.(*storagev1.StorageClass)
	return ok
}

// failingReader serves nothing: every Get/List returns the injected
// error — the direct-reader shape of an RBAC 403 or apiserver failure.
type failingReader struct {
	err error
}

func (f failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return f.err
}

func (f failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return f.err
}

// pinnedReconcilerFor mirrors reconcilerFor's mandatory overlay pins
// (design 0053 §4.5) but lets the test choose BOTH readers separately:
// cached (the embedded Client) and direct (APIReader).
func pinnedReconcilerFor(t *testing.T, cached client.Client, apiReader client.Reader) *WorkspaceReconciler {
	t.Helper()
	r := &WorkspaceReconciler{Client: cached, Scheme: testScheme(t), APIReader: apiReader}
	r.AgentdImage = testAgentdImage
	r.AgentdBinarySHA256AMD64 = testAgentdSHAAMD64
	r.AgentdBinarySHA256ARM64 = testAgentdSHAARM64
	r.OpencodeImage = testOpencodeImage
	r.OpencodeBinarySHA256AMD64 = testOpencodeSHAAMD64
	r.OpencodeBinarySHA256ARM64 = testOpencodeSHAARM64
	return r
}

// buildPodWithin runs buildPod in a goroutine and fails the test unless
// it returns inside neverSyncWindow — the executable form of #1587's
// "a stuck read must surface as a loud timeout, not a silent wedge".
func buildPodWithin(t *testing.T, r *WorkspaceReconciler, ws *v1.Workspace) *corev1.Pod {
	t.Helper()
	type buildResult struct {
		pod *corev1.Pod
		err error
	}
	done := make(chan buildResult, 1)
	go func() {
		pod, err := r.buildPod(context.Background(), ws)
		done <- buildResult{pod, err}
	}()
	select {
	case res := <-done:
		require.NoError(t, res.err)
		return res.pod
	case <-time.After(neverSyncWindow):
		t.Fatal("buildPod wedged: the runtime-image resolution read never returned — " +
			"a never-syncing cached informer is blocking the reconcile path (#1587)")
		return nil
	}
}

// TestBuildPod_NeverSyncingRuntimeEnvCacheDoesNotWedge — THE #1587 pin.
// A cache whose RuntimeEnvironment informer never syncs must not stall
// pod construction: resolution is served by the direct reader. The
// runtime "python:3.11" exercises every read leg (exact Get, colon-munged
// Get, language:version List) on the direct path. Pre-fix (resolution
// via r.Client) this test is red-by-timeout — the #1551 wedge, bounded.
func TestBuildPod_NeverSyncingRuntimeEnvCacheDoesNotWedge(t *testing.T) {
	scheme := testScheme(t)
	rte := makeRTE("python-3-11", "registry.example.com/runtimes/python:3.11", "python", "3.11")

	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	direct := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(rte).Build()
	r := pinnedReconcilerFor(t, newNeverSyncingCacheClient(t, cached, blockRuntimeEnvironments), direct)

	ws := newWorkspaceForPodBuilder(t)
	ws.Spec.Runtime = "python:3.11"

	pod := buildPodWithin(t, r, ws)
	require.NotNil(t, pod)
	assert.Equal(t, "registry.example.com/runtimes/python:3.11", pod.Spec.Containers[0].Image,
		"the RTE image must be resolved through the direct reader")
	assert.Equal(t, "python-3-11", pod.Annotations["llmsafespaces.dev/runtime-env"])
}

// TestBuildPod_NilAPIReaderNamedRuntimeFailsLoud — no silent cached
// fallback: with APIReader unwired and a named runtime to resolve,
// buildPod must fail with the wiring fix named, EVEN THOUGH the cached
// client could serve the object. A fallback to r.Client would reintroduce
// #1551's wedge the moment the informer is the broken thing.
func TestBuildPod_NilAPIReaderNamedRuntimeFailsLoud(t *testing.T) {
	scheme := testScheme(t)
	cachedWithRTE := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(makeRTE("python", "registry.example.com/runtimes/python:1", "python", "1")).
		Build()
	r := pinnedReconcilerFor(t, cachedWithRTE, nil)

	ws := newWorkspaceForPodBuilder(t)
	ws.Spec.Runtime = "python"

	pod, err := r.buildPod(context.Background(), ws)
	require.Error(t, err, "an unwired direct reader must fail loud, not fall back to the cached client")
	assert.Nil(t, pod)
	assert.Contains(t, err.Error(), "mgr.GetAPIReader()", "the error must name the wiring fix")
	assert.Contains(t, err.Error(), "RuntimeEnvironment", "the error must name what was being resolved")
}

// TestBuildPod_NilAPIReaderExplicitImageStillBuilds — the refusal is
// lazy by design: an explicit image reference ("/") never performs a
// cluster-scoped lookup, so an unwired direct reader must not block
// explicit-image workspaces from building.
func TestBuildPod_NilAPIReaderExplicitImageStillBuilds(t *testing.T) {
	r := pinnedReconcilerFor(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), nil)

	ws := newWorkspaceForPodBuilder(t)
	assert.Contains(t, ws.Spec.Runtime, "/", "fixture must use an explicit image reference")

	pod := buildPodWithin(t, r, ws)
	require.NotNil(t, pod)
}

// TestBuildPod_DirectReaderForbiddenPropagatesLoud — an RBAC gap on the
// direct path (403) must surface as a wrapped, retryable error through
// the existing recovery/requeue machinery — never swallowed into the
// "no RuntimeEnvironment found" not-found shape.
func TestBuildPod_DirectReaderForbiddenPropagatesLoud(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: v1.GroupName, Resource: "runtimeenvironments"}, "python",
		fmt.Errorf("User cannot get resource in API group at cluster scope"))
	r := pinnedReconcilerFor(t,
		fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		failingReader{err: forbidden})

	ws := newWorkspaceForPodBuilder(t)
	ws.Spec.Runtime = "python"

	pod, err := r.buildPod(context.Background(), ws)
	require.Error(t, err)
	assert.Nil(t, pod)
	assert.Contains(t, err.Error(), "looking up RuntimeEnvironment",
		"the direct-reader failure must propagate wrapped, not be swallowed")
	assert.NotContains(t, err.Error(), "no RuntimeEnvironment found matching",
		"a 403 is NOT a not-found: conflating them hides the RBAC gap")
	assert.True(t, apierrors.IsForbidden(err),
		"the apierrors class must survive the wrap chain (requeue/classification consumers)")
}

// --- StorageClass leg (pvcUsesWaitForFirstConsumer) — same defect
// class, same treatment (#1587: "RuntimeEnvironment and others"). ---

func wffcStorageClass(name string) *storagev1.StorageClass {
	mode := storagev1.VolumeBindingWaitForFirstConsumer
	return &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: name},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: &mode,
	}
}

func pvcWithStorageClass(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &name},
	}
}

// TestPVCWFFC_NeverSyncingStorageClassCacheDoesNotWedge — the Pending
// reconcile reads the cluster-scoped StorageClass to detect
// WaitForFirstConsumer binding; a never-syncing cached informer there
// wedges the worker exactly like the RuntimeEnvironment leg. The direct
// reader must answer it.
func TestPVCWFFC_NeverSyncingStorageClassCacheDoesNotWedge(t *testing.T) {
	scheme := testScheme(t)
	cached := fake.NewClientBuilder().WithScheme(scheme).Build()
	direct := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(wffcStorageClass("wffc-sc")).Build()
	r := pinnedReconcilerFor(t, newNeverSyncingCacheClient(t, cached, blockStorageClasses), direct)

	done := make(chan bool, 1)
	go func() {
		done <- r.pvcUsesWaitForFirstConsumer(context.Background(), pvcWithStorageClass("wffc-sc"))
	}()
	select {
	case got := <-done:
		assert.True(t, got, "WFFC must be detected through the direct reader")
	case <-time.After(neverSyncWindow):
		t.Fatal("pvcUsesWaitForFirstConsumer wedged: the StorageClass read never returned — " +
			"a never-syncing cached informer is blocking the Pending reconcile (#1587)")
	}
}

// TestPVCWFFC_DirectReaderFailureFailsOpen — the pre-existing semantics
// are fail-open (any read error → not-WFFC → the PVC binds or
// pending-timeout surfaces it); the reader swap must preserve exactly
// that on the direct path. Pinned explicitly: the error path returns
// false — not a hang, not a panic.
func TestPVCWFFC_DirectReaderFailureFailsOpen(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Group: "storage.k8s.io", Resource: "storageclasses"}, "wffc-sc",
		fmt.Errorf("User cannot get resource in API group at cluster scope"))
	r := pinnedReconcilerFor(t,
		fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
		failingReader{err: forbidden})

	done := make(chan bool, 1)
	go func() {
		done <- r.pvcUsesWaitForFirstConsumer(context.Background(), pvcWithStorageClass("wffc-sc"))
	}()
	select {
	case got := <-done:
		assert.False(t, got, "a direct-reader failure must fail open (not-WFFC), preserving the cached-path semantics")
	case <-time.After(neverSyncWindow):
		t.Fatal("pvcUsesWaitForFirstConsumer wedged on a failing (not blocking) direct reader")
	}
}

// TestPVCWFFC_UnwiredReaderFailsOpen — nil APIReader must degrade to
// the same fail-open false, never a nil-interface panic on the Pending
// path (direct constructions that never reach buildPod still hit this).
func TestPVCWFFC_UnwiredReaderFailsOpen(t *testing.T) {
	r := pinnedReconcilerFor(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), nil)
	assert.False(t, r.pvcUsesWaitForFirstConsumer(context.Background(), pvcWithStorageClass("wffc-sc")))
}

// TestSetupWithManager_RefusesNilAPIReader — boot-loud wiring guard:
// the manager path refuses to register a reconciler whose direct reader
// is unwired (the refusal fires before any manager call, so a nil
// manager is safe to pass — a builder call reaching a nil manager
// panics, which is this pin's own fail-loud).
func TestSetupWithManager_RefusesNilAPIReader(t *testing.T) {
	r := pinnedReconcilerFor(t, fake.NewClientBuilder().WithScheme(testScheme(t)).Build(), nil)
	err := r.SetupWithManager(nil)
	require.Error(t, err, "SetupWithManager must refuse an unwired direct reader at boot")
	assert.Contains(t, err.Error(), "mgr.GetAPIReader()", "the refusal must name the wiring fix")
}
