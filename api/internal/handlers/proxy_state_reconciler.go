// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"fmt"
	"time"
)

// stateReconciler is US-69.11's replacement for sseWatchReconciler: with
// the SSE tracker retired, the surviving periodic duties are —
//
//   - escalateHungs (D6/#998): the statusz-based unattended-escalation
//     sweep (data source unchanged; only the scheduler moved);
//   - usage-gate arming: every Active workspace gets one busy-gated ABI
//     subscription attempt (the gate itself drops the connection after
//     the idle settle window — arming is cheap and idempotent);
//   - gate teardown for workspaces that left Active.
//
// It deliberately does NOT re-derive session state: display state is the
// pod's projection (contract streams), billing/state bridges hang off the
// usage gates.
func (h *ProxyHandler) stateReconciler(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			// Shutdown race (#903 review): the select above may pick
			// ticker.C even when stopCh is already closed.
			select {
			case <-h.stopCh:
				return
			default:
			}
			if h.phaseSource == nil {
				continue
			}
			phases := h.phaseSource.GetAllKnownPhases()
			watched := make([]string, 0, len(phases))
			for id, phase := range phases {
				if phase == string(phaseActive) {
					h.UsageStream().Open(id)
					watched = append(watched, id)
				} else {
					h.UsageStream().Close(id)
				}
			}
			// D6 (#998): unattended-escalation sweep on the same tick —
			// notify, never execute. Failure-isolated.
			func() {
				defer func() {
					if r := recover(); r != nil {
						h.logger.Error("D6 escalation sweep panicked (isolated; reconciler continues)",
							fmt.Errorf("%v", r), "component", "d6")
					}
				}()
				h.escalateHungs(watched)
			}()
		}
	}
}
