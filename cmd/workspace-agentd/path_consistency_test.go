// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// =============================================================================
// Level 1: Path Resolution Consistency
//
// These tests verify that all three path-resolution mechanisms agree:
//   1. loadMaterializeConfig() — the production resolver (cmd/workspace-agentd)
//   2. DefaultPaths() — the fallback resolver (pkg/agentd/secrets)
//   3. agentd package constants — the source of truth (pkg/agentd/types.go)
//
// If any two diverge, the Materializer writes to one path while a reader
// (opencode, git, ssh) reads from another — credentials silently lost or
// silently written to the PVC.
// =============================================================================

// TestPathResolution_AllResolversAgreeOnTmpfs verifies that the production
// resolver (loadMaterializeConfig), the fallback (DefaultPaths), and the
// agentd constants all resolve credential paths to /sandbox-runtime (tmpfs),
// never to PVC-backed paths.
func TestPathResolution_AllResolversAgreeOnTmpfs(t *testing.T) {
	// Clear all env overrides so we test production defaults.
	for _, key := range []string{
		"LLMSAFESPACES_SECRETS_BASE_DIR",
		"LLMSAFESPACES_SSH_DIR",
		"LLMSAFESPACES_AGENT_CONFIG_PATH",
		"LLMSAFESPACES_SECRETS_ENV_PATH",
		"LLMSAFESPACES_GIT_CREDS_PATH",
	} {
		t.Setenv(key, "")
	}

	prod := loadMaterializeConfig().toPaths()
	fallback := secrets.DefaultPaths(os.Getenv("HOME"))

	paths := []struct {
		name   string
		prod   string
		fb     string
		constV string
	}{
		{"SecretsBaseDir", prod.SecretsBaseDir, fallback.SecretsBaseDir, agentd.SecretsBasePath},
		{"AgentConfigPath", prod.AgentConfigPath, fallback.AgentConfigPath, agentd.AgentConfigPath},
		{"SecretsEnvPath", prod.SecretsEnvPath, fallback.SecretsEnvPath, agentd.SecretsEnvPath},
	}

	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			assert.Equal(t, p.constV, p.prod,
				"loadMaterializeConfig must match agentd constant for %s", p.name)
			assert.Equal(t, p.constV, p.fb,
				"DefaultPaths must match agentd constant for %s", p.name)
			assert.True(t, strings.HasPrefix(p.prod, "/sandbox-runtime"),
				"%s must resolve to /sandbox-runtime (tmpfs), got %s", p.name, p.prod)
		})
	}

	// SSH and git paths are not agentd constants (they're derived), so check
	// prod vs fallback agreement + tmpfs prefix only.
	assert.Equal(t, prod.SSHDir, fallback.SSHDir,
		"SSHDir: loadMaterializeConfig and DefaultPaths must agree")
	assert.True(t, strings.HasPrefix(prod.SSHDir, "/sandbox-runtime"),
		"SSHDir must be tmpfs, got %s", prod.SSHDir)

	assert.Equal(t, prod.GitCredsPath, fallback.GitCredsPath,
		"GitCredsPath: loadMaterializeConfig and DefaultPaths must agree")
	assert.True(t, strings.HasPrefix(prod.GitCredsPath, "/sandbox-runtime"),
		"GitCredsPath must be tmpfs, got %s", prod.GitCredsPath)

}

// TestPathResolution_ToPathsPreservesAllFields verifies the toPaths() bridge
// doesn't silently drop a field. If a field is added to materializeConfig but
// not toPaths(), the Materializer would use the zero-value path (empty string
// → cwd-relative writes → unpredictable behavior).
func TestPathResolution_ToPathsPreservesAllFields(t *testing.T) {
	cfg := materializeConfig{
		home:            "/test/home",
		secretsBaseDir:  "/test/secrets",
		sshDir:          "/test/ssh",
		agentConfigPath: "/test/agent.json",
		secretsEnvPath:  "/test/env",
		gitCredsPath:    "/test/git",
	}

	paths := cfg.toPaths()

	assert.Equal(t, cfg.home, paths.Home)
	assert.Equal(t, cfg.secretsBaseDir, paths.SecretsBaseDir)
	assert.Equal(t, cfg.sshDir, paths.SSHDir)
	assert.Equal(t, cfg.agentConfigPath, paths.AgentConfigPath)
	assert.Equal(t, cfg.secretsEnvPath, paths.SecretsEnvPath)
	assert.Equal(t, cfg.gitCredsPath, paths.GitCredsPath)
}

// TestPathResolution_EnricherCacheStaysOnPVC verifies the enricher cache
// (model list cache) is NOT on tmpfs — it must persist across reloads to
// avoid re-fetching /v1/models from every provider on every credential cycle.
func TestPathResolution_EnricherCacheStaysOnPVC(t *testing.T) {
	t.Setenv("LLMSAFESPACES_ENRICHER_CACHE_DIR", "")
	cfg := loadMaterializeConfig()

	assert.False(t, strings.HasPrefix(cfg.enricherCacheDir, "/sandbox-runtime"),
		"enricher cache must NOT be on tmpfs (would be wiped by reset); got %s", cfg.enricherCacheDir)
	assert.Contains(t, cfg.enricherCacheDir, "/.local/state/",
		"enricher cache must be under $HOME/.local/state (PVC subPath home)")
}
