// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Design 0070 §2.4a: the exchange/ subPath handling of `init-fs` — the
// board payload exchange dir, the ONE PVC subtree both uid spaces
// write (runtime uid 1000 creates payload files; the uid-2000 sidecar
// unlinks consumed ones — deletion rides the PARENT dir's group-write
// bit, hence 0770 and NOT sticky: a sticky bit would block exactly the
// cross-uid unlink the dir exists for).
//
//	create — full run creates exchange/ alongside the other roots
//	         (single-container: the running uid 1000 is both writer and
//	         deleter, but the mode is the same dual-rw contract).
//	skip   — sidecar mode: platform-init (uid 1000) must NOT create it
//	         (the uid-2000 platform-dirs init owns it — same discipline
//	         as platform/, ownership follows the writer).
//	only   — the sidecar-mode platform-dirs init creates it alongside
//	         platform/.
//
// Cross-uid behavior itself (gid 1000 group-write across uid 1000/2000)
// is validated at the kind layer; these tests pin the script contract,
// the mode, and the cross-uid TOLERANCE (no chmod when the group bits
// are already right — the other mode's init owns the dir on a flipped
// PVC and a chmod would EPERM).

func TestInitFS_ExchangeSubPath_Create(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")

	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	info, err := os.Stat(filepath.Join(tr.pvc, "exchange"))
	require.NoError(t, err, "default (create) must make exchange/")
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o770), info.Mode().Perm(),
		"exchange/ mode 0770 — group rwx under the pod's shared gid 1000 is the dual-uid contract (sticky would block the cross-uid unlink)")
}

func TestInitFS_ExchangeSubPath_Skip(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")

	exit, stderr := runInitFSSubcommand(t, bin, append(tr.args(), "--exchange-subpath=skip")...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	_, err := os.Stat(filepath.Join(tr.pvc, "exchange"))
	assert.True(t, os.IsNotExist(err),
		"sidecar-mode platform-init must NOT create exchange/ — the uid-2000 platform-dirs init owns it")
}

func TestInitFS_ExchangeSubPath_Only(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)

	exit, stderr := runInitFSSubcommand(t, bin, "--pvc-root="+tr.pvc, "--platform-subpath=only", "--exchange-subpath=only")
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	info, err := os.Stat(filepath.Join(tr.pvc, "exchange"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
	assert.Equal(t, os.FileMode(0o770), info.Mode().Perm())

	for _, d := range []string{"workspace", "home", "tmp"} {
		_, err := os.Stat(filepath.Join(tr.pvc, d))
		assert.True(t, os.IsNotExist(err), "only-mode must not create %s (platform-init's job)", d)
	}
}

func TestInitFS_ExchangeSubPath_ExistingGroupRW_NoChmod(t *testing.T) {
	// The cross-uid tolerance pin: a dir whose group bits are already
	// right must NOT be chmod'd — on a mode-flipped PVC the OTHER uid's
	// init owns it and chmod would EPERM (init crash-loop). Proven via
	// ctime: a chmod(2) bumps ctime even when the mode is unchanged,
	// while the tolerant no-op path never issues the syscall. (A setgid
	// marker would be the cheaper probe, but an unprivileged chmod
	// cannot SET dir setgid on this kernel — ctime it is.)
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")
	exch := filepath.Join(tr.pvc, "exchange")
	require.NoError(t, os.Mkdir(exch, 0o770))
	require.NoError(t, os.Chmod(exch, 0o770))
	before := dirCtime(t, exch)

	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	info, err := os.Stat(exch)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o770), info.Mode().Perm(),
		"the group bits stay the dual-uid contract mode")
	assert.Equal(t, before, dirCtime(t, exch),
		"ctime UNCHANGED — proof no Chmod ran on the group-rwx-present dir (the tolerant cross-uid path; any chmod would bump it)")
}

// dirCtime returns one dir's ctime (the chmod-observable timestamp).
func dirCtime(t *testing.T, dir string) syscall.Timespec {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(dir, &st))
	return st.Ctim
}

func TestInitFS_ExchangeSubPath_RepairsWrongMode(t *testing.T) {
	// A pre-existing exchange/ missing the group-write bit (e.g. the
	// kubelet subPath fallback created it root:root 0755 on a
	// version-skewed pod) is repaired when the running uid owns it.
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")
	exch := filepath.Join(tr.pvc, "exchange")
	require.NoError(t, os.Mkdir(exch, 0o750))

	exit, stderr := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 0, exit, "stderr=%q", stderr)

	info, err := os.Stat(exch)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o770), info.Mode().Perm(), "wrong-mode dir is chmod'd back to the dual-uid contract")
}

func TestInitFS_ExchangeSubPath_RefusesPlantedFile(t *testing.T) {
	bin := buildAgentdBinary(t)
	tr := newInitFSTree(t)
	tr.writePasswordSource(t, "s3cret-pw\n", "")
	require.NoError(t, os.WriteFile(filepath.Join(tr.pvc, "exchange"), []byte("planted"), 0o644))

	exit, _ := runInitFSSubcommand(t, bin, tr.args()...)
	require.Equal(t, 1, exit,
		"a planted NON-DIRECTORY at exchange/ fails loudly into Init:Error (the payload contract must not boot on a broken tree)")
}
