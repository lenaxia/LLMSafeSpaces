// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package prompt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const (
	cacheTTL         = 5 * time.Minute
	cacheKeyPref     = "ws:prompt:"
	platformCacheKey = "platform:prompt:sys_prompt_platform"
)

// promptStore is the data-access surface for the PromptService.
type promptStore interface {
	GetPlatformSetting(ctx context.Context, key types.PlatformSettingKey) (*types.PlatformSetting, error)
	GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error)
	GetWorkspacePrompt(ctx context.Context, workspaceID string) (*types.WorkspacePrompt, error)
	GetWorkspaceOrgID(ctx context.Context, workspaceID string) (string, error)
	GetAgentRole(ctx context.Context, roleID string) (*types.AgentRole, error)
}

// Cache is the subset of the cache service used for prompt caching.
type Cache interface {
	GetObject(ctx context.Context, key string, value interface{}) error
	SetObject(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	Delete(ctx context.Context, key string) error
	DeleteByPrefix(ctx context.Context, prefix string) error
}

// Service resolves the effective agent prompt for a workspace by merging the
// three-tier hierarchy: platform → org → user. The result is cached per
// workspace (5-min TTL) and invalidated on any prompt mutation.
type Service struct {
	store promptStore
	cache Cache
}

// New constructs the PromptService. cache may be nil (no caching).
func New(store promptStore, cache Cache) *Service {
	return &Service{store: store, cache: cache}
}

func wsCacheKey(workspaceID string) string { return cacheKeyPref + workspaceID }

// ResolveEffective returns the fully merged prompt for a workspace. This is
// called by the bootstrap endpoint to deliver the admin prompt to the pod.
func (s *Service) ResolveEffective(ctx context.Context, workspaceID string) (*types.EffectivePrompt, error) {
	if s.cache != nil {
		// A cache MISS returns nil error from cache.GetObject (redis.Nil is
		// swallowed), leaving the pointer untouched (nil). A HIT unmarshals
		// the stored object and makes the pointer non-nil. We must consult
		// the store on miss, so nil-pointer is the miss sentinel.
		var cached *types.EffectivePrompt
		if err := s.cache.GetObject(ctx, wsCacheKey(workspaceID), &cached); err == nil && cached != nil {
			return cached, nil
		}
	}

	result, err := s.resolveUncached(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	if s.cache != nil {
		_ = s.cache.SetObject(ctx, wsCacheKey(workspaceID), result, cacheTTL)
	}
	return result, nil
}

func (s *Service) resolveUncached(ctx context.Context, workspaceID string) (*types.EffectivePrompt, error) {
	platform, err := s.getPlatformPrompt(ctx)
	if err != nil {
		return nil, fmt.Errorf("get platform prompt: %w", err)
	}

	orgID, err := s.store.GetWorkspaceOrgID(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get workspace org: %w", err)
	}

	result := &types.EffectivePrompt{
		PlatformPrompt: platform,
	}

	if orgID != "" {
		orgValues, err := s.getOrgPolicies(ctx, orgID)
		if err != nil {
			return nil, fmt.Errorf("get org policies: %w", err)
		}
		result.OrgPrompt = orgValues.OrgPrompt()
		result.AllowUserPrompt = orgValues.IsUserPromptAllowed()
	} else {
		result.AllowUserPrompt = true
	}

	if result.AllowUserPrompt {
		wsPrompt, err := s.store.GetWorkspacePrompt(ctx, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("get workspace prompt: %w", err)
		}
		if wsPrompt != nil {
			result.UserPrompt = wsPrompt.Prompt
			// Resolve the workspace's agent role system prompt. Walk the extends
			// chain so an inherited system prompt (leaf's System nil, parent's
			// set) is delivered — matches what GetEffectiveWorkspaceRole shows.
			if wsPrompt.AgentRoleID != nil && *wsPrompt.AgentRoleID != "" {
				if sys, ok := s.resolveRoleSystemPrompt(ctx, *wsPrompt.AgentRoleID); ok {
					result.RolePrompt = sys
				}
			}
		}
	}

	result.Resolved = mergePromptParts(result)
	return result, nil
}

// resolveRoleSystemPrompt walks the agent-role extends chain to find the
// effective system prompt: the leaf's System if set, otherwise the nearest
// ancestor's. This keeps prompt delivery consistent with the
// GetEffectiveWorkspaceRole endpoint (role.Service.ResolveEffective), which
// resolves the full chain. Bounded to maxChainDepth to prevent runaway walks.
func (s *Service) resolveRoleSystemPrompt(ctx context.Context, roleID string) (string, bool) {
	const maxChainDepth = 10
	visited := make(map[string]bool)
	current := roleID
	for current != "" {
		if visited[current] || len(visited) >= maxChainDepth {
			break
		}
		visited[current] = true
		r, err := s.store.GetAgentRole(ctx, current)
		if err != nil || r == nil {
			return "", false
		}
		if r.Config.System != nil && strings.TrimSpace(*r.Config.System) != "" {
			return *r.Config.System, true
		}
		if r.Extends == nil || *r.Extends == "" {
			break
		}
		current = *r.Extends
	}
	return "", false
}

func mergePromptParts(p *types.EffectivePrompt) string {
	var parts []string
	if strings.TrimSpace(p.PlatformPrompt) != "" {
		parts = append(parts, p.PlatformPrompt)
	}
	if strings.TrimSpace(p.OrgPrompt) != "" {
		parts = append(parts, p.OrgPrompt)
	}
	if strings.TrimSpace(p.RolePrompt) != "" {
		parts = append(parts, p.RolePrompt)
	}
	if strings.TrimSpace(p.UserPrompt) != "" {
		parts = append(parts, p.UserPrompt)
	}
	return strings.Join(parts, "\n\n")
}

func (s *Service) getPlatformPrompt(ctx context.Context) (string, error) {
	if s.cache != nil {
		// nil-pointer sentinel distinguishes miss (nil err, ptr stays nil)
		// from a legitimately-empty cached platform prompt ("").
		var cached *string
		if err := s.cache.GetObject(ctx, platformCacheKey, &cached); err == nil && cached != nil {
			return *cached, nil
		}
	}

	setting, err := s.store.GetPlatformSetting(ctx, types.SettingSysPromptPlatform)
	if err != nil {
		return "", err
	}
	var prompt string
	if setting != nil {
		_ = json.Unmarshal(setting.Value, &prompt)
	} else {
		// Fresh install: no sys_prompt_platform row (migration 000002
		// creates the schema, no seed). Fall back to the compiled-in
		// default so the platform tier is never silently empty — same
		// posture as DefaultJWTIssuer. A saved row, including an
		// explicitly empty one, always wins (admin's deliberate choice).
		prompt = types.DefaultPlatformPrompt
	}

	if s.cache != nil {
		_ = s.cache.SetObject(ctx, platformCacheKey, prompt, cacheTTL)
	}
	return prompt, nil
}

func (s *Service) getOrgPolicies(ctx context.Context, orgID string) (*types.OrgPolicyValues, error) {
	rows, err := s.store.GetOrgPolicies(ctx, orgID)
	if err != nil {
		return nil, err
	}
	vals := &types.OrgPolicyValues{}
	for _, p := range rows {
		switch p.Key {
		case types.PolicySysPromptOrg:
			var s string
			if err := json.Unmarshal(p.Value, &s); err == nil {
				vals.SysPromptOrg = &s
			}
		case types.PolicyAllowUserPrompt:
			var b bool
			if err := json.Unmarshal(p.Value, &b); err == nil {
				vals.AllowUserPrompt = &b
			}
		}
	}
	return vals, nil
}

// InvalidateWorkspaceCache evicts the cached effective prompt for a workspace.
func (s *Service) InvalidateWorkspaceCache(ctx context.Context, workspaceID string) {
	if s.cache != nil {
		_ = s.cache.Delete(ctx, wsCacheKey(workspaceID))
	}
}

// InvalidatePlatformCache evicts the cached platform prompt.
func (s *Service) InvalidatePlatformCache(ctx context.Context) {
	if s.cache != nil {
		_ = s.cache.Delete(ctx, platformCacheKey)
	}
}

// InvalidateOrgWorkspacesCache evicts cached prompts for all workspaces.
// Called when org prompt overlay or allow_user_prompt changes. Invalidates
// ALL workspace caches (not just the org's) since the cache key doesn't
// encode org ID. This is a broader invalidation than strictly needed, but
// org prompt changes are infrequent and the cost is one extra DB read per
// workspace on the next bootstrap.
func (s *Service) InvalidateOrgWorkspacesCache(ctx context.Context, _ string) {
	if s.cache != nil {
		_ = s.cache.DeleteByPrefix(ctx, cacheKeyPref)
	}
}
