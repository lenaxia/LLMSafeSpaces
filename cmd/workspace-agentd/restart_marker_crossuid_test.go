// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.uber.org/zap/zapcore"
)

// TestWriteRestartReasonMarker_ForeignOwnedPrimaryFallsBack pins the
// 2026-10-01 cross-uid rotation hole: the shared marker sits in the
// STICKY /sandbox-runtime dir, and 0640 gives the other container's uid
// group-READ only — so once it owns the primary, this uid's write fails
// EACCES. The write must fall back to the per-uid sibling rather than
// losing attribution. (Simulated with a 0444 primary: write permission
// bits bind the owner too, no chroot tricks needed.)
func TestWriteRestartReasonMarker_ForeignOwnedPrimaryFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-restart-reason.json")

	// The "foreign-owned" primary: readable but not writable.
	require.NoError(t, os.WriteFile(path, []byte(`{"reason":"stale"}`), 0o444))

	err := writeRestartReasonMarker(path, "credential_reload", nil)
	require.NoError(t, err, "the per-uid fallback must rescue the write")

	fallback := path + ".uid" + strconv.Itoa(os.Getuid())
	data, err := os.ReadFile(fallback)
	require.NoError(t, err, "fallback marker must exist")
	var m restartReason
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "credential_reload", m.Reason)
}

// TestReadRestartReasonMarker_NewestWinsAcrossWriters pins the reader
// side of the cross-uid fix: when a per-uid fallback (written by the
// uid that could not rotate the primary) is NEWER than the primary, the
// boot reader attributes the fallback's reason — and when the primary
// is newer it wins as before.
func TestReadRestartReasonMarker_NewestWinsAcrossWriters(t *testing.T) {
	older, newer := time.Now().Add(-time.Hour), time.Now()

	t.Run("fallback newer than primary", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "last-restart-reason.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"reason":"sidecar_reason"}`), 0o640))
		fallback := path + ".uid1000"
		require.NoError(t, os.WriteFile(fallback, []byte(`{"reason":"supervisor_reason"}`), 0o640))
		require.NoError(t, os.Chtimes(path, older, older))
		require.NoError(t, os.Chtimes(fallback, newer, newer))

		r, ok := readRestartReasonMarker(path, zapcore.NewNopCore())
		require.True(t, ok)
		require.Equal(t, "supervisor_reason", r.Reason, "the newer fallback is the truthful attribution")
	})

	t.Run("primary newer than fallback", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "last-restart-reason.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"reason":"sidecar_reason"}`), 0o640))
		fallback := path + ".uid1000"
		require.NoError(t, os.WriteFile(fallback, []byte(`{"reason":"supervisor_reason"}`), 0o640))
		require.NoError(t, os.Chtimes(path, newer, newer))
		require.NoError(t, os.Chtimes(fallback, older, older))

		r, ok := readRestartReasonMarker(path, zapcore.NewNopCore())
		require.True(t, ok)
		require.Equal(t, "sidecar_reason", r.Reason)
	})
}

// TestLogRestartReason_ConsumesNewestAndSweepsSiblings pins the one-shot
// contract across BOTH names: the boot read consumes the newest marker
// and removes stale siblings so they cannot re-surface as stale
// attributions on later boots.
func TestLogRestartReason_ConsumesNewestAndSweepsSiblings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-restart-reason.json")
	stale := path + ".uid2000"
	require.NoError(t, os.WriteFile(stale, []byte(`{"reason":"old_sidecar"}`), 0o640))
	fresh := path + ".uid1000"
	require.NoError(t, os.WriteFile(fresh,
		[]byte(`{"reason":"credential_reload","timestamp":"`+time.Now().UTC().Format(time.RFC3339)+`"}`), 0o640))
	require.NoError(t, os.Chtimes(stale, older(time.Now()), older(time.Now())))

	logs, core := testObserver(t)
	logRestartReason(path, core)

	require.NotEmpty(t, logs.All(), "the newest marker must be consumed and logged")
	require.NoFileExists(t, fresh, "the consumed marker must be removed")
	require.NoFileExists(t, stale, "stale siblings from the other uid must be swept")
	require.NoFileExists(t, path)
}

func older(t time.Time) time.Time { return t.Add(-time.Hour) }
