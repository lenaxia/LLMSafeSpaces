// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

// staging_rev_test.go — US-70.2 Part 2: the staged-file manifest is
// revision-anchored when the materialized batch carries a revision. The
// anchor is ADDITIVE: a revisioned publish writes
// {"rev":"<seq>:<manifestHash>","entries":[...]}; a legacy publish
// (push batches, no revision) keeps the bare array — byte-identical to
// the pre-revision manifest, so old readers never see a shape change.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func stageAndPublish(t *testing.T, dir string, rev string) {
	t.Helper()
	b := newStageBuilder(RealFS(), dir)
	require.NoError(t, b.add("k1", "/target/id_ed25519", ModeSSHPrivateKey, []byte("bytes")))
	b.rev = rev
	require.NoError(t, b.publish())
}

func TestStageBuilder_LegacyPublish_ByteCompat(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "staged")
	stageAndPublish(t, dir, "")

	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	require.NoError(t, err)
	assert.Equal(t, `[{"target":"/target/id_ed25519","mode":384,"file":"k1"}]`, string(data),
		"a legacy publish is the bare array, byte-identical to the pre-revision manifest")
}

func TestStageBuilder_RevisionedPublish_ObjectShape(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "staged")
	stageAndPublish(t, dir, "9:mh-9")

	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	require.NoError(t, err)
	var obj map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &obj))
	require.Contains(t, obj, "rev")
	require.Contains(t, obj, "entries")
	assert.Equal(t, `"9:mh-9"`, string(obj["rev"]))

	var entries []StagedEntry
	require.NoError(t, json.Unmarshal(obj["entries"], &entries))
	require.Len(t, entries, 1)
	assert.Equal(t, "/target/id_ed25519", entries[0].Target)
}

func TestReadStagingManifest_BothShapes(t *testing.T) {
	legacy := `[{"target":"/t","mode":384,"file":"k1"}]`
	entries, rev, err := ReadStagingManifest([]byte(legacy))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Empty(t, rev)

	revisioned := `{"rev":"4:mh","entries":[{"target":"/t","mode":384,"file":"k1"}]}`
	entries, rev, err = ReadStagingManifest([]byte(revisioned))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "4:mh", rev)

	_, _, err = ReadStagingManifest([]byte(`{"entries":"nope"}`))
	require.Error(t, err)
}
