// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

import (
	"context"
	"testing"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// #887 D5.1: the admin-mux bearer token must be a DISTINCT secret from
// the workspace password. Historically AGENTD_ADMIN_TOKEN == password,
// which opencode inherits and passes verbatim to every tool process
// (extendEnv) — so the admin-mux credential was in every bash tool's
// environment. ensurePasswordSecret converges every workspace onto an
// `admin-token` key that is generated once and never rotated in place
// (rotation would desync running pods' agentd from rebuilt probe specs).

func TestEnsurePasswordSecret_UpsertsDistinctAdminToken(t *testing.T) {
	ws := makeWorkspace("ws-upsert", "default", v1.WorkspacePhasePending)
	r := reconcilerFor(t)
	require.NoError(t, r.ensurePasswordSecret(context.Background(), ws))

	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName("ws-upsert"), Namespace: "default"}, sec))
	pw := string(sec.Data["password"])
	tok := string(sec.Data["admin-token"])
	require.NotEmpty(t, pw)
	require.NotEmpty(t, tok, "admin-token key must be created alongside password")
	assert.NotEqual(t, pw, tok, "admin token must be DISTINCT from the workspace password (design 0051 D5.1)")
	assert.Len(t, tok, 32, "admin token is a 32-char random string")
}

func TestEnsurePasswordSecret_ExistingSecretGainsKeyWithoutRotation(t *testing.T) {
	ws := makeWorkspace("ws-legacy", "default", v1.WorkspacePhasePending)
	existing := makePasswordSecret("ws-legacy", "default")
	existing.Data["admin-token"] = []byte("frozen-token-value")
	r := reconcilerFor(t, existing)
	require.NoError(t, r.ensurePasswordSecret(context.Background(), ws))

	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName("ws-legacy"), Namespace: "default"}, sec))
	assert.Equal(t, "frozen-token-value", string(sec.Data["admin-token"]),
		"an existing admin-token must NEVER be rotated in place — running pods hold it in memory while rebuilt probe specs would diverge")
	assert.Equal(t, "test-password", string(sec.Data["password"]), "password untouched")
}

func TestEnsurePasswordSecret_LegacySecretWithoutKeyGainsOne(t *testing.T) {
	ws := makeWorkspace("ws-old", "default", v1.WorkspacePhasePending)
	r := reconcilerFor(t, makePasswordSecret("ws-old", "default"))
	require.NoError(t, r.ensurePasswordSecret(context.Background(), ws))

	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName("ws-old"), Namespace: "default"}, sec))
	tok := string(sec.Data["admin-token"])
	require.NotEmpty(t, tok, "legacy Secrets must converge onto the admin-token key")
	assert.NotEqual(t, "test-password", tok)
}

// F4 regression: an existing Secret with nil Data (created without keys)
// must not panic the upsert.
func TestEnsurePasswordSecret_NilDataSecretNoPanic(t *testing.T) {
	ws := makeWorkspace("ws-nildata", "default", v1.WorkspacePhasePending)
	nilData := makePasswordSecret("ws-nildata", "default")
	nilData.Data = nil
	r := reconcilerFor(t, nilData)

	var err error
	require.NotPanics(t, func() {
		err = r.ensurePasswordSecret(context.Background(), ws)
	})
	require.NoError(t, err)

	sec := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: passwordSecretName("ws-nildata"), Namespace: "default"}, sec))
	assert.NotEmpty(t, sec.Data["admin-token"], "nil-Data Secret must converge onto the admin-token key")
}
