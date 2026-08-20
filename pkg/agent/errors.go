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
