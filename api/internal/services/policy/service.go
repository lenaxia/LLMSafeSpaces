// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const (
	cacheTTL     = 5 * time.Minute
	cacheKeyPref = "org:policy:"
)

// policyStore is the data-access surface the PolicyService reads from.
type policyStore interface {
	GetOrgPolicies(ctx context.Context, orgID string) ([]*types.OrgPolicy, error)
}

// Cache is the subset of the cache service used for policy caching.
type Cache interface {
	GetObject(ctx context.Context, key string, value interface{}) error
	SetObject(ctx context.Context, key string, value interface{}, expiration time.Duration) error
	Delete(ctx context.Context, key string) error
}

// Service reads org policies, caches them in Redis (5-min TTL), and provides
// typed accessors for enforcement. Per D16 the effective policy is
// `org ∩ platform`; platform policies are injected via the PlatformPolicy field.
type Service struct {
	store          policyStore
	cache          Cache
	platformPolicy types.OrgPolicyValues
}

// New constructs the PolicyService. cache may be nil (no caching, every call
// hits the DB) — useful for tests.
func New(store policyStore, cache Cache) *Service {
	return &Service{store: store, cache: cache}
}

// SetPlatformPolicy sets the platform-wide policy floor. Per D16 the effective
// policy is the intersection of org and platform: org can only restrict further,
// never loosen. Called once at startup from instance settings.
func (s *Service) SetPlatformPolicy(p types.OrgPolicyValues) {
	s.platformPolicy = p
}

func cacheKey(orgID string) string { return cacheKeyPref + orgID }

// GetEffectivePolicy returns the org's policy intersected with the platform
// policy. When the org has no policy set, returns the platform policy alone.
// When neither is set, returns an empty (unrestricted) values struct.
func (s *Service) GetEffectivePolicy(ctx context.Context, orgID string) (*types.OrgPolicyValues, error) {
	if orgID == "" {
		orgEmpty := intersect(&s.platformPolicy, nil)
		return orgEmpty, nil
	}

	orgValues, err := s.getOrgPolicy(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return intersect(&s.platformPolicy, orgValues), nil
}

// getOrgPolicy reads and caches the org's own policies. Cache miss → DB →
// populate cache. On DB error, returns the error (no silent fallback to
// unrestricted — that would be a security hole).
func (s *Service) getOrgPolicy(ctx context.Context, orgID string) (*types.OrgPolicyValues, error) {
	if s.cache != nil {
		var cached types.OrgPolicyValues
		if err := s.cache.GetObject(ctx, cacheKey(orgID), &cached); err == nil {
			return &cached, nil
		}
	}

	rows, err := s.store.GetOrgPolicies(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("get org policies: %w", err)
	}

	vals := &types.OrgPolicyValues{}
	for _, p := range rows {
		if err := applyPolicyValue(vals, p); err != nil {
			return nil, fmt.Errorf("parse policy %s: %w", p.Key, err)
		}
	}

	if s.cache != nil {
		_ = s.cache.SetObject(ctx, cacheKey(orgID), vals, cacheTTL) //nolint:errcheck // best-effort cache write; next Get recomputes on miss
	}
	return vals, nil
}

// InvalidateCache evicts the cached policy for an org. Called after SetOrgPolicy
// or DeleteOrgPolicy so the next read picks up the change immediately.
func (s *Service) InvalidateCache(ctx context.Context, orgID string) {
	if s.cache != nil {
		_ = s.cache.Delete(ctx, cacheKey(orgID))
	}
}

// applyPolicyValue unmarshals a single JSONB row into the typed values struct.
func applyPolicyValue(vals *types.OrgPolicyValues, p *types.OrgPolicy) error {
	switch p.Key {
	case types.PolicyAllowedModels:
		var models []string
		if err := json.Unmarshal(p.Value, &models); err != nil {
			return err
		}
		vals.AllowedModels = &models
	case types.PolicyAllowedProviders:
		var providers []string
		if err := json.Unmarshal(p.Value, &providers); err != nil {
			return err
		}
		vals.AllowedProviders = &providers
	case types.PolicyMaxWorkspacesPerMember:
		var n int
		if err := json.Unmarshal(p.Value, &n); err != nil {
			return err
		}
		vals.MaxWorkspacesPerMember = &n
	case types.PolicyMaxActiveWorkspacesPerMem:
		var n int
		if err := json.Unmarshal(p.Value, &n); err != nil {
			return err
		}
		vals.MaxActiveWorkspacesPerMem = &n
	case types.PolicySysPromptOrg:
		var s string
		if err := json.Unmarshal(p.Value, &s); err != nil {
			return err
		}
		vals.SysPromptOrg = &s
	case types.PolicyAllowUserPrompt:
		var b bool
		if err := json.Unmarshal(p.Value, &b); err != nil {
			return err
		}
		vals.AllowUserPrompt = &b
	case types.PolicyAllowUserMcpServers:
		var b bool
		if err := json.Unmarshal(p.Value, &b); err != nil {
			return err
		}
		vals.AllowUserMcpServers = &b
	case types.PolicyMaxMcpServersPerWorkspace:
		var n int
		if err := json.Unmarshal(p.Value, &n); err != nil {
			return err
		}
		vals.MaxMcpServersPerWorkspace = &n
	}
	return nil
}

// intersect returns the intersection of platform and org policies. For
// allow-lists, intersection means the smaller (more restrictive) set. For
// numeric limits, the minimum. A nil org means "use platform as-is".
func intersect(platform *types.OrgPolicyValues, org *types.OrgPolicyValues) *types.OrgPolicyValues {
	if org == nil {
		cp := *platform
		return &cp
	}
	result := &types.OrgPolicyValues{}

	result.AllowedModels = intersectLists(platform.AllowedModels, org.AllowedModels)
	result.AllowedProviders = intersectLists(platform.AllowedProviders, org.AllowedProviders)
	result.MaxWorkspacesPerMember = minInt(platform.MaxWorkspacesPerMember, org.MaxWorkspacesPerMember)
	result.MaxActiveWorkspacesPerMem = minInt(platform.MaxActiveWorkspacesPerMem, org.MaxActiveWorkspacesPerMem)

	// Prompt-overlay fields are org-scoped (no platform counterpart in the
	// intersection sense): pass the org value through, falling back to the
	// platform value when the org does not set one.
	result.SysPromptOrg = firstNonNil(org.SysPromptOrg, platform.SysPromptOrg)
	result.AllowUserPrompt = firstNonNilBool(org.AllowUserPrompt, platform.AllowUserPrompt)

	// Epic 53 — MCP server governance. AllowUserMcpServers is an org-scoped
	// toggle passed through like AllowUserPrompt. MaxMcpServersPerWorkspace is a
	// numeric cap, so the more restrictive (smaller) value wins.
	result.AllowUserMcpServers = firstNonNilBool(org.AllowUserMcpServers, platform.AllowUserMcpServers)
	result.MaxMcpServersPerWorkspace = minInt(platform.MaxMcpServersPerWorkspace, org.MaxMcpServersPerWorkspace)

	return result
}

// intersectLists returns the more restrictive of two allow-lists. nil means
// unrestricted, so:
//
//	platform=nil, org=nil → nil (unrestricted)
//	platform=nil, org=[a,b] → [a,b] (org restricts)
//	platform=[a,b,c], org=nil → [a,b,c] (platform restricts)
//	platform=[a,b,c], org=[a,b] → [a,b] (intersection)
func intersectLists(platform, org *[]string) *[]string {
	if platform == nil && org == nil {
		return nil
	}
	if platform == nil {
		return org
	}
	if org == nil {
		return platform
	}
	set := make(map[string]struct{}, len(*platform))
	for _, v := range *platform {
		set[v] = struct{}{}
	}
	var out []string
	for _, v := range *org {
		if _, ok := set[v]; ok {
			out = append(out, v)
		}
	}
	return &out
}

func minInt(a, b *int) *int {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *a < *b {
		return a
	}
	return b
}

// firstNonNil returns the first non-nil pointer, or nil. Used for org-scoped
// policy fields that pass through unchanged when set.
func firstNonNil(a, b *string) *string {
	if a != nil {
		return a
	}
	return b
}

// firstNonNilBool returns the first non-nil bool pointer, or nil.
func firstNonNilBool(a, b *bool) *bool {
	if a != nil {
		return a
	}
	return b
}
