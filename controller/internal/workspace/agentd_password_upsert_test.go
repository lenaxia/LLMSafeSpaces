// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// agentd_password_upsert_test.go — design 0051 US-3 / Q3: the NEW
// `agentdPassword` Secret key becomes the control-plane Basic secret
// (§D1 per-endpoint table), delivered env-only to the sidecar. The
// upsert follows the #933 admin-token convergence pattern EXACTLY:
// generated once when absent, NEVER rotated in place (running sidecar
// pods hold the accepted value in memory; in-place rotation desyncs
// them from the API server's credential reads), and it must land BEFORE
// any sidecar-enabled pod build — ensurePasswordSecret runs in
// handlePending, ahead of buildPod.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func TestEnsurePasswordSecret_UpsertsDistinctAgentdPassword(t *testing.T) {
	ws := makeWorkspace("ws-us3-upsert", "default", v1.WorkspacePhasePending)
	r := reconcilerFor(t)
	require.NoError(t, r.ensurePasswordSecret(context.Background(), ws))

	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName("ws-us3-upsert"), Namespace: "default"}, sec))
	pw := string(sec.Data["password"])
	tok := string(sec.Data["admin-token"])
	apw := string(sec.Data["agentdPassword"])
	require.NotEmpty(t, apw, "agentdPassword key must be created (US-3)")
	assert.NotEqual(t, pw, apw, "agentdPassword must be DISTINCT from the workspace password (design 0051 §D1 — it must never exist in uid-1000 space)")
	assert.NotEqual(t, tok, apw, "agentdPassword must be distinct from the admin token")
	assert.Len(t, apw, 32, "32-char random string, same generator class as the other keys")
}

func TestEnsurePasswordSecret_ExistingSecretGainsAgentdPasswordWithoutRotation(t *testing.T) {
	ws := makeWorkspace("ws-us3-legacy", "default", v1.WorkspacePhasePending)
	existing := makePasswordSecret("ws-us3-legacy", "default")
	existing.Data["admin-token"] = []byte("frozen-token")
	existing.Data["agentdPassword"] = []byte("frozen-agentd-pw")
	r := reconcilerFor(t, existing)
	require.NoError(t, r.ensurePasswordSecret(context.Background(), ws))

	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName("ws-us3-legacy"), Namespace: "default"}, sec))
	assert.Equal(t, "frozen-agentd-pw", string(sec.Data["agentdPassword"]),
		"an existing agentdPassword must NEVER be rotated in place (Q3: same convergence rule as the #933 admin token)")
	assert.Equal(t, "test-password", string(sec.Data["password"]), "password untouched")
}

func TestEnsurePasswordSecret_LegacySecretWithoutAgentdPasswordGainsOne(t *testing.T) {
	// The mixed-fleet convergence leg: Secrets created before US-3 (with
	// admin-token already converged) gain the key on the next reconcile.
	ws := makeWorkspace("ws-us3-old", "default", v1.WorkspacePhasePending)
	existing := makePasswordSecret("ws-us3-old", "default")
	existing.Data["admin-token"] = []byte("already-there")
	r := reconcilerFor(t, existing)
	require.NoError(t, r.ensurePasswordSecret(context.Background(), ws))

	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName("ws-us3-old"), Namespace: "default"}, sec))
	require.NotEmpty(t, sec.Data["agentdPassword"], "legacy Secret must converge onto the new key")
	assert.Equal(t, "already-there", string(sec.Data["admin-token"]), "convergence does not disturb the existing keys")
}
