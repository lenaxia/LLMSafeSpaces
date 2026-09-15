// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// pending_apply.go — #1342 item 4: credential-apply restarts ride a
// maintenance window. When a restart-worthy credential change lands
// while sessions are busy, the applying restart defers (unbounded while
// sessions progress) and THIS tracker holds the deferral for healthz →
// the Workspace CRD's CredentialsApplyPending condition, so the
// operator sees why the credential has not applied yet. Cleared when
// the restart fires or the defer is canceled.

import (
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// pendingApplyReasonCredentialChange is the closed reason set's first
// value: a credential change whose restart is deferred.
const pendingApplyReasonCredentialChange = "credential_change"

// pendingApplyTracker is the healthz-side state of one deferred apply.
// The restart goroutine is the sole writer (begin/refreshBusy/clear);
// the healthz handler reads snapshot(). All methods are nil-safe — the
// relay path wires no tracker.
type pendingApplyTracker struct {
	mu    sync.Mutex
	since time.Time
	busy  int
}

func newPendingApplyTracker() *pendingApplyTracker {
	return &pendingApplyTracker{}
}

// begin marks a deferred apply as pending (busy = sessions blocking the
// restart at defer time).
func (p *pendingApplyTracker) begin(busy int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.since = time.Now()
	p.busy = busy
}

// refreshBusy keeps the surfaced busy count honest across defer ticks.
func (p *pendingApplyTracker) refreshBusy(busy int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.since.IsZero() {
		return
	}
	p.busy = busy
}

// clear drops the pending state (the restart fired, or the defer was
// canceled at shutdown).
func (p *pendingApplyTracker) clear() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.since = time.Time{}
	p.busy = 0
}

// snapshot renders the healthz payload; nil when nothing is pending.
func (p *pendingApplyTracker) snapshot() *agentd.PendingApplyHealth {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.since.IsZero() {
		return nil
	}
	return &agentd.PendingApplyHealth{
		Reason:         pendingApplyReasonCredentialChange,
		WaitingSeconds: int(time.Since(p.since).Seconds()),
		BusySessions:   p.busy,
	}
}
