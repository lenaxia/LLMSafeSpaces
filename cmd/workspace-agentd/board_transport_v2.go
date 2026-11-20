// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_transport_v2.go — #2100's evidence-transport v2 switch (the
// lease-evidence-core design §2, chartered BEHIND ITS FLAG): when
// enabled (the default — the class-kill is the fix; the flag is the
// kill-switch), worker presence and turn facts ride the platform's own
// surfaces —
//
//   - emission is UNGATED: every harness turn-end publishes; the API
//     consumer's assignee-match is the claim filter (the
//     boardWorkerSessions registry consult that the #2100-A incident
//     proved EROFS-dead stops gating anything);
//   - `advanced` derives STORE-side (board_transitions/board_events
//     inside the turn window) — the agentd-side advance tail stops
//     being the answer.
//
// The file artifacts are no longer AUTHORITATIVE when enabled (the
// registry survives solely for the same-half self-certification
// refusal; the advance tail keeps its belt role for a flag flip-back).
// Flip LLMSAFESPACES_BOARD_PRESENCEV2=false to restore the v1
// emission path verbatim (process restart applies it).

import (
	"os"
	"strconv"
	"sync"
)

var (
	boardTransportV2Once sync.Once
	boardTransportV2Val  bool
)

// boardTransportV2Enabled resolves the transport-v2 flag (env, default
// ON; resolved once per process — the emission sites are hot paths and
// the flag is a deploy-shape decision, never a runtime toggle).
func boardTransportV2Enabled() bool {
	boardTransportV2Once.Do(func() {
		boardTransportV2Val = true
		if v, ok := os.LookupEnv("LLMSAFESPACES_BOARD_PRESENCEV2"); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				boardTransportV2Val = b
			}
		}
	})
	return boardTransportV2Val
}
