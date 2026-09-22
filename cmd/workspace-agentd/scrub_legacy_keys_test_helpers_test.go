// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func authPathUnder(root string) string { return filepath.Join(root, ".local", "opencode", "auth.json") }
func cfgPathOf(root string) string {
	return filepath.Join(root, ".local", "share", "agent-config.json")
}
func mkdirAllFor(path string) error       { return os.MkdirAll(filepath.Dir(path), 0o755) }
func writeRaw(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeRaw(path, string(b))
}
func symlink(target, link string) error { return os.Symlink(target, link) }
func readJSONFile(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var v any
	require.NoError(t, json.Unmarshal(b, &v))
	return v
}
func readRaw(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}
