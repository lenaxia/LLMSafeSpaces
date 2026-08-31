// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
)

// BatchEntry is one identified, versioned secret in a workspace batch —
// the W12 entry contract: (secret_id, version, type, name, value) plus
// stored (never derived) metadata where the type requires it. No
// timestamps, no derived content.
type BatchEntry struct {
	SecretID string          `json:"secretId"`
	Version  int64           `json:"version"`
	Type     SecretType      `json:"type"`
	Name     string          `json:"name"`
	Value    string          `json:"value"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// Batch is the canonical value-bearing secret batch for a workspace,
// stamped with its two-tier revision (design 0057 R1). Entries keep
// construction order (the legacy wire order); hashing goes through
// CanonicalBatch, which sorts.
type Batch struct {
	Entries  []BatchEntry  `json:"entries"`
	Revision BatchRevision `json:"revision"`
}

// BatchRevision is the two-tier revision of a built batch: Seq +
// ManifestHash identify the intended (decrypt-free) set as minted by the
// workspace revision row; BatchHash covers the actual value-bearing
// bytes for transport integrity and conditional pulls.
type BatchRevision struct {
	Seq          int64  `json:"seq"`
	ManifestHash string `json:"manifestHash"`
	BatchHash    string `json:"batchHash"`
}

// ManifestEntry is BatchEntry minus the value: the decrypt-free tier.
// Hashing it lets any replica decide whether a workspace's secret set
// changed without touching a single ciphertext.
type ManifestEntry struct {
	SecretID string          `json:"secretId"`
	Version  int64           `json:"version"`
	Type     SecretType      `json:"type"`
	Name     string          `json:"name"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// CanonicalBatch returns the canonical JSON encoding of the batch's
// entries: sorted by (Type, SecretID, Name), with object keys sorted by
// encoding/json — deterministic under any entry order or map insertion
// order (epic #1158 invariant I6). Per-entry metadata marshals as stored;
// key order within it is stable because PG jsonb output and the sole
// metadata-producing path (mcpServerMetadata, a Go map) both emit sorted
// keys. The revision stamp is deliberately
// excluded: canonical bytes describe content, not bookkeeping. The
// input Batch is never mutated.
func CanonicalBatch(batch Batch) []byte {
	entries := make([]BatchEntry, len(batch.Entries))
	copy(entries, batch.Entries)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Type != entries[j].Type {
			return entries[i].Type < entries[j].Type
		}
		if entries[i].SecretID != entries[j].SecretID {
			return entries[i].SecretID < entries[j].SecretID
		}
		return entries[i].Name < entries[j].Name
	})
	out, err := json.Marshal(entries)
	if err != nil {
		panic("secrets: marshal canonical batch: " + err.Error())
	}
	return out
}

// BatchHash returns the hex SHA-256 over CanonicalBatch — the
// value-bearing tier used for transport integrity and conditional pulls.
func BatchHash(batch Batch) string {
	sum := sha256.Sum256(CanonicalBatch(batch))
	return hex.EncodeToString(sum[:])
}

// ManifestHash returns the hex SHA-256 over the canonical manifest lines:
// the first line is "owner|<ownerID>", then one line per entry sorted by
// (Type, SecretID, Name), each "secretId\x00version\x00type\x00name\x00<metadata>"
// where <metadata> is the canonical (sorted-key, normalized) metadata
// JSON or empty. Deterministic under any input ordering (I6); scoped to
// the owner so identical entry sets never collide across owners.
func ManifestHash(ownerID string, entries []ManifestEntry) string {
	sorted := make([]ManifestEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Type != sorted[j].Type {
			return sorted[i].Type < sorted[j].Type
		}
		if sorted[i].SecretID != sorted[j].SecretID {
			return sorted[i].SecretID < sorted[j].SecretID
		}
		return sorted[i].Name < sorted[j].Name
	})

	h := sha256.New()
	h.Write([]byte("owner|" + ownerID))
	h.Write([]byte{'\n'})
	for _, e := range sorted {
		h.Write([]byte(e.SecretID))
		h.Write([]byte{0})
		h.Write([]byte(strconv.FormatInt(e.Version, 10)))
		h.Write([]byte{0})
		h.Write([]byte(e.Type))
		h.Write([]byte{0})
		h.Write([]byte(e.Name))
		h.Write([]byte{0})
		h.Write(canonicalMetadata(e.Metadata))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalMetadata normalizes a metadata JSON object to its canonical
// form: parsed and re-marshaled, which sorts keys and drops
// representation-level differences (whitespace, key order, equivalent
// number spellings). nil and empty objects canonicalize to empty bytes —
// they describe the same manifest.
func canonicalMetadata(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(raw)
	}
	if len(v) == 0 {
		return nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(raw)
	}
	return out
}

// LegacyBatchJSON renders the batch as the bare []InjectedSecret array
// the mixed fleet already consumes (W15): Value becomes Plaintext;
// Type/Name/Metadata keep their exact wire semantics, including native
// JSON types inside mcp-server metadata. An empty batch renders as "[]"
// so old pods CLEAR their live materializations.
func LegacyBatchJSON(batch Batch) []byte {
	out := make([]InjectedSecret, 0, len(batch.Entries))
	for _, e := range batch.Entries {
		out = append(out, InjectedSecret{
			Type:      e.Type,
			Name:      e.Name,
			Metadata:  e.Metadata,
			Plaintext: e.Value,
		})
	}
	data, err := json.Marshal(out)
	if err != nil {
		panic("secrets: marshal legacy batch: " + err.Error())
	}
	return data
}
