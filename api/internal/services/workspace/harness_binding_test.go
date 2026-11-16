// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	apiinterfaces "github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	kmocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// harness_binding_test.go — the API-tier pins of the bind-once harness
// model (owner ruling 2026-11-16: workspaces do NOT switch harnesses;
// the 2026-11-15 prelim's switch-with-recreate was REJECTED — there is
// no switch or recreate endpoint, pinned at the router tier in
// harness_no_switch_pin_test.go).

// --- fixtures ------------------------------------------------------------

// stubSettingsStore is the minimal InstanceStore: an empty map serves
// schema defaults; err != nil exercises the degrade path. The
// embedded interface panics on SetInstanceSetting — no test here
// writes settings (helm overrides pin them instead).
type stubSettingsStore struct {
	settings.InstanceStore
	err error
}

func (s stubSettingsStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	if s.err != nil {
		return nil, s.err
	}
	return map[string]json.RawMessage{}, nil
}

// newHarnessFixture builds the standard mocked service with instance
// settings wired. piEnabled pins the harness.enabled helm override to
// [opencode pi]; otherwise the schema default ([opencode]) serves.
func newHarnessFixture(t *testing.T, piEnabled bool) *ensureFixture {
	t.Helper()

	log := lmocks.NewMockLogger()
	log.On("Info", mock.Anything, mock.Anything).Maybe()
	log.On("Debug", mock.Anything, mock.Anything).Maybe()
	log.On("Warn", mock.Anything, mock.Anything).Maybe()
	log.On("Error", mock.Anything, mock.Anything, mock.Anything).Maybe()
	log.On("With", mock.Anything).Return(log).Maybe()

	k8s := kmocks.NewMockKubernetesClient()
	v1i := kmocks.NewMockLLMSafespacesV1Interface()
	ws := kmocks.NewMockWorkspaceInterface()
	db := &imocks.MockDatabaseService{}
	cache := &imocks.MockCacheService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	// verifyOwner's ownership row (Maybe: wrong-owner/not-found tests
	// never reach it or assert the error instead).
	db.On("GetWorkspace", mock.Anything, "ws-1").Return(&types.WorkspaceMetadata{UserID: "user-1"}, nil).Maybe()

	k8s.On("LlmsafespacesV1").Return(v1i, nil)
	v1i.On("Workspaces", "default").Return(ws)

	svc, err := New(log, k8s, db, cache, met, &Config{Namespace: "default", OpencodePort: 4096})
	require.NoError(t, err)

	isvc := settings.NewInstanceService(stubSettingsStore{}, log)
	if piEnabled {
		isvc.SetHelmOverrides(map[string]any{
			settings.KeyHarnessEnabled.Name(): []string{"opencode", "pi"},
		})
	}
	svc.SetInstanceSettings(isvc)

	return &ensureFixture{svc: svc, ws: ws, db: db}
}

// bornUnboundCRD is the frontend-picker-path workspace: no harness,
// the discriminator marker, parked in Creating (no pod).
func bornUnboundCRD() *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "ws-1",
			Namespace:   "default",
			Annotations: map[string]string{v1.AnnotationHarnessUnbound: "true"},
		},
		Spec:   v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "user-1"}},
		Status: v1.WorkspaceStatus{Phase: v1.WorkspacePhaseCreating},
	}
}

// boundCRD is a bound workspace (phase Active, pod running).
func boundCRD(h v1.HarnessType) *v1.Workspace {
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec:       v1.WorkspaceSpec{Runtime: "python:3.11", Owner: v1.WorkspaceOwner{UserID: "user-1"}, Harness: h},
		Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1"},
	}
}

// legacyCRD is a pre-binding-ruling object: harness absent, NO marker —
// bound opencode in every observable way (the old schema default).
func legacyCRD() *v1.Workspace {
	c := boundCRD("")
	c.Status = v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.1"}
	return c
}

// asAPIError asserts the error is the typed APIError and returns it.
func harnessAPIError(t *testing.T, err error) *apierrors.APIError {
	t.Helper()
	var apiErr *apierrors.APIError
	require.True(t, errors.As(err, &apiErr), "expected *APIError, got %T: %v", err, err)
	return apiErr
}

// --- EnabledHarnesses ---------------------------------------------------

func TestEnabledHarnesses(t *testing.T) {
	t.Run("default is opencode only", func(t *testing.T) {
		f := newHarnessFixture(t, false)
		assert.Equal(t, []string{"opencode"}, f.svc.EnabledHarnesses(context.Background()))
	})
	t.Run("pi joins when the instance enables it", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		assert.Equal(t, []string{"opencode", "pi"}, f.svc.EnabledHarnesses(context.Background()))
	})
	t.Run("opencode is unioned in even if the setting drops it", func(t *testing.T) {
		f := newHarnessFixture(t, false)
		isvc := settings.NewInstanceService(stubSettingsStore{}, nil)
		isvc.SetHelmOverrides(map[string]any{settings.KeyHarnessEnabled.Name(): []string{"pi"}})
		f.svc.SetInstanceSettings(isvc)
		assert.Equal(t, []string{"opencode", "pi"}, f.svc.EnabledHarnesses(context.Background()),
			"the platform's default harness cannot be withdrawn")
	})
	t.Run("settings failure degrades to opencode-only", func(t *testing.T) {
		f := newHarnessFixture(t, false)
		isvc := settings.NewInstanceService(stubSettingsStore{err: fmt.Errorf("db down")}, nil)
		f.svc.SetInstanceSettings(isvc)
		assert.Equal(t, []string{"opencode"}, f.svc.EnabledHarnesses(context.Background()))
	})
}

// --- effectiveBoundHarness ----------------------------------------------

func TestEffectiveBoundHarness(t *testing.T) {
	h, bound := effectiveBoundHarness(boundCRD(v1.HarnessOpencode))
	assert.Equal(t, "opencode", h)
	assert.True(t, bound)

	h, bound = effectiveBoundHarness(boundCRD(v1.HarnessPi))
	assert.Equal(t, "pi", h)
	assert.True(t, bound)

	h, bound = effectiveBoundHarness(legacyCRD())
	assert.Equal(t, "opencode", h, "legacy marker-less empty = the old schema default, opencode")
	assert.True(t, bound)

	h, bound = effectiveBoundHarness(bornUnboundCRD())
	assert.Equal(t, "", h)
	assert.False(t, bound, "born-unbound is the only genuinely unbound state")
}

// --- buildWorkspaceCRD ---------------------------------------------------

func TestBuildWorkspaceCRD_HarnessBinding(t *testing.T) {
	t.Run("explicit param binds at create", func(t *testing.T) {
		crd := buildWorkspaceCRD("ws-1", "user-1", types.CreateWorkspaceRequest{
			Name: "n", Runtime: "python:3.11", StorageSize: "5Gi", Harness: "pi",
		}, "default")
		assert.Equal(t, v1.HarnessPi, crd.Spec.Harness)
		assert.Empty(t, crd.Annotations[v1.AnnotationHarnessUnbound])
	})
	t.Run("omitted leaves the workspace unbound with the marker", func(t *testing.T) {
		crd := buildWorkspaceCRD("ws-1", "user-1", types.CreateWorkspaceRequest{
			Name: "n", Runtime: "python:3.11", StorageSize: "5Gi",
		}, "default")
		assert.Empty(t, crd.Spec.Harness, "no implicit opencode — the 2026-11-16 default change")
		assert.Equal(t, "true", crd.Annotations[v1.AnnotationHarnessUnbound])
	})
}

// --- BindWorkspaceHarness (PUT /harness) ---------------------------------

func TestBindWorkspaceHarness(t *testing.T) {
	ctx := context.Background()

	t.Run("bind-while-null binds exactly once and removes the marker", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		unbound := bornUnboundCRD()
		// Get #1: the endpoint's own fetch; Get #2: bindUnboundWorkspace's
		// fresh read under the conflict-retry (the write lane's contract).
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(unbound, nil).Twice()
		var updated *v1.Workspace
		f.ws.On("Update", mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) { updated = args.Get(1).(*v1.Workspace) }).
			Return(nil, nil).Once()

		res, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "pi")
		require.NoError(t, err)
		assert.Equal(t, "pi", res.Harness)
		assert.False(t, res.AlreadyBound)
		require.NotNil(t, updated)
		assert.Equal(t, v1.HarnessPi, updated.Spec.Harness)
		_, marker := updated.Annotations[v1.AnnotationHarnessUnbound]
		assert.False(t, marker, "the marker is removed at bind")
	})

	t.Run("same value on a bound workspace is idempotent (no write)", func(t *testing.T) {
		f := newHarnessFixture(t, false)
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(boundCRD(v1.HarnessOpencode), nil).Once()

		res, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "opencode")
		require.NoError(t, err)
		assert.Equal(t, "opencode", res.Harness)
		assert.True(t, res.AlreadyBound)
		// No Update expectation: an unexpected mock call fails the test —
		// the pin that the idempotent path never writes.
	})

	t.Run("bound workspace ignores enablement changes", func(t *testing.T) {
		// pi is NOT enabled in this fixture; the workspace is bound pi.
		// Re-binding the same value must still succeed (immutability
		// protects the bound value from later enablement changes).
		f := newHarnessFixture(t, false)
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(boundCRD(v1.HarnessPi), nil).Once()

		res, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "pi")
		require.NoError(t, err)
		assert.True(t, res.AlreadyBound)
	})

	t.Run("value to anything else is a loud 409 harness_immutable", func(t *testing.T) {
		for _, tc := range []struct {
			name, current, attempt string
		}{
			{"opencode to pi", "opencode", "pi"},
			{"pi to opencode", "pi", "opencode"},
			{"pi to unset (unbind)", "pi", ""},
		} {
			f := newHarnessFixture(t, true)
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(boundCRD(v1.HarnessType(tc.current)), nil).Once()

			_, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", tc.attempt)
			apiErr := harnessAPIError(t, err)
			assert.Equal(t, 409, apiErr.StatusCode(), tc.name)
			assert.Equal(t, "harness_immutable", apiErr.Code, tc.name)
			assert.Equal(t, "harness is immutable once bound for this workspace", apiErr.Message, tc.name)
		}
	})

	t.Run("disabled harness on an unbound workspace is 422", func(t *testing.T) {
		f := newHarnessFixture(t, false) // pi NOT enabled
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(bornUnboundCRD(), nil).Once()

		_, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "pi")
		apiErr := harnessAPIError(t, err)
		assert.Equal(t, 422, apiErr.StatusCode())
		assert.Equal(t, "validation_error", apiErr.Code)
		assert.Contains(t, apiErr.Message, "not enabled")
	})

	t.Run("unknown harness is 422", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(bornUnboundCRD(), nil).Once()

		_, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "bogus")
		apiErr := harnessAPIError(t, err)
		assert.Equal(t, 422, apiErr.StatusCode())
		assert.Contains(t, apiErr.Message, "unknown harness")
	})

	t.Run("empty harness is the 422 required-field error", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(bornUnboundCRD(), nil).Once()

		_, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "")
		apiErr := harnessAPIError(t, err)
		assert.Equal(t, 422, apiErr.StatusCode())
		assert.Contains(t, apiErr.Message, "harness is required")
	})

	t.Run("legacy empty workspace binds opencode only", func(t *testing.T) {
		t.Run("pi is refused as immutable (legacy = bound opencode)", func(t *testing.T) {
			f := newHarnessFixture(t, true)
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(legacyCRD(), nil).Once()

			_, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "pi")
			apiErr := harnessAPIError(t, err)
			assert.Equal(t, 409, apiErr.StatusCode())
			assert.Equal(t, "harness_immutable", apiErr.Code)
		})
		t.Run("opencode answers idempotently (the controller sweep stamps the spec)", func(t *testing.T) {
			f := newHarnessFixture(t, false)
			f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(legacyCRD(), nil).Once()

			res, err := f.svc.BindWorkspaceHarness(ctx, "user-1", "ws-1", "opencode")
			require.NoError(t, err)
			assert.Equal(t, "opencode", res.Harness)
			assert.True(t, res.AlreadyBound,
				"a legacy object reads as bound-opencode (effectiveBoundHarness); the explicit spec stamp is the controller's backfill, not a bind")
			// No Update expectation: nothing to bind — the effective
			// binding already is opencode.
		})
	})
}

// --- resolveSessionHarness (first-session binding) -----------------------

func TestResolveSessionHarness(t *testing.T) {
	ctx := context.Background()

	t.Run("bound workspace: omitted uses the bound harness, no write", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		h, err := f.svc.resolveSessionHarness(ctx, boundCRD(v1.HarnessPi), "ws-1", "")
		require.NoError(t, err)
		assert.Equal(t, "pi", h)
	})

	t.Run("bound workspace: matching harness passes", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		h, err := f.svc.resolveSessionHarness(ctx, boundCRD(v1.HarnessOpencode), "ws-1", "opencode")
		require.NoError(t, err)
		assert.Equal(t, "opencode", h)
	})

	t.Run("bound workspace: mismatched harness is 409 harness_immutable", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		_, err := f.svc.resolveSessionHarness(ctx, boundCRD(v1.HarnessOpencode), "ws-1", "pi")
		apiErr := harnessAPIError(t, err)
		assert.Equal(t, 409, apiErr.StatusCode())
		assert.Equal(t, "harness_immutable", apiErr.Code)
	})

	t.Run("unbound + omitted binds opencode (the non-interactive default)", func(t *testing.T) {
		f := newHarnessFixture(t, false)
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(bornUnboundCRD(), nil).Once()
		var updated *v1.Workspace
		f.ws.On("Update", mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) { updated = args.Get(1).(*v1.Workspace) }).
			Return(nil, nil).Once()

		h, err := f.svc.resolveSessionHarness(ctx, bornUnboundCRD(), "ws-1", "")
		require.NoError(t, err)
		assert.Equal(t, "opencode", h)
		require.NotNil(t, updated)
		assert.Equal(t, v1.HarnessOpencode, updated.Spec.Harness)
	})

	t.Run("unbound + picker submit binds it (the frontend path)", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(bornUnboundCRD(), nil).Once()
		var updated *v1.Workspace
		f.ws.On("Update", mock.Anything, mock.Anything).
			Run(func(args mock.Arguments) { updated = args.Get(1).(*v1.Workspace) }).
			Return(nil, nil).Once()

		h, err := f.svc.resolveSessionHarness(ctx, bornUnboundCRD(), "ws-1", "pi")
		require.NoError(t, err)
		assert.Equal(t, "pi", h)
		require.NotNil(t, updated)
		assert.Equal(t, v1.HarnessPi, updated.Spec.Harness)
		_, marker := updated.Annotations[v1.AnnotationHarnessUnbound]
		assert.False(t, marker)
	})

	t.Run("unbound + disabled picker submit is 422", func(t *testing.T) {
		f := newHarnessFixture(t, false)
		_, err := f.svc.resolveSessionHarness(ctx, bornUnboundCRD(), "ws-1", "pi")
		apiErr := harnessAPIError(t, err)
		assert.Equal(t, 422, apiErr.StatusCode())
	})

	t.Run("legacy workspace: pi is 409, omitted is opencode without a write", func(t *testing.T) {
		f := newHarnessFixture(t, true)
		_, err := f.svc.resolveSessionHarness(ctx, legacyCRD(), "ws-1", "pi")
		apiErr := harnessAPIError(t, err)
		assert.Equal(t, 409, apiErr.StatusCode())
		assert.Equal(t, "harness_immutable", apiErr.Code)

		h, err := f.svc.resolveSessionHarness(ctx, legacyCRD(), "ws-1", "")
		require.NoError(t, err)
		assert.Equal(t, "opencode", h)
	})
}

// fakePiAdapter overrides CreateSession on the embedded Adapter seam
// (the fakeEnsureIndex pattern: embed the interface, exercise one call).
type fakePiAdapter struct {
	agent.Adapter
	created    int
	returnedID string
}

func (a *fakePiAdapter) CreateSession(_ context.Context, _, _, _ string) (*session.Session, error) {
	a.created++
	return &session.Session{ID: a.returnedID}, nil
}

// captureIndex records the harness string the registration seam saw.
type captureIndex struct {
	apiinterfaces.SessionIndexService
	harness string
}

func (c *captureIndex) Register(_ context.Context, _, _, harness string) string {
	c.harness = harness
	return ""
}

// A pi-bound workspace's session-create goes through the platform
// Agent Adapter (the harness router's pi leg), and the registration
// seam sees the BOUND harness — never a hardcoded "opencode".
func TestEnsureSession_PiBoundWorkspace_CreatesViaAdapter(t *testing.T) {
	f := newHarnessFixture(t, true)
	f.ws.On("Get", mock.Anything, "ws-1", mock.Anything).Return(boundCRD(v1.HarnessPi), nil)

	adapter := &fakePiAdapter{returnedID: "pi-sess-1"}
	f.svc.SetAgentAdapter(adapter)
	idx := &captureIndex{}
	f.svc.SetSessionIndex(idx)

	resp, err := f.svc.EnsureSession(context.Background(), "user-1", "ws-1", "")
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.created)
	assert.Equal(t, "pi", resp.Harness)
	assert.Equal(t, "pi-sess-1", resp.SessionID)
	assert.Equal(t, "pi", idx.harness, "the session index row carries the bound harness")
}

// The create entrance enforces the enabled set like the other two bind
// entrances (review r1 F1): a disabled or unknown harness is a 422
// BEFORE any CRD write — an unexpected k8s/db mock call fails the test.
func TestCreateWorkspace_DisabledOrUnknownHarnessRejected(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, harness string
		piEnabled     bool
	}{
		{"pi on an opencode-only instance", "pi", false},
		{"unknown harness value", "bogus", true},
		{"unknown harness value, pi disabled too", "claude", false},
	} {
		f := newHarnessFixture(t, tc.piEnabled)
		req := types.CreateWorkspaceRequest{
			Name: "ws", Runtime: "python:3.11", StorageSize: "5Gi", Harness: tc.harness,
		}
		_, err := f.svc.CreateWorkspace(ctx, "user-1", req)
		apiErr := harnessAPIError(t, err)
		assert.Equal(t, 422, apiErr.StatusCode(), tc.name)
		assert.Equal(t, "validation_error", apiErr.Code, tc.name)
		// The CRD-create mock has NO expectations: reaching it fails
		// the test — the validation must front every write.
	}
}
