// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- #1310 slice B (epic-71 / 2a): pending inputs are leases ---------------
//
// Projected asks are claims requiring re-verification against the harness
// live registry — never facts. The lease pass diffs projection against
// store truth: dropped asks resolve (browsers clear), unknown live asks
// appear, BUSY re-derives from harness status past the convergence bound.
// A gather failure never mutates the projection (never an authoritative
// empty).

// leaseStore is a store-truth fake with per-session pending inputs,
// statuses, an injectable failure, and a gather counter (leg 9's
// cheapness bound).
type leaseStore struct {
	mu      sync.Mutex
	seeds   map[string]sessionstate.SessionSeed
	err     error
	gathers int
}

func newLeaseStore() *leaseStore {
	return &leaseStore{seeds: map[string]sessionstate.SessionSeed{}}
}

func (s *leaseStore) seed(sid string, status abiv1.SessionStatus, inputs ...*abiv1.InputRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sd := s.seeds[sid]
	sd.Status = status
	// Always REPLACE the pending set: a re-seed with no inputs models the
	// harness having dropped its asks.
	sd.PendingInputs = append([]*abiv1.InputRequest(nil), inputs...)
	s.seeds[sid] = sd
}

func (s *leaseStore) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *leaseStore) gatherCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gathers
}

func (s *leaseStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gathers++
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]sessionstate.SessionSeed, len(s.seeds))
	for k, v := range s.seeds {
		out[k] = v
	}
	return out, nil
}

func (s *leaseStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func leaseAuthority(t *testing.T, store *leaseStore) *sessionstate.Authority {
	t.Helper()
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &fixtureParser{},
		Store:       store,
		Passwords:   []string{"pw"},
		FastCursor:  true,
		Capabilities: &abiv1.CapabilityReport{
			SupportedActions: []abiv1.ActionType{abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func input(id string) *abiv1.InputRequest {
	return &abiv1.InputRequest{Id: id, Kind: abiv1.InputKind_INPUT_KIND_PERMISSION}
}

// TestLeasePass_DiffMatrix: projected {A,B} × live {B,C} → A resolves
// (event emitted, browsers clear), C appears, B untouched. Empty live set
// empties the projection.
func TestLeasePass_DiffMatrix(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE, input("B"), input("C"))
	a := leaseAuthority(t, store)
	stream, cancel, err := a.Stream(context.Background())
	require.NoError(t, err)
	defer cancel()

	seedPendingInput(t, a, "ses-1", "A")
	seedPendingInput(t, a, "ses-1", "B")
	require.Equal(t, 2, pendingCount(a, "ses-1"))

	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.LeaseResolved, "A resolved by absence")
	assert.Equal(t, 1, stats.LeaseAppeared, "C appeared from live truth")

	v := a.State().Sessions["ses-1"]
	ids := map[string]bool{}
	for _, in := range v.PendingInputs {
		ids[in.GetId()] = true
	}
	assert.True(t, ids["B"], "B untouched")
	assert.True(t, ids["C"], "C present")
	assert.False(t, ids["A"], "A dropped")

	frames := collectFrames(stream, 4)
	got := map[abiv1.EventType][]string{}
	for _, f := range frames {
		evt := f.GetEvent().GetEvent()
		got[evt.GetType()] = append(got[evt.GetType()], evt.GetInput().GetId())
	}
	assert.Contains(t, got[abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED], "A", "browsers clear on the resolved event")
	assert.Contains(t, got[abiv1.EventType_EVENT_TYPE_INPUT_REQUEST], "C", "browsers learn of the late ask")
}

// TestLeasePass_EmptyLiveEmpties: the harness holds nothing → the whole
// projected set resolves (the 2026-09-10 incident's cure, on cadence).
func TestLeasePass_EmptyLiveEmpties(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")
	seedPendingInput(t, a, "ses-1", "que_1")

	a.Reconcile(context.Background())
	assert.Equal(t, 0, pendingCount(a, "ses-1"))
}

// TestLeasePass_GatherFailureKeepsProjection: a store failure never
// mutates the projection — never an authoritative empty.
func TestLeasePass_GatherFailureKeepsProjection(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")

	store.fail(errStringOf("store unreachable"))
	a.Reconcile(context.Background())
	assert.Equal(t, 1, pendingCount(a, "ses-1"), "gather failure leaves the projection untouched")
}

// TestLeasePass_StatusReDerivation: BUSY + harness idle past the
// convergence bound → converges to idle; a busy-mark inside the bound
// holds (the lease window); harness busy + projection idle → busy.
func TestLeasePass_StatusReDerivation(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, store)

	// Project busy via the event path (status + the busy-mark ride the seq).
	a.IngestForTest(&abiv1.Event{
		SessionId: "ses-1",
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		Status:    abiv1.SessionStatus_SESSION_STATUS_BUSY,
	})
	v := a.State().Sessions["ses-1"]
	require.NotNil(t, v)
	require.Equal(t, abiv1.SessionStatus_SESSION_STATUS_BUSY, v.Status)

	// Inside the lease window: the busy-mark holds even though the
	// harness already went idle (events may just be late).
	a.Reconcile(context.Background())
	require.Equal(t, abiv1.SessionStatus_SESSION_STATUS_BUSY, a.State().Sessions["ses-1"].Status,
		"the lease window protects fresh busy-marks from racing status reads")

	// Past the bound: re-derivation converges to harness truth.
	a.SetLeaseBoundForTest(-time.Second) // force expiry for the test clock
	a.Reconcile(context.Background())
	assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, a.State().Sessions["ses-1"].Status)

	// The other direction: harness busy + projection idle → busy.
	a.IngestForTest(&abiv1.Event{
		SessionId: "ses-1",
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		Status:    abiv1.SessionStatus_SESSION_STATUS_IDLE,
	})
	require.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, a.State().Sessions["ses-1"].Status)
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_BUSY)
	a.Reconcile(context.Background())
	assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_BUSY, a.State().Sessions["ses-1"].Status,
		"status event loss converges from harness truth too")
}

// TestSnapshotServe_RefreshesLease (the incident replay, serve path): the
// stranded ask clears on the browser's refresh — snapshot serve refreshes
// the session's lease before building.
func TestSnapshotServe_RefreshesLease(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_stranded")

	res, err := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
	require.NoError(t, err)
	assert.Empty(t, res.Msg.GetPendingInputs(), "the stranded ask cleared on serve")
	assert.Equal(t, 1, store.gatherCount(), "one gather per serve — pod-local, no stampede (leg 9)")
}

// TestSnapshotServe_GatherFailureServesProjection: a failing gather on
// serve serves the projection as-is (degraded truth, never empty-authoritative).
func TestSnapshotServe_GatherFailureServesProjection(t *testing.T) {
	store := newLeaseStore()
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")

	store.fail(errStringOf("store unreachable"))
	res, err := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
	require.NoError(t, err)
	assert.Len(t, res.Msg.GetPendingInputs(), 1, "degraded serve keeps the projection")
}

// TestLeasePass_Leg1CadenceConverges (fault leg 1): the ask was silently
// dropped by the harness — the cadence pass resolves it within the L3
// bound (one ReconcileCadence tick after the drop).
func TestLeasePass_Leg1CadenceConverges(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE, input("per_1"))
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")
	require.Equal(t, 1, pendingCount(a, "ses-1"))

	// The harness drops the ask with NO event (leg 1)...
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE) // truth: nothing pending

	// ...one cadence tick later, the projection converged (L3).
	a.Reconcile(context.Background())
	assert.Equal(t, 0, pendingCount(a, "ses-1"), "leg 1: silently-dropped ask converges within one pass")
}
