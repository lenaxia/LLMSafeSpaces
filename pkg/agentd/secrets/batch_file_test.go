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
