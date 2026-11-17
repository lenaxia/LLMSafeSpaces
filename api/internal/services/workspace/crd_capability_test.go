// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// crd_capability_test.go — the #2092 ruling-4 pins: the create-time
// capability probe's cache discipline and the fail-loud gate, plus the
// ruling-3 condition upsert that carries the degraded verdict onto the
// workspace object.
//
// BORN-RED: on a skewed verdict the harness-less create-refusal pin and
// the explicit-harness-still-works pin fail on pre-change main (the
// gate did not exist).

// --- the cache ------------------------------------------------------------

func TestCapabilityCache_ProbesOncePerTTLAndInvalidates(t *testing.T) {
	calls := 0
	c := newCapabilityCache(func(_ context.Context) crdCapability {
		calls++
		return crdCapabilitySupported
	})
	ctx := context.Background()

	assert.Equal(t, crdCapabilitySupported, c.get(ctx), "first get probes")
	assert.Equal(t, crdCapabilitySupported, c.get(ctx), "second get serves the cache")
	assert.Equal(t, 1, calls, "one probe per TTL window, not per request")

	c.invalidate()
	assert.Equal(t, crdCapabilitySupported, c.get(ctx), "post-invalidate get re-probes")
	assert.Equal(t, 2, calls, "invalidation drops the cached verdict")
}

func TestCapabilityCache_NilCacheIsUnknownAndNeverGates(t *testing.T) {
	var c *capabilityCache
	assert.Equal(t, crdCapabilityUnknown, c.get(context.Background()))
	c.invalidate() // must not panic
}

// --- the create-time gate --------------------------------------------------

func stubCapability(svc *Service, v crdCapability) *capabilityCache {
	c := newCapabilityCache(func(_ context.Context) crdCapability { return v })
	svc.harnessCapability = c
	return c
}

// THE born-red pin (ruling 4): on a skewed cluster, a harness-less
// create FAILS LOUD with the named error and the operator remediation —
// never a workspace that can never bind. The CRD-create mock has NO
// expectations: reaching it fails the test (the gate must front every
// write).
func TestCreateWorkspace_SkewedCRD_RefusesHarnesslessCreate(t *testing.T) {
	f := newFixture(t)
	stubCapability(f.svc, crdCapabilitySkewed)

	before := testutil.ToFloat64(harnessBindNotPersistedTotal.WithLabelValues(bindNotPersistedSurfaceCreateProbe))
	_, err := f.svc.CreateWorkspace(context.Background(), "user1", types.CreateWorkspaceRequest{
		Name: "ws", Runtime: "python:3.10", StorageSize: "10Gi",
	})
	apiErr := harnessAPIError(t, err)
	assert.Equal(t, 503, apiErr.StatusCode(),
		"503 service_unavailable (review r1): a named, temporary, server-side condition — never an unexpected 500")
	assert.Equal(t, "crd_missing_harness_capability", apiErr.Code)
	assert.Equal(t, "the cluster's Workspace CRD predates bind-once: pass an explicit harness or upgrade the CRD",
		apiErr.Message, "the message names both exits")
	require.NotNil(t, apiErr.Err)
	assert.Contains(t, apiErr.Err.Error(), "pass an explicit harness or upgrade the CRD",
		"the refusal names both exits (explicit harness / CRD apply)")
	assert.Contains(t, apiErr.Err.Error(), "never bind",
		"the refusal states WHY: the born-unbound workspace can never bind on this cluster")
	assert.Equal(t, before+1, testutil.ToFloat64(harnessBindNotPersistedTotal.WithLabelValues(bindNotPersistedSurfaceCreateProbe)),
		"the refusal ticks the create_probe surface of the counter")
	f.ws.AssertNotCalled(t, "Create", "the gate must front the CRD write")
}

// The legacy exit stays open (ruling 6 pin): an EXPLICIT harness on the
// same skewed cluster still creates — bound-at-create degrades to the
// legacy path if the write is pruned, which the controller's backfill
// handles; it never wedges.
func TestCreateWorkspace_SkewedCRD_ExplicitHarnessStillSucceeds(t *testing.T) {
	f := newFixture(t)
	stubCapability(f.svc, crdCapabilitySkewed)
	// "opencode" is union-enforced (always enabled) — no settings wiring
	// needed for the explicit-harness exit.

	f.ws.On("Create", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(
		crdWorkspace("ws-1", "default", "user1", "10Gi"), nil)
	f.db.On("CreateWorkspace", mock.Anything, mock.MatchedBy(func(m *types.WorkspaceMetadata) bool {
		return m.UserID == "user1"
	})).Return(nil)

	result, err := f.svc.CreateWorkspace(context.Background(), "user1", types.CreateWorkspaceRequest{
		Name: "my-workspace", Runtime: "python:3.10", StorageSize: "10Gi", Harness: "opencode",
	})
	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// Inconclusive NEVER gates (#1685 posture): an unreachable/inconclusive
// probe leaves the harness-less create path exactly as before — the
// bind-verify remains the runtime authority.
func TestCreateWorkspace_UnknownVerdict_NeverGates(t *testing.T) {
	f := newFixture(t)
	stubCapability(f.svc, crdCapabilityUnknown)

	f.ws.On("Create", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(
		crdWorkspace("ws-1", "default", "user1", "10Gi"), nil)
	f.db.On("CreateWorkspace", mock.Anything, mock.Anything).Return(nil)

	result, err := f.svc.CreateWorkspace(context.Background(), "user1", types.CreateWorkspaceRequest{
		Name: "my-workspace", Runtime: "python:3.10", StorageSize: "10Gi",
	})
	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// A SUPPORTED verdict is the healthy cluster: harness-less creates
// proceed (the picker path) — the gate is invisible when nothing is
// wrong.
func TestCreateWorkspace_SupportedVerdict_Proceeds(t *testing.T) {
	f := newFixture(t)
	stubCapability(f.svc, crdCapabilitySupported)

	f.ws.On("Create", mock.Anything, mock.AnythingOfType("*v1.Workspace")).Return(
		crdWorkspace("ws-1", "default", "user1", "10Gi"), nil)
	f.db.On("CreateWorkspace", mock.Anything, mock.Anything).Return(nil)

	result, err := f.svc.CreateWorkspace(context.Background(), "user1", types.CreateWorkspaceRequest{
		Name: "my-workspace", Runtime: "python:3.10", StorageSize: "10Gi",
	})
	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// --- the degraded condition (ruling 3) -------------------------------------

func TestUpsertBindNotPersistedCondition(t *testing.T) {
	t.Run("appends on a condition-less object", func(t *testing.T) {
		ws := bornUnboundCRD()
		require.True(t, upsertBindNotPersistedCondition(ws))
		require.Len(t, ws.Status.Conditions, 1)
		c := ws.Status.Conditions[0]
		assert.Equal(t, v1.WorkspaceConditionHarnessBindingPending, c.Type)
		assert.Equal(t, "True", string(c.Status))
		assert.Equal(t, v1.WorkspaceReasonBindNotPersisted, c.Reason)
		assert.Contains(t, c.Message, "operator action required")
	})

	t.Run("idempotent when already standing", func(t *testing.T) {
		ws := bornUnboundCRD()
		require.True(t, upsertBindNotPersistedCondition(ws))
		assert.False(t, upsertBindNotPersistedCondition(ws),
			"a standing verdict is not rewritten (no churn, no transition-time bumps)")
	})

	t.Run("overwrites the parked HarnessUnbound verdict", func(t *testing.T) {
		ws := bornUnboundCRD()
		ws.Status.Conditions = []v1.WorkspaceCondition{{
			Type: v1.WorkspaceConditionHarnessBindingPending, Status: "True", Reason: "HarnessUnbound",
		}}
		require.True(t, upsertBindNotPersistedCondition(ws))
		assert.Equal(t, v1.WorkspaceReasonBindNotPersisted, ws.Status.Conditions[0].Reason)
	})
}

func TestHarnessBindBlocked_Helper(t *testing.T) {
	blocked := bornUnboundCRD()
	require.True(t, upsertBindNotPersistedCondition(blocked))
	assert.True(t, harnessBindBlocked(blocked))

	parked := bornUnboundCRD()
	parked.Status.Conditions = []v1.WorkspaceCondition{{
		Type: v1.WorkspaceConditionHarnessBindingPending, Status: "True", Reason: "HarnessUnbound",
	}}
	assert.False(t, harnessBindBlocked(parked),
		"the steady unbound wait is NOT bind-blocked")

	healed := bornUnboundCRD()
	healed.Status.Conditions = []v1.WorkspaceCondition{{
		Type: v1.WorkspaceConditionHarnessBindingPending, Status: "False", Reason: "HarnessBound",
	}}
	assert.False(t, harnessBindBlocked(healed),
		"a bound workspace never reads blocked")

	assert.False(t, harnessBindBlocked(bornUnboundCRD()), "no condition = not blocked")
}
