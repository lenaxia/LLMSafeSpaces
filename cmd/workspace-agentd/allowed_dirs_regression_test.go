package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadAllowedDirs_ResetOnReload is a regression test for the
// loadAllowedDirs append bug. The initial constructor call loads the
// default path (which may contain /tmp/* in a real workspace). A
// subsequent call with a different (non-existent) path must clear
// the stale entries — not leave them behind via append.
func TestLoadAllowedDirs_ResetOnReload(t *testing.T) {
	dir := t.TempDir()

	// Write a real allowed-dirs file with entries.
	goodPath := filepath.Join(dir, "allowed-dirs.json")
	require.NoError(t, os.WriteFile(goodPath, []byte(`["/tmp/*","/custom/*"]`), 0644))

	// Write a non-existent path.
	badPath := filepath.Join(dir, "nonexistent.json")

	path := filepath.Join(dir, "agent-config.json")
	w := newAgentConfigWriter(path)

	// Step 1: load from the good path — should populate allowedDirs.
	w.setAllowedDirsPath(goodPath)
	w.loadAllowedDirs()
	require.NotEmpty(t, w.allowedDirs,
		"loading from a valid file must populate allowedDirs")
	assert.Contains(t, w.allowedDirs, "/tmp/*")

	// Step 2: switch to a non-existent path and reload.
	w.setAllowedDirsPath(badPath)
	w.loadAllowedDirs()

	// Step 3: allowedDirs must be empty — the stale entries from step 1
	// must NOT survive the reload. Before the fix, loadAllowedDirs appended
	// (leaving /tmp/* and /custom/* behind), causing the mode block to be
	// emitted when it shouldn't be.
	assert.Empty(t, w.allowedDirs,
		"loading from a non-existent path must clear stale entries (append bug regression)")
}
