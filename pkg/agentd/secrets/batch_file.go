// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// batch_file.go — US-70.2 Part 2: the batch-file tier. The file the
// bootstrap subcommand persists (and the materializer consumes) accepts
// BOTH wire generations: the revisioned envelope the v2 API serves
// ({"entries":[...],"revision":{...}} — written verbatim by bootstrap)
// and the legacy bare array (mixed fleet, pushes). The revision rides
// only in the envelope.

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// BatchFile is a parsed secrets batch: the materialization entries plus
// the revision the entries arrived under. Revision is nil for legacy
// batches (bare arrays and pushes) — no revision anchoring, byte-compat
// with the pre-US-70.2 behavior.
type BatchFile struct {
	Secrets  []Secret
	Revision *sec.BatchRevision
}

// LoadBatchFile reads and parses a secrets batch file, accepting both
// wire generations. Envelope detection is by the "entries" key: the v2
// API's envelope always carries it; a legacy file is a JSON array.
func LoadBatchFile(path string) (BatchFile, error) {
	//nolint:gosec // G304: path is the deployment-configured batch coordinate (--from / sidecar tmpfs)
	data, err := os.ReadFile(path)
	if err != nil {
		return BatchFile{}, fmt.Errorf("reading %s: %w", path, err)
	}
	bf, err := ParseBatchFile(data)
	if err != nil {
		return BatchFile{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return bf, nil
}

// ParseBatchFile parses raw batch bytes into a BatchFile.
func ParseBatchFile(data []byte) (BatchFile, error) {
	var probe struct {
		Entries  json.RawMessage    `json:"entries"`
		Revision *sec.BatchRevision `json:"revision"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return batchFileFromLegacy(data, err)
	}
	if probe.Entries == nil {
		return batchFileFromLegacy(data, nil)
	}

	var envelope sec.Batch
	if err := json.Unmarshal(data, &envelope); err != nil {
		return BatchFile{}, err
	}
	out := BatchFile{Secrets: make([]Secret, 0, len(envelope.Entries))}
	for _, e := range envelope.Entries {
		out.Secrets = append(out.Secrets, secretFromBatchEntry(e))
	}
	out.Revision = probe.Revision
	return out, nil
}

// batchFileFromLegacy decodes the legacy bare array, distinguishing a
// genuine parse failure (returned as err, or envelopeErr when the probe
// rejected an object-shaped body) from a non-array body.
func batchFileFromLegacy(data []byte, envelopeErr error) (BatchFile, error) {
	var legacy []Secret
	if err := json.Unmarshal(data, &legacy); err != nil {
		if envelopeErr != nil {
			return BatchFile{}, envelopeErr
		}
		return BatchFile{}, err
	}
	return BatchFile{Secrets: legacy}, nil
}

// secretFromBatchEntry converts an envelope entry to the
// materialization representation through the SAME wire path a legacy
// entry takes (Secret.UnmarshalJSON), preserving the dual-shape
// metadata contract and the MetadataInvalid verdict semantics.
func secretFromBatchEntry(e sec.BatchEntry) Secret {
	wire, err := json.Marshal(map[string]any{
		"type":      string(e.Type),
		"name":      e.Name,
		"metadata":  e.Metadata,
		"plaintext": e.Value,
	})
	if err != nil {
		return Secret{Type: string(e.Type), Name: e.Name, Plaintext: e.Value}
	}
	var s Secret
	if err := json.Unmarshal(wire, &s); err != nil {
		return Secret{Type: string(e.Type), Name: e.Name, Plaintext: e.Value}
	}
	return s
}

// revisionAnchor renders a revision as the anchor string the spawn
// seams carry: "<seq>:<manifestHash>". This is the prefix of the served
// rev ("<seq>:<manifestHash>:<contentHash>") and the content of the
// <secrets-env>.rev sibling anchor.
func revisionAnchor(rev *sec.BatchRevision) string {
	if rev == nil {
		return ""
	}
	return fmt.Sprintf("%d:%s", rev.Seq, rev.ManifestHash)
}

// MergeBatchFile merges the boot base (a pulled batch, possibly an
// envelope) with the reload-secrets cache (the newer live state,
// always legacy-shaped). The cache wins per Type+Name.
//
// Revision semantics: the revision describes the PULLED set. An empty
// cache leaves the effective set equal to the pull — the revision is
// kept. Any cache overlay that changes the effective set (replacement
// or addition) produces a set the revision does not describe — it is
// dropped, and the effective batch behaves legacy (no anchoring, no
// apply-guard).
func MergeBatchFile(base BatchFile, cache []Secret) ([]Secret, *sec.BatchRevision) {
	if len(cache) == 0 {
		return base.Secrets, base.Revision
	}
	merged := MergeSecretBatches(base.Secrets, cache)
	if base.Revision != nil && reflect.DeepEqual(base.Secrets, merged) {
		return merged, base.Revision
	}
	return merged, nil
}

// MergeSecretBatches combines a base batch with a layered batch,
// resolving duplicates in favor of the layered batch (the dedup key is
// Type+Name — the materializer's identity for a secret within a
// workspace). Base order is preserved; layered replacements land in
// place, layered additions append.
func MergeSecretBatches(base, layered []Secret) []Secret {
	if len(base) == 0 {
		return layered
	}
	seen := make(map[string]int, len(base)+len(layered))
	merged := make([]Secret, 0, len(base)+len(layered))
	key := func(s Secret) string { return s.Type + "\x00" + s.Name }
	for _, s := range base {
		seen[key(s)] = len(merged)
		merged = append(merged, s)
	}
	for _, s := range layered {
		if idx, ok := seen[key(s)]; ok {
			merged[idx] = s
			continue
		}
		seen[key(s)] = len(merged)
		merged = append(merged, s)
	}
	return merged
}
