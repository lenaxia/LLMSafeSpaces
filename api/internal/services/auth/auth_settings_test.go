// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	mocks "github.com/lenaxia/llmsafespaces/api/internal/mocks"
	lmocks "github.com/lenaxia/llmsafespaces/mocks/logger"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type stubStore struct {
	data map[string]json.RawMessage
}

func (s *stubStore) GetAllInstanceSettings(_ context.Context) (map[string]json.RawMessage, error) {
	return s.data, nil
}
func (s *stubStore) SetInstanceSetting(_ context.Context, _ string, _ json.RawMessage) error {
	return nil
}

func newSettingsService(vals map[string]any) *settings.InstanceService {
	data := make(map[string]json.RawMessage)
	for k, v := range vals {
		raw, _ := json.Marshal(v)
		data[k] = raw
	}
	var log pkginterfaces.LoggerInterface = lmocks.NewMockLogger()
	svc := settings.NewInstanceService(&stubStore{data: data}, log)
	_ = svc.Start()
	return svc
}

func newLockoutServiceWithSettings(t *testing.T, settingsData map[string]any) (*Service, *mocks.MockDatabaseService, *mocks.MockCacheService) {
	t.Helper()
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	// Static config: lockout DISABLED (settings should override)
	cfg.Auth.LockoutEnabled = false
	cfg.Auth.LockoutAttempts = 99
	cfg.Auth.LockoutDuration = 1 * time.Hour
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	svc, err := New(cfg, log, mockDb, mockCache)
	require.NoError(t, err)
	if settingsData != nil {
		svc.SetInstanceSettings(newSettingsService(settingsData))
	}
	return svc, mockDb, mockCache
}

// === US-13.11: Auth lockout reads from instance settings ===

func TestLogin_LockoutFromSettings_Enabled(t *testing.T) {
	// Static config has lockout DISABLED, but settings enable it with 2 attempts
	svc, _, mockCache := newLockoutServiceWithSettings(t, map[string]any{
		"auth.lockoutEnabled":         true,
		"auth.lockoutAttempts":        2,
		"auth.lockoutDurationMinutes": 5,
	})
	ctx := context.Background()

	// User has 2 failed attempts already
	mockCache.On("Get", ctx, "lockout:test@e.com").Return("2", nil)

	_, err := svc.Login(ctx, types.LoginRequest{Email: "test@e.com", Password: "x"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "locked")
}

func TestLogin_LockoutFromSettings_Disabled(t *testing.T) {
	// Static config has lockout ENABLED, but settings disable it
	log, _ := logger.New(true, "debug", "console")
	cfg := &config.Config{}
	cfg.Auth.JWTSecret = "test-secret-1234567890"
	cfg.Auth.TokenDuration = 24 * time.Hour
	cfg.Auth.APIKeyPrefix = "lsp_"
	cfg.Auth.LockoutEnabled = true // static says enabled
	cfg.Auth.LockoutAttempts = 1
	cfg.Auth.LockoutDuration = 15 * time.Minute
	mockDb := new(mocks.MockDatabaseService)
	mockCache := new(mocks.MockCacheService)
	svc, _ := New(cfg, log, mockDb, mockCache)
	svc.SetInstanceSettings(newSettingsService(map[string]any{
		"auth.lockoutEnabled": false, // settings say disabled
	}))
	ctx := context.Background()

	// Even with 99 failed attempts, lockout is disabled via settings
	mockCache.On("Get", ctx, mock.Anything).Return("99", nil).Maybe()
	mockDb.On("GetUserByEmail", ctx, "test@e.com").Return(nil, nil)

	_, err := svc.Login(ctx, types.LoginRequest{Email: "test@e.com", Password: "x"})
	// Should NOT be "locked" error — should be generic auth error
	require.Error(t, err)
	require.NotContains(t, err.Error(), "locked")
}

func TestLogin_LockoutFromSettings_NilSettings_FallsBackToConfig(t *testing.T) {
	// No settings injected — should use static config (lockout disabled)
	svc, mockDb, mockCache := newLockoutServiceWithSettings(t, nil)
	ctx := context.Background()

	mockCache.On("Get", ctx, mock.Anything).Return("99", nil).Maybe()
	mockDb.On("GetUserByEmail", ctx, "test@e.com").Return(nil, nil)

	_, err := svc.Login(ctx, types.LoginRequest{Email: "test@e.com", Password: "x"})
	// Config has lockout disabled, so no lock error
	require.Error(t, err)
	require.NotContains(t, err.Error(), "locked")
}

func TestRecordFailedAttempt_UsesSettingsDuration(t *testing.T) {
	svc, _, mockCache := newLockoutServiceWithSettings(t, map[string]any{
		"auth.lockoutEnabled":         true,
		"auth.lockoutAttempts":        5,
		"auth.lockoutDurationMinutes": 10,
	})
	ctx := context.Background()

	mockCache.On("Get", ctx, "lockout:x@e.com").Return("", errors.New("miss"))
	mockCache.On("Set", ctx, "lockout:x@e.com", "1", 10*time.Minute).Return(nil)

	svc.recordFailedAttempt(ctx, "x@e.com")
	mockCache.AssertExpectations(t)
}

func TestClearFailedAttempts_SettingsDisabled_NoOp(t *testing.T) {
	svc, _, mockCache := newLockoutServiceWithSettings(t, map[string]any{
		"auth.lockoutEnabled": false,
	})
	ctx := context.Background()

	// Should NOT call Delete on cache when lockout is disabled
	svc.clearFailedAttempts(ctx, "test@e.com")
	mockCache.AssertNotCalled(t, "Delete", mock.Anything, mock.Anything)
}

func TestClearFailedAttempts_SettingsEnabled_DeletesKey(t *testing.T) {
	svc, _, mockCache := newLockoutServiceWithSettings(t, map[string]any{
		"auth.lockoutEnabled": true,
	})
	ctx := context.Background()

	mockCache.On("Delete", ctx, "lockout:test@e.com").Return(nil)
	svc.clearFailedAttempts(ctx, "test@e.com")
	mockCache.AssertCalled(t, "Delete", ctx, "lockout:test@e.com")
}
