// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package opencode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// client_modelinfo_contract_test.go — RECORDED-FIXTURE contract tests
// against the PINNED opencode's actual endpoint shapes (review r2
// missing-test-3: the OR-merge and the synthesized-false discovery were
// pinned only against the FAKE's assumed shape; an opencode shape drift
// would pass every other test).
//
// Fixtures recorded 2026-10-05 from the pinned opencode binary via a
// throwaway serve with a probe provider declaring:
//
//	"vision-declared": {"attachment": true,  "limit": {...}}
//	"undeclared":      {}
//
// What the recordings prove (and what drifts if these tests fail):
//   - GET /config/providers: config attachment lands at
//     capabilities.attachment; input.image stays the SYNTHESIZED false
//     for models.dev-absent models (declared or not)
//   - GET /provider: the same capabilities.attachment carries the
//     declaration on the catalog endpoint the API side parses
//
// (fixture apiKey redacted post-capture — no secret material in testdata)
// If opencode changes either shape: re-record per testdata/REFRESH.md
// and re-derive the resolver semantics — do NOT edit expectations to
// match a new shape without re-probing.

func recordedFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return raw
}

func fixtureServer(t *testing.T, path string, body []byte) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "pw", nil)
}

// TestModelInfo_RecordedConfigProviders_DeclaredVision: the classifier
// case against the REAL pinned response shape — declared attachment
// (capabilities.attachment=true) resolves vision-capable even though
// input.image is the recorded synthesized false.
func TestModelInfo_RecordedConfigProviders_DeclaredVision(t *testing.T) {
	c := fixtureServer(t, "/config/providers", recordedFixture(t, "opencode-config-providers-recorded.json"))
	info, err := c.ModelInfo(context.Background(), "probeprov", "vision-declared")
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.True(t, info.ImageInputKnown, "declared attachment must make the capability KNOWN")
	assert.True(t, info.ImageInput, "declared attachment=true must override the synthesized input.image=false")
	assert.Equal(t, int64(1000), info.ContextLimit, "config-declared limit rides the same recorded entry")
}

// TestModelInfo_RecordedConfigProviders_UndeclaredSynthesizedFalse: an
// undeclared custom model's recorded entry is the all-text-only
// synthesized block — known-false, the honest refusal basis.
func TestModelInfo_RecordedConfigProviders_UndeclaredSynthesizedFalse(t *testing.T) {
	c := fixtureServer(t, "/config/providers", recordedFixture(t, "opencode-config-providers-recorded.json"))
	info, err := c.ModelInfo(context.Background(), "probeprov", "undeclared")
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.True(t, info.ImageInputKnown, "the synthesized block is PRESENT data — known, not unknown")
	assert.False(t, info.ImageInput)
}

// TestAvailableModels_RecordedProviderCatalog: the /provider endpoint's
// recorded shape (capabilities.attachment included) parses into the
// contract ModelInfo list with provider/id intact.
func TestAvailableModels_RecordedProviderCatalog(t *testing.T) {
	c := fixtureServer(t, "/provider", recordedFixture(t, "opencode-provider-recorded.json"))
	models, err := c.AvailableModels(context.Background())
	require.NoError(t, err)
	byRef := map[string]bool{}
	for _, m := range models {
		byRef[m.Provider+"/"+m.ID] = true
	}
	assert.True(t, byRef["probeprov/vision-declared"], "declared model present in the parsed catalog")
	assert.True(t, byRef["probeprov/undeclared"], "undeclared model present in the parsed catalog")
	assert.Equal(t, 2, len(models), "fixture serves exactly the probe provider (connected trimmed)")
}
