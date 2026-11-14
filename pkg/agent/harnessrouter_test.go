// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

// harnessrouter_test.go — S5 harness-selection plumbing pins
// (pi-scoping §6 item 4): the Adapter is constructed per-harness via
// the registry and routed per-workspace from the CRD.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// recordingAdapter wraps fakeAdapter and records which harness handled
// each workspace-addressed call.
type recordingAdapter struct {
	fakeAdapter
	name string

	mu         sync.Mutex
	workspaces []string
}

func (r *recordingAdapter) CreateSession(_ context.Context, _, workspaceID, _ string) (*session.Session, error) {
	r.mu.Lock()
	r.workspaces = append(r.workspaces, r.name+":"+workspaceID)
	r.mu.Unlock()
	return &session.Session{}, nil
}

func (r *recordingAdapter) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.workspaces...)
}

// --- per-workspace dispatch ---------------------------------------------------

func TestHarnessRouter_RoutesPerWorkspace(t *testing.T) {
	oc := &recordingAdapter{name: "opencode"}
	pi := &recordingAdapter{name: "pi"}
	lookups := atomic.Int64{}

	r, err := NewHarnessRouter(HarnessRouterConfig{
		Default: AgentTypeOpenCode,
		Lookup: func(_ context.Context, workspaceID string) (AgentType, error) {
			lookups.Add(1)
			if workspaceID == "ws-pi" {
				return AgentTypePi, nil
			}
			return AgentTypeOpenCode, nil
		},
		Factory: func(agentType AgentType, _ AdapterFactoryDeps) (Adapter, error) {
			if agentType == AgentTypePi {
				return pi, nil
			}
			return oc, nil
		},
	})
	require.NoError(t, err)

	ctx := context.Background()
	_, err = r.CreateSession(ctx, "u1", "ws-a", "t")
	require.NoError(t, err)
	_, err = r.CreateSession(ctx, "u1", "ws-pi", "t")
	require.NoError(t, err)
	_, err = r.CreateSession(ctx, "u1", "ws-b", "t")
	require.NoError(t, err)

	assert.Equal(t, []string{"opencode:ws-a", "opencode:ws-b"}, oc.calls())
	assert.Equal(t, []string{"pi:ws-pi"}, pi.calls(),
		"the pi-harness workspace must be served by the pi adapter — per-workspace dispatch, not a shared one")
}

// TestHarnessRouter_LookupFailureFallsBackToDefault: an apiserver
// hiccup must not take down the (previously only) opencode fleet's hot
// path — the call routes to the default harness.
func TestHarnessRouter_LookupFailureFallsBackToDefault(t *testing.T) {
	oc := &recordingAdapter{name: "opencode"}

	r, err := NewHarnessRouter(HarnessRouterConfig{
		Lookup: func(_ context.Context, _ string) (AgentType, error) {
			return "", errors.New("apiserver unavailable")
		},
		Factory: func(AgentType, AdapterFactoryDeps) (Adapter, error) { return oc, nil },
	})
	require.NoError(t, err)

	_, err = r.CreateSession(context.Background(), "u1", "ws-x", "t")
	require.NoError(t, err)
	assert.Equal(t, []string{"opencode:ws-x"}, oc.calls())
}

// TestHarnessRouter_LazyConstructionFailureFallsBackToDefault: a pi
// adapter construction failure surfaces on pi calls (wrong-harness
// error), never a panic, never on the opencode fleet.
func TestHarnessRouter_LazyConstructionFailureFallsBackToDefault(t *testing.T) {
	oc := &recordingAdapter{name: "opencode"}

	r, err := NewHarnessRouter(HarnessRouterConfig{
		Lookup: func(_ context.Context, workspaceID string) (AgentType, error) {
			if workspaceID == "ws-pi" {
				return AgentTypePi, nil
			}
			return AgentTypeOpenCode, nil
		},
		Factory: func(agentType AgentType, _ AdapterFactoryDeps) (Adapter, error) {
			if agentType == AgentTypePi {
				return nil, errors.New("pi transport unusable")
			}
			return oc, nil
		},
	})
	require.NoError(t, err)

	// The pi call falls back to the default adapter (loud log) instead
	// of panicking; the opencode fleet is untouched.
	_, err = r.CreateSession(context.Background(), "u1", "ws-pi", "t")
	require.NoError(t, err)
	assert.Equal(t, []string{"opencode:ws-pi"}, oc.calls())
}

// TestHarnessRouter_DefaultConstructionFailureIsFatal: the default
// harness's adapter construction failure fails NewHarnessRouter — the
// default harness must exist for the platform to boot (exactly as the
// pre-S5 hard-wired construction was).
func TestHarnessRouter_DefaultConstructionFailureIsFatal(t *testing.T) {
	_, err := NewHarnessRouter(HarnessRouterConfig{
		Factory: func(AgentType, AdapterFactoryDeps) (Adapter, error) {
			return nil, errors.New("boom")
		},
	})
	require.Error(t, err)
}

// TestHarnessRouter_CachesLookupWithinTTL: the harness is immutable
// after create; the TTL-bounded cache amortizes the CRD lookup to one
// per workspace per window.
func TestHarnessRouter_CachesLookupWithinTTL(t *testing.T) {
	oc := &recordingAdapter{name: "opencode"}
	lookups := atomic.Int64{}

	r, err := NewHarnessRouter(HarnessRouterConfig{
		Lookup: func(_ context.Context, _ string) (AgentType, error) {
			lookups.Add(1)
			return AgentTypeOpenCode, nil
		},
		Factory: func(AgentType, AdapterFactoryDeps) (Adapter, error) { return oc, nil },
	})
	require.NoError(t, err)

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err = r.CreateSession(ctx, "u1", "ws-a", "t")
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), lookups.Load(),
		"five calls over one workspace must issue exactly one lookup inside the TTL window")
}

// TestHarnessRouter_StatelessTranslatorsUseDefault: the stateless
// event translators and Capabilities carry no workspace identity and
// delegate to the DEFAULT harness's adapter — proven behaviorally: the
// default adapter's Capabilities returns a marker the pi fake's nil
// never produces.
func TestHarnessRouter_StatelessTranslatorsUseDefault(t *testing.T) {
	oc := &recordingAdapter{name: "opencode"}
	pi := &recordingAdapter{name: "pi"}
	ocCaps := &defaultCapsAdapter{recordingAdapter: oc}

	r, err := NewHarnessRouter(HarnessRouterConfig{
		Default: AgentTypeOpenCode,
		Lookup: func(_ context.Context, _ string) (AgentType, error) {
			return AgentTypePi, nil
		},
		Factory: func(agentType AgentType, _ AdapterFactoryDeps) (Adapter, error) {
			if agentType == AgentTypePi {
				return pi, nil
			}
			return ocCaps, nil
		},
	})
	require.NoError(t, err)

	// Even after a pi-routed call constructed the pi adapter, the
	// stateless surface stays on the default adapter.
	_, err = r.CreateSession(context.Background(), "u1", "ws-pi", "t")
	require.NoError(t, err)
	assert.Equal(t, []session.Capability{session.CapSteer}, r.Capabilities(),
		"Capabilities must come from the DEFAULT harness's adapter (the opencode marker), not pi's")
	_, _, ok := r.ContextUsageFromEvent("x", "{}")
	assert.False(t, ok)
	_, _, ok = r.RetryFromEvent("x", "{}")
	assert.False(t, ok)
	_, err = r.FormatProviderConfig(nil)
	require.NoError(t, err)
	_, err = r.ValidateCredentials(nil)
	require.NoError(t, err)
}

// defaultCapsAdapter stamps a marker Capabilities set so stateless
// delegation to the default adapter is observable.
type defaultCapsAdapter struct {
	*recordingAdapter
}

func (d *defaultCapsAdapter) Capabilities() []session.Capability {
	return []session.Capability{session.CapSteer}
}

// --- registry (RegisterAdapter / NewAdapterForType) ---------------------------

const testFactoryAgentType AgentType = "test-factory-harness"

func TestRegisterAdapter_FactoryRoundTrip(t *testing.T) {
	RegisterAdapter(testFactoryAgentType, func(deps AdapterFactoryDeps) (Adapter, error) {
		require.NotNil(t, deps.Logger, "NewAdapterForType must default a nil logger")
		return &fakeAdapter{}, nil
	})
	t.Cleanup(func() { UnregisterAdapter(testFactoryAgentType) })

	a, err := NewAdapterForType(testFactoryAgentType, AdapterFactoryDeps{})
	require.NoError(t, err)
	assert.NotNil(t, a)
}

func TestNewAdapterForType_UnknownTypeErrors(t *testing.T) {
	_, err := NewAdapterForType("never-registered", AdapterFactoryDeps{})
	require.ErrorContains(t, err, "no adapter factory registered")
}

func TestNewAdapterForType_NilLoggerDefaults(t *testing.T) {
	var seenLogger bool
	RegisterAdapter(testFactoryAgentType, func(deps AdapterFactoryDeps) (Adapter, error) {
		seenLogger = deps.Logger != nil
		return &fakeAdapter{}, nil
	})
	t.Cleanup(func() { UnregisterAdapter(testFactoryAgentType) })

	_, err := NewAdapterForType(testFactoryAgentType, AdapterFactoryDeps{})
	require.NoError(t, err)
	assert.True(t, seenLogger, "factories must never receive a nil logger")
}
