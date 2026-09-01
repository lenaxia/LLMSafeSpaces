// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// batch_file_test.go — US-70.2 Part 2: the batch-file loader accepts
// BOTH wire generations — the revisioned envelope
// ({"entries":[...],"revision":{...}}) and the legacy bare array. The
// revision rides only in the envelope; legacy parses to a nil revision.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sec "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

func writeBatchFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestLoadBatchFile_Envelope(t *testing.T) {
	path := writeBatchFile(t, `{"entries":[
		{"secretId":"s1","version":2,"type":"env-secret","name":"db","value":"v","metadata":{"var_name":"DB"}},
		{"secretId":"s2","version":1,"type":"mcp-server","name":"m","value":"{}","metadata":{"transport":"stdio","args":["-y","pkg"]}}
	],"revision":{"seq":5,"manifestHash":"mh","batchHash":"bh"}}`)

	bf, err := LoadBatchFile(path)
	require.NoError(t, err)
	require.Len(t, bf.Secrets, 2)
	require.NotNil(t, bf.Revision)
	assert.EqualValues(t, 5, bf.Revision.Seq)
	assert.Equal(t, "mh", bf.Revision.ManifestHash)

	assert.Equal(t, "env-secret", bf.Secrets[0].Type)
	assert.Equal(t, "db", bf.Secrets[0].Name)
	assert.Equal(t, "v", bf.Secrets[0].Plaintext)
	assert.Equal(t, "DB", bf.Secrets[0].Metadata["var_name"])

	assert.Equal(t, "mcp-server", bf.Secrets[1].Type)
	assert.Equal(t, `["-y","pkg"]`, bf.Secrets[1].Metadata["args"],
		"native JSON metadata types ride through the same dual-shape contract as the legacy wire")
}

func TestLoadBatchFile_LegacyArray_NilRevision(t *testing.T) {
	path := writeBatchFile(t, `[{"type":"env-secret","name":"db","metadata":{"var_name":"DB"},"plaintext":"v"}]`)

	bf, err := LoadBatchFile(path)
	require.NoError(t, err)
	require.Len(t, bf.Secrets, 1)
	assert.Nil(t, bf.Revision, "legacy batches carry no revision")
	assert.Equal(t, "v", bf.Secrets[0].Plaintext)
}

func TestLoadBatchFile_EmptyShapes(t *testing.T) {
	for name, body := range map[string]string{
		"empty legacy":    `[]`,
		"empty envelope":  `{"entries":[],"revision":{"seq":1,"manifestHash":"mh","batchHash":"bh"}}`,
		"envelope no rev": `{"entries":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			bf, err := LoadBatchFile(writeBatchFile(t, body))
			require.NoError(t, err)
			assert.Empty(t, bf.Secrets)
			if name == "empty envelope" {
				require.NotNil(t, bf.Revision)
			} else {
				assert.Nil(t, bf.Revision)
			}
		})
	}
}

func TestLoadBatchFile_Malformed(t *testing.T) {
	for name, body := range map[string]string{
		"not json":        `{{{`,
		"entries not arr": `{"entries":42}`,
		"entry not obj":   `{"entries":[42]}`,
		"legacy not obj":  `[42]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadBatchFile(writeBatchFile(t, body))
			require.Error(t, err, "malformed batch files must fail loudly (exit 2 class)")
		})
	}
}

func TestLoadBatchFile_Absent(t *testing.T) {
	_, err := LoadBatchFile(filepath.Join(t.TempDir(), "absent.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

// --- merge semantics -------------------------------------------------------
//
// Envelope base + legacy reload cache: the cache is the newer live
// state and wins per Type+Name. The envelope's revision survives ONLY
// when the effective set is the envelope's set (empty cache); any cache
// overlay that changes the set drops it — a merged set is no longer the
// revisioned pull.

func TestMergeBatchFile_EmptyCache_KeepsRevision(t *testing.T) {
	base := BatchFile{
		Secrets:  []Secret{{Type: "env-secret", Name: "db", Plaintext: "v"}},
		Revision: &sec.BatchRevision{Seq: 3, ManifestHash: "mh"},
	}
	got, rev := MergeBatchFile(base, nil)
	assert.Equal(t, base.Secrets, got)
	assert.NotNil(t, rev, "empty cache ⇒ the revisioned pull IS the effective set")
	assert.EqualValues(t, 3, rev.Seq)
}

func TestMergeBatchFile_CacheOverlay_DropsRevision(t *testing.T) {
	base := BatchFile{
		Secrets:  []Secret{{Type: "env-secret", Name: "db", Plaintext: "old"}},
		Revision: &sec.BatchRevision{Seq: 3, ManifestHash: "mh"},
	}
	cache := []Secret{{Type: "env-secret", Name: "db", Plaintext: "new"}}

	got, rev := MergeBatchFile(base, cache)
	require.Len(t, got, 1)
	assert.Equal(t, "new", got[0].Plaintext, "cache wins on Type+Name")
	assert.Nil(t, rev, "overlay changed the effective set ⇒ revision dropped")
}

func TestMergeBatchFile_CacheAdditive_DropsRevision(t *testing.T) {
	base := BatchFile{
		Secrets:  []Secret{{Type: "env-secret", Name: "db", Plaintext: "v"}},
		Revision: &sec.BatchRevision{Seq: 3, ManifestHash: "mh"},
	}
	cache := []Secret{{Type: "ssh-key", Name: "k", Plaintext: "kk"}}

	got, rev := MergeBatchFile(base, cache)
	require.Len(t, got, 2)
	assert.Nil(t, rev, "an additive overlay changes the effective set ⇒ revision dropped")
}

func TestMergeBatchFile_LegacyBase_StaysLegacy(t *testing.T) {
	base := BatchFile{Secrets: []Secret{{Type: "env-secret", Name: "db", Plaintext: "v"}}}
	cache := []Secret{{Type: "env-secret", Name: "db", Plaintext: "new"}}

	got, rev := MergeBatchFile(base, cache)
	require.Len(t, got, 1)
	assert.Equal(t, "new", got[0].Plaintext)
	assert.Nil(t, rev)
}

// --- MergeSecretBatches: the Type+Name overlay (ported from the deleted
// cmd-local copy; cache wins on duplicate, distinct entries all present) ---

func TestMergeSecretBatches_CacheWinsOnDuplicate(t *testing.T) {
	base := []Secret{
		{Type: "env-secret", Name: "gh", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "base-value"},
		{Type: "ssh-key", Name: "k", Metadata: map[string]string{"key_type": "ed25519"}, Plaintext: "base-ssh"},
	}
	cache := []Secret{
		{Type: "env-secret", Name: "gh", Metadata: map[string]string{"var_name": "GH_TOKEN"}, Plaintext: "cache-value"},
	}

	merged := MergeSecretBatches(base, cache)

	require.Len(t, merged, 2)
	assert.Equal(t, "cache-value", merged[0].Plaintext, "cache must win for duplicate Type+Name")
	assert.Equal(t, "base-ssh", merged[1].Plaintext, "non-duplicated base entries survive")
}

func TestMergeSecretBatches_NoDuplicate_AllPresent(t *testing.T) {
	base := []Secret{{Type: "llm-provider", Name: "anthropic", Plaintext: `{}`}}
	cache := []Secret{
		{Type: "env-secret", Name: "gh", Plaintext: "tok"},
		{Type: "git-credential", Name: "g", Plaintext: "user:pass"},
	}
	assert.Len(t, MergeSecretBatches(base, cache), 3, "all distinct entries from both batches must be present")
}

func TestMergeSecretBatches_EmptyBaseAndLayered(t *testing.T) {
	assert.Empty(t, MergeSecretBatches(nil, nil))
	assert.Empty(t, MergeSecretBatches([]Secret{}, []Secret{}))
	assert.Equal(t, []Secret{{Type: "env-secret", Name: "x", Plaintext: "v"}},
		MergeSecretBatches([]Secret{{Type: "env-secret", Name: "x", Plaintext: "v"}}, nil))
	assert.Equal(t, []Secret{{Type: "env-secret", Name: "x", Plaintext: "v"}},
		MergeSecretBatches(nil, []Secret{{Type: "env-secret", Name: "x", Plaintext: "v"}}))
}

func TestMergeSecretBatches_SameTypeDifferentName_BothKept(t *testing.T) {
	base := []Secret{{Type: "env-secret", Name: "a", Plaintext: "1"}}
	cache := []Secret{{Type: "env-secret", Name: "b", Plaintext: "2"}}
	assert.Len(t, MergeSecretBatches(base, cache), 2,
		"different Name under the same Type is NOT a duplicate")
}
