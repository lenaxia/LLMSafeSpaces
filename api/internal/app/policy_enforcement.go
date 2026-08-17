// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"github.com/lenaxia/llmsafespaces/api/internal/handlers"
	"github.com/lenaxia/llmsafespaces/api/internal/services/policy"
)

// wirePolicyEnforcement connects the org-policy service to every handler
// that enforces allowed-models/allowed-providers: ListModels (catalog
// filtering, US-43.8), SetModel (selection gate, PR #912), and the proxy's
// per-prompt override gate on /prompt and /message (PR #912).
//
// Extracted from New so the wiring is unit-testable: a nil policySvc (org
// policies disabled) wires nothing, and a non-nil one reaches BOTH
// handlers — the #912 round-2 review found the proxy gate shipped unwired,
// making its enforcement test-only dead code in production.
func wirePolicyEnforcement(
	policySvc *policy.Service,
	modelsHandler *handlers.ModelsHandler,
	proxyHandler *handlers.ProxyHandler,
) {
	if policySvc == nil {
		return
	}
	if modelsHandler != nil {
		modelsHandler.SetPolicyChecker(policySvc)
	}
	if proxyHandler != nil {
		proxyHandler.SetModelPolicyChecker(policySvc)
	}
}
