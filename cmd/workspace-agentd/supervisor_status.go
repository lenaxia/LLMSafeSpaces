// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// supervisor_status.go — US-70.1 (design 0057 I10/I13): the sidecar's
// periodic mirror of the supervisor's terminal-verified spawn-env state
// into healthz. The supervisor owns the fact (it measured the env the
// child actually spawned with); the sidecar owns the scrape surface
// (kubelet + controller hit its admin mux). Pull-only and cached — the
// healthz handler never touches the socket, preserving the US-22.1
// process-only liveness contract.

import (
	"context"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// supervisorStatusPollInterval matches the controller's healthz poll
// cadence: a degrade observed at spawn surfaces in healthz within one
// controller cycle.
const supervisorStatusPollInterval = 15 * time.Second

// supervisorStatusStore caches the last successfully-read supervisor
// status. A failed poll keeps the previous snapshot — the supervisor
// restarting (socket briefly down) must not flap healthz; the NEXT
// successful poll restores freshness. Nil until the first successful
// poll: healthz then omits the spawnEnv field entirely (no evidence is
// not a degrade).
type supervisorStatusStore struct {
	mu   sync.Mutex
	last *controlStatus
}

func (s *supervisorStatusStore) set(st *controlStatus) {
	s.mu.Lock()
	s.last = st
	s.mu.Unlock()
}

func (s *supervisorStatusStore) snapshot() *controlStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// spawnEnvHealth projects the cached supervisor status into the healthz
// spawn-env field.
func (s *supervisorStatusStore) spawnEnvHealth() *agentd.SpawnEnvHealth {
	st := s.snapshot()
	if st == nil {
		return nil
	}
	return &agentd.SpawnEnvHealth{
		SpawnedRev: st.SpawnedRev,
		Degraded:   st.SpawnEnvDegraded,
		Reason:     st.SpawnEnvReason,
	}
}

// startSupervisorStatusPoller polls the supervisor's control-socket
// status until ctx is done. First poll fires immediately so a boot-time
// degrade (e.g. first spawn with a dead sidecar) surfaces without
// waiting a full interval.
func startSupervisorStatusPoller(ctx context.Context, wg *sync.WaitGroup, cc *controlClient, store *supervisorStatusStore) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(supervisorStatusPollInterval)
		defer ticker.Stop()
		poll := func() {
			st, err := cc.Status(ctx)
			if err != nil {
				// Keep the last snapshot — see supervisorStatusStore.
				return
			}
			store.set(st)
		}
		poll()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				poll()
			}
		}
	}()
}

// spawnEnvWarning renders the machine-readable degrade line for the
// healthz warnings array (relayed by the controller into the
// AgentHealthy condition message; the copy must not contain semicolons).
// Empty string when there is no degrade to report.
func spawnEnvWarning(h *agentd.SpawnEnvHealth) string {
	if h == nil || !h.Degraded || h.Reason == "" {
		return ""
	}
	return "degraded:" + h.Reason
}
