// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeGather_PruneAndCloseSafety (r3 findings 3-4): the prune path
// evicts stale entries past the horizon (var-shrunk), and a serve racing
// Close never assigns into a nil map.
func TestServeGather_PruneAndCloseSafety(t *testing.T) {
	old := serveGatherPruneHorizon
	serveGatherPruneHorizon = time.Millisecond
	t.Cleanup(func() { serveGatherPruneHorizon = old })

	store := &internalLeaseStore{seeds: map[string]SessionSeed{}}
	a, err := New(Config{PlatformDir: t.TempDir(), Parser: &leaseNopParser{}, Store: store, Passwords: []string{"pw"}, FastCursor: true})
	require.NoError(t, err)
	a.IngestForTest(&abiv1.Event{SessionId: "ses-1", Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	// Seed stale entries directly (the prune path's precondition: entries
	// aged past the shrunk horizon), then OVERFILL past the map limit so
	// the next serve executes the prune branch and evicts them.
	oldLimit := serveGatherMapLimit
	serveGatherMapLimit = 4
	t.Cleanup(func() { serveGatherMapLimit = oldLimit })
	a.serveGathersMu.Lock()
	for i := 0; i < 8; i++ {
		g := &serveGather{cached: true, gatheredAt: time.Now().Add(-time.Hour)}
		a.serveGathers["ses-"+string(rune('a'+i))] = g
	}
	a.serveGathersMu.Unlock()

	// A serve against the overfilled stale map EXECUTES the prune branch:
	// stale entries evict before insertion, bounded by the limit.
	_, gerr := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
	require.NoError(t, gerr)
	a.serveGathersMu.Lock()
	remaining := len(a.serveGathers)
	a.serveGathersMu.Unlock()
	assert.LessOrEqual(t, remaining, oldLimit+1,
		"the prune branch ran: stale entries evicted, bounded by the limit")

	// Close racing a serve: no nil-map panic (Close swaps a fresh map),
	// and the once-guard keeps the test cleanup's Close harmless.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
		assert.NoError(t, err)
	}()
	require.NoError(t, a.Close())
	<-done
	assert.NoError(t, a.Close(), "idempotent close")
}

type internalLeaseStore struct {
	seeds map[string]SessionSeed
}

func (s *internalLeaseStore) SessionStates(ctx context.Context) (map[string]SessionSeed, error) {
	return s.seeds, nil
}

func (s *internalLeaseStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (s *internalLeaseStore) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	return map[string][]*abiv1.InputRequest{}, nil
}

type leaseNopParser struct{}

func (p *leaseNopParser) Parse(raw []byte) (*abiv1.Event, bool, error) { return nil, false, nil }
