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
	"github.com/lenaxia/llmsafespaces/api/internal/middleware"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// mockSettingsStore implements both InstanceStore and UserStore for testing.
type mockSettingsStore struct {
	instanceData map[string]json.RawMessage
	userData     map[string]map[string]json.RawMessage
}

func newMockSettingsStore() *mockSettingsStore {
	return &mockSettingsStore{
		instanceData: make(map[string]json.RawMessage),
		userData:     make(map[string]map[string]json.RawMessage),
	}
}

func (m *mockSettingsStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	cp := make(map[string]json.RawMessage, len(m.instanceData))
	for k, v := range m.instanceData {
		cp[k] = v
	}
	return cp, nil
}

func (m *mockSettingsStore) SetInstanceSetting(_ context.Context, key string, value json.RawMessage) error {
	m.instanceData[key] = value
	return nil
}

func (m *mockSettingsStore) GetAllUserSettings(_ context.Context, userID string) (map[string]json.RawMessage, error) {
	if m.userData[userID] == nil {
		return map[string]json.RawMessage{}, nil
	}
	cp := make(map[string]json.RawMessage, len(m.userData[userID]))
	for k, v := range m.userData[userID] {
		cp[k] = v
	}
	return cp, nil
}

func (m *mockSettingsStore) SetUserSetting(_ context.Context, userID, key string, value json.RawMessage) error {
	if m.userData[userID] == nil {
		m.userData[userID] = make(map[string]json.RawMessage)
	}
	m.userData[userID][key] = value
	return nil
}

type mockSettingsLogger struct{}

func (l *mockSettingsLogger) Debug(msg string, keysAndValues ...interface{})            {}
func (l *mockSettingsLogger) Info(msg string, keysAndValues ...interface{})             {}
func (l *mockSettingsLogger) Warn(msg string, keysAndValues ...interface{})             {}
func (l *mockSettingsLogger) Error(msg string, err error, keysAndValues ...interface{}) {}
func (l *mockSettingsLogger) Fatal(msg string, err error, keysAndValues ...interface{}) {}
func (l *mockSettingsLogger) With(keysAndValues ...interface{}) pkginterfaces.LoggerInterface {
	return l
}
func (l *mockSettingsLogger) Sync() error { return nil }

func setupSettingsRouter(role string) (*gin.Engine, *mockSettingsStore) {
	gin.SetMode(gin.TestMode)
	store := newMockSettingsStore()
	var logger pkginterfaces.LoggerInterface = &mockSettingsLogger{}

	instanceSvc := settings.NewInstanceService(store, logger)
	userSvc := settings.NewUserService(store, logger)
	handler := NewSettingsHandler(instanceSvc, userSvc)

	r := gin.New()

	// Simulate auth middleware setting userID and userRole
	r.Use(func(c *gin.Context) {
		c.Set("userID", "test-user-1")
		c.Set("userRole", role)
		c.Next()
	})

	// Admin routes
	admin := r.Group("/api/v1/admin/settings")
	admin.Use(middleware.AdminGuard())
	admin.GET("", handler.GetAdminSettings)
	admin.GET("/schema", handler.GetAdminSettingsSchema)
	admin.PUT("/:key", handler.SetAdminSetting)

	// User routes
	user := r.Group("/api/v1/users/me/settings")
	user.GET("", handler.GetUserSettings)
	user.GET("/schema", handler.GetUserSettingsSchema)
	user.PUT("/:key", handler.SetUserSetting)

	return r, store
}

func TestAdminSettings_GET_ReturnsAllSettings(t *testing.T) {
	r, _ := setupSettingsRouter("admin")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/admin/settings", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["schemaVersion"] == nil {
		t.Error("expected schemaVersion in response")
	}
	settingsMap, ok := resp["settings"].(map[string]any)
	if !ok {
		t.Fatal("expected settings map in response")
	}
	if len(settingsMap) != len(settings.InstanceSettings()) {
		t.Errorf("expected %d settings, got %d", len(settings.InstanceSettings()), len(settingsMap))
	}
}

func TestAdminSettings_GET_NonAdminGets404(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/admin/settings", nil)
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404 for non-admin, got %d", w.Code)
	}
}

func TestAdminSettings_Schema_ReturnsFullSchema(t *testing.T) {
	r, _ := setupSettingsRouter("admin")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/admin/settings/schema", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	defs, ok := resp["settings"].([]any)
	if !ok {
		t.Fatal("expected settings array in schema response")
	}
	if len(defs) != len(settings.InstanceSettings()) {
		t.Errorf("expected %d definitions, got %d", len(settings.InstanceSettings()), len(defs))
	}
}

func TestAdminSettings_PUT_ValidValue(t *testing.T) {
	r, _ := setupSettingsRouter("admin")

	body := `{"value": 10}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/admin/settings/auth.lockoutAttempts", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAdminSettings_PUT_InvalidValue(t *testing.T) {
	r, _ := setupSettingsRouter("admin")

	body := `{"value": 999}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/admin/settings/auth.lockoutAttempts", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for out-of-range value, got %d: %s", w.Code, w.Body.String())
	}
}

// HTTP-level boundary for the floating-tag fix (2026-08-15): the router
// wiring must surface the RejectMutableTags validation as 400 with the
// pinning message — not just the service-level Set (unit-tested in
// pkg/settings). This is the exact write an admin would attempt.
func TestAdminSettings_PUT_WorkspaceDefaultImage_FloatingTagsRejected(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"latest", "ghcr.io/lenaxia/llmsafespaces/base:latest"},
		{"dev", "ghcr.io/lenaxia/llmsafespaces/base:dev"},
		{"stable alias", "ghcr.io/lenaxia/llmsafespaces/base:stable"},
		{"untagged implicit latest", "ghcr.io/lenaxia/llmsafespaces/base"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := setupSettingsRouter("admin")

			body, _ := json.Marshal(map[string]string{"value": tc.value})
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPut, "/api/v1/admin/settings/workspace.defaultImage", bytes.NewBuffer(body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)

			if w.Code != 400 {
				t.Errorf("expected 400 for %q, got %d: %s", tc.value, w.Code, w.Body.String())
			}
			if !bytes.Contains(w.Body.Bytes(), []byte("mutable")) &&
				!bytes.Contains(w.Body.Bytes(), []byte(":latest")) {
				t.Errorf("error should explain the pinning requirement, got: %s", w.Body.String())
			}
		})
	}
}

func TestAdminSettings_PUT_WorkspaceDefaultImage_PinnedAccepted(t *testing.T) {
	r, store := setupSettingsRouter("admin")

	body := `{"value": "ghcr.io/lenaxia/llmsafespaces/base:0.15.5"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/admin/settings/workspace.defaultImage", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200 for pinned tag, got %d: %s", w.Code, w.Body.String())
	}
	if got := string(store.instanceData["workspace.defaultImage"]); got != `"ghcr.io/lenaxia/llmsafespaces/base:0.15.5"` {
		t.Errorf("pinned value not persisted, store has %s", got)
	}
}

func TestAdminSettings_PUT_UnknownKey(t *testing.T) {
	r, _ := setupSettingsRouter("admin")

	body := `{"value": true}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/admin/settings/nonexistent.key", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for unknown key, got %d", w.Code)
	}
}

func TestAdminSettings_PUT_NonAdminGets404(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	body := `{"value": 10}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/admin/settings/auth.lockoutAttempts", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Errorf("expected 404 for non-admin PUT, got %d", w.Code)
	}
}

func TestUserSettings_GET_ReturnsAllSettings(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/users/me/settings", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	settingsMap, ok := resp["settings"].(map[string]any)
	if !ok {
		t.Fatal("expected settings map in response")
	}
	if len(settingsMap) != len(settings.UserSettings()) {
		t.Errorf("expected %d settings, got %d", len(settings.UserSettings()), len(settingsMap))
	}
}

func TestUserSettings_Schema_ReturnsFullSchema(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/users/me/settings/schema", nil)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestUserSettings_PUT_ValidValue(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	body := `{"value": "dark"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/users/me/settings/theme", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserSettings_PUT_InvalidEnum(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	body := `{"value": "neon"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/users/me/settings/theme", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for invalid enum, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUserSettings_PUT_UnknownKey(t *testing.T) {
	r, _ := setupSettingsRouter("user")

	body := `{"value": true}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/v1/users/me/settings/nonexistent", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != 400 {
		t.Errorf("expected 400 for unknown key, got %d", w.Code)
	}
}
