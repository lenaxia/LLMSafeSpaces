// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Epic 69 US-69.2: the platform/ subPath handling of `init-fs`.
//
//	create — full run creates platform/ alongside the other roots
//	         (single-container: owned by the running uid 1000).
//	skip   — sidecar mode: platform-init must NOT create it (a uid-1000
//	         mkdir would own the dir and the uid-2000 sidecar could not write).
//	only   — the sidecar-mode platform-dirs init (uid 2000) creates only
//	         platform/, owned by the running uid.
//
// Ownership follows the creator because cross-uid chown is impossible for
// non-root; the pod spec pins the container uid (see platform_subpath_test.go).
// Cross-uid behavior itself is validated at the kind layer; these tests pin
// the script contract and modes.

func TestInitFS_PlatformSubPath_Create(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")

	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	info, err := os.Stat(filepath.Join(tr.pvc, "platform"))
	require.NoError(t, err, "default (create) must make platform/")
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm(),
		"platform/ mode 0750 — 0640 on a directory breaks traversal; the 0640 rule applies to payload files")
}

func TestInitFS_PlatformSubPath_Skip_Sidecar(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")

	exit, stderr := runInitFSSubcommand(t, bin, append(tr.args(), "--platform-subpath=skip")...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	_, err := os.Stat(filepath.Join(tr.pvc, "platform"))
	assert.True(t, os.IsNotExist(err),
		"sidecar-mode platform-init must NOT create platform/ — ownership belongs to the uid-2000 init")
}

func TestInitFS_PlatformSubPath_Only(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)

	// Only-mode needs no password source (the sidecar-mode platform-dirs
	// init mounts just the PVC + agentd image volume).
	exit, stderr := runInitFSSubcommand(t, bin, "--pvc-root="+tr.pvc, "--platform-subpath=only")
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	info, err := os.Stat(filepath.Join(tr.pvc, "platform"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm())

	for _, d := range []string{"workspace", "home", "tmp"} {
		_, err := os.Stat(filepath.Join(tr.pvc, d))
		assert.True(t, os.IsNotExist(err), "only-mode must not create %s (platform-init's job)", d)
	}
	_, err = os.Stat(tr.cfg + "/password")
	assert.True(t, os.IsNotExist(err), "only-mode must not install credentials")
}

func TestInitFS_PlatformSubPath_ExistingPVC_Idempotent(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")

	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)
	// Pre-existing platform/ with content survives a re-run (existing PVCs
	// boot through the same init every time).
	payload := filepath.Join(tr.pvc, "platform", "seq-cursor")
	require.NoError(t, os.WriteFile(payload, []byte("41\n"), 0o640))

	exit, stderr = runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)
	data, err := os.ReadFile(payload)
	require.NoError(t, err)
	assert.Equal(t, "41\n", string(data), "re-run over an existing platform/ must not disturb payloads")

	info, err := os.Stat(filepath.Join(tr.pvc, "platform"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm())
}
