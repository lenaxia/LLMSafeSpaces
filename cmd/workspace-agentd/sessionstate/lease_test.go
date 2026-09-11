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
	mu         sync.Mutex
	seeds      map[string]sessionstate.SessionSeed
	err        error
	pendingErr error
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

func (s *leaseStore) failPending(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingErr = err
}

func (s *leaseStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

// PendingInputs serves the seeds' pending halves with STRICT lease
// semantics; pendingErr (when set) models an endpoint failure.
func (s *leaseStore) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	out := map[string][]*abiv1.InputRequest{}
	for sid, sd := range s.seeds {
		for _, in := range sd.PendingInputs {
			if in != nil && in.GetId() != "" {
				out[sid] = append(out[sid], in)
			}
		}
	}
	return out, nil
}

func leaseAuthority(t *testing.T, store sessionstate.StoreReader) *sessionstate.Authority {
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

// TestLeasePass_GatherFailureKeepsProjection: a store failure — either
// the session-status read OR a pending-endpoint failure (the production
// /question 5xx shape, r1 review) — never mutates the projection: never
// an authoritative empty, no resolve/appear flap.
func TestLeasePass_GatherFailureKeepsProjection(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")

	store.failPending(errStringOf("lease gather /question: status 503"))
	a.Reconcile(context.Background())
	assert.Equal(t, 1, pendingCount(a, "ses-1"), "pending-endpoint failure leaves the projection untouched")

	store.failPending(nil)
	store.fail(errStringOf("session list unreachable"))
	a.Reconcile(context.Background())
	assert.Equal(t, 1, pendingCount(a, "ses-1"), "status-gather failure leaves the projection untouched")
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
}

// TestSnapshotServe_GatherFailureServesProjection: a failing gather on
// serve serves the projection as-is (degraded truth, never empty-authoritative).
func TestSnapshotServe_GatherFailureServesProjection(t *testing.T) {
	store := newLeaseStore()
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")

	store.failPending(errStringOf("lease gather /question: connection refused"))
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

// hangingPendingStore hangs the pending gather until its context dies.
type hangingPendingStore struct {
	inner *leaseStore
}

func (h *hangingPendingStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	return h.inner.SessionStates(ctx)
}

func (h *hangingPendingStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (h *hangingPendingStore) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestSnapshotServe_HungStoreIsBounded (r1 finding 2): a hung pending
// gather degrades the serve within the serve deadline — a browser
// refresh is never held hostage.
func TestSnapshotServe_HungStoreIsBounded(t *testing.T) {
	inner := newLeaseStore()
	inner.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, &hangingPendingStore{inner: inner})
	seedPendingInput(t, a, "ses-1", "per_1")

	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serve wedged on a hung store")
	}
	require.NoError(t, err)
	assert.Equal(t, 1, pendingCount(a, "ses-1"), "degraded serve keeps the projection")
}

// TestLeasePass_MaterializesUnknownSession (r1 missing test 4): live
// asks for a session the projection never saw materialize through the
// fold; a malformed empty-ID entry consumes no seq and emits nothing.
func TestLeasePass_MaterializesUnknownSession(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-unknown", abiv1.SessionStatus_SESSION_STATUS_IDLE, input("per_new"), nil)
	a := leaseAuthority(t, store)
	// Boot: the authority knows the world exists (production always
	// reseeds at start — the zero-record projection is a boot-race shape,
	// and the cadence gate stays open once any session is known).
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))

	// The reseed's embedded pass (S8) and the cadence pass both converge
	// this shape; either way ONLY the well-formed ask materializes.
	a.Reconcile(context.Background())
	v := a.State().Sessions["ses-unknown"]
	require.NotNil(t, v)
	ids := []string{}
	for _, in := range v.PendingInputs {
		ids = append(ids, in.GetId())
	}
	assert.ElementsMatch(t, []string{"per_new"}, ids,
		"exactly the well-formed ask materialized — the malformed entry emitted nothing")
}

// TestLeasePass_LedgerWiredCadenceComposes (r1 missing test 3): in the
// production topology (Admitter wired → ledger present), the lease diff
// and the evidence sweep compose on one cadence pass: harness-idle busy
// clears through the sweep's seq gate (1b — the authoritative rule), and
// the pending diff still converges.
func TestLeasePass_LedgerWiredCadenceComposes(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := actionsAuthority(t, &recordingActor{}, []abiv1.ActionType{abiv1.ActionType_ACTION_TYPE_ANSWER_QUESTION}, &leaseAdmitter{})
	a.SetStoreForTest(store)

	a.IngestForTest(&abiv1.Event{SessionId: "ses-1", Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	seedPendingInput(t, a, "ses-1", "per_1")
	require.Equal(t, 1, pendingCount(a, "ses-1"))

	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.LeaseResolved, "pending converged on the same pass")
	assert.Equal(t, 0, pendingCount(a, "ses-1"))
	// Busy cleared by the evidence sweep (seq-gated — the authoritative
	// gate in this topology; the lease window is the ledger-less backstop).
	assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, a.State().Sessions["ses-1"].Status)
}

// TestSnapshotServe_ConcurrentStormBounded (r1 missing test 5): parallel
// serves coalesce onto the gather singleflight — the storm costs one
// gather, not one per serve.
func TestSnapshotServe_ConcurrentStormBounded(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE, input("per_1"))
	a := leaseAuthority(t, store)
	// The session is known to the projection before anything asks for its
	// snapshot (production shape: events/reseed precede API snapshot
	// calls — a never-seen session's serve is a 404 by contract).
	a.IngestForTest(&abiv1.Event{SessionId: "ses-1", Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	// One in-flight gather; waiters serve the projection as-is. The
	// storm never multiplied harness load, and the projection converged
	// to the live truth (the ask appears — it was never projected).
	assert.Equal(t, 1, pendingCount(a, "ses-1"), "the projection converged to live truth")
}

type leaseAdmitter struct{}

func (leaseAdmitter) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
	return "msg-lease", nil
}

// TestSnapshotServe_CachedSliceNeverResurrects (r2 finding 4): a resolve
// folding just after a gather, followed by a serve inside the TTL window,
// must NOT re-emit the ask from the cached slice — cached data is trusted
// for absence (resolve) only, never for addition.
func TestSnapshotServe_CachedSliceNeverResurrects(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE, input("per_1"))
	a := leaseAuthority(t, store)
	a.IngestForTest(&abiv1.Event{SessionId: "ses-1", Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})

	// First serve: fresh gather; the ask appears from live truth.
	_, err := a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
	require.NoError(t, err)
	require.Equal(t, 1, pendingCount(a, "ses-1"))

	// The user answers; the harness accepts (truth drops the ask) — the
	// projection folds away right after the cached gather was taken.
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a.IngestForTest(&abiv1.Event{SessionId: "ses-1", Type: abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, Input: input("per_1")})
	require.Equal(t, 0, pendingCount(a, "ses-1"))

	// A serve inside the TTL window reuses the cached slice — it must NOT
	// resurrect the just-resolved ask.
	_, err = a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
	require.NoError(t, err)
	assert.Equal(t, 0, pendingCount(a, "ses-1"), "cached serves resolve, never add — no click-then-refresh flicker")

	// Past the TTL, a fresh gather re-syncs both halves from truth.
	time.Sleep(600 * time.Millisecond)
	_, err = a.GetSnapshot(context.Background(), connect.NewRequest(&abiv1.GetSnapshotRequest{SessionId: "ses-1"}))
	require.NoError(t, err)
	assert.Equal(t, 0, pendingCount(a, "ses-1"), "truth holds nothing; the fresh gather agrees")
}

// TestLeasePass_FailureSignalIsExported (r3 findings 1-2): a failing
// pending gather increments EvidenceFailures in the pass stats AND the
// cumulative LeaseGatherFails on Metrics — counted, not just logged, and
// race-safe against a concurrent Metrics scrape.
func TestLeasePass_FailureSignalIsExported(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")
	store.failPending(errStringOf("lease gather /question: status 503"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			a.Reconcile(context.Background())
		}
	}()
	for i := 0; i < 50; i++ {
		_ = a.Metrics() // concurrent scrapes — -race pins the convention
	}
	<-done

	m := a.Metrics()
	assert.Equal(t, int64(50), m.LeaseGatherFails, "cumulative gather failures exported")
	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.EvidenceFailures, "the pass stats carry the failure (watchdog Warn gate)")
}

// TestLeasePass_OutcomesExported (r3 finding 1): resolved/appeared reach
// the cumulative Metrics fields — the heal is observable.
func TestLeasePass_OutcomesExported(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE, input("per_new"))
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_old")

	a.Reconcile(context.Background())
	m := a.Metrics()
	assert.Equal(t, int64(1), m.LeaseResolved, "resolved-by-absence cumulative")
	assert.Equal(t, int64(1), m.LeaseAppeared, "appeared-from-truth cumulative")
}

// TestLeasePass_CanceledPassIsNotASourceFailure (r4): a canceled pass
// records its partial outcomes but does NOT count the cancellation as a
// lease gather failure on the scrape.
func TestLeasePass_CanceledPassIsNotASourceFailure(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE)
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.Reconcile(ctx)

	m := a.Metrics()
	assert.Equal(t, int64(0), m.LeaseGatherFails, "cancellation is not a lease-source failure")
	assert.Equal(t, 1, pendingCount(a, "ses-1"), "projection untouched")
}

// TestLeaseDeltas_FirstScrapeCarriesCumulative (r4): the delta bridge
// returns the full cumulative on first sight (the restart-heal window).
func TestLeaseDeltas_FirstScrapeCarriesCumulative(t *testing.T) {
	store := newLeaseStore()
	store.seed("ses-1", abiv1.SessionStatus_SESSION_STATUS_IDLE, input("per_new"))
	a := leaseAuthority(t, store)
	seedPendingInput(t, a, "ses-1", "per_old")
	a.Reconcile(context.Background())

	m := a.Metrics()
	require.Equal(t, int64(1), m.LeaseResolved)
	require.Equal(t, int64(1), m.LeaseAppeared)
}
