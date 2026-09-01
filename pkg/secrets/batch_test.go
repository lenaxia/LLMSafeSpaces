// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeBatchEntry builds a test entry with a stable metadata blob.
func makeBatchEntry(secretID string, version int64, typ SecretType, name, value string) BatchEntry {
	return BatchEntry{
		SecretID: secretID,
		Version:  version,
		Type:     typ,
		Name:     name,
		Value:    value,
		Metadata: json.RawMessage(`{"k":"v"}`),
	}
}

func sampleBatch() Batch {
	return Batch{Entries: []BatchEntry{
		makeBatchEntry("sec-3", 1, SecretTypeSSHKey, "deploy-key", "pk-3"),
		makeBatchEntry("sec-1", 4, SecretTypeEnvSecret, "db-url", "postgres://x"),
		makeBatchEntry("sec-2", 1, SecretTypeLLMProvider, "anthropic", `{"kind":"anthropic"}`),
	}}
}

// TestCanonicalBatch_SortedByTypeSecretIDName pins the canonical entry
// ordering: (Type, SecretID, Name).
func TestCanonicalBatch_SortedByTypeSecretIDName(t *testing.T) {
	batch := sampleBatch()
	out := CanonicalBatch(batch)

	var entries []BatchEntry
	require.NoError(t, json.Unmarshal(out, &entries))
	require.Len(t, entries, 3)
	assert.Equal(t, SecretTypeEnvSecret, entries[0].Type, "env-secret sorts first by type")
	assert.Equal(t, SecretTypeLLMProvider, entries[1].Type)
	assert.Equal(t, SecretTypeSSHKey, entries[2].Type)
}

// TestCanonicalBatch_DeterministicUnderShuffledEntries is the I6 property:
// identical logical entries under any input order produce identical bytes.
func TestCanonicalBatch_DeterministicUnderShuffledEntries(t *testing.T) {
	base := sampleBatch()
	want := string(CanonicalBatch(base))

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 25; i++ {
		shuffled := Batch{Entries: append([]BatchEntry(nil), base.Entries...)}
		rng.Shuffle(len(shuffled.Entries), func(a, b int) {
			shuffled.Entries[a], shuffled.Entries[b] = shuffled.Entries[b], shuffled.Entries[a]
		})
		assert.Equal(t, want, string(CanonicalBatch(shuffled)),
			"iteration %d: canonical bytes must not depend on entry order", i)
	}
}

// TestCanonicalBatch_DoesNotMutateInput pins that canonicalization copies:
// the caller's Batch keeps its construction order.
func TestCanonicalBatch_DoesNotMutateInput(t *testing.T) {
	base := sampleBatch()
	before := fmt.Sprintf("%v", base.Entries)
	_ = CanonicalBatch(base)
	assert.Equal(t, before, fmt.Sprintf("%v", base.Entries))
}

// TestBatchHash_ShuffleInvariant: same logical entries, shuffled orders →
// identical hashes.
func TestBatchHash_ShuffleInvariant(t *testing.T) {
	base := sampleBatch()
	want := BatchHash(base)

	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 25; i++ {
		shuffled := Batch{Entries: append([]BatchEntry(nil), base.Entries...)}
		rng.Shuffle(len(shuffled.Entries), func(a, b int) {
			shuffled.Entries[a], shuffled.Entries[b] = shuffled.Entries[b], shuffled.Entries[a]
		})
		assert.Equal(t, want, BatchHash(shuffled))
	}
}

// TestBatchHash_ValueSensitivity: any field change → different hash.
func TestBatchHash_ValueSensitivity(t *testing.T) {
	base := sampleBatch()
	want := BatchHash(base)

	variants := map[string]func(Batch) Batch{
		"secretID": func(b Batch) Batch { b.Entries[0].SecretID = "sec-3-changed"; return b },
		"version":  func(b Batch) Batch { b.Entries[0].Version++; return b },
		"type": func(b Batch) Batch {
			b.Entries[0].Type = SecretTypeGitCredential
			return b
		},
		"name":  func(b Batch) Batch { b.Entries[0].Name = "changed"; return b },
		"value": func(b Batch) Batch { b.Entries[0].Value = "changed"; return b },
	}
	for label, mutate := range variants {
		mutated := mutate(sampleBatch())
		assert.NotEqual(t, want, BatchHash(mutated), "changing %s must change the hash", label)
	}
}

// TestBatchHash_EmptyBatchStableNonEmpty: an empty batch hashes to a stable,
// hex-encoded SHA-256 (not the hash of "" — it is the hash of "[]").
func TestBatchHash_EmptyBatchStableNonEmpty(t *testing.T) {
	empty := Batch{}
	got := BatchHash(empty)
	assert.Equal(t, got, BatchHash(Batch{Entries: nil}))
	assert.Len(t, got, 64, "hex SHA-256")
	assert.NotEqual(t, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", got,
		"empty batch must hash the canonical empty entries array, not the empty string")
}

// TestManifestHash_MapInsertionOrderInvariant: metadata canonicalization
// makes the hash independent of JSON object key order.
func TestManifestHash_MapInsertionOrderInvariant(t *testing.T) {
	forward := []ManifestEntry{
		{SecretID: "sec-1", Version: 1, Type: SecretTypeEnvSecret, Name: "db",
			Metadata: json.RawMessage(`{"a":"v-a","b":"v-b","c":"v-c","d":"v-d","e":"v-e"}`)},
	}
	reverse := []ManifestEntry{
		{SecretID: "sec-1", Version: 1, Type: SecretTypeEnvSecret, Name: "db",
			Metadata: json.RawMessage(`{"e":"v-e","d":"v-d","c":"v-c","b":"v-b","a":"v-a"}`)},
	}
	assert.Equal(t, ManifestHash("owner-1", forward), ManifestHash("owner-1", reverse))
}

// TestManifestHash_EntryOrderInvariant: shuffled entries → identical hash.
func TestManifestHash_EntryOrderInvariant(t *testing.T) {
	entries := []ManifestEntry{
		{SecretID: "sec-3", Version: 2, Type: SecretTypeSSHKey, Name: "k"},
		{SecretID: "sec-1", Version: 1, Type: SecretTypeEnvSecret, Name: "e", Metadata: json.RawMessage(`{"var_name":"X"}`)},
		{SecretID: "sec-2", Version: 7, Type: SecretTypeMcpServer, Name: "m"},
	}
	reversed := []ManifestEntry{entries[2], entries[0], entries[1]}
	assert.Equal(t, ManifestHash("owner-1", entries), ManifestHash("owner-1", reversed))
}

// TestManifestHash_FieldSensitivity: every manifest field participates.
func TestManifestHash_FieldSensitivity(t *testing.T) {
	base := []ManifestEntry{
		{SecretID: "sec-1", Version: 1, Type: SecretTypeEnvSecret, Name: "db", Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`)},
	}
	want := ManifestHash("owner-1", base)

	bumpVersion := []ManifestEntry{{SecretID: "sec-1", Version: 2, Type: SecretTypeEnvSecret, Name: "db", Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`)}}
	changeName := []ManifestEntry{{SecretID: "sec-1", Version: 1, Type: SecretTypeEnvSecret, Name: "db2", Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`)}}
	changeID := []ManifestEntry{{SecretID: "sec-9", Version: 1, Type: SecretTypeEnvSecret, Name: "db", Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`)}}
	changeType := []ManifestEntry{{SecretID: "sec-1", Version: 1, Type: SecretTypeSSHKey, Name: "db", Metadata: json.RawMessage(`{"var_name":"DATABASE_URL"}`)}}
	changeMeta := []ManifestEntry{{SecretID: "sec-1", Version: 1, Type: SecretTypeEnvSecret, Name: "db", Metadata: json.RawMessage(`{"var_name":"OTHER"}`)}}

	for label, entries := range map[string][]ManifestEntry{
		"version": bumpVersion, "name": changeName, "secretID": changeID,
		"type": changeType, "metadata": changeMeta,
	} {
		assert.NotEqual(t, want, ManifestHash("owner-1", entries),
			"changing %s must change the manifest hash", label)
	}
}

// TestManifestHash_OwnerScoped: the owner line means two owners with
// identical entry sets never share a manifest hash.
func TestManifestHash_OwnerScoped(t *testing.T) {
	entries := []ManifestEntry{{SecretID: "sec-1", Version: 1, Type: SecretTypeEnvSecret, Name: "db"}}
	assert.NotEqual(t, ManifestHash("owner-1", entries), ManifestHash("owner-2", entries))
}

// TestManifestHash_EmptyStable: empty manifest set is stable and hex-shaped.
func TestManifestHash_EmptyStable(t *testing.T) {
	got := ManifestHash("owner-1", nil)
	assert.Len(t, got, 64)
	assert.Equal(t, got, ManifestHash("owner-1", []ManifestEntry{}))
	assert.NotEqual(t, ManifestHash("owner-2", nil), got, "owner line present even with no entries")
}

// TestManifestHash_NullAndEmptyMetadataEquivalent: a nil metadata map and
// an empty metadata map describe the same manifest (the empty map is a
// Go-side artifact, not a content difference).
func TestManifestHash_NullAndEmptyMetadataEquivalent(t *testing.T) {
	withNil := []ManifestEntry{{SecretID: "s", Version: 1, Type: SecretTypeSSHKey, Name: "n", Metadata: nil}}
	empty := []ManifestEntry{{SecretID: "s", Version: 1, Type: SecretTypeSSHKey, Name: "n", Metadata: json.RawMessage(`{}`)}}
	assert.Equal(t, ManifestHash("owner", withNil), ManifestHash("owner", empty))
}

// TestHashes_ThousandEntryDeterminism: 1000 entries, built twice with
// different orders, hash identically — the scale at which any accidental
// map-order dependence would surface.
func TestHashes_ThousandEntryDeterminism(t *testing.T) {
	build := func(step int) Batch {
		b := Batch{Entries: make([]BatchEntry, 0, 1000)}
		for i := 0; i < 1000; i++ {
			idx := (i * step) % 1000
			b.Entries = append(b.Entries, BatchEntry{
				SecretID: fmt.Sprintf("sec-%04d", idx),
				Version:  int64(idx) + 1,
				Type:     SecretTypeEnvSecret,
				Name:     fmt.Sprintf("name-%04d", idx),
				Value:    fmt.Sprintf("value-%04d", idx),
			})
		}
		return b
	}
	assert.Equal(t, BatchHash(build(1)), BatchHash(build(7)))
}

// TestLegacyBatchJSON_Shape: the mixed-fleet body is a bare []InjectedSecret
// array with Value → Plaintext and identical Type/Name/Metadata semantics.
func TestLegacyBatchJSON_Shape(t *testing.T) {
	batch := Batch{Entries: []BatchEntry{
		{
			SecretID: "sec-1", Version: 3, Type: SecretTypeLLMProvider, Name: "anthropic",
			Value:    `{"kind":"anthropic","slug":"anthropic","apiKey":"k"}`,
			Metadata: json.RawMessage(`{"z":"last","a":"first"}`),
		},
		{
			SecretID: "sec-2", Version: 1, Type: SecretTypeEnvSecret, Name: "db",
			Value: "postgres://x",
		},
	}}

	out := LegacyBatchJSON(batch)
	var legacy []InjectedSecret
	require.NoError(t, json.Unmarshal(out, &legacy))
	require.Len(t, legacy, 2)

	assert.Equal(t, SecretTypeLLMProvider, legacy[0].Type)
	assert.Equal(t, "anthropic", legacy[0].Name)
	assert.Equal(t, batch.Entries[0].Value, legacy[0].Plaintext)
	assert.Equal(t, string(batch.Entries[0].Metadata), string(legacy[0].Metadata))

	assert.Equal(t, SecretTypeEnvSecret, legacy[1].Type)
	assert.Equal(t, "null", string(legacy[1].Metadata),
		"omitted metadata keeps today's wire shape (InjectedSecret.Metadata has no omitempty → null)")
}

// TestLegacyBatchJSON_EmptyIsBareArray: agentd uses '[]' to CLEAR live
// materializations — the empty batch must serialize as the bare array,
// not null.
func TestLegacyBatchJSON_EmptyIsBareArray(t *testing.T) {
	assert.Equal(t, []byte("[]"), LegacyBatchJSON(Batch{}))
	assert.Equal(t, []byte("[]"), LegacyBatchJSON(Batch{Entries: []BatchEntry{}}))
}

// TestLegacyBatchJSON_MCPMetadataNativeTypes: mcp-server metadata keeps
// native JSON types (args array, numeric timeoutMs) — the MATERIALIZE-CONTRACT
// shape old pods already consume.
func TestLegacyBatchJSON_MCPMetadataNativeTypes(t *testing.T) {
	batch := Batch{Entries: []BatchEntry{{
		SecretID: "srv-1", Version: 1, Type: SecretTypeMcpServer, Name: "github-tools",
		Value:    `{"env":{},"headers":{}}`,
		Metadata: json.RawMessage(`{"transport":"stdio","url":"","command":"npx","args":["-y","pkg"],"timeoutMs":5000}`),
	}}}

	var legacy []InjectedSecret
	require.NoError(t, json.Unmarshal(LegacyBatchJSON(batch), &legacy))

	var meta map[string]any
	require.NoError(t, json.Unmarshal(legacy[0].Metadata, &meta))
	assert.Equal(t, []any{"-y", "pkg"}, meta["args"])
	assert.Equal(t, float64(5000), meta["timeoutMs"])
}
