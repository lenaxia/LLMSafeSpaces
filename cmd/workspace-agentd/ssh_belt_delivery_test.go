// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	secretpkg "github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
)

// #1793 review r1: the corrupt-fixture happy path through the REAL
// staged→delivered wiring (the same fileDelivery the boot matrix
// exercises) — the DELIVERED file must parse with stock semantics.
// This mechanically pins the no-bypass claim the PR audited by hand:
// Materialize (belt) → staging tree → loadStagedFiles → apply → the
// uid-1000-visible ~/.ssh/id_ed25519_* file.
func TestSSHKeyBelt_CorruptKeyDeliveredClean(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "generated@llmsafespaces-test")
	require.NoError(t, err)
	clean := string(pem.EncodeToMemory(block))
	corrupt := func() string {
		lines := strings.Split(clean, "\n")
		for i, l := range lines {
			if l != "" {
				lines[i] = l + " "
			}
		}
		return strings.Join(lines, "\n")
	}()

	paths := matrixPaths(t)
	m := secretpkg.NewMaterializer()
	m.Paths = paths
	m.CrossUID = true
	_, err = m.Materialize([]secretpkg.Secret{{
		Type:      "ssh-key",
		Name:      "deploy",
		Metadata:  map[string]string{"key_type": "ed25519"},
		Plaintext: corrupt,
	}})
	require.NoError(t, err)

	d := fileDelivery{
		roots:      []string{paths.SSHDir, paths.SecretsBaseDir, filepath.Dir(paths.GitCredsPath)},
		ledgerPath: filepath.Join(t.TempDir(), "led.json"),
		sshDir:     paths.SSHDir,
	}
	resp, ok := loadStagedFiles(paths.StagingDir)
	require.True(t, ok)
	_, err = d.apply(resp.Files)
	require.NoError(t, err)

	delivered, err := os.ReadFile(filepath.Join(paths.SSHDir, "id_ed25519_deploy"))
	require.NoError(t, err)
	require.Equal(t, clean, string(delivered), "the DELIVERED key must be the repaired clean PEM")
	_, err = ssh.ParsePrivateKey([]byte(delivered))
	require.NoError(t, err, "the delivered key must load — the live symptom was 'error in libcrypto'")
}
