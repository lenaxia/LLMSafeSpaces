// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import "context"

// stillborn.go — #1832 WS4: stillborn-spawn claim hygiene (the NARROW
// evidence fix; the platform-derived liveness REWORK is #1847's, another
// lane). A spawn that mints a session but produces ZERO turns consumed a
// claim, and a lease-keeper renewed the corpse because heartbeat
// extension trusted the evidence TIMESTAMP alone. The guard: extension
// requires ACTUAL TURN EVIDENCE — when the installed source positively
// reports the assignee session at zero messages/outputs, the heartbeat
// refuses to extend (the wall clock reclaims the stillborn's lease).
// RED-FIRST SKELETON: the seam installs; Heartbeat consults it in the
// implementation commit.

// TurnEvidenceSource reports a session's actual turn evidence (its
// message/output count) — the stillborn guard's source. Installed at
// app wiring over the session index; nil keeps today's semantics.
type TurnEvidenceSource interface {
	SessionTurnEvidence(ctx context.Context, sessionID string) (int, error)
}

// WithTurnEvidenceSource installs the stillborn guard's evidence source.
func WithTurnEvidenceSource(src TurnEvidenceSource) Option {
	return func(s *Service) { s.turnEvidence = src }
}
