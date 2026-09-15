// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import "errors"

// ErrHTTPStatus marks adapter errors where the agent PROCESSED the
// request and answered with a status >= 400: the outcome is definitive
// (rejected), not ambiguous. Callers driving at-least-once delivery
// (the D3 outbox) use this to distinguish "safe to retry" (rejection)
// from "outcome unknown" (transport cut / timeout mid-request — those
// need delivery verification, #987). Transport-level classification,
// not agent-specific: any HTTP-speaking adapter wraps its status
// errors with this sentinel.
var ErrHTTPStatus = errors.New("agent http status")

// ErrImageInTextOnlyHistory marks the #1307 wedge class: the session's
// replayed history carries an image part and the active model is
// text-only, so the provider rejects EVERY turn until the user switches
// to a vision-capable model or the image is removed from the history.
// Retrying the same payload can never succeed. Adapter-level
// classification of a provider failure body; wraps ErrHTTPStatus so the
// at-least-once semantics (definitive rejection) are preserved.
var ErrImageInTextOnlyHistory = errors.New("session history contains an image the active text-only model cannot process")
