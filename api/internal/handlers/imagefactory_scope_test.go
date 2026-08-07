// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/imagefactory"
)

// --- Org/Platform-scoped config creation (design/0047 D1) ---

// TestCreateOrgConfig_SetsOrgScope verifies the org-scoped create path
// produces a config with Scope=org and the correct OrgID.
func TestCreateOrgConfig_SetsOrgScope(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 42}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)

	body, _ := json.Marshal(createConfigRequest{
		Name: "org-python", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "admin-1")
	c.Params = gin.Params{{Key: "id", Value: "org-999"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.CreateOrgConfig(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.True(t, disp.called, "dispatch must fire for a novel build")
	require.NotNil(t, store.lastCreatedConfig)
	assert.Equal(t, imagefactory.ScopeOrg, store.lastCreatedConfig.Scope)
	require.NotNil(t, store.lastCreatedConfig.OrgID)
	assert.Equal(t, "org-999", *store.lastCreatedConfig.OrgID)
	assert.Nil(t, store.lastCreatedConfig.OwnerID, "org-scope config must not have owner_id")

	// Build row must carry scope + org_id for billing attribution (design/0047 Q1).
	require.NotNil(t, store.lastCreatedBuild, "build row must be committed")
	assert.Equal(t, imagefactory.ScopeOrg, store.lastCreatedBuild.Scope)
	require.NotNil(t, store.lastCreatedBuild.OrgID)
	assert.Equal(t, "org-999", *store.lastCreatedBuild.OrgID)
}

// TestCreatePlatformConfig_SetsPlatformScope verifies the platform-scoped
// create path produces a config with Scope=platform and no owner/org.
func TestCreatePlatformConfig_SetsPlatformScope(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 99}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)

	body, _ := json.Marshal(createConfigRequest{
		Name: "platform-go", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "admin-1")
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.CreatePlatformConfig(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.True(t, disp.called)
	require.NotNil(t, store.lastCreatedConfig)
	assert.Equal(t, imagefactory.ScopePlatform, store.lastCreatedConfig.Scope)
	assert.Nil(t, store.lastCreatedConfig.OwnerID)
	assert.Nil(t, store.lastCreatedConfig.OrgID)

	// Build row must carry scope for billing attribution (design/0047 Q1).
	require.NotNil(t, store.lastCreatedBuild)
	assert.Equal(t, imagefactory.ScopePlatform, store.lastCreatedBuild.Scope)
	assert.Nil(t, store.lastCreatedBuild.OrgID)
}

// TestCreateConfig_MemberScope_Unchanged verifies the member-scope path
// still works correctly after the refactor (no regression).
func TestCreateConfig_MemberScope_Unchanged(t *testing.T) {
	t.Parallel()
	store := s4Store()
	disp := &fakeDispatcher{ghRunID: 7}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)

	body, _ := json.Marshal(createConfigRequest{
		Name: "my-python", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "user-1")
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.CreateConfig(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, store.lastCreatedConfig)
	assert.Equal(t, imagefactory.ScopeMember, store.lastCreatedConfig.Scope)
	require.NotNil(t, store.lastCreatedConfig.OwnerID)
	assert.Equal(t, "user-1", *store.lastCreatedConfig.OwnerID)

	// Build row must carry scope for billing attribution (design/0047 Q1).
	require.NotNil(t, store.lastCreatedBuild)
	assert.Equal(t, imagefactory.ScopeMember, store.lastCreatedBuild.Scope)
	assert.Nil(t, store.lastCreatedBuild.OrgID)
}

// TestCreateOrgConfig_CoalescesOnExistingBuild verifies cross-scope
// coalescing (design/0047 Q2): an org config creation coalesces onto an
// existing build without dispatching.
func TestCreateOrgConfig_CoalescesOnExistingBuild(t *testing.T) {
	t.Parallel()
	store := s4Store()
	store.existingBuild = &imagefactory.Build{
		Status: imagefactory.BuildSucceeded,
	}
	disp := &fakeDispatcher{ghRunID: 1}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)
	h.SetDispatcher(disp)

	body, _ := json.Marshal(createConfigRequest{
		Name: "org-coalesce", Selection: []string{"ffmpeg"}, BaseName: "bookworm",
	})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("userID", "admin-1")
	c.Params = gin.Params{{Key: "id", Value: "org-1"}}
	c.Request = httptest.NewRequest("POST", "/", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	h.CreateOrgConfig(c)

	require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
	assert.False(t, disp.called, "dispatch must NOT fire when coalescing")
	assert.False(t, store.calledCreateBuild, "build row must NOT be created when coalescing")
	assert.True(t, store.calledCreateConfig, "config row must be created (linked to existing build)")
	require.NotNil(t, store.lastCreatedConfig)
	assert.Equal(t, imagefactory.StatusReady, store.lastCreatedConfig.Status)
	assert.Equal(t, imagefactory.ScopeOrg, store.lastCreatedConfig.Scope)
}

// --- Extended Delete/Rename (design/0047 D2) ---

// TestCanMutateScope_OrgAdmin_CanDeleteOrgConfig verifies an org admin
// can delete an org-scoped config.
func TestCanMutateScope_OrgAdmin_CanDeleteOrgConfig(t *testing.T) {
	t.Parallel()
	orgID := "org-1"
	store := &fakeIFStore{
		configByHash: map[string]imagefactory.Config{
			"hash-1": {ID: "cfg-1", Hash: "hash-1", Scope: imagefactory.ScopeOrg, OrgID: &orgID, Status: imagefactory.StatusReady},
		},
	}
	orgs := &fakeOrgResolver{
		orgIDByUser: map[string]string{"admin-1": "org-1"},
		adminUsers:  map[string]bool{"admin-1:org-1": true},
	}
	h := NewImageFactoryHandler(store, orgs)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "admin-1"); c.Next() })
	r.DELETE("/configs/:hash", h.DeleteConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/configs/hash-1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code, "org admin should be able to delete org config; body: %s", w.Body.String())
}

// TestCanMutateScope_OrgMember_CannotDeleteOrgConfig verifies a regular
// org member (not admin) cannot delete org-scoped configs. This is the
// privilege-escalation regression: the initial implementation checked org
// membership instead of admin status.
func TestCanMutateScope_OrgMember_CannotDeleteOrgConfig(t *testing.T) {
	t.Parallel()
	orgID := "org-1"
	store := &fakeIFStore{
		configByHash: map[string]imagefactory.Config{
			"hash-1": {ID: "cfg-1", Hash: "hash-1", Scope: imagefactory.ScopeOrg, OrgID: &orgID, Status: imagefactory.StatusReady},
		},
	}
	orgs := &fakeOrgResolver{
		orgIDByUser: map[string]string{"member-1": "org-1"},
		adminUsers:  map[string]bool{}, // member-1 is NOT an admin
	}
	h := NewImageFactoryHandler(store, orgs)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "member-1"); c.Next() })
	r.DELETE("/configs/:hash", h.DeleteConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/configs/hash-1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "org member (not admin) should not delete org config")
}

// TestCanMutateScope_NonOrgMember_CannotDeleteOrgConfig verifies a user
// outside the org cannot delete an org-scoped config.
func TestCanMutateScope_NonOrgMember_CannotDeleteOrgConfig(t *testing.T) {
	t.Parallel()
	orgID := "org-1"
	store := &fakeIFStore{
		configByHash: map[string]imagefactory.Config{
			"hash-1": {ID: "cfg-1", Hash: "hash-1", Scope: imagefactory.ScopeOrg, OrgID: &orgID, Status: imagefactory.StatusReady},
		},
	}
	orgs := &fakeOrgResolver{
		orgIDByUser: map[string]string{"user-2": "org-2"},
		adminUsers:  map[string]bool{},
	}
	h := NewImageFactoryHandler(store, orgs)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "user-2"); c.Next() })
	r.DELETE("/configs/:hash", h.DeleteConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/configs/hash-1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "non-org-member should not delete org config")
}

// TestCanMutateScope_PlatformAdmin_CanDeletePlatformConfig verifies a
// platform admin can delete a platform-scoped config.
func TestCanMutateScope_PlatformAdmin_CanDeletePlatformConfig(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{
		configByHash: map[string]imagefactory.Config{
			"hash-1": {ID: "cfg-1", Hash: "hash-1", Scope: imagefactory.ScopePlatform, Status: imagefactory.StatusReady},
		},
	}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "admin-1"); c.Set("userRole", "admin"); c.Next() })
	r.DELETE("/configs/:hash", h.DeleteConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/configs/hash-1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code, "platform admin should delete platform config; body: %s", w.Body.String())
}

// TestCanMutateScope_RegularUser_CannotDeletePlatformConfig verifies a
// regular member cannot delete a platform-scoped config.
func TestCanMutateScope_RegularUser_CannotDeletePlatformConfig(t *testing.T) {
	t.Parallel()
	store := &fakeIFStore{
		configByHash: map[string]imagefactory.Config{
			"hash-1": {ID: "cfg-1", Hash: "hash-1", Scope: imagefactory.ScopePlatform, Status: imagefactory.StatusReady},
		},
	}
	orgs := &fakeOrgResolver{}
	h := NewImageFactoryHandler(store, orgs)

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Set("userRole", "member"); c.Next() })
	r.DELETE("/configs/:hash", h.DeleteConfig)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/configs/hash-1", nil)
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code, "regular user should not delete platform config")
}
