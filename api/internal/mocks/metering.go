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

type MockMeteringService struct {
	mock.Mock
}

var _ interfaces.MeteringService = (*MockMeteringService)(nil)

func (m *MockMeteringService) Record(event types.UsageEvent) {
	m.Called(event)
}

func (m *MockMeteringService) RecordLifecycleEvent(ctx context.Context, workspaceID, ownerID string, ownerType types.OwnerType, fromPhase, toPhase, resourceTier string, eventTime time.Time) error {
	args := m.Called(ctx, workspaceID, ownerID, ownerType, fromPhase, toPhase, resourceTier, eventTime)
	return args.Error(0)
}

func (m *MockMeteringService) GetUsage(ctx context.Context, owner types.BillingOwner, from, to time.Time) (*types.UsageReport, error) {
	args := m.Called(ctx, owner, from, to)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.UsageReport), args.Error(1)
}

func (m *MockMeteringService) GetUsageByWorkspace(ctx context.Context, owner types.BillingOwner, workspaceID string, from, to time.Time) (*types.UsageReport, error) {
	args := m.Called(ctx, owner, workspaceID, from, to)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).(*types.UsageReport), args.Error(1)
}

func (m *MockMeteringService) GetQuotaStatus(ctx context.Context, owner types.BillingOwner) ([]types.QuotaStatus, error) {
	args := m.Called(ctx, owner)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]types.QuotaStatus), args.Error(1)
}

func (m *MockMeteringService) CheckQuota(ctx context.Context, owner types.BillingOwner, eventType string) (bool, int64, error) {
	args := m.Called(ctx, owner, eventType)
	return args.Bool(0), args.Get(1).(int64), args.Error(2)
}

func (m *MockMeteringService) ReserveQuota(ctx context.Context, owner types.BillingOwner, eventType string, quantity int64) (bool, int64, error) {
	args := m.Called(ctx, owner, eventType, quantity)
	return args.Bool(0), args.Get(1).(int64), args.Error(2)
}

func (m *MockMeteringService) ExportUsage(ctx context.Context) (int, error) {
	args := m.Called(ctx)
	return args.Int(0), args.Error(1)
}

func (m *MockMeteringService) Start() error {
	args := m.Called()
	return args.Error(0)
}

func (m *MockMeteringService) Stop() error {
	args := m.Called()
	return args.Error(0)
}
