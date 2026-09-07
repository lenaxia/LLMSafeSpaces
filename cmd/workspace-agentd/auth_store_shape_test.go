// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// auth_store_shape_test.go — pins authStoreEntry's marshaled bytes
// against the live PUT /auth payload shape (client.go:258-265): {key,
// type} always; metadata ONLY when the provider carries a baseURL.
// The r1 review caught a struct-typed Metadata emitting "metadata":{}
// under omitempty — encoding/json never omits non-pointer structs.

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

func TestAuthStoreEntry_MarshalMatchesLivePutShape(t *testing.T) {
	// No baseURL: the entry must be EXACTLY {"key":…,"type":"api"} —
	// no metadata key at all.
	b, err := json.Marshal(authStoreEntry{Key: "sk-x", Type: authStoreEntryTypeAPI})
	require.NoError(t, err)
	assert.JSONEq(t, `{"key":"sk-x","type":"api"}`, string(b))
	assert.NotContains(t, string(b), "metadata", "no-baseURL entry must not carry a metadata key")

	// With baseURL: {"key":…,"type":"api","metadata":{"baseURL":…}}.
	b, err = json.Marshal(authStoreEntry{
		Key:      "sk-x",
		Type:     authStoreEntryTypeAPI,
		Metadata: &authStoreMetadata{BaseURL: "https://api.example/v1"},
	})
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"key":"sk-x","type":"api","metadata":{"baseURL":"https://api.example/v1"}}`,
		string(b))
}

func TestWriteStagedProvidersToAuthStore_NoBaseURLOmitsMetadataKey(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/auth.json"
	staged := []sec.LLMProviderData{
		{Slug: "prov-a", APIKey: "sk-a"}, // no BaseURL
		{Slug: "prov-b", APIKey: "sk-b", BaseURL: "https://b.example/v1"},
	}
	require.NoError(t, writeStagedProvidersToAuthStoreW(&nopWriter{}, p, staged))

	var got map[string]json.RawMessage
	data := readTB(t, p)
	require.NoError(t, json.Unmarshal(data, &got))
	assert.JSONEq(t, `{"key":"sk-a","type":"api"}`, string(got["prov-a"]),
		"prov-a must have NO metadata key")
	assert.Contains(t, string(got["prov-b"]), `"metadata"`)
}

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }

func readTB(t *testing.T, p string) []byte {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
