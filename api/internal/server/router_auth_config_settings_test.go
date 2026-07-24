// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	apilogger "github.com/lenaxia/llmsafespaces/api/internal/logger"
	imocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type settingsStore struct {
	data map[string]json.RawMessage
}

func (s *settingsStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	return s.data, nil
}
func (s *settingsStore) SetInstanceSetting(_ context.Context, key string, value json.RawMessage) error {
	s.data[key] = value
	return nil
}

func newAuthConfigWithSettings(t *testing.T, vals map[string]any) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	data := make(map[string]json.RawMessage)
	for k, v := range vals {
		raw, _ := json.Marshal(v)
		data[k] = raw
	}
	instanceSettings := settings.NewInstanceService(&settingsStore{data: data}, nil)
	instanceSettings.Start()

	apiLog, _ := apilogger.New(false, "error", "json")
	auth := &imocks.MockAuthMiddlewareService{}
	met := &imocks.MockMetricsService{}
	met.On("RecordRequest", mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Maybe()
	auth.On("AuthMiddleware").Return(gin.HandlerFunc(func(c *gin.Context) { c.Next() })).Maybe()
	auth.On("GetUserID", mock.Anything).Return("").Maybe()

	svc := &authMockServices{auth: auth, metrics: met, database: &imocks.MockDatabaseService{}, cache: &imocks.MockCacheService{}}
	router := NewRouter(svc, apiLog, nil, RouterConfig{
		Debug:            false,
		InstanceSettings: instanceSettings,
	})
	return router
}

func TestAuthConfig_ReturnsInstanceNameFromSettings(t *testing.T) {
	router := newAuthConfigWithSettings(t, map[string]any{
		"instance.name": "My Corp AI",
		"instance.motd": "Welcome to the AI sandbox!",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var cfg types.AuthConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.Equal(t, "My Corp AI", cfg.InstanceName)
	assert.Equal(t, "Welcome to the AI sandbox!", cfg.MOTD)
}

func TestAuthConfig_DefaultInstanceName_WhenNotSet(t *testing.T) {
	router := newAuthConfigWithSettings(t, map[string]any{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var cfg types.AuthConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.Equal(t, "LLMSafeSpaces", cfg.InstanceName)
	assert.Empty(t, cfg.MOTD)
}

// TestAuthConfig_MOTDKeyAlwaysPresentInJSON is the regression test for the
// SDK canary failure on PR #595: when instance.motd is unset (the default
// for every fresh install), the motd JSON key was omitted from the response
// because AuthConfig.MOTD had json:"motd,omitempty". The Go SDK canary
// (sdks/canary/go/scenarios/s-auth-config) and the frontend both expect the
// key to always be present — an empty string is a valid "no message" value.
// This test unmarshals into a raw map (like the canary does) rather than a
// typed struct so it catches the omitempty omission the struct-based test
// above cannot.
func TestAuthConfig_MOTDKeyAlwaysPresentInJSON(t *testing.T) {
	router := newAuthConfigWithSettings(t, map[string]any{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.Contains(t, raw, "motd",
		"motd key must always be present in /auth/config JSON, even when empty — "+
			"frontend clients and SDK canaries reference it unconditionally")
	assert.Equal(t, "", raw["motd"])
}

func TestAuthConfig_RegistrationDisabledFromSettings(t *testing.T) {
	router := newAuthConfigWithSettings(t, map[string]any{
		"auth.registrationEnabled": false,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var cfg types.AuthConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.False(t, cfg.RegistrationEnabled)
}

func TestAuthConfig_EmptyInstanceName_FallsBackToDefault(t *testing.T) {
	router := newAuthConfigWithSettings(t, map[string]any{
		"instance.name": "", // explicitly empty
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/config", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	var cfg types.AuthConfig
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cfg))
	assert.Equal(t, "LLMSafeSpaces", cfg.InstanceName) // falls back to default
}
