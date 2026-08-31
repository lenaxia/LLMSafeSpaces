// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/agentd/secrets"
	pkgsecrets "github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// W8 spawn-workflow rows: the complete bind -> stage -> pull -> deliver
// path under the size ceilings — a near-cap value delivers whole, and a
// refused over-ceiling batch never disturbs the delivered last-good set.

func nearCapValue() string {
	return strings.Repeat("C", pkgsecrets.MaxSecretValueBytes)
}

// TestSupervisorSubprocess_SpawnFiles_NearCapValueDeliversWhole: a
// file-class secret exactly at the API-side per-secret cap must survive
// the full workflow — materialize, staged, pulled at spawn, delivered
// byte-complete with the mode contract.
func TestSupervisorSubprocess_SpawnFiles_NearCapValueDeliversWhole(t *testing.T) {
	rtDir := t.TempDir()
	value := nearCapValue()
	staging := materializeStaged(t, rtDir, []secrets.Secret{
		{Type: "secret-file", Name: "big", Plaintext: value,
			Metadata: map[string]string{"mount_path": "big.bin"}},
	})
	secretsEnv := writeSecretsEnv(t, t.TempDir(), "export PULL_PROBE='x'\n")
	m := startFilesMux(t, secretsEnv, &staging)

	sp := startSupervisorSubprocessEnv(t, filesEnvFor(m.addr, rtDir, filepath.Join(t.TempDir(), "led.json"))...)
	cc := newControlClient(sp.addr)

	require.Eventually(t, func() bool { return sp.childPIDOf(t, cc) > 0 },
		15*time.Second, 100*time.Millisecond, "spawn must happen (delivery never blocks it)")

	big := filepath.Join(rtDir, "secrets", "big.bin")
	require.Eventually(t, func() bool {
		info, err := os.Stat(big)
		return err == nil && info.Size() == int64(len(value))
	}, 15*time.Second, 200*time.Millisecond,
		"the near-cap file must deliver byte-complete at spawn")

	data, err := os.ReadFile(big)
	require.NoError(t, err)
	require.Len(t, data, len(value))
	require.Equal(t, value[:1024], string(data[:1024]), "content spot-check")
	info, err := os.Stat(big)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "secret-file mode contract at delivery")
}

// TestSupervisorSubprocess_SpawnFiles_RefusedBatch_KeepsDeliveredSet: a
// whole-batch over-ceiling re-stage refuses loudly and the spawn seam
// keeps serving the last-good generation — the delivered set on a live
// pod is never retracted by an anomalous batch.
func TestSupervisorSubprocess_SpawnFiles_RefusedBatch_KeepsDeliveredSet(t *testing.T) {
	rtDir := t.TempDir()
	staging := materializeStaged(t, rtDir, []secrets.Secret{
		{Type: "secret-file", Name: "good", Plaintext: "GOOD_BYTES",
			Metadata: map[string]string{"mount_path": "good.bin"}},
	})
	secretsEnv := writeSecretsEnv(t, t.TempDir(), "export PULL_PROBE='x'\n")
	stagingPtr := staging
	m := startFilesMux(t, secretsEnv, &stagingPtr)

	sp := startSupervisorSubprocessEnv(t, filesEnvFor(m.addr, rtDir, filepath.Join(t.TempDir(), "led.json"))...)
	cc := newControlClient(sp.addr)
	cc.timeout = 30 * time.Second

	require.Eventually(t, func() bool { return sp.childPIDOf(t, cc) > 0 },
		15*time.Second, 100*time.Millisecond, "baseline spawn must happen")
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(rtDir, "secrets", "good.bin"))
		return err == nil && string(data) == "GOOD_BYTES"
	}, 15*time.Second, 200*time.Millisecond, "baseline delivery")

	var before []byte
	var err error
	before, err = os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)

	overBudget := make([]secrets.Secret, 0, 5)
	for i := 0; i < 5; i++ {
		overBudget = append(overBudget, secrets.Secret{
			Type:      "secret-file",
			Name:      fmt.Sprintf("part-%d", i),
			Plaintext: strings.Repeat("P", agentd.StagedFilesMaxBytes/4),
			Metadata:  map[string]string{"mount_path": fmt.Sprintf("part-%d.bin", i)},
		})
	}
	paths := secrets.Paths{
		Home:            t.TempDir(),
		SecretsBaseDir:  filepath.Join(rtDir, "secrets"),
		SSHDir:          filepath.Join(rtDir, "ssh"),
		AgentConfigPath: filepath.Join(t.TempDir(), "agent-config.json"),
		SecretsEnvPath:  filepath.Join(t.TempDir(), "secrets-env"),
		GitCredsPath:    filepath.Join(rtDir, "git-credentials"),
		StagingDir:      staging,
	}
	_, mErr := (&secrets.Materializer{FS: secrets.RealFS(), Paths: paths}).Materialize(overBudget)
	require.Error(t, mErr)
	require.Contains(t, mErr.Error(), "size_exceeded")

	after, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	require.NoError(t, err)
	require.Equal(t, string(before), string(after),
		"the refused batch may not retract the published last-good generation")

	_, rErr := cc.Restart(context.Background(), "credential_reload", 5)
	require.NoError(t, rErr)
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(rtDir, "secrets", "good.bin"))
		return err == nil && string(data) == "GOOD_BYTES"
	}, 20*time.Second, 200*time.Millisecond,
		"the next spawn still delivers the last-good set (never-block-spawn holds)")
}
