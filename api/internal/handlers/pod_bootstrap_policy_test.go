// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/google/uuid"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// policyAwareInjector records the per-step call order (policy sync,
// manifest, build) so tests can pin that global-default convergence
// runs BEFORE the manifest tier derives the live hash — otherwise a
// boot-time sync would mint a hash the same request already compared
// against.
type policyAwareInjector struct {
	fakeBootstrapInjector
	events    *[]string
	syncErr   error
	syncCalls int
}

func (p *policyAwareInjector) BuildWorkspaceBatch(ctx context.Context, u, w string) (*secrets.Batch, *secrets.BuildDegrade, error) {
	*p.events = append(*p.events, "build")
	return p.fakeBootstrapInjector.BuildWorkspaceBatch(ctx, u, w)
}

func (p *policyAwareInjector) ManifestFor(ctx context.Context, u, w string) (string, error) {
	*p.events = append(*p.events, "manifest")
	return p.fakeBootstrapInjector.ManifestFor(ctx, u, w)
}

func (p *policyAwareInjector) SyncGlobalDefaultBindings(_ context.Context, _, _ string) ([]string, []string, error) {
	p.syncCalls++
	*p.events = append(*p.events, "policy-sync")
	if p.syncErr != nil {
		return nil, nil, p.syncErr
	}
	return nil, nil, nil
}

func newPolicyBootstrapTest(t *testing.T) (*gin.Engine, *policyAwareInjector, *[]string) {
	t.Helper()
	var events []string
	injector := &policyAwareInjector{fakeBootstrapInjector: fakeBootstrapInjector{
		manifestHash: "hash-v1",
		currentSeq:   1, currentHash: "hash-v1", currentOK: true,
	}, events: &events}
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-abc"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)
	return router, injector, &events
}

// TestPodBootstrap_PolicySyncRunsBeforeManifest: a v2 conditional pull
// converges global-default bindings first, so the manifest hash this
// request computes (and 304-compares) already reflects any materialized
// policy rows.
func TestPodBootstrap_PolicySyncRunsBeforeManifest(t *testing.T) {
	router, injector, events := newPolicyBootstrapTest(t)

	w := doBootstrap(t, router, "valid-token",
		`{"workspaceID":"ws-abc","contractVersion":2,"clientManifestHash":"hash-v1"}`)

	require.Equal(t, http.StatusNotModified, w.Code)
	assert.Equal(t, 1, injector.syncCalls)
	require.Len(t, *events, 2)
	assert.Equal(t, []string{"policy-sync", "manifest"}, *events)
}

// TestPodBootstrap_PolicySyncFailureIsBestEffort: a failing policy sync
// must NOT brick the pod boot — it is logged and the request proceeds
// (the reconcile loop re-converges on its next pass).
func TestPodBootstrap_PolicySyncFailureIsBestEffort(t *testing.T) {
	router, injector, events := newPolicyBootstrapTest(t)
	injector.syncErr = context.DeadlineExceeded

	w := doBootstrap(t, router, "valid-token", `{"workspaceID":"ws-abc"}`)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, injector.syncCalls)
	require.Len(t, *events, 2)
	assert.Equal(t, []string{"policy-sync", "build"}, *events)
}

// TestPodBootstrap_PolicySyncSkippedForLegacyInjector: an injector
// without the policy seam (pre-upgrade API replica, test fakes) keeps
// the pre-policy behavior — no sync, straight to build.
func TestPodBootstrap_PolicySyncSkippedForLegacyInjector(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-abc"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-abc", UserID: "user-1"}}
	injector := &legacyOnlyInjector{entry: &secrets.BatchEntry{
		SecretID: "sec-1", Version: 1, Type: secrets.SecretTypeEnvSecret, Name: "n", Value: "v",
	}}
	router := newTestBootstrapRouter(t, reviewer, injector, lookup)

	w := doBootstrap(t, router, "valid-token", `{"workspaceID":"ws-abc"}`)

	require.Equal(t, http.StatusOK, w.Code)
}

// globalDefaultE2EStore upgrades the fatal-on-touch e2eSecretStore with
// a functional user-secret surface (provenance-tracked bindings + a
// real revision row) so the REAL SecretService can run the full
// bootstrap composition: policy sync → manifest → decrypt → batch.
type globalDefaultE2EStore struct {
	e2eSecretStore
	rev *conditionalRevStore

	userSecrets map[string]*secrets.UserSecret
	bindings    map[string]map[string]string // ws -> secretID -> source
}

func newGlobalDefaultE2EStore() *globalDefaultE2EStore {
	return &globalDefaultE2EStore{
		rev:         &conditionalRevStore{},
		userSecrets: make(map[string]*secrets.UserSecret),
		bindings:    make(map[string]map[string]string),
	}
}

func (s *globalDefaultE2EStore) CreateSecret(_ context.Context, sec *secrets.UserSecret) error {
	if sec.ID == "" {
		// The DB mints a uuid; the fixture needs one too or every
		// binding key collapses to "".
		sec.ID = uuid.NewString()
	}
	s.userSecrets[sec.ID] = sec
	return nil
}
func (s *globalDefaultE2EStore) GetSecret(_ context.Context, _, id string) (*secrets.UserSecret, error) {
	if sec, ok := s.userSecrets[id]; ok {
		cp := *sec
		return &cp, nil
	}
	return nil, nil
}
func (s *globalDefaultE2EStore) ListGlobalDefaultSecrets(_ context.Context, userID string) ([]*secrets.UserSecret, error) {
	var out []*secrets.UserSecret
	for _, sec := range s.userSecrets {
		if sec.UserID == userID && sec.GlobalDefault {
			cp := *sec
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *globalDefaultE2EStore) GetBindings(_ context.Context, ws string) ([]*secrets.UserSecret, error) {
	var out []*secrets.UserSecret
	for id := range s.bindings[ws] {
		if sec, ok := s.userSecrets[id]; ok {
			cp := *sec
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (s *globalDefaultE2EStore) SetBindings(_ context.Context, ws string, ids []string) error {
	next := make(map[string]string, len(ids))
	for _, id := range ids {
		next[id] = secrets.BindSourceManual
	}
	s.bindings[ws] = next
	return nil
}
func (s *globalDefaultE2EStore) SyncGlobalDefaultBindings(_ context.Context, ws string, ids []string) ([]string, []string, error) {
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	rows := s.bindings[ws]
	if rows == nil {
		rows = make(map[string]string)
	}
	var added, removed []string
	for id := range want {
		if _, ok := rows[id]; !ok {
			rows[id] = secrets.BindSourceGlobalDefault
			added = append(added, id)
		}
	}
	for id, src := range rows {
		if src != secrets.BindSourceGlobalDefault {
			continue
		}
		if _, ok := want[id]; !ok {
			delete(rows, id)
			removed = append(removed, id)
		}
	}
	if len(rows) > 0 {
		s.bindings[ws] = rows
	} else {
		delete(s.bindings, ws)
	}
	return added, removed, nil
}
func (s *globalDefaultE2EStore) CurrentRevision(ctx context.Context, ws string) (int64, string, bool, error) {
	return s.rev.CurrentRevision(ctx, ws)
}
func (s *globalDefaultE2EStore) EnsureRevision(ctx context.Context, ws, hash string) (int64, error) {
	return s.rev.EnsureRevision(ctx, ws, hash)
}

// TestPodBootstrap_GlobalDefaultSecretReachesBatch is the boot-side
// e2e for the production bug: a global-default secret created AFTER
// the workspace existed (no binding row) must still reach the
// delivered batch on the workspace's next bootstrap — materialized by
// the handler's policy step, hashed by the manifest tier, and
// decrypted end-to-end through the server-side DEK unwrap.
func TestPodBootstrap_GlobalDefaultSecretReachesBatch(t *testing.T) {
	store := newGlobalDefaultE2EStore()
	keySvc := newE2EKeyService(t, bytes.Repeat([]byte{0xD7}, 32))
	svc := secrets.NewSecretService(keySvc, store)

	// The secret is created after the workspace exists — the exact
	// shipped-bug shape: no seed ran, no binding row.
	_, err := svc.CreateSecret(context.Background(), "user-e2e", "", nil, secrets.CreateSecretRequest{
		Name:          "gd-appid",
		Type:          secrets.SecretTypeEnvSecret,
		Value:         "app-id-123",
		Metadata:      json.RawMessage(`{"var_name":"GD_APPID"}`),
		GlobalDefault: true,
	})
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodBootstrapHandler(
		&staticTokenReviewer{username: "system:serviceaccount:" + testBootstrapNamespace + ":workspace-ws-e2e"},
		svc,
		&wsMetaLookup{ws: &types.WorkspaceMetadata{ID: "ws-e2e", UserID: "user-e2e"}},
		nil,
		testBootstrapNamespace,
	)
	r.POST("/internal/v1/pod-bootstrap", h.Bootstrap)

	w := doBootstrap(t, r, "valid-token", `{"workspaceID":"ws-e2e"}`)
	require.Equal(t, http.StatusOK, w.Code)

	// The legacy (non-v2) response wraps the bare entries array under
	// "secrets".
	var resp struct {
		Secrets []secrets.InjectedSecret `json:"secrets"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	var found *secrets.InjectedSecret
	for i := range resp.Secrets {
		if resp.Secrets[i].Name == "gd-appid" {
			found = &resp.Secrets[i]
		}
	}
	require.NotNil(t, found, "the global-default secret must reach the delivered batch: secrets=%+v", resp.Secrets)
	assert.Equal(t, "app-id-123", found.Plaintext, "the plaintext must survive the full decrypt round-trip")
	sources := store.bindings["ws-e2e"]
	require.NotEmpty(t, sources, "the policy step must have materialized a binding row")
	for id, src := range sources {
		assert.Equal(t, secrets.BindSourceGlobalDefault, src,
			"the materialized row for %s must carry global_default provenance", id)
	}
}
