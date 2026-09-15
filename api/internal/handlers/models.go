// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// ModelStore provides all database operations needed by model endpoints.
// Satisfied by *database.Service.
type ModelStore interface {
	UpdateWorkspace(ctx context.Context, workspaceID string, updates types.WorkspaceUpdates) error
	GetDefaultModel(ctx context.Context, workspaceID string) (string, error)
	GetWorkspace(ctx context.Context, workspaceID string) (*types.WorkspaceMetadata, error)
}

// ModelSelectionRequest is the request body for PUT /workspaces/:id/model.
type ModelSelectionRequest struct {
	Model string `json:"model" binding:"required"`
}

// --- Model catalog cache ---

// ModelCache abstracts model catalog caching. Default: in-process map.
type ModelCache interface {
	Get(workspaceID string) []byte
	Set(workspaceID string, data []byte)
	Evict(workspaceID string)
	Keys() []string
}

type inMemoryModelCache struct {
	mu    sync.Mutex
	cache map[string]*modelCacheEntry
}

type modelCacheEntry struct {
	data      []byte
	expiresAt time.Time
}

type modelCachePayload struct {
	Models        []annotatedModel `json:"models"`
	RelayInjected bool             `json:"relayInjected"`
}

func NewInMemoryModelCache() ModelCache {
	return newInMemoryModelCache()
}

func newInMemoryModelCache() ModelCache {
	return &inMemoryModelCache{cache: make(map[string]*modelCacheEntry)}
}

func (c *inMemoryModelCache) Get(workspaceID string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.cache[workspaceID]
	if entry == nil || time.Now().After(entry.expiresAt) {
		return nil
	}
	return entry.data
}

func (c *inMemoryModelCache) Set(workspaceID string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[workspaceID] = &modelCacheEntry{data: data, expiresAt: time.Now().Add(5 * time.Second)}
}

func (c *inMemoryModelCache) Evict(workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, workspaceID)
}

func (c *inMemoryModelCache) Keys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.cache))
	for k := range c.cache {
		keys = append(keys, k)
	}
	return keys
}

var defaultModelCache ModelCache = newInMemoryModelCache()

func clearModelCache() {
	// Evict-all by evicting each known key. The interface doesn't expose
	// a Clear method, but Evict per-key is sufficient for test isolation.
	// Tests that need a clean slate call this before seeding.
	for _, k := range defaultModelCache.Keys() {
		defaultModelCache.Evict(k)
	}
}

// --- Model annotation types and functions ---

type annotatedModel struct {
	ID            string            `json:"id"`
	ProviderID    string            `json:"providerID"`
	Name          string            `json:"name"`
	Family        string            `json:"family,omitempty"`
	Enabled       bool              `json:"enabled"`
	Status        string            `json:"status,omitempty"`
	Availability  ModelAvailability `json:"availability"`
	Tier          string            `json:"tier"`
	FreeTier      bool              `json:"freeTier"`
	ProxyRequired bool              `json:"proxyRequired"`
	Selected      bool              `json:"selected"`
	Details       json.RawMessage   `json:"details"`
	// SupportsVision is tri-state (issue #1307): nil/absent = the catalog
	// carried no capability metadata (unknown); false = known text-only —
	// clients warn that image-bearing tool flows wedge sessions on it.
	SupportsVision *bool `json:"supportsVision,omitempty"`
}

type ModelAvailability string

const (
	ModelAvailable   ModelAvailability = "available"
	ModelUnavailable ModelAvailability = "unavailable"
	ModelFreeTier    ModelAvailability = "free"
)

// annotateModels enriches a parsed Catalog with tier, availability, and relay
// information. Only models whose providerID is in connected[] are returned.
// Worklog 0377 H1-a′: takes *Catalog (parsed by ModelCatalogParser) instead
// of raw []byte, removing the parsing burden from this function.
func annotateModels(cat *Catalog, relayGloballyEnabled, relayInjected bool) []annotatedModel {
	if cat == nil {
		return nil
	}
	connectedSet := cat.connectedSet()

	var result []annotatedModel
	for _, p := range cat.Providers {
		if !connectedSet[p.ID] {
			continue
		}
		for modelKey, m := range p.Models {
			id := m.ID
			if id == "" {
				id = modelKey
			}
			avail := classifyAvailability(p.ID, m.Cost)
			providerID := p.ID

			if relayGloballyEnabled && relayInjected && avail == ModelFreeTier && p.ID == "opencode" {
				providerID = "opencode-relay"
			}
			result = append(result, annotatedModel{
				ID:             id,
				ProviderID:     providerID,
				Name:           m.Name,
				Enabled:        true,
				Availability:   avail,
				Tier:           tierFromAvailability(avail),
				FreeTier:       avail == ModelFreeTier,
				ProxyRequired:  avail == ModelFreeTier,
				SupportsVision: m.SupportsVision,
			})
		}
	}
	return result
}

func classifyAvailability(providerID string, cost ProviderModelCost) ModelAvailability {
	if isZeroCostOpencode(providerID, cost) {
		return ModelFreeTier
	}
	return ModelAvailable
}

func isZeroCostOpencode(providerID string, cost ProviderModelCost) bool {
	if providerID != "opencode" && providerID != "opencode-relay" {
		return false
	}
	return cost.Input == 0 && cost.Output == 0
}

func tierFromAvailability(a ModelAvailability) string {
	if a == ModelFreeTier {
		return "free"
	}
	return "paid"
}
