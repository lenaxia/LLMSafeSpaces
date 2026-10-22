// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_liveness.go — the #1847 platform-liveness reporter: agentd
// pushes its sessionStatusTracker's per-session observability (busy =
// an open turn, started = the session began, the activity watermark)
// over the pod SA-token automation seam — the resource-pressure
// reporter precedent. The engine's RC1 sweep classifies lapsed claims
// against these rows: an open turn holds the lease across arbitrarily
// long silent tool calls (the case worker heartbeats cannot cover),
// and no worker-DECLARED timestamp exists anywhere on this path.
//
// Failure posture: quiet, next tick retries (the pressure-loop
// pattern). Silence is self-healing on the engine side — a stale row's
// busy latch voids at LivenessStaleBound and its activity ages past
// the lease grace.

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
)

// boardLivenessLoop pushes the pod's per-session liveness snapshot per
// interval until the context ends.
func boardLivenessLoop(ctx context.Context, log *slog.Logger, tracker *sessionStatusTracker, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pushBoardLiveness(ctx, log, tracker)
		}
	}
}

// pushBoardLiveness resolves the automation deps and posts one
// snapshot (only when the pod tracks any session — an idle pod pushes
// nothing; its stale rows age out server-side by design).
func pushBoardLiveness(ctx context.Context, log *slog.Logger, tracker *sessionStatusTracker) {
	saToken, apiURL, workspaceID, err := automationDeps()
	if err != nil {
		return // automation not configured on this pod — nothing to do
	}
	rows := tracker.livenessSnapshot()
	if len(rows) == 0 {
		return
	}
	body, err := json.Marshal(map[string]any{"sessions": rows})
	if err != nil {
		return
	}
	client := opencode.NewLoopbackClient(apiURL, "")
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := client.BoardSessionLiveness(cctx, saToken, workspaceID, body); err != nil && log != nil {
		log.Debug("board: session-liveness push failed (next tick retries)", "error", err.Error())
	}
}
