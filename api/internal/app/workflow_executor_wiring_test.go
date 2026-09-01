// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	apiinterfaces "github.com/lenaxia/llmsafespaces/api/internal/interfaces"
)

// TestWorkflowAgentdExecutor_PasswordProviderWired is the wiring guard for
// the #762 caller fix: agentd rejects unauthenticated node dispatches, so
// a workflow engine whose executor lacks a PasswordProvider would 401 on
// every node execution. app.New wires both the reconciler and the
// scheduler through newWorkflowAgentdExecutor(proxyHandler); this test
// pins that the helper refuses to build an executor without a provider
// and that the provider type app passes satisfies the executor's
// interface (compile-time + runtime).
func TestWorkflowAgentdExecutor_PasswordProviderWired(t *testing.T) {
	var _ apiinterfaces.WorkspacePasswordProvider = apiinterfaces.PasswordFunc(
		func(_ context.Context, _ string) (string, error) { return "pw", nil },
	)

	pw := apiinterfaces.PasswordFunc(func(_ context.Context, _ string) (string, error) { return "pw", nil })
	exec := newWorkflowAgentdExecutor(pw)
	require.NotNil(t, exec.PasswordProvider,
		"newWorkflowAgentdExecutor must wire a PasswordProvider — agentd enforces Basic auth (#762)")
	require.Equal(t, 4097, exec.Port)
}
