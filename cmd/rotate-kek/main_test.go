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

// TestRun_MasterKeyFileTooShortFailsClosed mirrors migrate-kek's
// TestRun_MasterKeyFileTooShortFailsClosed: a master keyfile shorter than
// 32 bytes must fail the run before any DB connection is attempted
// (fail-closed, not weak-key).
func TestRun_MasterKeyFileTooShortFailsClosed(t *testing.T) {
	dir := t.TempDir()
	oldKey := filepath.Join(dir, "old.key")
	newKey := filepath.Join(dir, "new.key")
	require.NoError(t, os.WriteFile(oldKey, []byte("0f0e0d0c0b0a09080706050403020100"), 0o600)) // 30 hex chars → 15 bytes
	require.NoError(t, os.WriteFile(newKey, []byte("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"), 0o600))
	err := run(oldKey, newKey, "postgres://127.0.0.1:1/nope", "", "all", "", 2, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shorter than 32 bytes")
	assert.Contains(t, err.Error(), "old master key", "the OLD key file is the short one")
	assert.NotContains(t, err.Error(), "connect to Postgres", "the short-key failure must fire before any DB connection")
}

// TestRun_TableAllWithResumeFromRejected pins the guard: --resume-from is a
// per-table cursor, but RotateAll takes no cursor — silently ignoring the
// flag would strand pre-cursor rows. Suggested by PR #1409 review.
func TestRun_TableAllWithResumeFromRejected(t *testing.T) {
	dir := t.TempDir()
	oldKey := filepath.Join(dir, "old.key")
	newKey := filepath.Join(dir, "new.key")
	require.NoError(t, os.WriteFile(oldKey, []byte("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"), 0o600))
	require.NoError(t, os.WriteFile(newKey, []byte("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"), 0o600))
	err := run(oldKey, newKey, "postgres://127.0.0.1:1/nope", "", "all", "some-row-id", 2, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--resume-from applies per table")
}

// TestPrintResumeHint_DryRunNeverSuggestsResumeFrom is the PR #1409 review
// Finding 2 regression: in a dry-run nothing was written, so suggesting
// --resume-from would make the apply run SKIP every row before the cursor —
// stranding them on the old KEK. The dry-run hint must tell the operator to
// re-run plainly instead.
func TestPrintResumeHint_DryRunNeverSuggestsResumeFrom(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stderr
	os.Stderr = w
	printResumeHint("api_keys", "key-9", true)
	w.Close()
	os.Stderr = saved
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()
	out := string(buf[:n])

	assert.NotContains(t, out, "--resume-from key-9", "dry-run hint must not suggest resuming FROM the reported cursor")
	assert.Contains(t, out, "re-run without --dry-run")
}

// TestPrintResumeHint_ApplyRunSuggestsResumeFrom pins the apply-run shape.
func TestPrintResumeHint_ApplyRunSuggestsResumeFrom(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stderr
	os.Stderr = w
	printResumeHint("api_keys", "key-9", false)
	w.Close()
	os.Stderr = saved
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	r.Close()
	out := string(buf[:n])

	assert.Contains(t, out, "--resume-from key-9")
	assert.Contains(t, out, "--table api_keys")
}
