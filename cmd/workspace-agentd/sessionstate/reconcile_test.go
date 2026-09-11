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
			// "with no evidence" (#1311): a BUSY session is evidence an
			// admission/turn may be live for this row — the sweep holds.
			name:       "ledgered past deadline busy session holds",
			start:      LedgerStateLedgered,
			states:     map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
			want:       LedgerStateLedgered,
			passesDead: true,
		},
		{
			name:       "ledgered past deadline idle no evidence fails",
			start:      LedgerStateLedgered,
			states:     map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
			want:       LedgerStateFailed,
			wantStats:  ReconcileStats{Failed: 1},
			passesDead: true,
		},
		{
			name:       "ledgered past deadline absent session fails",
			start:      LedgerStateLedgered,
			states:     map[string]abiv1.SessionStatus{},
			want:       LedgerStateFailed,
			wantStats:  ReconcileStats{Failed: 1},
			passesDead: true,
		},
		{
			// STALLED with AGREEING evidence (busy, message absent)
			// persists — no clock forces it off while evidence agrees.
			name:   "stalled busy no message stays",
			start:  LedgerStateStalled,
			states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_BUSY},
			msgs:   map[string]map[string]bool{"s1": {}},
			want:   LedgerStateStalled,
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
			if wantDepth == 0 && (tt.want == LedgerStateLedgered || tt.want == LedgerStateAdmitted || tt.want == LedgerStateStalled) {
				wantDepth = 1
			}
			assert.Equal(t, wantDepth, a.ledger.queueDepth("s1"), "queue depth after reconcile")
		})
	}
}

// TestReconcile_FailedRowIsReArmable: the FAILED sweep outcome is re-armable
// — attempt+1 creates a fresh row that admits normally.
func TestReconcile_FailedRowIsReArmable(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
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

// TestReconcile_SerializesWithInFlightAdmission: a session mid-admission
// (its single-flight lock held across the V1 turn) is SKIPPED by the sweep
// — never waited on, never failed — so an admission completing during a
// pass can never land on a row the sweep already FAILED (the
// FAILED-with-landed-message duplication hazard).
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

	// Past-deadline row + a pass that must NOT touch the locked session.
	a.SetAdmissionDeadlineForTest(-time.Second)
	done := make(chan ReconcileStats, 1)
	go func() { done <- a.Reconcile(context.Background()) }()
	select {
	case stats := <-done:
		assert.Equal(t, ReconcileStats{}, stats, "the locked session is skipped entirely")
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile blocked behind a live admission — head-of-line blocking")
	}
	block.mu.Lock()
	block.proceed <- struct{}{}
	block.mu.Unlock()
	<-released

	waitFor(t, func() bool {
		row, ok := a.ledger.status("e1", 1)
		return ok && row.State == LedgerStateAdmitted
	}, "admission lands after the skipped pass (no manufactured failure)")
}

// blockingAdmit blocks inside Admit until released (serialization proofs).
type blockingAdmit struct {
	mu      sync.Mutex
	entered bool
	proceed chan struct{}
}

func (b *blockingAdmit) Admit(ctx context.Context, sessionID, messageID, text, model string) (string, error) {
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

// --- review round 1 findings ------------------------------------------------

// gateStore lets the test hold the pass at a chosen point (review r1
// finding 2: state that lands between evidence-gather and the session lock
// must not be resolved on stale evidence; r2-1: state that lands DURING
// the store read must postdate the freshness stamp).
type gateStore struct {
	evidenceStore
	onStates   func()
	onMessages func()
}

func (g *gateStore) SessionStates(ctx context.Context) (map[string]SessionSeed, error) {
	if g.onStates != nil {
		g.onStates()
	}
	return g.evidenceStore.SessionStates(ctx)
}

func (g *gateStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	if g.onMessages != nil {
		g.onMessages()
	}
	return g.evidenceStore.MessagePresence(ctx, sessionID, messageIDs)
}

// TestReconcile_LiveTurnDuringEvidenceWaitNotMisresolved (r1-2): while the
// sweep waits between evidence-gather and the session lock, a live turn
// starts — a NEW admitted row appears and the view re-marks busy. The stale
// evidence must not turn-end the new row (its messageID was never queried)
// and must not clear the freshly-marked busy.
func TestReconcile_LiveTurnDuringEvidenceWaitNotMisresolved(t *testing.T) {
	store := &gateStore{evidenceStore: evidenceStore{
		states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
		msgs:   map[string]map[string]bool{"s1": {"m-old": true}},
	}}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e-old", "m-old", LedgerStateAdmitted)

	store.onMessages = func() {
		// The live turn lands while the pass holds stale (idle) evidence:
		// a fresh busy fold + a new admitted row the pass never queried.
		a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
		seedRow(t, a, "s1", "e-new", "m-new", LedgerStateAdmitted)
		store.onMessages = nil
	}

	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.Promoted, "the queried row resolves on real evidence")

	newRow, ok := a.ledger.status("e-new", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateAdmitted, newRow.State, "a row whose messageID was never queried must not resolve on stale evidence")

	st := a.State()
	require.NotNil(t, st.Sessions["s1"])
	assert.True(t, st.Sessions["s1"].Busy, "busy marked AFTER the evidence read must survive the pass")
}

// TestReconcile_LockedSessionSkippedNotBlocked (r1-robustness-1): a session
// whose single-flight lock is held (a live admission — up to 3 minutes) is
// SKIPPED this pass, not waited on: one in-flight turn must not stall the
// whole sweep (head-of-line blocking would break the 30s bound).
func TestReconcile_LockedSessionSkippedNotBlocked(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}, msgs: map[string]map[string]bool{"s1": {"m1": true}}}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)

	lock := a.sessionLock("s1")
	lock.Lock()
	done := make(chan ReconcileStats, 1)
	go func() { done <- a.Reconcile(context.Background()) }()
	select {
	case stats := <-done:
		assert.Equal(t, ReconcileStats{}, stats, "locked session skipped, nothing converged this pass")
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile blocked on a session mid-admission — head-of-line blocking")
	}
	row, _ := a.ledger.status("e1", 1)
	assert.Equal(t, LedgerStateAdmitted, row.State, "locked session's rows untouched")

	lock.Unlock()
	require.Equal(t, ReconcileStats{Promoted: 1}, a.Reconcile(context.Background()), "next pass converges")
}

// TestReconcile_ContextCancelRecordsOutcomes (r1-3): a pass canceled
// mid-sweep still records the outcomes that DID happen — the cumulative
// Metrics counters must not diverge from the returned stats.
func TestReconcile_ContextCancelRecordsOutcomes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &gateStore{evidenceStore: evidenceStore{
		states: map[string]abiv1.SessionStatus{
			"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE,
			"s2": abiv1.SessionStatus_SESSION_STATUS_IDLE,
		},
		msgs: map[string]map[string]bool{"s1": {"m1": true}, "s2": {"m2": true}},
	}}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)
	seedRow(t, a, "s2", "e2", "m2", LedgerStateAdmitted)

	store.onMessages = func() {
		cancel() // after s1's evidence, before s2's loop turn
		store.onMessages = nil
	}
	stats := a.Reconcile(ctx)
	m := a.Metrics()
	assert.Equal(t, int64(stats.Promoted), m.ReconcilePromoted, "returned stats and cumulative counters agree on a canceled pass")
	assert.GreaterOrEqual(t, m.ReconcilePromoted, int64(1), "the outcome that happened before cancellation is recorded")
}

// TestReconcile_EvidenceDeadlineBounds (r1-robustness-2): the pass bounds
// its own evidence I/O — a hung store cannot wedge the watchdog.
func TestReconcile_EvidenceDeadlineBounds(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)
	a.SetReconcileTimeoutForTest(50 * time.Millisecond)

	// Swap in a hanging store AFTER construction.
	hang := &hangMessagesStore{inner: store}
	a.SetStoreForTest(hang)

	start := time.Now()
	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.EvidenceFailures)
	assert.Less(t, time.Since(start), 5*time.Second, "evidence I/O is deadline-bounded")
}

type hangMessagesStore struct {
	inner StoreReader
}

func (h *hangMessagesStore) SessionStates(ctx context.Context) (map[string]SessionSeed, error) {
	return h.inner.SessionStates(ctx)
}

func (h *hangMessagesStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestReconcile_BusyFoldDuringStatesReadSurvives (r2-1): the freshness
// stamp is taken BEFORE the store read — a busy-fold landing DURING the
// SessionStates read must postdate it and survive the busy-clear.
func TestReconcile_BusyFoldDuringStatesReadSurvives(t *testing.T) {
	store := &gateStore{evidenceStore: evidenceStore{
		states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE},
		msgs:   map[string]map[string]bool{"s1": {"m1": true}},
	}}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)
	store.onStates = func() {
		a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
		store.onStates = nil
	}

	stats := a.Reconcile(context.Background())
	assert.Equal(t, 1, stats.Promoted, "queried evidence still resolves the row")
	st := a.State()
	require.NotNil(t, st.Sessions["s1"])
	assert.True(t, st.Sessions["s1"].Busy, "busy folded DURING the store read survives the pass — the stamp predates the read")
}

// TestReseedSweep_EvidenceDeadlineBounds (r3-2): the reseed-embedded sweep
// carries the same pass deadline the cadence path has — a hung store must
// not wedge the reseed; the sweep gives up and rows retry on the next
// reconcile pass.
func TestReseedSweep_EvidenceDeadlineBounds(t *testing.T) {
	store := &evidenceStore{states: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	a := newReconcileAuthority(t, store)
	seedRow(t, a, "s1", "e1", "m1", LedgerStateAdmitted)
	a.SetReconcileTimeoutForTest(50 * time.Millisecond)
	a.SetStoreForTest(&hangMessagesStore{inner: store})

	done := make(chan error, 1)
	go func() { done <- a.Reseed(context.Background(), ReseedReasonBoot) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reseed wedged on hung evidence I/O — the embedded sweep needs the pass deadline")
	}

	// The message leg hung, but the status evidence (idle) was read: the
	// r2 fall-through decision applies — the row converges via the
	// turn-ended arm (promotion refinement lost, truth preserved).
	row, ok := a.ledger.status("e1", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateTurnEnded, row.State, "status evidence alone converges the row under the pass deadline")
	assert.Equal(t, 0, a.ledger.queueDepth("s1"))
}
