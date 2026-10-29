// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package board

import "context"

// stillborn.go — the TurnEvidenceSource seam (#1832 WS4's narrow
// evidence fix, later generalized): a spawn that mints a session but
// produces ZERO turns must not be treated as a live worker. Born as a
// heartbeat-extension guard (extension refused on positive zero-turn
// evidence), the seam's surviving consumer after the item-6 amendment
// is the #1959c bounce re-dispatch: a zero-turn original implementer
// falls back to a fresh-worker spawn instead of re-dispatching into a
// corpse (stage.go redispatchBounce). The stillborn CLASS itself is
// owned by the wedge verdict — a corpse cannot call tools, so it can
// never answer a lease-nudge; silence on both channels fails it.
// Installed at app wiring over the session index; nil keeps today's
// semantics.

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
