// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mocks

import (
	"context"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// MockCacheService implements the CacheService interface for testing.
type MockCacheService struct {
	mock.Mock
}

// Compile-time check against the real interface.
var _ interfaces.CacheService = (*MockCacheService)(nil)

func (m *MockCacheService) Start() error { return m.Called().Error(0) }
func (m *MockCacheService) Stop() error  { return m.Called().Error(0) }

func (m *MockCacheService) Get(ctx context.Context, key string) (string, error) {
	args := m.Called(ctx, key)
	return args.String(0), args.Error(1)
}

func (m *MockCacheService) Set(ctx context.Context, key string, value string, expiration time.Duration) error {
	return m.Called(ctx, key, value, expiration).Error(0)
}

func (m *MockCacheService) SetNX(ctx context.Context, key string, value string, expiration time.Duration) (bool, error) {
	args := m.Called(ctx, key, value, expiration)
	return args.Bool(0), args.Error(1)
}

func (m *MockCacheService) Incr(ctx context.Context, key string, expiration time.Duration) (int64, error) {
	args := m.Called(ctx, key, expiration)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockCacheService) Delete(ctx context.Context, key string) error {
	return m.Called(ctx, key).Error(0)
}

func (m *MockCacheService) DeleteByPrefix(ctx context.Context, prefix string) error {
	return m.Called(ctx, prefix).Error(0)
}

func (m *MockCacheService) GetObject(ctx context.Context, key string, value interface{}) error {
	return m.Called(ctx, key, value).Error(0)
}

func (m *MockCacheService) SetObject(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	return m.Called(ctx, key, value, expiration).Error(0)
}

func (m *MockCacheService) GetSession(ctx context.Context, sessionID string) (*types.CachedSession, error) {
	args := m.Called(ctx, sessionID)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.CachedSession), args.Error(1)
}

func (m *MockCacheService) SetSession(ctx context.Context, sessionID string, session types.CachedSession, expiration time.Duration) error {
	return m.Called(ctx, sessionID, session, expiration).Error(0)
}

func (m *MockCacheService) DeleteSession(ctx context.Context, sessionID string) error {
	return m.Called(ctx, sessionID).Error(0)
}

func (m *MockCacheService) Ping(ctx context.Context) error {
	return m.Called(ctx).Error(0)
}
