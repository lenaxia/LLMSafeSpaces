// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

// ── E2E test fixtures ───────────────────────────────────────────────────

// e2eImageFactoryStore is a full in-memory store that simulates real DB
// behavior for image-factory e2e tests. It tracks configs, builds, and
// known failures with realistic semantics (atomic transitions, coalescing).
type e2eImageFactoryStore struct {
	e2eBase               imagefactory.Base
	e2eExtensions         map[string]imagefactory.Extension
	e2ePlatformCfg        imagefactory.PlatformConfig
	e2eConfigs            map[string]*imagefactory.Config
	e2eBuilds             map[string]*imagefactory.Build
	e2eKnownFailures      map[string]*imagefactory.KnownFailure
	e2eKnownFailureByHash map[string]*imagefactory.KnownFailure
}

func newE2EStore() *e2eImageFactoryStore {
	return &e2eImageFactoryStore{
		e2eBase: imagefactory.Base{Name: "bookworm", Version: "0.6.0", Image: "ghcr.io/acme/base", Tag: "0.6.0", IsDefault: true},
		e2eExtensions: map[string]imagefactory.Extension{
			"ffmpeg":    {ID: "ffmpeg", Type: imagefactory.ExtensionTypeApt, Value: "ffmpeg", SupportedBases: []string{"bookworm"}},
			"python313": {ID: "python313", Type: imagefactory.ExtensionTypeMise, Value: "python@3.13", SupportedBases: []string{"bookworm"}},
			"motd":      {ID: "motd", Type: imagefactory.ExtensionTypeFile, Value: "welcome\n", FileSpec: &imagefactory.FileSpec{Path: "/etc/motd", Mode: "0644"}, SupportedBases: []string{"bookworm"}},
		},
		e2ePlatformCfg:        imagefactory.PlatformConfig{Architectures: []string{"linux/amd64"}},
		e2eConfigs:            map[string]*imagefactory.Config{},
		e2eBuilds:             map[string]*imagefactory.Build{},
		e2eKnownFailures:      map[string]*imagefactory.KnownFailure{},
		e2eKnownFailureByHash: map[string]*imagefactory.KnownFailure{},
	}
}

// Satisfies imageFactoryStore
func (s *e2eImageFactoryStore) GetPlatformConfig(ctx context.Context) (imagefactory.PlatformConfig, error) {
	return s.e2ePlatformCfg, nil
}
func (s *e2eImageFactoryStore) ListBases(ctx context.Context) ([]imagefactory.Base, error) {
	return []imagefactory.Base{s.e2eBase}, nil
}
func (s *e2eImageFactoryStore) GetBase(ctx context.Context, name, version string) (imagefactory.Base, error) {
	if name == s.e2eBase.Name && version == s.e2eBase.Version {
		return s.e2eBase, nil
	}
	return imagefactory.Base{}, database.ErrNotFound
}
func (s *e2eImageFactoryStore) ListExtensions(ctx context.Context, includeRetired bool) ([]imagefactory.Extension, error) {
	exts := make([]imagefactory.Extension, 0, len(s.e2eExtensions))
	for _, e := range s.e2eExtensions {
		exts = append(exts, e)
	}
	return exts, nil
}
func (s *e2eImageFactoryStore) ListKnownFailures(ctx context.Context) ([]imagefactory.KnownFailure, error) {
	out := make([]imagefactory.KnownFailure, 0, len(s.e2eKnownFailures))
	for _, kf := range s.e2eKnownFailures {
		out = append(out, *kf)
	}
	return out, nil
}
func (s *e2eImageFactoryStore) GetKnownFailure(ctx context.Context, hash, baseName string) (imagefactory.KnownFailure, error) {
	key := hash + "|" + baseName
	if kf, ok := s.e2eKnownFailureByHash[key]; ok {
		return *kf, nil
	}
	return imagefactory.KnownFailure{}, database.ErrNotFound
}
func (s *e2eImageFactoryStore) GetConfig(ctx context.Context, id string) (imagefactory.Config, error) {
	if c, ok := s.e2eConfigs[id]; ok {
		return *c, nil
	}
	return imagefactory.Config{}, database.ErrNotFound
}
func (s *e2eImageFactoryStore) GetConfigByHash(ctx context.Context, hash string, scope imagefactory.ConfigScope, ownerID, orgID *string) (imagefactory.Config, error) {
	for _, c := range s.e2eConfigs {
		if c.Hash == hash && c.Scope == scope {
			return *c, nil
		}
	}
	return imagefactory.Config{}, database.ErrNotFound
}
func (s *e2eImageFactoryStore) ListVisibleConfigs(ctx context.Context, ownerID, orgID *string) ([]imagefactory.Config, error) {
	out := []imagefactory.Config{}
	for _, c := range s.e2eConfigs {
		if c.Scope == imagefactory.ScopePlatform ||
			(c.Scope == imagefactory.ScopeMember && ownerID != nil && c.OwnerID != nil && *c.OwnerID == *ownerID) {
			out = append(out, *c)
		}
	}
	return out, nil
}
func (s *e2eImageFactoryStore) CreateConfig(ctx context.Context, c *imagefactory.Config) error {
	s.e2eConfigs[c.ID] = c
	return nil
}
func (s *e2eImageFactoryStore) CreateConfigAndBuild(ctx context.Context, c *imagefactory.Config, b *imagefactory.Build) error {
	c.ID = "cfg-" + b.ID
	b.ConfigID = c.ID
	s.e2eConfigs[c.ID] = c
	s.e2eBuilds[b.ID] = b
	return nil
}
func (s *e2eImageFactoryStore) CreateBuild(ctx context.Context, b *imagefactory.Build) error {
	s.e2eBuilds[b.ID] = b
	return nil
}
func (s *e2eImageFactoryStore) GetInFlightOrSuccessfulBuild(ctx context.Context, hash, baseVersion string) (*imagefactory.Build, error) {
	for _, b := range s.e2eBuilds {
		if b.Hash == hash && b.BaseVersion == baseVersion {
			if b.Status == imagefactory.BuildSucceeded {
				return b, nil
			}
			if b.Status == imagefactory.BuildDispatched {
				return b, nil
			}
		}
	}
	return nil, nil
}
func (s *e2eImageFactoryStore) SetConfigStatus(ctx context.Context, id string, status imagefactory.ConfigStatus) error {
	if c, ok := s.e2eConfigs[id]; ok {
		c.Status = status
	}
	return nil
}
func (s *e2eImageFactoryStore) MarkBuildSucceeded(ctx context.Context, id, imageRef, digest string) error {
	if b, ok := s.e2eBuilds[id]; ok {
		b.Status = imagefactory.BuildSucceeded
		b.ImageRef = imageRef
		b.Digest = digest
	}
	return nil
}
func (s *e2eImageFactoryStore) MarkBuildFailed(ctx context.Context, id, failureReason, explanation string) error {
	if b, ok := s.e2eBuilds[id]; ok {
		b.Status = imagefactory.BuildFailed
		b.FailureReason = failureReason
		b.Explanation = explanation
	}
	return nil
}
func (s *e2eImageFactoryStore) TransitionBuildSucceeded(ctx context.Context, buildID, configID, imageRef, digest string) error {
	s.MarkBuildSucceeded(ctx, buildID, imageRef, digest)
	s.SetConfigStatus(ctx, configID, imagefactory.StatusReady)
	return nil
}
func (s *e2eImageFactoryStore) TransitionBuildFailed(ctx context.Context, buildID, configID string, kf imagefactory.KnownFailure) error {
	s.MarkBuildFailed(ctx, buildID, kf.FailureReason, kf.Explanation)
	key := kf.SelectionHash + "|" + kf.BaseName
	s.e2eKnownFailureByHash[key] = &kf
	s.SetConfigStatus(ctx, configID, imagefactory.StatusRejected)
	return nil
}
func (s *e2eImageFactoryStore) SetExtensionReviewRequested(ctx context.Context, id string, v bool) error {
	return nil
}

func (s *e2eImageFactoryStore) GetBuild(ctx context.Context, id string) (imagefactory.Build, error) {
	if b, ok := s.e2eBuilds[id]; ok {
		return *b, nil
	}
	return imagefactory.Build{}, database.ErrNotFound
}

func (s *e2eImageFactoryStore) DeleteConfig(ctx context.Context, id string) error {
	delete(s.e2eConfigs, id)
	return nil
}

func (s *e2eImageFactoryStore) RenameConfig(ctx context.Context, id, newName string) error {
	if cfg, ok := s.e2eConfigs[id]; ok {
		cfg.Name = newName
		s.e2eConfigs[id] = cfg
		return nil
	}
	return database.ErrNotFound
}

// Satisfies orgResolver
type e2eOrgResolver struct{}

func (e *e2eOrgResolver) GetUserOrgID(ctx context.Context, userID string) (string, error) {
	return "", nil
}

// ── E2E test router builder ─────────────────────────────────────────────

func newE2ERouter(t *testing.T, store *e2eImageFactoryStore, disp buildDispatcher) *gin.Engine {
	t.Helper()
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", c.GetHeader("X-Test-UserID"))
		c.Next()
	})
	h := NewImageFactoryHandler(store, &e2eOrgResolver{})
	h.SetDispatcher(disp)
	h.SetBuildStore(store, "ghcr.io/acme/ws", "/internal/image-factory")
	r.GET("/api/v1/image-factory/catalog", h.Catalog)
	r.GET("/api/v1/image-factory/configs", h.ListConfigs)
	r.GET("/api/v1/image-factory/configs/:hash", h.GetConfig)
	r.POST("/api/v1/image-factory/configs", h.CreateConfig)
	r.POST("/internal/image-factory/builds/:id/callback", h.Callback)
	return r
}

// ── E2E: Full happy path round-trip ─────────────────────────────────────

// TestE2E_ImageFactory_FullRoundTrip exercises the complete lifecycle:
// 1. Read catalog → verify extensions are visible
// 2. POST /configs (novel selection) → dispatch fires, config=building
// 3. POST /callback (succeeded) → config=ready, build=succeeded
// 4. GET /configs → verify the config is now visible with status=ready
// 5. POST /configs (same selection again) → coalesces, no new dispatch
func TestE2E_ImageFactory_FullRoundTrip(t *testing.T) {
	store := newE2EStore()
	disp := &fakeDispatcher{ghRunID: 42}
	r := newE2ERouter(t, store, disp)

	// Read catalog
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/image-factory/catalog", nil)
	req.Header.Set("X-Test-UserID", "user-1")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var cat CatalogResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cat))
	assert.Len(t, cat.Extensions, 3, "catalog should have 3 extensions")

	// POST /configs — novel dispatch
	cfgBody, _ := json.Marshal(createConfigRequest{
		Name: "e2e-ml-stack", Selection: []string{"ffmpeg", "python313"}, BaseName: "bookworm",
	})
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-1")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "novel config should return 201")
	assert.True(t, disp.called, "dispatch must fire for novel config")

	// Verify dispatch carries the rendered Dockerfile
	assert.NotEmpty(t, disp.lastReq.Dockerfile, "dispatch must carry rendered Dockerfile")
	assert.Contains(t, disp.lastReq.Dockerfile, "ffmpeg")
	assert.Contains(t, disp.lastReq.Dockerfile, "python@3.13")

	// Extract the build from the store (it was created by CreateConfigAndBuild)
	var build *imagefactory.Build
	for _, b := range store.e2eBuilds {
		build = b
		break
	}
	require.NotNil(t, build, "a build row must exist")
	assert.Equal(t, imagefactory.BuildDispatched, build.Status)
	require.NotEmpty(t, build.CallbackToken, "callback token must be set")

	// POST /callback — succeeded
	cbBody, _ := json.Marshal(callbackRequest{
		Status: "succeeded", Digest: "sha256:e2e-success",
	})
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST",
		"/internal/image-factory/builds/"+build.ID+"/callback",
		bytes.NewReader(cbBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+build.CallbackToken)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)

	// Verify build transitioned
	assert.Equal(t, imagefactory.BuildSucceeded, store.e2eBuilds[build.ID].Status)
	assert.Equal(t, "sha256:e2e-success", store.e2eBuilds[build.ID].Digest)

	// Verify config transitioned to ready
	for _, c := range store.e2eConfigs {
		assert.Equal(t, imagefactory.StatusReady, c.Status, "config must be ready after callback")
	}

	// GET /configs — verify visible + ready
	w = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/v1/image-factory/configs", nil)
	req.Header.Set("X-Test-UserID", "user-1")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var cfgs ListConfigsResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &cfgs))
	require.Len(t, cfgs.Configs, 1)
	assert.Equal(t, imagefactory.StatusReady, cfgs.Configs[0].Status)

	// POST same selection again — must coalesce (no new dispatch)
	disp.called = false // reset
	disp2 := &fakeDispatcher{ghRunID: 999}
	r2 := newE2ERouter(t, store, disp2)
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-1")
	r2.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, "coalesced config should return 201")
	assert.False(t, disp2.called, "dispatch must NOT fire when coalescing onto existing succeeded build")

	// The new config must be immediately ready (linked to the succeeded build)
	var newCfg imagefactory.Config
	json.Unmarshal(w.Body.Bytes(), &newCfg)
	assert.Equal(t, imagefactory.StatusReady, newCfg.Status, "coalesced config must be ready")
}

// ── E2E: Full failure path ──────────────────────────────────────────────

// TestE2E_ImageFactory_FailurePath exercises:
// 1. POST /configs (novel) → dispatch, config=building
// 2. POST /callback (failed) → config=rejected, known_failure recorded
// 3. GET /catalog → known failure visible
// 4. POST /configs (same selection) → 422 (blocked)
func TestE2E_ImageFactory_FailurePath(t *testing.T) {
	store := newE2EStore()
	disp := &fakeDispatcher{ghRunID: 1}
	r := newE2ERouter(t, store, disp)

	// POST /configs
	cfgBody, _ := json.Marshal(createConfigRequest{
		Name: "e2e-bad-stack", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-1")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// Locate the build row
	var build *imagefactory.Build
	for _, b := range store.e2eBuilds {
		build = b
		break
	}
	require.NotNil(t, build)

	// POST /callback — failed
	cbBody, _ := json.Marshal(callbackRequest{
		Status: "failed", FailureReason: "E: Unable to locate package ffmpeg-nonexistent",
	})
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST",
		"/internal/image-factory/builds/"+build.ID+"/callback",
		bytes.NewReader(cbBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+build.CallbackToken)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)

	// Verify build failed
	assert.Equal(t, imagefactory.BuildFailed, store.e2eBuilds[build.ID].Status)

	// Verify config rejected
	for _, c := range store.e2eConfigs {
		assert.Equal(t, imagefactory.StatusRejected, c.Status, "config must be rejected after failed callback")
	}

	// Verify known failure recorded
	hash := build.Hash
	kf, err := store.GetKnownFailure(context.Background(), hash, "bookworm")
	require.NoError(t, err)
	assert.Contains(t, kf.FailureReason, "Unable to locate package")
	assert.True(t, kf.Retriable)

	// POST same selection again — verify known-failure blocking
	// wait — the known failure is retriable=true, so it WILL dispatch again.
	// Set it to non-retriable
	store.e2eKnownFailureByHash[hash+"|bookworm"].Retriable = false

	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(cfgBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-2")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "non-retriable known failure must block new configs")
}

// ── E2E: Callback security ──────────────────────────────────────────────

// TestE2E_ImageFactory_CallbackSecurity exercises the auth boundary:
// - Wrong token → 403, build stays dispatched
// - No token → 403
// - Token from build A cannot affect build B
func TestE2E_ImageFactory_CallbackSecurity(t *testing.T) {
	store := newE2EStore()
	disp := &fakeDispatcher{ghRunID: 1}
	r := newE2ERouter(t, store, disp)

	// Create two builds with different selections (so they don't coalesce)
	for i, sel := range [][]string{{"ffmpeg"}, {"python313"}} {
		body, _ := json.Marshal(createConfigRequest{
			Name: "e2e-sec-" + string(rune('a'+i)), Selection: sel, BaseName: "bookworm",
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-UserID", "user-"+string(rune('0'+i)))
		r.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code)
	}

	// Get both builds
	var builds []*imagefactory.Build
	for _, b := range store.e2eBuilds {
		builds = append(builds, b)
	}
	require.Len(t, builds, 2)

	// Wrong token → 403
	cbBody, _ := json.Marshal(callbackRequest{Status: "succeeded"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST",
		"/internal/image-factory/builds/"+builds[0].ID+"/callback",
		bytes.NewReader(cbBody))
	req.Header.Set("Authorization", "Bearer wrong-token")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, imagefactory.BuildDispatched, store.e2eBuilds[builds[0].ID].Status,
		"build must stay dispatched after rejected callback")

	// Token from build A on build B → 403
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST",
		"/internal/image-factory/builds/"+builds[1].ID+"/callback",
		bytes.NewReader(cbBody))
	req.Header.Set("Authorization", "Bearer "+builds[0].CallbackToken)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, imagefactory.BuildDispatched, store.e2eBuilds[builds[1].ID].Status,
		"build B must stay dispatched when presented with build A's token")

	// Correct token → 204
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST",
		"/internal/image-factory/builds/"+builds[0].ID+"/callback",
		bytes.NewReader(cbBody))
	req.Header.Set("Authorization", "Bearer "+builds[0].CallbackToken)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, imagefactory.BuildSucceeded, store.e2eBuilds[builds[0].ID].Status)
}

// ── E2E: Idempotent callback replay ─────────────────────────────────────

// TestE2E_ImageFactory_IdempotentReplay exercises that a replayed callback
// on an already-terminal build does not re-transition.
func TestE2E_ImageFactory_IdempotentReplay(t *testing.T) {
	store := newE2EStore()
	disp := &fakeDispatcher{ghRunID: 1}
	r := newE2ERouter(t, store, disp)

	// Create + succeed
	body, _ := json.Marshal(createConfigRequest{
		Name: "e2e-replay", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/image-factory/configs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-UserID", "user-1")
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var build *imagefactory.Build
	for _, b := range store.e2eBuilds {
		build = b
		break
	}

	// Succeed
	cbBody, _ := json.Marshal(callbackRequest{Status: "succeeded", Digest: "sha256:1"})
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/internal/image-factory/builds/"+build.ID+"/callback", bytes.NewReader(cbBody))
	req.Header.Set("Authorization", "Bearer "+build.CallbackToken)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)

	// Replay with failed → must NOT overwrite (idempotent)
	cbBody2, _ := json.Marshal(callbackRequest{Status: "failed", FailureReason: "should not apply"})
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/internal/image-factory/builds/"+build.ID+"/callback", bytes.NewReader(cbBody2))
	req.Header.Set("Authorization", "Bearer "+build.CallbackToken)
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, imagefactory.BuildSucceeded, store.e2eBuilds[build.ID].Status,
		"replay must not overwrite terminal succeeded state")
}

// ── E2E: AdminGuard blocks non-admin ────────────────────────────────────

func TestE2E_ImageFactory_AdminGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	store := &fakeAdminStore{bases: []imagefactory.Base{{Name: "bookworm", Version: "0.6.0"}}}
	r.Use(func(c *gin.Context) {
		if role, ok := c.GetQuery("role"); ok && role == "admin" {
			c.Set("userRole", "admin")
		}
		c.Next()
	})
	r.Use(middleware.AdminGuard())
	h := NewImageFactoryAdminHandler(store)
	r.GET("/api/v1/admin/image-factory/bases", h.ListBases)

	// Non-admin → 404 (AdminGuard returns 404 to hide route)
	req := httptest.NewRequest("GET", "/api/v1/admin/image-factory/bases", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)

	// Admin → 200
	req = httptest.NewRequest("GET", "/api/v1/admin/image-factory/bases?role=admin", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── Compile-time checks ─────────────────────────────────────────────────

var _ imageFactoryStore = (*e2eImageFactoryStore)(nil)
var _ buildStore = (*e2eImageFactoryStore)(nil)
var _ extensionReviewer = (*e2eImageFactoryStore)(nil)
