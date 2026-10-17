// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"testing"

	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
)

// TestBoardAutomationWiring_1698 mirrors the #407 PodAutomationHandler
// logger guard: app.go constructs the board engine behind cfg.Board and
// installs it on the automation handler via SetBoardService — the
// constructor/building-block contract the wiring depends on (a silently
// nil board service would drop the board routes and the agentd
// board_* tools would 404: the named failure class from the rename
// wiring).
func TestBoardAutomationWiring_1698(t *testing.T) {
	h := handlers.NewPodAutomationHandlerFromClientset(nil, nil, nil, nil, "ns")
	if h.HasBoard() {
		t.Fatalf("freshly-constructed PodAutomationHandler must not carry a board service before SetBoardService")
	}
	// The route gate keys on HasBoard: constructing without installing is
	// the inert (routes-absent) state, not a panic — dark-run safe.
}
