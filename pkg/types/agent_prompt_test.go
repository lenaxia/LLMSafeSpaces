// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package types

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultPlatformPromptDocumentsPVCMountPoints (#1759): the
// platform-tier prompt must carry the validated mount-topology
// knowledge a cleanup agent needs — one shared PVC across the three
// persistent roots (including /tmp), the tmpfs contrast with the
// symlink contract, the live-state guardrails, and the
// inspect-all-three cleanup directive. Content pins, not goldens: the
// contract is the FACTS, not the phrasing.
func TestDefaultPlatformPromptDocumentsPVCMountPoints(t *testing.T) {
	p := DefaultPlatformPrompt
	assert.Contains(t, p, "/workspace, /tmp, and /home/sandbox are subPaths of ONE persistent volume",
		"the three persistent roots must be documented as one shared volume")
	assert.Contains(t, p, "filling one fills all",
		"the shared-capacity consequence must be stated")
	assert.Contains(t, p, "including /tmp",
		"the counter-intuitive /tmp persistence must be explicit")
	assert.Contains(t, p, "never delete the symlinks",
		"the credential-symlink contract must be stated (dangling targets are expected)")
	assert.Contains(t, p, "inspect ALL THREE persistent mount points",
		"the cleanup directive must name all three roots")
	for _, liveState := range []string{"/workspace/.local", "/tmp/opencode", "/home/sandbox/.config"} {
		assert.Contains(t, p, liveState,
			"live-state guardrail %s must be documented", liveState)
	}
	assert.Contains(t, p, "/proc/*/maps",
		"the live-mapping check before deleting hidden runtime files must be stated")
	assert.Contains(t, p, "mtime",
		"the mtime half of the mtime + /proc/*/maps check must be stated")
	assert.Contains(t, p, "regenerable",
		"the caches-are-regenerable classification must be present")
	// The retained runtime-homes bullet (issue requirement 3) — the one
	// block fact set that predates this change; pin it so a future edit
	// cannot silently drop it.
	assert.Contains(t, p, "CARGO_HOME",
		"the package/runtime home vars under /workspace/.local must stay documented")
	assert.Contains(t, p, "XDG_DATA_HOME=/workspace/.local",
		"the XDG data home placement must stay documented")
}

// TestDefaultPlatformPromptPersistenceBlockContrast: the persistence
// contrast must stay real — every documented ephemeral mount name must
// co-occur, in its own bullet, with the tmpfs/emptyDir classification
// that makes it ephemeral (a path drifting out of the classified bullet
// fails here), the superseded backwards /tmp line must not resurface,
// and the prompt must stay under the per-tier round-trip cap.
func TestDefaultPlatformPromptPersistenceBlockContrast(t *testing.T) {
	p := DefaultPlatformPrompt
	persist := "/workspace, /tmp, and /home/sandbox are subPaths of ONE persistent volume"
	idx := strings.Index(p, persist)
	assert.GreaterOrEqual(t, idx, 0, "the persistence sentence must exist")
	for _, eph := range []string{"/sandbox-runtime", "/sandbox-cfg", "/agentd-"} {
		bullet := bulletContaining(t, p, eph)
		assert.True(t, strings.Contains(bullet, "tmpfs") || strings.Contains(bullet, "emptyDir"),
			"ephemeral mount %s must be classified tmpfs/emptyDir in its bullet: %q", eph, bullet)
	}
	assert.NotContains(t, p, "do not rely on its contents surviving",
		"the superseded backwards /tmp line (#1759) must not resurface — /tmp DOES survive")
	assert.LessOrEqual(t, len(p), MaxPromptPerLevel(),
		"the default must stay under the per-tier cap so admins can round-trip it through the UI")
}

// bulletContaining returns the "- " line containing substr, failing the
// test if absent — the persistence block's unit of classification.
func bulletContaining(t *testing.T, p, substr string) string {
	t.Helper()
	for _, line := range strings.Split(p, "\n") {
		if strings.HasPrefix(line, "- ") && strings.Contains(line, substr) {
			return line
		}
	}
	t.Fatalf("no bullet contains %q", substr)
	return ""
}
