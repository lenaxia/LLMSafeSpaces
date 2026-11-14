// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_suspend_source.go — the workspace-suspension freeze's production
// adapter (pkg/board/suspend.go's SuspendSource): answers from the
// workspace watcher's in-process phase cache — Suspending/Suspended ⇒
// suspended; everything else (absent, Active, Resuming, Terminating…)
// ⇒ not suspended.
//
// The CRD's phases (controller/internal/workspace/phase_*.go) are the
// single suspension truth: manual suspend, idle auto-suspend, and the
// rate-limit suspension flow all land Spec.Suspend=true → the
// controller walks the workspace to Suspending → Suspended (pod torn
// down, worker sessions gone with it). Resuming → Active leaves the
// set and the board's clocks resume — no rearm, no manual sweep.
//
// Honesty (the liveness seam's discipline): the watcher's cache is
// event-fed and can lag or be empty (watcher not yet started, a watch
// reconnect) — an unseen workspace is NEVER reported suspended (nil
// map = today's lease semantics, the honest lapse path), and a phase
// seen stays until the CRD says otherwise. There is no error shape to
// swallow: GetAllKnownPhases is an in-memory read (nil-safe on the
// proxy handler when the watcher is down), which is why the seam's
// call hygiene (cheap, never a Kubernetes round trip) holds here.

import (
	"context"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// newBoardSuspendSource builds the production SuspendSource over the
// proxy handler's phase cache (the watcher's GetAllKnownPhases —
// nil-safe: nil map before the watcher starts, which decodes as
// nothing-suspended, the honest posture for a seam that must never
// freeze the fleet's leases on its own blind window).
func newBoardSuspendSource(phases func() map[string]string) board.SuspendSource {
	return boardSuspendSource{phases: phases}
}

type boardSuspendSource struct {
	phases func() map[string]string
}

func (s boardSuspendSource) SuspendedWorkspaces(_ context.Context) map[string]bool {
	if s.phases == nil {
		return nil
	}
	all := s.phases()
	if len(all) == 0 {
		return nil
	}
	out := make(map[string]bool, 2)
	for id, phase := range all {
		switch phase {
		case string(v1.WorkspacePhaseSuspending), string(v1.WorkspacePhaseSuspended):
			out[id] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
