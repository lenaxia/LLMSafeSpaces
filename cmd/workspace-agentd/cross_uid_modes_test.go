// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// cross_uid_modes_test.go — US-2 (design 0051 §D1): the boot-file trio
// is written and read ACROSS the uid split (sidecar uid 2000 stamps;
// opencode/supervisor uid 1000 reads; bootstrap init uid 1000 writes;
// sidecar reads). The pod's shared gid 1000 is the bridge — which means
// these files must carry group-read bits. Credential files (secrets.json,
// secrets-env, rt/secrets/*) stay 0600: their readers are same-uid.
//
// These tests pin the modes at every write site that participates in the
// boot-read path. A mode regression here breaks sidecar-mode boot with a
// bare EACCES in opencode's config read — hard to diagnose from logs.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agent"
	opencode "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// TestBootAgentConfig_GroupReadableAfterStamp: the #857 boot stamp (the
// sidecar's ensureBootAgentConfig) must leave agent-config.json readable
// by gid 1000 — opencode (uid 1000) reads it at its first boot.
func TestBootAgentConfig_GroupReadableAfterStamp(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs.json")
	require.NoError(t, os.WriteFile(promptPath, []byte("prompt"), 0o640))
	require.NoError(t, os.WriteFile(dirsPath, []byte(`[]`), 0o640))

	w := opencode.NewConfigWriter(cfgPath,
		opencode.WithAdminPromptPath(promptPath),
		opencode.WithAllowedDirsPath(dirsPath),
	)
	_, err := w.Apply(agent.AgentConfigInput{})
	require.NoError(t, err)

	info, err := os.Stat(cfgPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o640), info.Mode().Perm(),
		"agent-config.json must be group-readable (gid 1000 = the uid-1000 reader set, design 0051 §D1 residual)")
}

// TestBootstrapPair_GroupReadable: bootstrap's admin-prompt.md and
// allowed-dirs.json (uid-1000 writes, uid-2000 sidecar reads) must be
// 0640; secrets.json stays 0600 (same-uid reader).
func TestBootstrapPair_GroupReadable(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs.json")

	require.NoError(t, atomicWriteSecrets(promptPath, []byte("p"), 0o640))
	require.NoError(t, atomicWriteSecrets(dirsPath, []byte("[]"), 0o640))

	for _, p := range []string{promptPath, dirsPath} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o640), info.Mode().Perm(), p)
	}
}

// TestMaterializeBase_AgentConfigGroupReadable: the materialize
// subcommand's base agent-config.json (uid-1000 init writes; the uid-2000
// sidecar's boot stamp READS it before rewriting) must be group-readable.
func TestMaterializeBase_AgentConfigGroupReadable(t *testing.T) {
	require.Equal(t, os.FileMode(0o640), secrets.AgentConfigWriteMode,
		"materializer's agent-config write mode must be 0640 (cross-uid boot path)")
}
