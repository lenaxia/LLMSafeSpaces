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

	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// TestIntegration_FullSettingsLifecycle exercises the complete flow:
// seed → admin reads defaults → admin writes → admin reads updated → user writes → user reads
func TestIntegration_FullSettingsLifecycle(t *testing.T) {
	r, store := setupSettingsRouter("admin")

	// 1. Seed the store (simulates app startup)
	seedStore := &seedableStore{store: store}
	result, err := settings.Seed(context.Background(), seedStore, nil)
	if err != nil {
		t.Fatalf("seed failed: %v", err)
	}
	if result.Inserted != len(settings.InstanceSettings()) {
		t.Fatalf("expected %d seeded, got %d", len(settings.InstanceSettings()), result.Inserted)
	}

	// 2. Admin reads all settings — should have seeded defaults
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/admin/settings", nil)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("GET admin settings: %d", w.Code)
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	allSettings := resp["settings"].(map[string]any)
	// auth.registrationEnabled should be true (schema default)
	if allSettings["auth.registrationEnabled"] != true {
		t.Errorf("expected default true, got %v", allSettings["auth.registrationEnabled"])
	}

	// 3. Admin writes a new value
	body := `{"value": false}`
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/v1/admin/settings/auth.registrationEnabled", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT: %d %s", w.Code, w.Body.String())
	}

	// 4. Admin reads again — should see updated value
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/admin/settings", nil)
	r.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &resp)
	allSettings = resp["settings"].(map[string]any)
	if allSettings["auth.registrationEnabled"] != false {
		t.Errorf("expected false after write, got %v", allSettings["auth.registrationEnabled"])
	}

	// 5. Re-seed — should NOT overwrite the admin's change
	result2, _ := settings.Seed(context.Background(), seedStore, nil)
	if result2.Inserted != 0 {
		t.Errorf("re-seed should insert 0 (all exist), got %d", result2.Inserted)
	}

	// 6. Verify admin's value persisted through re-seed
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/admin/settings", nil)
	r.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &resp)
	allSettings = resp["settings"].(map[string]any)
	if allSettings["auth.registrationEnabled"] != false {
		t.Errorf("re-seed overwrote admin value!")
	}
}

// TestIntegration_UserSettingsIsolation verifies that user settings are
// isolated between users and don't affect admin settings.
func TestIntegration_UserSettingsIsolation(t *testing.T) {
	// Setup two "users" by creating two routers with different userIDs
	r1, store := setupSettingsRouterWithUserID("user-alice", "user")
	r2, _ := setupSettingsRouterWithStore("user-bob", "user", store)

	// Alice sets theme to dark
	body := `{"value": "dark"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/users/me/settings/theme", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r1.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("alice PUT: %d", w.Code)
	}

	// Bob reads — should get default (system), not Alice's value
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/users/me/settings", nil)
	r2.ServeHTTP(w, req)
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	bobSettings := resp["settings"].(map[string]any)
	if bobSettings["theme"] != "system" {
		t.Errorf("bob should see default 'system', got %v", bobSettings["theme"])
	}

	// Alice reads — should see her value
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/v1/users/me/settings", nil)
	r1.ServeHTTP(w, req)
	json.Unmarshal(w.Body.Bytes(), &resp)
	aliceSettings := resp["settings"].(map[string]any)
	if aliceSettings["theme"] != "dark" {
		t.Errorf("alice should see 'dark', got %v", aliceSettings["theme"])
	}
}

// TestIntegration_AdminCannotAccessUserSettings verifies admin settings
// and user settings are completely separate namespaces.
func TestIntegration_AdminCannotAccessUserSettings(t *testing.T) {
	r, _ := setupSettingsRouter("admin")

	// Try to set a user setting key via admin endpoint — should fail
	body := `{"value": "dark"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/admin/settings/theme", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 for user key on admin endpoint, got %d", w.Code)
	}
}

// TestIntegration_UserCannotAccessAdminSettings verifies user cannot
// set admin settings via user endpoint.
func TestIntegration_UserCannotAccessAdminSettings(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	body := `{"value": false}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/users/me/settings/auth.registrationEnabled", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 400 {
		t.Errorf("expected 400 for admin key on user endpoint, got %d", w.Code)
	}
}

// --- Helpers ---

// seedableStore wraps mockSettingsStore to implement SeedStore.
type seedableStore struct {
	store *mockSettingsStore
}

func (s *seedableStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	return s.store.instanceData, nil
}

func (s *seedableStore) InsertInstanceSettingIfMissing(_ context.Context, key string, value json.RawMessage) (bool, error) {
	if _, exists := s.store.instanceData[key]; exists {
		return false, nil
	}
	s.store.instanceData[key] = value
	return true, nil
}

func setupSettingsRouterWithUserID(userID, role string) (*gin.Engine, *mockSettingsStore) {
	gin.SetMode(gin.TestMode)
	store := newMockSettingsStore()
	var logger pkginterfaces.LoggerInterface = &mockSettingsLogger{}
	instanceSvc := settings.NewInstanceService(store, logger)
	userSvc := settings.NewUserService(store, logger)
	handler := NewSettingsHandler(instanceSvc, userSvc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("userRole", role)
		c.Next()
	})

	admin := r.Group("/api/v1/admin/settings")
	admin.Use(middleware.AdminGuard())
	admin.GET("", handler.GetAdminSettings)
	admin.GET("/schema", handler.GetAdminSettingsSchema)
	admin.PUT("/:key", handler.SetAdminSetting)

	user := r.Group("/api/v1/users/me/settings")
	user.GET("", handler.GetUserSettings)
	user.GET("/schema", handler.GetUserSettingsSchema)
	user.PUT("/:key", handler.SetUserSetting)

	return r, store
}

func setupSettingsRouterWithStore(userID, role string, store *mockSettingsStore) (*gin.Engine, *mockSettingsStore) {
	gin.SetMode(gin.TestMode)
	var logger pkginterfaces.LoggerInterface = &mockSettingsLogger{}
	instanceSvc := settings.NewInstanceService(store, logger)
	userSvc := settings.NewUserService(store, logger)
	handler := NewSettingsHandler(instanceSvc, userSvc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userID", userID)
		c.Set("userRole", role)
		c.Next()
	})

	user := r.Group("/api/v1/users/me/settings")
	user.GET("", handler.GetUserSettings)
	user.GET("/schema", handler.GetUserSettingsSchema)
	user.PUT("/:key", handler.SetUserSetting)

	return r, store
}

// TestIntegration_HelmManagedSetting_RejectsWrite verifies that PUT on a
// helm-managed (read-only) setting returns 409 Conflict, not 200 or 400.
// This is the US-49.2 contract: helm-managed keys are immutable via the API.
func TestIntegration_HelmManagedSetting_RejectsWrite(t *testing.T) {
	_, store := setupSettingsRouter("admin")
	seedStore := &seedableStore{store: store}
	if _, err := settings.Seed(context.Background(), seedStore, nil); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// Re-construct the handler with a helm-managed key to simulate email.enabled=true.
	gin.SetMode(gin.TestMode)
	instanceSvc := settings.NewInstanceService(store, &mockSettingsLogger{})
	instanceSvc.SetHelmOverrides(map[string]any{
		"email.provider": "ses",
	})
	userSvc := settings.NewUserService(store, &mockSettingsLogger{})
	handler := NewSettingsHandler(instanceSvc, userSvc)
	r2 := gin.New()
	r2.Use(func(c *gin.Context) {
		c.Set("userID", "admin")
		c.Set("userRole", "admin")
		c.Next()
	})
	admin := r2.Group("/api/v1/admin/settings")
	admin.Use(middleware.AdminGuard())
	admin.PUT("/:key", handler.SetAdminSetting)

	// PUT on the helm-managed key must return 409.
	body := `{"value": "noop"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/admin/settings/email.provider", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r2.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for helm-managed setting, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	assert.Contains(t, resp["error"], "Helm")
}

// TestIntegration_HelmManagedSetting_SchemaReadOnly verifies the schema
// endpoint reports readOnly=true for helm-managed keys so the frontend can
// disable the field.
func TestIntegration_HelmManagedSetting_SchemaReadOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newMockSettingsStore()
	instanceSvc := settings.NewInstanceService(store, &mockSettingsLogger{})
	instanceSvc.SetHelmOverrides(map[string]any{
		"email.provider": "ses",
	})
	userSvc := settings.NewUserService(store, &mockSettingsLogger{})
	handler := NewSettingsHandler(instanceSvc, userSvc)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set("userRole", "admin")
		c.Next()
	})
	admin := r.Group("/api/v1/admin/settings")
	admin.Use(middleware.AdminGuard())
	admin.GET("/schema", handler.GetAdminSettingsSchema)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/admin/settings/schema", nil)
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Settings []settings.SettingDef `json:"settings"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	for _, def := range resp.Settings {
		if def.Key == "email.provider" {
			assert.True(t, def.ReadOnly, "helm-managed key must be readOnly in schema")
			return
		}
	}
	t.Fatal("email.provider not found in schema response")
}
