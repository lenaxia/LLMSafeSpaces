// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// orphan_reason_pin_test.go — #1342: the projection sweep
// (sessionstate.OrphanSweepReason) and the adapter's transcript repair
// (session.ToolAbortReasonHarnessRestart) are two repair sites for the
// same incident class; the reason users see must never fork by read
// path. The constant is duplicated across the packages (the module seal
// admits only the ABI schema into sessionstate), so the equality is
// pinned HERE — the one package allowed to see both.

import (
	"os"
	"testing"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOrphanSweepReasonMatchesContractConstant(t *testing.T) {
	assert.Equal(t, session.ToolAbortReasonHarnessRestart, sessionstate.OrphanSweepReason,
		"the projection sweep and the adapter transcript repair must close orphaned parts with the SAME reason")
}

// TestGenerationChangeReseedWiredToRetryingDriver pins the supervisor
// wiring (#1342): the child-started hook must fire the RETRYING driver
// (startStateAuthorityReseed), not a one-shot Reseed — a harness still
// booting after a restart would otherwise leave the S12 orphan-sweep
// backstop unfired until the NEXT generation. Source pin in the
// TestOpencodeBootLayersWiring discipline (a one-line regression to the
// one-shot form would pass every other test).
func TestGenerationChangeReseedWiredToRetryingDriver(t *testing.T) {
	body, err := os.ReadFile("main.go")
	require.NoError(t, err)
	src := string(body)
	require.Contains(t, src, "go startStateAuthorityReseed(bgCtx, a, sessionstate.ReseedReasonGenerationChange)",
		"the child-started hook must ride the retrying reseed driver — a one-shot Reseed re-opens the unfired-backstop window")
	assert.NotContains(t, src, "a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange)",
		"the pre-#1342 one-shot generation reseed must not return")
}
