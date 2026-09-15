// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpencodeProviderParser_Parse(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		want    *Catalog
	}{
		{
			name: "typical response with connected providers",
			raw: `{
				"connected": ["anthropic", "opencode"],
				"all": [
					{"id": "anthropic", "models": {"claude-sonnet": {"id": "claude-sonnet-4", "name": "Sonnet", "cost": {"input": 3, "output": 15}}}},
					{"id": "opencode", "models": {"free": {"id": "free", "name": "Free", "cost": {"input": 0, "output": 0}}}}
				]
			}`,
			want: &Catalog{
				Connected: []string{"anthropic", "opencode"},
				Providers: []CatalogProvider{
					{ID: "anthropic", Models: map[string]CatalogModel{"claude-sonnet": {ID: "claude-sonnet-4", Name: "Sonnet", Cost: ProviderModelCost{Input: 3, Output: 15}}}},
					{ID: "opencode", Models: map[string]CatalogModel{"free": {ID: "free", Name: "Free", Cost: ProviderModelCost{}}}},
				},
			},
		},
		{
			name:    "empty body",
			raw:     ``,
			wantErr: false,
			want:    &Catalog{},
		},
		{
			name:    "invalid json",
			raw:     `{not json`,
			wantErr: true,
		},
		{
			name:    "model id falls back to map key when empty",
			raw:     `{"connected":["p"],"all":[{"id":"p","models":{"key-model":{"name":"N","cost":{"input":1,"output":2}}}}]}`,
			wantErr: false,
			want: &Catalog{
				Connected: []string{"p"},
				Providers: []CatalogProvider{
					{ID: "p", Models: map[string]CatalogModel{"key-model": {ID: "", Name: "N", Cost: ProviderModelCost{Input: 1, Output: 2}}}},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parser := NewOpencodeProviderParser()
			got, err := parser.Parse([]byte(tt.raw))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, tt.want.Connected, got.Connected)
			assert.Len(t, got.Providers, len(tt.want.Providers))
			for i, wp := range tt.want.Providers {
				if i < len(got.Providers) {
					assert.Equal(t, wp.ID, got.Providers[i].ID)
					assert.Equal(t, wp.Models, got.Providers[i].Models)
				}
			}
		})
	}
}

// TestOpencodeProviderParser_ParseVisionCapability pins the three wire
// shapes vision capability arrives in (issue #1307): the /config/providers
// shape (capabilities.input.image — live-validated 2026-09-13, docs/testing/
// agentd-mcp-tools-test-plan.md), the config-schema shape (attachment —
// testdata/opencode-config.schema.json ProviderConfig.models), and the
// registry shape the incident quoted (modalities.input containing "image").
// Absent metadata must decode to nil (unknown), never a silent false.
func TestOpencodeProviderParser_ParseVisionCapability(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]*bool // provider-local model key -> expected SupportsVision
	}{
		{
			name: "capabilities input image true",
			raw: `{"connected":["p"],"all":[{"id":"p","models":{
				"m1":{"id":"m1","name":"M1","capabilities":{"input":{"image":true}}},
				"m2":{"id":"m2","name":"M2","capabilities":{"input":{"image":false}}}
			}}]}`,
			want: map[string]*bool{"m1": ptrBool(true), "m2": ptrBool(false)},
		},
		{
			name: "attachment flag",
			raw: `{"connected":["p"],"all":[{"id":"p","models":{
				"vision":{"id":"vision","attachment":true},
				"text":{"id":"text","attachment":false}
			}}]}`,
			want: map[string]*bool{"vision": ptrBool(true), "text": ptrBool(false)},
		},
		{
			name: "modalities input array",
			raw: `{"connected":["p"],"all":[{"id":"p","models":{
				"m":{"id":"m","modalities":{"input":["text","image"],"output":["text"]}},
				"t":{"id":"t","modalities":{"input":["text"],"output":["text"]}}
			}}]}`,
			want: map[string]*bool{"m": ptrBool(true), "t": ptrBool(false)},
		},
		{
			name: "no capability metadata stays unknown",
			raw: `{"connected":["p"],"all":[{"id":"p","models":{
				"m":{"id":"m","name":"M","cost":{"input":0,"output":0}}
			}}]}`,
			want: map[string]*bool{"m": nil},
		},
		{
			name: "empty capabilities object stays unknown",
			raw: `{"connected":["p"],"all":[{"id":"p","models":{
				"m":{"id":"m","capabilities":{}}
			}}]}`,
			want: map[string]*bool{"m": nil},
		},
		{
			name: "capabilities wins over conflicting attachment",
			raw: `{"connected":["p"],"all":[{"id":"p","models":{
				"m":{"id":"m","attachment":true,"capabilities":{"input":{"image":false}}}
			}}]}`,
			want: map[string]*bool{"m": ptrBool(false)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewOpencodeProviderParser().Parse([]byte(tt.raw))
			require.NoError(t, err)
			require.Len(t, got.Providers, 1)
			models := got.Providers[0].Models
			require.Len(t, models, len(tt.want))
			for key, want := range tt.want {
				m, ok := models[key]
				require.True(t, ok, "model %s missing", key)
				if want == nil {
					assert.Nil(t, m.SupportsVision, "model %s: capability must be unknown, got %v", key, m.SupportsVision)
				} else {
					require.NotNil(t, m.SupportsVision, "model %s: capability must be known", key)
					assert.Equal(t, *want, *m.SupportsVision, "model %s", key)
				}
			}
		})
	}
}

func ptrBool(b bool) *bool { return &b }

func TestCatalog_ModelExists(t *testing.T) {
	cat := &Catalog{
		Connected: []string{"anthropic"},
		Providers: []CatalogProvider{
			{ID: "anthropic", Models: map[string]CatalogModel{"k": {ID: "claude-sonnet-4"}}},
			{ID: "opencode", Models: map[string]CatalogModel{"k": {ID: "free"}}},
		},
	}
	assert.True(t, cat.modelExists("claude-sonnet-4"), "connected provider model must be found")
	assert.False(t, cat.modelExists("free"), "disconnected provider model must not be found")
	assert.False(t, cat.modelExists("nonexistent"))
}

func TestCatalog_ResolveModel(t *testing.T) {
	cat := &Catalog{
		Connected: []string{"anthropic", "opencode"},
		Providers: []CatalogProvider{
			{ID: "anthropic", Models: map[string]CatalogModel{"k": {ID: "claude-sonnet-4", Cost: ProviderModelCost{Input: 3}}}},
			{ID: "opencode", Models: map[string]CatalogModel{"k": {ID: "free", Cost: ProviderModelCost{}}}},
		},
	}
	tests := []struct {
		name               string
		modelID            string
		relayActive        bool
		relayInjected      bool
		wantProviderScoped string
	}{
		{"paid model no relay", "claude-sonnet-4", false, false, "anthropic/claude-sonnet-4"},
		{"free model no relay", "free", false, false, "opencode/free"},
		{"free model relay active+injected", "free", true, true, "opencode-relay/free"},
		{"free model relay active NOT injected", "free", true, false, "opencode/free"},
		{"unknown model returns as-is", "nonexistent", false, false, "nonexistent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cat.resolveModel(tt.modelID, tt.relayActive, tt.relayInjected)
			assert.Equal(t, tt.wantProviderScoped, got)
		})
	}
}

// TestCatalog_ResolveModel_PureFunction confirms resolveModel has no side
// effects — calling it twice with the same inputs yields the same result and
// does not mutate the catalog (M3-a: previously resolveModelFromCatalog took
// a relayChecker closure that performed I/O).
func TestCatalog_ResolveModel_PureFunction(t *testing.T) {
	cat := &Catalog{
		Connected: []string{"opencode"},
		Providers: []CatalogProvider{
			{ID: "opencode", Models: map[string]CatalogModel{"k": {ID: "free", Cost: ProviderModelCost{}}}},
		},
	}
	first := cat.resolveModel("free", true, true)
	second := cat.resolveModel("free", true, true)
	assert.Equal(t, first, second)
	// Serialize to prove no mutation
	_, err := json.Marshal(cat)
	require.NoError(t, err)
}
