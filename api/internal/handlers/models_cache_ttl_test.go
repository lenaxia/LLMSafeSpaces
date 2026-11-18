// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Listing-latency work — the model-catalog cache TTL (the /models
// endpoint proxies the workspace pod's opencode, ~840ms at the median
// live). The cache invalidation seams (SetModel evict, credential-push
// evict via agentpush.WithModelCache, the #467 relay-transition live
// guard) cover every catalog-mutation path; the TTL is what bounds
// pod-restart staleness. The pin: the TTL is the deliberate
// listing-latency value (modelCacheTTL), not the historical 5s that
// made nearly every picker open pay the pod round trip.

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestModelCache_TTLIsModelCacheTTL — Set stamps expiry at
// now+modelCacheTTL (in-package: the entry is read directly).
func TestModelCache_TTLIsModelCacheTTL(t *testing.T) {
	c := newInMemoryModelCache().(*inMemoryModelCache)

	before := time.Now()
	c.Set("ws-1", []byte(`{"models":[]}`))

	c.mu.Lock()
	entry := c.cache["ws-1"]
	require.NotNil(t, entry, "Set stored the entry")
	expiresAt := entry.expiresAt
	c.mu.Unlock()

	require.WithinDuration(t, before.Add(modelCacheTTL), expiresAt, 2*time.Second,
		"the entry expires at now+modelCacheTTL")
	assert.Equal(t, time.Minute, modelCacheTTL,
		"the TTL is the deliberate listing-latency value — long enough to absorb the picker's refreshes, short enough that a pod-restart catalog change self-heals within a minute")
}

// TestModelCache_ExpiryStillServesAndDrops — the behavioral twin: a
// fresh entry serves; an expired entry misses (the TTL actually gates,
// not just stamps).
func TestModelCache_ExpiryStillServesAndDrops(t *testing.T) {
	c := newInMemoryModelCache()
	payload := []byte(`{"models":[]}`)

	c.Set("ws-1", payload)
	require.NotNil(t, c.Get("ws-1"), "a fresh entry serves")

	c.(*inMemoryModelCache).mu.Lock()
	entry := c.(*inMemoryModelCache).cache["ws-1"]
	entry.expiresAt = time.Now().Add(-time.Second)
	c.(*inMemoryModelCache).mu.Unlock()

	assert.Nil(t, c.Get("ws-1"), "an expired entry misses — the TTL gates")
}
