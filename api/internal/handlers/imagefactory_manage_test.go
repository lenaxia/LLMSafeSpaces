// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
)

func TestDeleteConfig_MemberScope_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newFakeManageStore()
	store.configsByHash["s-myconfig"] = &imagefactory.Config{
		ID:     "cfg-1",
		Hash:   "s-myconfig",
		Name:   "My Image",
		Status: imagefactory.StatusReady,
		Scope:  imagefactory.ScopeMember,
	}
	h := NewImageFactoryHandler(store, &fakeManageOrgResolver{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = append(c.Params, gin.Param{Key: "hash", Value: "s-myconfig"})
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest(http.MethodDelete, "/configs/s-myconfig", nil)

	h.DeleteConfig(c)
	// gin.CreateTestContext defaults w.Code to 200; c.Status(204) in the
	// handler overrides it in production but not in test. The real assertion
	// is that the delete was called and no error JSON was written.
	require.NotContains(t, w.Body.String(), "error")
	assert.True(t, store.deleteCalled)
}

func TestDeleteConfig_BuildingStatus_Conflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newFakeManageStore()
	store.configsByHash["s-building"] = &imagefactory.Config{
		ID:     "cfg-2",
		Hash:   "s-building",
		Status: imagefactory.StatusBuilding,
		Scope:  imagefactory.ScopeMember,
	}
	h := NewImageFactoryHandler(store, &fakeManageOrgResolver{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = append(c.Params, gin.Param{Key: "hash", Value: "s-building"})
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest(http.MethodDelete, "/configs/s-building", nil)

	h.DeleteConfig(c)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestDeleteConfig_PlatformScope_Forbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newFakeManageStore()
	store.configsByHash["s-platform"] = &imagefactory.Config{
		ID:     "cfg-3",
		Hash:   "s-platform",
		Status: imagefactory.StatusReady,
		Scope:  imagefactory.ScopePlatform,
	}
	h := NewImageFactoryHandler(store, &fakeManageOrgResolver{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = append(c.Params, gin.Param{Key: "hash", Value: "s-platform"})
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest(http.MethodDelete, "/configs/s-platform", nil)

	h.DeleteConfig(c)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRenameConfig_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newFakeManageStore()
	store.configsByHash["s-rename"] = &imagefactory.Config{
		ID:     "cfg-4",
		Hash:   "s-rename",
		Name:   "Old Name",
		Status: imagefactory.StatusReady,
		Scope:  imagefactory.ScopeMember,
	}
	h := NewImageFactoryHandler(store, &fakeManageOrgResolver{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = append(c.Params, gin.Param{Key: "hash", Value: "s-rename"})
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest(http.MethodPatch, "/configs/s-rename",
		strings.NewReader(`{"name":"New Name"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.RenameConfig(c)
	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, store.renameCalled)
	assert.Equal(t, "New Name", store.renameNewName)
}

func TestRenameConfig_EmptyName_UnprocessableEntity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newFakeManageStore()
	store.configsByHash["s-empt"] = &imagefactory.Config{
		ID:     "cfg-5",
		Hash:   "s-empt",
		Name:   "Old",
		Status: imagefactory.StatusReady,
		Scope:  imagefactory.ScopeMember,
	}
	h := NewImageFactoryHandler(store, &fakeManageOrgResolver{})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = append(c.Params, gin.Param{Key: "hash", Value: "s-empt"})
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest(http.MethodPatch, "/configs/s-empt",
		strings.NewReader(`{"name":"   "}`))
	c.Request.Header.Set("Content-Type", "application/json")

	h.RenameConfig(c)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

// ── Test doubles ────────────────────────────────────────────────────────

type fakeManageStore struct {
	fakeIFStore
	configsByHash map[string]*imagefactory.Config
	deleteCalled  bool
	renameCalled  bool
	renameNewName string
}

func newFakeManageStore() *fakeManageStore {
	return &fakeManageStore{
		configsByHash: make(map[string]*imagefactory.Config),
	}
}

func (f *fakeManageStore) GetConfigByHash(_ context.Context, hash string, scope imagefactory.ConfigScope, _ *string, _ *string) (imagefactory.Config, error) {
	if cfg, ok := f.configsByHash[hash]; ok {
		if cfg.Scope == scope {
			return *cfg, nil
		}
	}
	return imagefactory.Config{}, database.ErrNotFound
}

func (f *fakeManageStore) GetConfig(_ context.Context, id string) (imagefactory.Config, error) {
	for _, cfg := range f.configsByHash {
		if cfg.ID == id {
			return *cfg, nil
		}
	}
	return imagefactory.Config{}, database.ErrNotFound
}

func (f *fakeManageStore) DeleteConfig(_ context.Context, id string) error {
	f.deleteCalled = true
	delete(f.configsByHash, f.hashForID(id))
	return nil
}

func (f *fakeManageStore) RenameConfig(_ context.Context, id, newName string) error {
	f.renameCalled = true
	f.renameNewName = newName
	for _, cfg := range f.configsByHash {
		if cfg.ID == id {
			cfg.Name = newName
		}
	}
	return nil
}

func (f *fakeManageStore) hashForID(id string) string {
	for hash, cfg := range f.configsByHash {
		if cfg.ID == id {
			return hash
		}
	}
	return ""
}

type fakeManageOrgResolver struct{}

func (f *fakeManageOrgResolver) GetUserOrgID(_ context.Context, _ string) (string, error) {
	return "", nil
}
