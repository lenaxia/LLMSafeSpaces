// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package chart_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Design 0072 (the workspace event bus rollout): the chart arms the
// controller's --workspace-eventbus-addr / --workspace-eventbus-password-secret
// flags from controller.workspaceEventbusAddr / workspaceEventbusPasswordSecret.
// These tests pin the render contract: both flags when both values are
// set, the bare address alone (authless Redis), NOTHING by default (the
// bus stays dormant), and a hard render failure when a password secret
// is set without an address (the install-time twin of the controller's
// boot validation).

// TestControllerArgs_WorkspaceEventbus_AbsentByDefault: unset values
// render no eventbus flags — every pod keeps the dormant pre-0072
// posture until the operator arms the bus.
func TestControllerArgs_WorkspaceEventbus_AbsentByDefault(t *testing.T) {
	docs := helmTemplate(t, "")
	args := findControllerArgs(t, docs)
	for _, a := range args {
		require.NotContains(t, a, "--workspace-eventbus-",
			"eventbus flags must not render by default, got %q", a)
	}
}

// TestControllerArgs_WorkspaceEventbus_AddrAndSecretRenderBothFlags:
// the production shape — valkey svc address plus the credentials
// secret ref — renders both flags verbatim.
func TestControllerArgs_WorkspaceEventbus_AddrAndSecretRenderBothFlags(t *testing.T) {
	docs := helmTemplate(t, `controller:
  workspaceEventbusAddr: llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379
  workspaceEventbusPasswordSecret: llmsafespaces-credentials:redis-password
`)
	args := findControllerArgs(t, docs)
	join := strings.Join(args, "\n")
	require.Contains(t, join,
		"--workspace-eventbus-addr=llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379",
		"the bus address must render verbatim")
	require.Contains(t, join,
		"--workspace-eventbus-password-secret=llmsafespaces-credentials:redis-password",
		"the NAME:KEY secret ref must render verbatim")
}

// TestControllerArgs_WorkspaceEventbus_AddrOnlyOmitsSecretFlag: an
// authless Redis arms the bus with the address flag alone.
func TestControllerArgs_WorkspaceEventbus_AddrOnlyOmitsSecretFlag(t *testing.T) {
	docs := helmTemplate(t, `controller:
  workspaceEventbusAddr: some-redis:6379
`)
	args := findControllerArgs(t, docs)
	join := strings.Join(args, "\n")
	require.Contains(t, join, "--workspace-eventbus-addr=some-redis:6379")
	require.NotContains(t, join, "--workspace-eventbus-password-secret",
		"no secret value set: the secret flag must not render")
}

// TestControllerArgs_WorkspaceEventbus_SecretWithoutAddrFailsRender:
// the render-fail guard — a password secret with no address refuses
// the install up front (belt and braces with the controller's boot
// validation).
func TestControllerArgs_WorkspaceEventbus_SecretWithoutAddrFailsRender(t *testing.T) {
	err := helmTemplateErr(t, `controller:
  workspaceEventbusPasswordSecret: llmsafespaces-credentials:redis-password
`)
	require.Error(t, err, "a password secret without an address must refuse to render")
	require.Contains(t, err.Error(), "workspaceEventbusPasswordSecret requires controller.workspaceEventbusAddr")
}

// TestControllerArgs_WorkspaceEventbus_CredentialedAddrFailsRender:
// the credentialess invariant at render time — an address carrying
// userinfo ("@" appears in no legitimate Redis coordinate) refuses to
// render, so a password can never ride the addr value into a pod spec
// or a controller log line. The controller's boot guard is the same
// check.
func TestControllerArgs_WorkspaceEventbus_CredentialedAddrFailsRender(t *testing.T) {
	for _, addr := range []string{
		"redis://:sekrit@llmsafespaces-valkey.llmsafespaces.svc.cluster.local:6379",
		"redis://agent:pw@some-redis:6379",
		"agent:pw@some-redis:6379",
	} {
		err := helmTemplateErr(t, "controller:\n  workspaceEventbusAddr: \""+addr+"\"\n")
		require.Error(t, err, "a userinfo-bearing addr (%q) must refuse to render", addr)
		require.Contains(t, err.Error(), "workspaceEventbusAddr must be credentialess")
	}
}
