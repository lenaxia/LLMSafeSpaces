// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestJWTSessionJanitor_RunOnce_PrunesExpiredRows(t *testing.T) {
	store := newMockJWTSessionStore()
	now := time.Now()
	expired := uuid.New()
	active := uuid.New()
	store.seed(&JWTSession{JTI: expired, UserID: "u-1", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: now.Add(-time.Hour)})
	store.seed(&JWTSession{JTI: active, UserID: "u-1", WrappedDEK: []byte("w"), KEKSalt: []byte("s"), ExpiresAt: now.Add(time.Hour)})

	j := NewJWTSessionJanitor(store, 0, nil)
	n := j.runOnce(context.Background())
	if n != 1 {
		t.Errorf("runOnce pruned %d, want 1", n)
	}
	if store.has(expired) {
		t.Errorf("expired row should be gone")
	}
	if !store.has(active) {
		t.Errorf("active row should remain")
	}
}

func TestJWTSessionJanitor_RunOnce_HandlesStoreError(t *testing.T) {
	store := newMockJWTSessionStore()
	store.expireErr = errors.New("transient PG outage")

	j := NewJWTSessionJanitor(store, 0, nil)
	if n := j.runOnce(context.Background()); n != 0 {
		t.Errorf("error path should return 0, got %d", n)
	}
	// No panic, no propagation — failure is acceptable for this best-effort cron.
}

func TestJWTSessionJanitor_RunOnce_EmptyTable_NoLogSpam(t *testing.T) {
	store := newMockJWTSessionStore()
	// Empty table → 0 deletes → don't log INFO (would be noisy on
	// idle clusters). Validated by the absence of any logger calls,
	// which we approximate by passing nil and asserting non-panic.
	j := NewJWTSessionJanitor(store, 0, nil)
	if n := j.runOnce(context.Background()); n != 0 {
		t.Errorf("empty table prune count = %d, want 0", n)
	}
}

func TestJWTSessionJanitor_Run_RespectsContext(t *testing.T) {
	store := newMockJWTSessionStore()
	j := NewJWTSessionJanitor(store, 10*time.Millisecond, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()

	// Let it tick a few times, then cancel.
	time.Sleep(35 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Confirmed shutdown.
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after ctx cancellation")
	}
	if store.ExpireCount < 2 {
		t.Errorf("expected at least 2 ticks in 35ms with 10ms interval, got %d", store.ExpireCount)
	}
}

func TestJWTSessionJanitor_NewWithZeroInterval_UsesDefault(t *testing.T) {
	j := NewJWTSessionJanitor(newMockJWTSessionStore(), 0, nil)
	if j.interval != DefaultJWTSessionJanitorInterval {
		t.Errorf("interval = %v, want %v", j.interval, DefaultJWTSessionJanitorInterval)
	}
}

func TestJWTSessionJanitor_NewWithExplicitInterval_HonorsIt(t *testing.T) {
	j := NewJWTSessionJanitor(newMockJWTSessionStore(), 5*time.Minute, nil)
	if j.interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", j.interval)
	}
}
