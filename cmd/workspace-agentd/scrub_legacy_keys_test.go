// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// US-72.6 — the legacy-key scrub matrix (design 0058 §8, the narrow
// platform-shaped-path boundary from design 0051 §3):
//
//   - a pre-US-35.7 REGULAR-FILE auth.json at
//     <root>/.local/opencode/auth.json → its key material is stripped
//     (structure preserved), the file survives
//   - an agent-config.json COPY under <root>/.local → provider
//     options.apiKey fields stripped
//   - the LIVE auth.json (a symlink to rt/auth.json — the #1296 shape)
//     is NEVER touched by the scrub (the platform owns it through the
//     runtime store path, not this one)
//   - user-authored files (any other name, any other shape) are NEVER
//     touched — including a user's own JSON with key-shaped fields
//   - idempotent: a second run removes nothing
func TestScrubLegacyKeys_RegularFileAuthStripsKeys(t *testing.T) {
	root := t.TempDir()
	authPath := authPathUnder(root)
	require.NoError(t, mkdirAllFor(authPath))
	require.NoError(t, writeJSONFile(authPath, map[string]any{
		"thekaocloud": map[string]any{"type": "api", "key": "sk-LEGACY-KEY-MATERIAL"},
		"zen":         map[string]any{"type": "api", "key": "sk-OTHER-LEGACY"},
	}))

	report, err := scrubLegacyKeys(root)
	require.NoError(t, err)
	assert.Equal(t, 2, report.AuthKeysRemoved, "both legacy keys removed")

	after := readJSONFile(t, authPath)
	entries := after.(map[string]any)
	require.Len(t, entries, 2, "the file and its entry structure survive")
	for k, v := range entries {
		e := v.(map[string]any)
		assert.NotContains(t, e, "key", "entry %s must lose its key", k)
		assert.Equal(t, "api", e["type"], "sibling fields preserved")
	}
}

func TestScrubLegacyKeys_AgentConfigCopiesStripProviderKeys(t *testing.T) {
	root := t.TempDir()
	cfgCopy := root + "/.local/share/agent-config.json"
	require.NoError(t, mkdirAllFor(cfgCopy))
	require.NoError(t, writeJSONFile(cfgCopy, map[string]any{
		"provider": map[string]any{
			"thekaocloud": map[string]any{"options": map[string]any{"apiKey": "sk-CONFIG-COPY-KEY", "baseURL": "https://x/v1"}},
		},
		"model": "thekaocloud/x",
	}))

	report, err := scrubLegacyKeys(root)
	require.NoError(t, err)
	assert.Equal(t, 1, report.ConfigKeysRemoved)

	after := readJSONFile(t, cfgPathOf(root)).(map[string]any)
	prov := after["provider"].(map[string]any)
	entry := prov["thekaocloud"].(map[string]any)
	opts := entry["options"].(map[string]any)
	assert.NotContains(t, opts, "apiKey", "the copied config loses the provider key")
	assert.Equal(t, "https://x/v1", opts["baseURL"], "sibling options preserved")
	assert.Equal(t, "thekaocloud/x", after["model"], "sibling config preserved")
}

// The LIVE shape: a symlink at the auth path — skipped (the platform
// owns the resolved store through its own machinery; the scrub's
// boundary is the legacy REGULAR-FILE residue only).
func TestScrubLegacyKeys_SymlinkAuthUntouched(t *testing.T) {
	root := t.TempDir()
	authPath := authPathUnder(root)
	require.NoError(t, mkdirAllFor(authPath))
	target := root + "/rt-auth-target.json"
	require.NoError(t, writeJSONFile(target, map[string]any{
		"thekaocloud": map[string]any{"type": "api", "key": "sk-LIVE-STORE-KEY"},
	}))
	require.NoError(t, symlink(target, authPath))

	report, err := scrubLegacyKeys(root)
	require.NoError(t, err)
	assert.Equal(t, 0, report.AuthKeysRemoved, "a symlink is not legacy residue")
	after := readJSONFile(t, target)
	entries := after.(map[string]any)
	assert.Contains(t, entries["thekaocloud"].(map[string]any), "key",
		"the resolved store is untouched through the scrub path")
}

// The design 0051 §3 boundary: user files are NEVER touched — even
// with key-shaped fields, even inside the walked tree, if they are not
// the platform-shaped names.
func TestScrubLegacyKeys_UserFilesUntouched(t *testing.T) {
	root := t.TempDir()
	userFile := root + "/.local/opencode/my-auth-backup.json"
	require.NoError(t, mkdirAllFor(userFile))
	original := `{"myprovider": {"type": "api", "key": "sk-USERS-OWN-KEY"}}`
	require.NoError(t, writeRaw(userFile, original))

	report, err := scrubLegacyKeys(root)
	require.NoError(t, err)
	assert.Equal(t, 0, report.AuthKeysRemoved)
	assert.Equal(t, 0, report.ConfigKeysRemoved)
	assert.Equal(t, original, readRaw(t, userFile), "byte-identical — the user's file is not ours")
}

// A user-authored agent-config.json in an unrelated tree is not
// scrubbed either (the copy class is under .local only).
func TestScrubLegacyKeys_ConfigOutsideLocalUntouched(t *testing.T) {
	root := t.TempDir()
	outside := root + "/projects/agent-config.json"
	require.NoError(t, mkdirAllFor(outside))
	original := `{"provider":{"x":{"options":{"apiKey":"sk-NOT-OURS"}}}}`
	require.NoError(t, writeRaw(outside, original))

	report, err := scrubLegacyKeys(root)
	require.NoError(t, err)
	assert.Equal(t, 0, report.ConfigKeysRemoved)
	assert.Equal(t, original, readRaw(t, outside))
}

func TestScrubLegacyKeys_Idempotent(t *testing.T) {
	root := t.TempDir()
	authPath := authPathUnder(root)
	require.NoError(t, mkdirAllFor(authPath))
	require.NoError(t, writeJSONFile(authPath, map[string]any{
		"thekaocloud": map[string]any{"type": "api", "key": "sk-LEGACY"},
	}))

	first, err := scrubLegacyKeys(root)
	require.NoError(t, err)
	assert.Equal(t, 1, first.AuthKeysRemoved)

	second, err := scrubLegacyKeys(root)
	require.NoError(t, err)
	assert.Equal(t, 0, second.AuthKeysRemoved, "the second run removes nothing")
	assert.True(t, second.AlreadyClean)
}
