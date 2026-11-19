// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package services

import (
	"fmt"

	"github.com/lenaxia/llmsafespaces/api/internal/config"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/auth"
	"github.com/lenaxia/llmsafespaces/api/internal/services/cache"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metering"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/api/internal/services/ratelimit"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
)

type Services struct {
	Auth        interfaces.AuthService
	Database    interfaces.DatabaseService
	Cache       interfaces.CacheService
	Metrics     interfaces.MetricsService
	Workspace   interfaces.WorkspaceService
	RateLimiter interfaces.RateLimiterService
	Metering    interfaces.MeteringService
}

var _ interfaces.Services = &Services{}

func (s *Services) GetAuth() interfaces.AuthService {
	return s.Auth
}

func (s *Services) GetDatabase() interfaces.DatabaseService {
	return s.Database
}

func (s *Services) GetCache() interfaces.CacheService {
	return s.Cache
}

func (s *Services) GetMetrics() interfaces.MetricsService {
	return s.Metrics
}

func (s *Services) GetWorkspace() interfaces.WorkspaceService {
	return s.Workspace
}

func (s *Services) GetRateLimiter() interfaces.RateLimiterService {
	return s.RateLimiter
}

func (s *Services) GetMetering() interfaces.MeteringService {
	return s.Metering
}

func New(cfg *config.Config, log *logger.Logger, k8sClient interfaces.KubernetesClient) (*Services, error) {
	metricsService := metrics.New(log)

	dbService, err := database.New(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize database service: %w", err)
	}

	cacheService, err := cache.New(cfg, log)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize cache service: %w", err)
	}

	authService, err := auth.New(cfg, log, dbService, cacheService)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize auth service: %w", err)
	}

	workspaceConfig := &workspace.Config{
		Namespace: cfg.Kubernetes.Namespace,
	}

	workspaceService, err := workspace.New(
		log,
		k8sClient,
		dbService,
		cacheService,
		metricsService,
		workspaceConfig,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize workspace service: %w", err)
	}
	// #2092 ruling 4 — arm the create-time CRD capability probe: on a
	// cluster whose Workspace CRD predates spec.harness, a create WITHOUT
	// an explicit harness fails loud at the gate instead of birthing a
	// workspace that can never bind (eternal Creating). Cached (60s TTL
	// + invalidation on live skew evidence), never per-request.
	workspaceService.ArmCRDCapabilityProbe()

	// Reuse the cache service's Redis connection pool for the rate limiter
	// instead of opening a second client/pool to the same Redis instance.
	// The cache service owns the client lifecycle (it closes the pool on
	// Stop), so the rate limiter treats it as borrowed and does not close it.
	rateLimiterService := ratelimit.NewWithRedisClient(log, cacheService.GetClient())

	var meteringService interfaces.MeteringService
	ms, err := metering.New(cfg, log, dbService.DB)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize metering service: %w", err)
	}
	meteringService = ms
	ms.SetBillingProvider(dbService)

	return &Services{
		Auth:        authService,
		Database:    dbService,
		Cache:       cacheService,
		Metrics:     metricsService,
		Workspace:   workspaceService,
		RateLimiter: rateLimiterService,
		Metering:    meteringService,
	}, nil
}

func (s *Services) Start() error {
	if err := s.Metrics.Start(); err != nil {
		return fmt.Errorf("failed to start metrics service: %w", err)
	}
	if err := s.Database.Start(); err != nil {
		return fmt.Errorf("failed to start database service: %w", err)
	}
	if err := s.Cache.Start(); err != nil {
		return fmt.Errorf("failed to start cache service: %w", err)
	}
	if err := s.Auth.Start(); err != nil {
		return fmt.Errorf("failed to start auth service: %w", err)
	}
	if err := s.Workspace.Start(); err != nil {
		return fmt.Errorf("failed to start workspace service: %w", err)
	}
	if err := s.RateLimiter.Start(); err != nil {
		return fmt.Errorf("failed to start rate limiter service: %w", err)
	}
	if s.Metering != nil {
		if err := s.Metering.Start(); err != nil {
			return fmt.Errorf("failed to start metering service: %w", err)
		}
	}
	return nil
}

func (s *Services) Stop() error {
	var errs []error
	if s.Metering != nil {
		if err := s.Metering.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := s.RateLimiter.Stop(); err != nil {
		errs = append(errs, err)
	}
	if err := s.Workspace.Stop(); err != nil {
		errs = append(errs, err)
	}
	if err := s.Auth.Stop(); err != nil {
		errs = append(errs, err)
	}
	if err := s.Cache.Stop(); err != nil {
		errs = append(errs, err)
	}
	if err := s.Database.Stop(); err != nil {
		errs = append(errs, err)
	}
	if err := s.Metrics.Stop(); err != nil {
		errs = append(errs, err)
	}
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}
