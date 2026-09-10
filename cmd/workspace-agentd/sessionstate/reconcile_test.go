// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// epic-71 / 1b (#1311): ledger store-evidence convergence. The projection
// reseed heals the projection; these tests pin that the LEDGER joins the
// convergence — S7 (no stranded LEDGERED/ADMITTED/STALLED row survives
// contrary store evidence), S8 (reopen+reseed satisfies S7), L4 (busy
// re-derives from truth), L5 (deadline-bounded sweep).

// evidenceStore is the controllable store-evidence fake: session statuses
// plus message presence, with independent failure injection.
type evidenceStore struct {
	mu        sync.Mutex
	states    map[string]abiv1.SessionStatus // missing session = absent from store
	msgs      map[string]map[string]bool     // session -> messageID -> present
	statesErr error
	msgsErr   error
	msgCalls  int
}

func (s *evidenceStore) SessionStates(ctx context.Context) (map[string]SessionSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statesErr != nil {
		return nil, s.statesErr
	}
	out := make(map[string]SessionSeed, len(s.states))
	for k, v := range s.states {
		out[k] = SessionSeed{Status: v}
	}
	return out, nil
}

func (s *evidenceStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgCalls++
	if s.msgsErr != nil {
		return nil, s.msgsErr
	}
	present := map[string]bool{}
	for _, id := range messageIDs {
		present[id] = s.msgs[sessionID][id]
	}
	return present, nil
}

func (s *evidenceStore) setStates(states map[string]abiv1.SessionStatus) {
	s.mu.Lock()
	s.states = states
	s.mu.Unlock()
}

// newReconcileAuthority builds an authority with a wired ledger (fake
// admitter) and the evidence store.
func newReconcileAuthority(t *testing.T, store StoreReader) *Authority {
	t.Helper()
	a, err := New(Config{
		PlatformDir: t.TempDir(),
		Parser:      &reconcileNopParser{},
		Store:       store,
		Passwords:   []string{"pw"},
		Admitter:    &fakeAdmitter{},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	return a
}

type reconcileNopParser struct{}

func (reconcileNopParser) Parse(_ []byte) (*abiv1.Event, bool, error) { return nil, false, nil }

func seedRow(t *testing.T, a *Authority, sessionID, entryID, messageID string, state LedgerState) {
	t.Helper()
	_, _, err := a.ledger.ledger(sessionID, entryID, 1, []string{"p"}, "")
	require.NoError(t, err)
	switch state {
	case LedgerStateAdmitted, LedgerStateStalled, LedgerStatePromoted:
		require.NoError(t, a.ledger.markAdmitted(entryID, 1, messageID))
	}
	if state == LedgerStatePromoted {
		require.NoError(t, a.ledger.markPromoted(entryID, 1, messageID))
	}
	if state == LedgerStateStalled {
		a.ledger.mu.Lock()
		a.ledger.deadline = 0
		a.ledger.mu.Unlock()
		a.ledger.checkStalls(context.Background(), func(context.Context, string) error { return nil }, time.Now().Add(10*time.Millisecond))
	}
}

// TestReconcile_Matrix is the #1311 reconciliation matrix: starting state ×
// store evidence → terminal state. No path may leave a contradicting row.
func TestReconcile_Matrix(t *testing.T) {
	tests := []struct {
		name       string
		start      LedgerState
		states     map[string]abiv1.SessionStatus
		msgs       map[string]map[string]bool
		want       LedgerState
		wantStats  ReconcileStats
		wantDepth  int
		passesDead bool // age the row past the admission deadline first
	}{
		{
			name:  "admitted message present promotes",
			start: LedgerStateAdmitted, msgs: map[string]map[string]bool{"s1": {"m1": true}},
			states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
			want:   LedgerStatePromoted, wantStats: ReconcileStats{Promoted: 1},
		},
		{
			name:  "stalled message present promotes",
			start: LedgerStateStalled, msgs: map[string]map[string]bool{"s1": {"m1": true}},
			states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
			want:   LedgerStatePromoted, wantStats: ReconcileStats{Promoted: 1},
		},
		{
			name:   "admitted idle turn ends",
			start:  LedgerStateAdmitted,
			states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
			want:   LedgerStateTurnEnded, wantStats: ReconcileStats{TurnEnded: 1},
		},
		{
			name:   "stalled idle turn ends",
			start:  LedgerStateStalled,
			states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
			want:   LedgerStateTurnEnded, wantStats: ReconcileStats{TurnEnded: 1},
		},
		{
			name:  "admitted session absent turn ends",
			start: LedgerStateAdmitted, states: map[string]abiv1.SessionStatus{},
			want: LedgerStateTurnEnded, wantStats: ReconcileStats{TurnEnded: 1},
		},
		{
			name:   "admitted busy no message stays",
			start:  LedgerStateAdmitted,
			states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
			msgs:   map[string]map[string]bool{"s1": {}},
			want:   LedgerStateAdmitted,
		},
		{
			name:   "ledgered within deadline stays",
			start:  LedgerStateLedgered,
			states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
			want:   LedgerStateLedgered,
		},
		{
			name:       "ledgered past deadline fails re-armable",
			start:      LedgerStateLedgered,
			states:     map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
			want:       LedgerStateFailed,
			wantStats:  ReconcileStats{Failed: 1},
			passesDead: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &evidenceStore{states: tt.states, msgs: tt.msgs}
			if store.msgs == nil {
				store.msgs = map[string]map[string]bool{}
			}
			a := newReconcileAuthority(t, store)
			seedRow(t, a, "s1", "e1", "m1", tt.start)
			if tt.passesDead {
				a.SetAdmissionDeadlineForTest(-time.Second)
			} else {
				a.SetAdmissionDeadlineForTest(time.Hour)
			}
			// Row seeding wrote UpdatedAt=now; a zero deadline makes age>0 enough.
			got := a.Reconcile(context.Background())
			assert.Equal(t, tt.wantStats, got, "stats")
			row, ok := a.ledger.status("e1", 1)
			require.True(t, ok)
			assert.Equal(t, tt.want, row.State, "row state")
			wantDepth := tt.wantDepth
			if wantDepth == 0 && (tt.want == LedgerStateLedgered || tt.want == LedgerStateAdmitted) {
				wantDepth = 1
			}
			assert.Equal(t, wantDepth, a.ledger.queueDepth("s1"), "queue depth after reconcile")
		})
	}
}

// TestReconcile_FailedRowIsReArmable: the FAILED sweep outcome is re-armable
// — attempt+1 creates a fresh row that admits normally.
func TestReconcile_FailedRowIsReArmable(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY}}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateLedgered)
	a.SetAdmissionDeadlineForTest(-time.Second)
	require.Equal(t, ReconcileStats{Failed: 1}, a.Reconcile(context.Background()))

	_, created, err := a.ledger.ledger("s1", "e1", 2, []string{"p"}, "")
	require.NoError(t, err)
	require.True(t, created, "attempt+1 re-arms after a swept failure")
	require.NoError(t, a.ledger.markAdmitted("e1", 2, "m2"))
	row, _ := a.ledger.status("e1", 2)
	assert.Equal(t, LedgerStateAdmitted, row.State)
}

// TestReconcile_NoPrematureSweep: a LEDGERED row inside its admission
// deadline is untouched even when evidence is idle — the retry/replay path
// owns it until the deadline passes.
func TestReconcile_NoPrematureSweep(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)
	a.SetAdmissionDeadlineForTest(time.Hour)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateLedgered)

	stats := a.Reconcile(context.Background())
	assert.Equal(t, ReconcileStats{}, stats)
	row, _ := a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStateLedgered, row.State)
}

// TestReconcile_EvidenceFailureNeverAuthoritative: a store-evidence error
// leaves every row untouched (never an authoritative empty) and counts the
// failure; a later healthy pass converges.
func TestReconcile_EvidenceFailureNeverAuthoritative(t *testing.T) {
	store := &evidenceStore{
		states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
		msgs:   map[string]map[string]bool{"s1": {"m1": true}},
	}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)

	store.mu.Lock()
	store.statesErr = errors.New("store down")
	store.mu.Unlock()
	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.EvidenceFailures, "failure counted")
	row, _ := a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStateAdmitted, row.State, "row untouched on evidence failure")

	store.mu.Lock()
	store.statesErr = nil
	store.mu.Unlock()
	require.Equal(t, ReconcileStats{Promoted: 1}, a.Reconcile(context.Background()))
}

// TestReconcile_MessageEvidenceFailureSkipsRowsOnly: status evidence stands
// when message evidence errors — busy rows are untouched, and idle-status
// rows still converge via the turn-ended arm (promotion refinement lost,
// truth preserved).
func TestReconcile_MessageEvidenceFailureSkipsRowsOnly(t *testing.T) {
	store := &evidenceStore{
		states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
		msgs:   map[string]map[string]bool{"s1": {}},
	}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)

	store.mu.Lock()
	store.msgsErr = errors.New("message store down")
	store.mu.Unlock()
	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.EvidenceFailures)
	row, _ := a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStateAdmitted, row.State, "busy session: row untouched on message-evidence failure")

	// Idle status evidence alone converges the row (no message evidence).
	store.mu.Lock()
	store.states = map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}
	store.mu.Unlock()
	stats = a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.EvidenceFailures, "message evidence still failing")
	assert.Equal(t, 1, stats.TurnEnded, "status evidence alone turn-ends")
	row, _ = a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStateTurnEnded, row.State)
}

// TestReconcile_StatusReDerivation (L4): a BUSY view over a reconciled-empty
// ledger with an idle harness store reports idle — BUSY must not survive a
// reconcile pass that observes idle truth.
func TestReconcile_StatusReDerivation(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY}, msgs: map[string]map[string]bool{"s1": {"m1": true}}}
	a := newReconcileAuthority(t, store)
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)

	// busy session evidence first: busy survives (turn may still be real)
	store.setStates(map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY})
	stats := a.Reconcile(context.Background())
	assert.Equal(t, ReconcileStats{Promoted: 1, BusyCleared: 0}, stats)
	st := a.State()
	assert.True(t, st.Sessions["s1"].Busy, "busy evidence keeps busy")

	// now wedge the view busy again with idle truth and no live rows
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	store.setStates(map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE})
	stats = a.Reconcile(context.Background())
	assert.Equal(t, ReconcileStats{BusyCleared: 1}, stats)
	st = a.State()
	require.NotNil(t, st.Sessions["s1"])
	assert.False(t, st.Sessions["s1"].Busy, "idle truth + empty ledger clears busy")
	assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, st.Sessions["s1"].Status)
}

// TestReconcile_SnapshotQueueDepthPostSweep: the incident shape — nine
// admitted-unpromoted rows for turns that completed — converges to
// queueDepth 0; genuinely queued rows still count.
func TestReconcile_SnapshotQueueDepthPostSweep(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)
	a.SetAdmissionDeadlineForTest(time.Hour)
	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	for i := 0; i < 9; i++ {
		seedRow(t, a, "s1", "e-incident-"+string(rune('a'+i)), "m-"+string(rune('a'+i)), LedgerStateAdmitted)
	}
	seedRow(t, a, "s1", "e-live", "", LedgerStateLedgered) // genuinely queued

	stats := a.Reconcile(context.Background())
	// BusyCleared: the BUSY view re-derives idle from harness truth even
	// with a queued LEDGERED row — queued ≠ running (#1312 ownership
	// table; the turn's MESSAGE_START re-marks busy when it starts).
	assert.Equal(t, ReconcileStats{TurnEnded: 9, BusyCleared: 1}, stats)

	a.mu.Lock()
	snap := a.sessionSnapshotLocked("s1", a.sessions["s1"])
	a.mu.Unlock()
	assert.Equal(t, int32(1), snap.GetQueueDepth(), "only the genuinely-queued row remains")
	assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, snap.GetStatus())
}

// TestReseedSweepsLedger_BootAutoHeal: the deploy-heals-everything row — a
// reopen (kill semantics) over a wedged WAL + idle store reseeds to a
// converged state with no operator action (S8's executable form).
func TestReseedSweepsLedger_BootAutoHeal(t *testing.T) {
	dir := t.TempDir()
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}

	a1, err := New(Config{PlatformDir: dir, Parser: &nopParser{}, Store: store, Passwords: []string{"pw"}, Admitter: &fakeAdmitter{}, FastCursor: true})
	require.NoError(t, err)
	for i := 0; i < 9; i++ {
		seedRow(t, a1, "s1", "e-"+string(rune('a'+i)), "m-"+string(rune('a'+i)), LedgerStateAdmitted)
	}
	a1.KillForTest() // abandon resources: hard-death semantics

	a2, err := New(Config{PlatformDir: dir, Parser: &nopParser{}, Store: store, Passwords: []string{"pw"}, Admitter: &fakeAdmitter{}, FastCursor: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a2.Close() })
	require.NoError(t, a2.Reseed(context.Background(), ReseedReasonBoot))

	a2.mu.Lock()
	snap := a2.sessionSnapshotLocked("s1", a2.sessions["s1"])
	a2.mu.Unlock()
	require.NotNil(t, snap)
	assert.Equal(t, int32(0), snap.GetQueueDepth(), "wedged rows swept at boot reseed")
	assert.Equal(t, abiv1.SessionStatus_SESSION_STATUS_IDLE, snap.GetStatus())
}

// TestReconcile_CrashInjectionLeg4: kill agentd at each ledger state
// transition × {before, after} the WAL write lands — post-restart reseed
// satisfies S7 in every cell. "Before the WAL write" is modeled by the
// predecessor state on disk (the write that never happened).
func TestReconcile_CrashInjectionLeg4(t *testing.T) {
	cells := []struct {
		onDisk   LedgerState // what the WAL holds at the kill
		evidence map[string]abiv1.SessionStatus
		msgs     map[string]bool
		want     LedgerState
	}{
		{LedgerStateLedgered, map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY}, nil, LedgerStateLedgered},
		{LedgerStateAdmitted, map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}, nil, LedgerStateTurnEnded},
		{LedgerStateAdmitted, map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY}, map[string]bool{"m1": true}, LedgerStatePromoted},
		{LedgerStateStalled, map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}, nil, LedgerStateTurnEnded},
		{LedgerStateStalled, map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY}, map[string]bool{"m1": true}, LedgerStatePromoted},
		{LedgerStatePromoted, map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}, map[string]bool{"m1": true}, LedgerStatePromoted},
	}
	for _, cell := range cells {
		t.Run(cell.onDisk.String(), func(t *testing.T) {
			dir := t.TempDir()
			store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": cell.evidence["s1"]}, msgs: map[string]map[string]bool{"s1": cell.msgs}}
			cfg := Config{PlatformDir: dir, Parser: &nopParser{}, Store: store, Passwords: []string{"pw"}, Admitter: &fakeAdmitter{}, FastCursor: true}

			a1, err := New(cfg)
			require.NoError(t, err)
			seedRow(t, a1, "s1", "e1", "m1", cell.onDisk)
			a1.KillForTest()

			a2, err := New(cfg)
			require.NoError(t, err)
			t.Cleanup(func() { _ = a2.Close() })
			a2.SetAdmissionDeadlineForTest(time.Hour)
			require.NoError(t, a2.Reseed(context.Background(), ReseedReasonBoot))

			row, ok := a2.ledger.status("e1", 1)
			require.True(t, ok)
			assert.Equal(t, cell.want, row.State, "post-restart reseed satisfies S7")
		})
	}
}

// TestReconcile_Leg5HarnessDeadMidTurn: the harness dies mid-turn — no
// status or promotion events ever arrive — and the turn's rows resolve via
// store evidence within one reconcile pass (L5's shape; the 30s bound is
// the cadence's, asserted separately by the wiring test).
func TestReconcile_Leg5HarnessDeadMidTurn(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)

	// Real delivery path: deliver → admitted. Events never arrive (nop parser).
	_, _, err := a.deliver.deliver(context.Background(), "s1", "e1", 1, []string{"turn prompt"}, "")
	require.NoError(t, err)
	waitFor(t, func() bool {
		st, _ := a.ledger.status("e1", 1)
		return st != nil && st.State == LedgerStateAdmitted
	}, "row admitted")

	// Store truth says the turn ended and the message landed.
	store.mu.Lock()
	store.msgs = map[string]map[string]bool{"s1": {"msg-1": true}}
	store.mu.Unlock()

	stats := a.Reconcile(context.Background())
	assert.Equal(t, ReconcileStats{Promoted: 1}, stats)
	row, _ := a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStatePromoted, row.State, "harness-dead turn resolves via store evidence")
}

// TestReconcile_SerializesWithInFlightAdmission: the sweep holds the
// session's single-flight lock — an admission completing during a sweep can
// never land on a row the sweep already FAILED (the FAILED-with-landed-
// message duplication hazard).
func TestReconcile_SerializesWithInFlightAdmission(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY}}
	a := newReconcileAuthority(t, store)
	block := &blockingAdmit{proceed: make(chan struct{})}
	a.deliver.admit = block

	released := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		_, _, _ = a.deliver.deliver(context.Background(), "s1", "e1", 1, []string{"p"}, "")
		close(released)
	}()
	<-started
	waitFor(t, func() bool {
		block.mu.Lock()
		defer block.mu.Unlock()
		return block.entered
	}, "admission in flight under the session lock")

	// Zero deadline + a reconcile that must wait on the session lock: the
	// sweep cannot fail the row while the admission holds it.
	a.SetAdmissionDeadlineForTest(-time.Second)
	done := make(chan ReconcileStats, 1)
	go func() { done <- a.Reconcile(context.Background()) }()
	select {
	case <-done:
		t.Fatal("reconcile completed while an admission held the session lock — serialization broken")
	case <-time.After(50 * time.Millisecond):
	}
	block.mu.Lock()
	block.proceed <- struct{}{}
	block.mu.Unlock()
	<-released
	<-done

	row, _ := a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStateAdmitted, row.State, "admission wins over the concurrent sweep; no manufactured failure")
}

// blockingAdmit blocks inside Admit until released (serialization proofs).
type blockingAdmit struct {
	mu      sync.Mutex
	entered bool
	proceed chan struct{}
}

func (b *blockingAdmit) Admit(ctx context.Context, sessionID, text, model string) (string, error) {
	b.mu.Lock()
	b.entered = true
	b.mu.Unlock()
	<-b.proceed
	return "msg-1", nil
}

// TestObserveEvent_IdleTurnEndsPromotedRows pins the event-path wiring of
// PROMOTED → TURN_ENDED (the previously-unwired observeTurnEnded): a
// session-idle contract event terminates that session's promoted rows.
func TestObserveEvent_IdleTurnEndsPromotedRows(t *testing.T) {
	a := newReconcileAuthority(t, &evidenceStore{states: map[string]abiv1.SessionStatus{}})
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)
	require.NoError(t, a.ledger.markPromoted("e1", 1, "m1"))

	a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	row, _ := a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStateTurnEnded, row.State, "idle event turn-ends promoted rows")
}

// TestLeaseConvergenceBoundIsExported: the epic's shared lease clock (one
// constant, consumed by 1b's sweep cadence and 2a's pending lease).
func TestLeaseConvergenceBoundIsExported(t *testing.T) {
	assert.Equal(t, 30*time.Second, LeaseConvergenceBound)
	assert.True(t, ReconcileCadence <= LeaseConvergenceBound, "sweep cadence must fit the convergence bound")
}
