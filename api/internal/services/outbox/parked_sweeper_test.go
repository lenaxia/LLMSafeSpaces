// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/obs"
	"github.com/prometheus/client_golang/prometheus"
	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- 0b (#1316): parked-error sweeper + never-park-while-ledger-admits -----
//
// The 2026-09-10 incident parked entries as status:error on honest
// timeouts while the agentd ledger still held them admitted — the queue
// UI showed error pills only manual Valkey surgery could clear. The
// sweeper re-verifies parked entries against the ledger (the truth
// source) and the park write itself refuses to park a row the ledger
// still holds.

func rowKeyOf(entryID string, attempt uint32) string {
	return fmt.Sprintf("%s|%d", entryID, attempt)
}

// probeFunc builds a LedgerProbe from a canned map keyed by
// "entryID|attempt". A missing key yields ("", nil) — no row. The
// special "probe-error" value forces probe failures.
func probeFunc(t *testing.T, states map[string]string) (LedgerProbe, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	return func(ctx context.Context, workspaceID, sessionID, entryID string, attempt uint32) (string, error) {
		calls.Add(1)
		if v, ok := states[rowKeyOf(entryID, attempt)]; ok {
			if v == "probe-error" {
				return "", errors.New("agent unreachable")
			}
			return v, nil
		}
		return "", nil
	}, &calls
}

// seedParkedEntry seeds a status:error entry with the given history.
func seedParkedEntry(t *testing.T, s *Service, ws, ses, id string, attempts int, lastErr string) Entry {
	t.Helper()
	e := Entry{ID: id, ClientMessageID: "cmid-" + id, UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusError, Attempts: attempts, LastError: lastErr}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey(ws, ses), string(raw)).Err())
	return e
}

// TestSweepParkedErrors_Matrix is the #1316 decision table: parked error
// entries re-verified against the ledger's GetDeliveryStatus for their
// last attempt (and the ambiguous in-flight attempt+1 when no row
// exists). admitted-or-later completes; failed with budget re-arms;
// failed and exhausted stays (terminal, user retry/dismiss); ledgered
// stays (agentd owns admission — converges when its deadline resolves
// the row); unverifiable never blind re-arms (#987).
func TestSweepParkedErrors_Matrix(t *testing.T) {
	tests := []struct {
		name string
		// ledger rows: entryID|attempt -> state ("" probes return no-row)
		rows         map[string]string
		attempts     int
		lastErr      string
		wantStatus   string // "" = entry removed (completed)
		wantAttempts int
		wantDeliver  bool
	}{
		{
			name:        "admitted at last attempt completes",
			rows:        map[string]string{"e1|5": LedgerStateAdmitted},
			attempts:    5,
			lastErr:     "context deadline exceeded",
			wantStatus:  "",
			wantDeliver: true,
		},
		{
			name:        "promoted implies admitted — completes",
			rows:        map[string]string{"e1|5": LedgerStatePromoted},
			attempts:    5,
			lastErr:     "context deadline exceeded",
			wantStatus:  "",
			wantDeliver: true,
		},
		{
			name:        "turn-ended completes",
			rows:        map[string]string{"e1|5": LedgerStateTurnEnded},
			attempts:    5,
			lastErr:     "context deadline exceeded",
			wantStatus:  "",
			wantDeliver: true,
		},
		{
			name:        "stalled completes",
			rows:        map[string]string{"e1|5": LedgerStateStalled},
			attempts:    5,
			lastErr:     "context deadline exceeded",
			wantStatus:  "",
			wantDeliver: true,
		},
		{
			name:       "ledgered stays parked (agentd owns admission)",
			rows:       map[string]string{"e1|5": LedgerStateLedgered},
			attempts:   5,
			lastErr:    "context deadline exceeded",
			wantStatus: StatusError,
		},
		{
			name:       "failed and exhausted stays parked",
			rows:       map[string]string{"e1|5": LedgerStateFailed},
			attempts:   5,
			lastErr:    "context deadline exceeded",
			wantStatus: StatusError,
		},
		{
			name:         "failed with budget re-arms pending",
			rows:         map[string]string{"e1|2": LedgerStateFailed},
			attempts:     2,
			lastErr:      "context deadline exceeded",
			wantStatus:   StatusPending,
			wantAttempts: 2,
		},
		{
			name:         "no rows and budget re-arms pending (ledger rotated)",
			rows:         map[string]string{},
			attempts:     2,
			lastErr:      "context deadline exceeded",
			wantStatus:   StatusPending,
			wantAttempts: 2,
		},
		{
			name:       "no rows and exhausted stays parked",
			rows:       map[string]string{},
			attempts:   5,
			lastErr:    "context deadline exceeded",
			wantStatus: StatusError,
		},
		{
			name:        "ambiguous in-flight attempt+1 admitted completes (unverifiable class)",
			rows:        map[string]string{"e1|3": LedgerStateAdmitted},
			attempts:    2,
			lastErr:     lastErrUnverifiable,
			wantStatus:  "",
			wantDeliver: true,
		},
		{
			name:         "attempt+1 failed adopts the failure and re-arms at the true count",
			rows:         map[string]string{"e1|3": LedgerStateFailed},
			attempts:     2,
			lastErr:      "ambiguous: context deadline exceeded",
			wantStatus:   StatusPending,
			wantAttempts: 3,
		},
		{
			name:       "attempt+1 failed with adopted budget exhausted stays parked",
			rows:       map[string]string{"e1|5": LedgerStateFailed},
			attempts:   4,
			lastErr:    "ambiguous: context deadline exceeded",
			wantStatus: StatusError,
		},
		{
			name:       "unverifiable with no rows never blind re-arms (#987)",
			rows:       map[string]string{},
			attempts:   2,
			lastErr:    lastErrUnverifiable,
			wantStatus: StatusError,
		},
		{
			name:       "unverifiable with failed row and budget stays (verify-first class)",
			rows:       map[string]string{"e1|2": LedgerStateFailed},
			attempts:   2,
			lastErr:    lastErrUnverifiable,
			wantStatus: StatusError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestService(t)
			probe, _ := probeFunc(t, tt.rows)
			s.SetLedgerProbe(probe)
			var delivered atomic.Int32
			s.SetOnDelivered(func(ws, ses string, e Entry) { delivered.Add(1) })

			seedParkedEntry(t, s, "ws-1", "ses-1", "e1", tt.attempts, tt.lastErr)

			n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
			require.NoError(t, err)
			if tt.wantStatus == "" {
				assert.Equal(t, 1, n, "one entry recovered")
				entries := readQueueEntries(t, s, "ws-1", "ses-1")
				assert.Empty(t, entries, "completed entry removed")
				assert.Equal(t, int32(1), delivered.Load(), "onDelivered fired (queue.update sent rides it)")
				return
			}
			entries := readQueueEntries(t, s, "ws-1", "ses-1")
			require.Len(t, entries, 1)
			assert.Equal(t, tt.wantStatus, entries[0].Status)
			if tt.wantAttempts > 0 {
				assert.Equal(t, tt.wantAttempts, entries[0].Attempts)
			}
			if tt.wantStatus == StatusPending {
				assert.Empty(t, entries[0].LastError, "re-arm clears the stale error")
				assert.True(t, entries[0].NextAttemptAt.IsZero() || !entries[0].NextAttemptAt.After(time.Now().Add(time.Minute)),
					"re-armed entry is due (or due soon)")
				assert.Equal(t, int32(0), delivered.Load(), "re-arm is not delivery")
			}
			if tt.wantStatus == StatusError {
				assert.Equal(t, int32(0), delivered.Load())
			}
		})
	}
}

// TestSweepParkedErrors_NoProbeIsNoOp: without a wired probe (adapter
// mode) the sweep touches nothing — legacy behavior preserved.
func TestSweepParkedErrors_NoProbeIsNoOp(t *testing.T) {
	s, _ := newTestService(t)
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
}

// TestSweepParkedErrors_ProbeErrorIsIndeterminate: an unreachable probe
// leaves the entry parked for the next sweep — never guesses — and the
// outcome counter distinguishes indeterminate from terminal stays (the
// canary must not page on a dead pod the same way as a real terminal).
func TestSweepParkedErrors_ProbeErrorIsIndeterminate(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{"e1|5": "probe-error"})
	s.SetLedgerProbe(probe)
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	before := promtestutil.ToFloat64(parkedSweepOutcomes.WithLabelValues("indeterminate"))
	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "indeterminate — nothing recovered")
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
	assert.Equal(t, float64(1), promtestutil.ToFloat64(parkedSweepOutcomes.WithLabelValues("indeterminate"))-before)
}

// TestSweepParkedErrors_DefersToDeliveryLock: the sweep mutates session
// lists under the per-session delivery lock — a session mid-delivery is
// deferred to the next pass after a bounded wait, never raced (the
// LRange→LSet window against a concurrently mutating list would
// overwrite the wrong entry).
func TestSweepParkedErrors_DefersToDeliveryLock(t *testing.T) {
	s, _ := newTestService(t)
	probe, calls := probeFunc(t, map[string]string{"e1|5": LedgerStateAdmitted})
	s.SetLedgerProbe(probe)
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	oldEvery, oldBudget := sweepLockRetryEvery, sweepLockRetryBudget
	sweepLockRetryEvery, sweepLockRetryBudget = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sweepLockRetryEvery, sweepLockRetryBudget = oldEvery, oldBudget })

	token, ok := s.acquireLock(context.Background(), "ws-1", "ses-1")
	require.True(t, ok)

	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "locked session skipped after the bounded wait")
	assert.Equal(t, int32(0), calls.Load(), "no probe issued against a mid-delivery session")
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1, "entry untouched while delivery owns the session")

	s.releaseLock(context.Background(), "ws-1", "ses-1", token)
	n, err = s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "next pass recovers it")
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
}

// TestSweepParkedErrors_WaitsOutShortLockHold: a same-tick deliverOne
// holds the session lock for bookkeeping-scale durations — the sweep
// waits it out and completes in the SAME pass instead of deferring a
// full interval (the e2e regression: skip-on-first-miss made every
// sweep lose the spawn race and never run).
func TestSweepParkedErrors_WaitsOutShortLockHold(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{"e1|5": LedgerStateAdmitted})
	s.SetLedgerProbe(probe)
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	oldEvery, oldBudget := sweepLockRetryEvery, sweepLockRetryBudget
	sweepLockRetryEvery, sweepLockRetryBudget = 5*time.Millisecond, 2*time.Second
	t.Cleanup(func() { sweepLockRetryEvery, sweepLockRetryBudget = oldEvery, oldBudget })

	token, ok := s.acquireLock(context.Background(), "ws-1", "ses-1")
	require.True(t, ok)
	go func() {
		time.Sleep(30 * time.Millisecond) // deliverOne-scale hold
		s.releaseLock(context.Background(), "ws-1", "ses-1", token)
	}()

	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "short hold waited out — recovered in this pass")
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
}

// TestSweepParkedErrors_ScopeFilter: the workspace filter leaves other
// workspaces untouched (parity with SweepWorkspaceUnverifiable).
func TestSweepParkedErrors_ScopeFilter(t *testing.T) {
	s, _ := newTestService(t)
	probe, calls := probeFunc(t, map[string]string{
		"mine|5":  LedgerStateAdmitted,
		"other|5": LedgerStateAdmitted,
	})
	s.SetLedgerProbe(probe)
	seedParkedEntry(t, s, "ws-mine", "ses-1", "mine", 5, "context deadline exceeded")
	seedParkedEntry(t, s, "ws-other", "ses-1", "other", 5, "context deadline exceeded")

	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-mine")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, readQueueEntries(t, s, "ws-mine", "ses-1"))
	other := readQueueEntries(t, s, "ws-other", "ses-1")
	require.Len(t, other, 1)
	assert.Equal(t, StatusError, other[0].Status, "other workspace untouched")
	assert.Equal(t, int32(1), calls.Load(), "only the scoped workspace's entry probed")
}

// TestSweepParkedErrors_OnlyErrorEntries: pending/verifying/parked
// (operator) entries are invisible to the sweeper.
func TestSweepParkedErrors_OnlyErrorEntries(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, nil)
	s.SetLedgerProbe(probe)
	seedQueueEntry(t, s, "ws-1", "ses-1", "p1", StatusPending)
	seedQueueEntry(t, s, "ws-1", "ses-1", "v1", StatusVerifying)
	seedQueueEntry(t, s, "ws-1", "ses-1", "k1", StatusParked)

	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Len(t, readQueueEntries(t, s, "ws-1", "ses-1"), 3, "nothing touched")
}

// TestSweepParkedErrors_MultiEntrySamePass (review defect 1): with a
// completing entry BEFORE a re-arming one in the same session, the
// completed LRem shifts indices — an ascending snapshot LSet overwrote
// the wrong entry, silently destroying an innocent neighbor (S3) and
// duplicating the re-armed one. Descending iteration must leave every
// non-error neighbor intact.
func TestSweepParkedErrors_MultiEntrySamePass(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{
		"eA|5": LedgerStateAdmitted, // completes (LRem shifts later indices)
		"eB|2": LedgerStateFailed,   // re-arms (LSet at snapshot index)
	})
	s.SetLedgerProbe(probe)
	seedParkedEntry(t, s, "ws-1", "ses-1", "eA", 5, "context deadline exceeded")
	seedParkedEntry(t, s, "ws-1", "ses-1", "eB", 2, "context deadline exceeded")
	seedQueueEntry(t, s, "ws-1", "ses-1", "eC", StatusPending) // the victim that died ascending

	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "eA completed, eB re-armed")

	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 2, "eA removed; eB and eC survive exactly once each")
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	assert.Contains(t, byID, "eB", "eB survives")
	assert.Equal(t, StatusPending, byID["eB"].Status)
	assert.Equal(t, 2, byID["eB"].Attempts)
	assert.Contains(t, byID, "eC", "the innocent pending neighbor is never destroyed (S3)")
	assert.Equal(t, StatusPending, byID["eC"].Status)
}

// TestDeliverOne_PriorPendingNeverMints (review defect 2): cycle 2 of
// an owns-admission timeout — the terminus only re-POLLED the prior
// LEDGERED row — must not increment Attempts (a phantom attempt parked
// the entry against a row the sweeper probes can never find). The
// entry stays delivering, numbering stays truthful, and the sweeper
// still resolves the row.
func TestDeliverOne_PriorPendingNeverMints(t *testing.T) {
	s, _ := newTestService(t)
	probe, probeCalls := probeFunc(t, map[string]string{"e1|5": LedgerStateLedgered})
	s.SetLedgerProbe(probe)

	// The guard-held shape cycle 1 leaves behind: delivering, Attempts
	// == the LEDGERED row's number, re-poll due.
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusDelivering, Attempts: MaxAttempts}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())

	d := func(ctx context.Context, ws, ses string, e Entry) error {
		return outboxErrPriorPendingForTest()
	}
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))

	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusDelivering, entries[0].Status, "never parks while agentd owns admission")
	assert.Equal(t, MaxAttempts, entries[0].Attempts, "no phantom attempt minted")
	assert.False(t, entries[0].NextAttemptAt.Before(time.Now().Add(time.Second)),
		"re-poll backoff-gated")
	_ = probeCalls
}

// (The guard-held entry's full recovery composition — hold → ledger
// admits → re-poll completes — is pinned by
// TestDeliverOne_GuardHeldRecoveryComposition; the sweeper itself only
// touches status:error entries, so probing it here was vacuous.)

func outboxErrPriorPendingForTest() error {
	return PriorAttemptPending(errors.New("agentd terminus: attempt 5 still LEDGER_STATE_LEDGERED (agentd owns admission)"))
}

// TestSweepParkedErrors_Metrics: outcomes counters and the last-run
// gauge move (the #1312 canary consumes them; a dead loop must be
// detectable). Delta-based: the counters are package-level and
// accumulate across the package's tests.
func TestSweepParkedErrors_Metrics(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{"e1|5": LedgerStateAdmitted})
	s.SetLedgerProbe(probe)
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	completedBefore := promtestutil.ToFloat64(parkedSweepOutcomes.WithLabelValues("completed"))
	lastRunBefore := promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper))
	_, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, float64(1), promtestutil.ToFloat64(parkedSweepOutcomes.WithLabelValues("completed"))-completedBefore)
	assert.Equal(t, lastRunBefore, promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper)),
		"direct (on-transition) sweeps are NOT loop liveness — the stamp is loop-owned (r1 finding 1)")
}

// --- The park-write guard ---------------------------------------------------

// guardHarness drives deliverOne to the park decision with a failing
// deliverer and a scripted probe: an entry with `attempts` recorded
// failures and one more failing delivery reaches the park threshold.
func guardHarness(t *testing.T, probeState string, attempts int) (*Service, *atomic.Int32) {
	t.Helper()
	s, _ := newTestService(t)
	var probe LedgerProbe
	switch probeState {
	case "nil":
		probe = nil
	default:
		var rows map[string]string
		if probeState != "" {
			rows = map[string]string{rowKeyOf("e1", uint32(attempts+1)): probeState}
		}
		probe = func(ctx context.Context, ws, ses, id string, attempt uint32) (string, error) {
			if v, ok := rows[rowKeyOf(id, attempt)]; ok {
				return v, nil
			}
			return "", nil
		}
	}
	s.SetLedgerProbe(probe)
	var delivered atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { delivered.Add(1) })
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusPending, Attempts: attempts}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())
	return s, &delivered
}

var errGuardDeliver = errors.New("context deadline exceeded")

// TestDeliverOne_ParkGuardAdmittedCompletes: at the park threshold with
// the ledger holding the row admitted, the failure branch COMPLETES the
// entry instead of parking it — the automated #1308 manual procedure.
func TestDeliverOne_ParkGuardAdmittedCompletes(t *testing.T) {
	s, delivered := guardHarness(t, LedgerStateAdmitted, MaxAttempts-1)
	d := func(ctx context.Context, ws, ses string, e Entry) error { return errGuardDeliver }
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"), "completed — removed from main")
	staged, err := s.client.LRange(context.Background(), dKey("ws-1", "ses-1"), 0, -1).Result()
	require.NoError(t, err)
	assert.Empty(t, staged, "staging drained")
	assert.Equal(t, int32(1), delivered.Load(), "onDelivered fired")
}

// TestDeliverOne_ParkGuardLedgeredStaysDelivering: a terminus timeout
// with the row still LEDGERED never parks — the entry stays delivering
// on a bounded re-poll (agentd owns admission; the prior-attempt
// resolution re-polls, never re-POSTs).
func TestDeliverOne_ParkGuardLedgeredStaysDelivering(t *testing.T) {
	s, delivered := guardHarness(t, LedgerStateLedgered, MaxAttempts-1)
	d := func(ctx context.Context, ws, ses string, e Entry) error { return errGuardDeliver }
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusDelivering, entries[0].Status, "never parked while the ledger holds the row")
	assert.Equal(t, MaxAttempts, entries[0].Attempts, "attempt numbering stays truthful for prior-attempt polls")
	assert.False(t, entries[0].NextAttemptAt.Before(time.Now().Add(time.Second)),
		"re-poll is backoff-gated, not a hot loop")
	assert.Equal(t, int32(0), delivered.Load())
}

// TestDeliverOne_ParkGuardFailedStillParks: a FAILED ledger row is a
// real terminal failure — the park proceeds (budget exhausted).
func TestDeliverOne_ParkGuardFailedStillParks(t *testing.T) {
	s, _ := guardHarness(t, LedgerStateFailed, MaxAttempts-1)
	d := func(ctx context.Context, ws, ses string, e Entry) error { return errGuardDeliver }
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
}

// TestDeliverOne_ParkGuardNoRowStillParks: no ledger row (adapter-mode
// entry or rotated ledger) parks as before — the guard only protects
// rows the ledger demonstrably holds.
func TestDeliverOne_ParkGuardNoRowStillParks(t *testing.T) {
	s, _ := guardHarness(t, "", MaxAttempts-1)
	d := func(ctx context.Context, ws, ses string, e Entry) error { return errGuardDeliver }
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
}

// TestDeliver1_ParkGuardNilProbeStillParks: no probe wired (adapter
// mode) — behavior identical to today.
func TestDeliverOne_ParkGuardNilProbeStillParks(t *testing.T) {
	s, _ := guardHarness(t, "nil", MaxAttempts-1)
	d := func(ctx context.Context, ws, ses string, e Entry) error { return errGuardDeliver }
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status)
}

// TestDeliverOne_GuardBelowThresholdUnchanged: below the park threshold
// the failure branch re-arms to pending exactly as before — the guard
// only guards the park write.
func TestDeliverOne_GuardBelowThresholdUnchanged(t *testing.T) {
	s, _ := guardHarness(t, LedgerStateAdmitted, 0)
	d := func(ctx context.Context, ws, ses string, e Entry) error { return errGuardDeliver }
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusPending, entries[0].Status, "below threshold: normal re-arm")
	assert.Equal(t, 1, entries[0].Attempts)
}

// --- The periodic sweep in Run ----------------------------------------------

// TestRun_SweepsParkedPeriodically: the Run loop drives the parked sweep
// on its interval (L9's engine) and the sweep never blocks delivery
// ticks (it runs detached from the loop).
func TestRun_SweepsParkedPeriodically(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{"e1|5": LedgerStateAdmitted})
	s.SetLedgerProbe(probe)
	var delivered atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { delivered.Add(1) })
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	old := ParkedSweepInterval
	ParkedSweepInterval = 50 * time.Millisecond
	t.Cleanup(func() { ParkedSweepInterval = old })

	started := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Run(ctx, func(ctx context.Context, ws, ses string, e Entry) error { return nil }, 10*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if delivered.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	assert.Equal(t, int32(1), delivered.Load(), "parked entry completed by the periodic sweep")
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
	assert.Greater(t, promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper)), float64(0))
	// L9 mechanism bound: parked → dispositioned within one sweep
	// interval's worth of scheduling slop (2x the configured cadence —
	// the 5-minute L9 budget implies a configured cadence well under it).
	assert.Less(t, time.Since(started), 2*ParkedSweepInterval+2*time.Second,
		"convergence latency tracks the configured sweep cadence (L9 mechanism)")
}

// TestSweeperFaultLeg_AgentdDownThenAdmitted (#1316 leg 4 shape): while
// the pod is down the probe fails — indeterminate, the entry stays
// parked, nothing is lost; once agentd is back holding the row
// admitted, the next pass completes it. S3/L9 under the crash-restart
// fault.
func TestSweeperFaultLeg_AgentdDownThenAdmitted(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{"e1|5": LedgerStateAdmitted})
	reachable := true
	s.SetLedgerProbe(func(ctx context.Context, ws, ses, id string, attempt uint32) (string, error) {
		if !reachable {
			return "", errors.New("agent unreachable")
		}
		return probe(ctx, ws, ses, id, attempt)
	})
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	reachable = false
	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "pod down: indeterminate, never guesses")
	require.Len(t, readQueueEntries(t, s, "ws-1", "ses-1"), 1, "nothing lost")

	reachable = true
	n, err = s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "pod back with the row admitted: next pass completes it")
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
}

// TestRetryVsSweepConcurrency (review r2 finding 1): the lock-free
// snapshot LSet in Retry racing the sweeper's LRem used to shift
// indices and overwrite an innocent neighbor. With both under the
// session delivery lock, ANY interleaving leaves every non-swept entry
// exactly once — the victim survives.
func TestRetryVsSweepConcurrency(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{"eA|5": LedgerStateAdmitted})
	s.SetLedgerProbe(probe)
	seedParkedEntry(t, s, "ws-1", "ses-1", "eA", 5, "context deadline exceeded")
	seedParkedEntry(t, s, "ws-1", "ses-1", "eB", 5, "context deadline exceeded")
	seedQueueEntry(t, s, "ws-1", "ses-1", "eC", StatusPending) // the victim

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	}()
	go func() {
		defer wg.Done()
		_ = s.Retry(context.Background(), "ws-1", "ses-1", "eB")
	}()
	wg.Wait()

	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	ids := map[string]int{}
	states := map[string]string{}
	for _, e := range entries {
		ids[e.ID]++
		states[e.ID] = e.Status
	}
	assert.Equal(t, 1, ids["eC"], "the innocent neighbor survives exactly once (S3)")
	assert.Equal(t, StatusPending, states["eC"])
	assert.Equal(t, 1, ids["eB"], "the retried entry survives exactly once")
	assert.NotContains(t, ids, "eA", "the ledger-admitted entry completed")
}

// TestDeliverOne_GuardHeldRecoveryComposition (review r2 missing-test
// 2): the full owns-admission lifecycle — hold (prior LEDGERED → stays
// delivering, numbering frozen) → ledger admits → the re-poll completes
// the entry through the single onDelivered seam.

// shrinkRePoll speeds up guard-held re-poll cycles for tests.
func shrinkRePoll(t *testing.T) {
	t.Helper()
	old := ownsAdmissionRePollBackoff
	ownsAdmissionRePollBackoff = 10 * time.Millisecond
	t.Cleanup(func() { ownsAdmissionRePollBackoff = old })
}

func TestDeliverOne_GuardHeldRecoveryComposition(t *testing.T) {
	s, _ := newTestService(t)
	shrinkRePoll(t)
	var delivered atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { delivered.Add(1) })
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusDelivering, Attempts: MaxAttempts}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())

	hold := func(ctx context.Context, ws, ses string, e Entry) error {
		return PriorAttemptPending(errors.New("attempt 5 still LEDGER_STATE_LEDGERED"))
	}
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", hold))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusDelivering, entries[0].Status)
	assert.Equal(t, MaxAttempts, entries[0].Attempts, "numbering frozen while agentd owns admission")

	time.Sleep(20 * time.Millisecond)                                             // let the re-poll backoff lapse
	ok := func(ctx context.Context, ws, ses string, e Entry) error { return nil } // the poll observed admission
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", ok))
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"), "admitted re-poll completes")
	assert.Equal(t, int32(1), delivered.Load())
}

// TestDeliverOne_TransientDriftNeverParksWhileAdmitted (review r2
// finding 2): resolve-error cycles during an owns-admission hold must
// never mint attempt numbers — at the transient budget the guard still
// sees the real row (numbering aligned), holds the entry delivering,
// and the admission completes it. No parked-while-admitted, ever.
func TestDeliverOne_TransientDriftNeverParksWhileAdmitted(t *testing.T) {
	s, _ := newTestService(t)
	shrinkRePoll(t)
	probe, _ := probeFunc(t, map[string]string{"e1|5": LedgerStateLedgered})
	s.SetLedgerProbe(probe)
	var delivered atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { delivered.Add(1) })

	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusDelivering,
		Attempts: MaxAttempts, TransientFails: MaxTransientFailures - 1}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())

	blip := func(ctx context.Context, ws, ses string, e Entry) error {
		return Transient(errors.New("agentd terminus: resolve: no pod IP"))
	}
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", blip))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusDelivering, entries[0].Status, "transient budget reached, row LEDGERED — held, never parked")
	assert.Equal(t, MaxAttempts, entries[0].Attempts, "no drift: numbering stays on the real row")
	assert.Equal(t, MaxTransientFailures, entries[0].TransientFails)

	// The row admits; the re-poll completes — never a false terminal.
	time.Sleep(20 * time.Millisecond) // let the re-poll backoff lapse
	ok := func(ctx context.Context, ws, ses string, e Entry) error { return nil }
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", ok))
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
	assert.Equal(t, int32(1), delivered.Load())
}

// TestTransientFailuresConvergeToParkWithoutLedger: with no ledger rows
// at all (adapter-era entry, probe finds nothing), transient failures
// still converge to an honest terminal park on their own budget.
func TestTransientFailuresConvergeToParkWithoutLedger(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, nil) // no rows anywhere
	s.SetLedgerProbe(probe)

	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusPending, TransientFails: MaxTransientFailures - 1}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())

	blip := func(ctx context.Context, ws, ses string, e Entry) error {
		return Transient(errors.New("agentd terminus: resolve: no pod IP"))
	}
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", blip))
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	require.Len(t, entries, 1)
	assert.Equal(t, StatusError, entries[0].Status, "no-attempt failures converge on their own budget")
	assert.Equal(t, 0, entries[0].Attempts, "no attempt was ever minted")
}

// TestDismissVsSweepMidPass (review r3 finding 1): Dismiss arriving
// INSIDE the sweeper's probe window must defer to the session lock —
// its lock-free value-LRem used to shift the sweeper's snapshot, whose
// descending LSet then overwrote an innocent neighbor. The probe window
// is widened deterministically (blocking probe) so the interleaving is
// forced; integrity is asserted on the final state.
func TestDismissVsSweepMidPass(t *testing.T) {
	s, _ := newTestService(t)
	probeEntered := make(chan struct{})
	releaseProbe := make(chan struct{})
	s.SetLedgerProbe(func(ctx context.Context, ws, ses, id string, attempt uint32) (string, error) {
		if id == "eA" {
			close(probeEntered)
			<-releaseProbe
			return LedgerStateAdmitted, nil
		}
		return LedgerStateFailed, nil
	})
	seedParkedEntry(t, s, "ws-1", "ses-1", "eA", 5, "context deadline exceeded")
	seedParkedEntry(t, s, "ws-1", "ses-1", "eB", 5, "context deadline exceeded")
	seedQueueEntry(t, s, "ws-1", "ses-1", "eC", StatusPending) // the victim

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		_, _ = s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	}()
	<-probeEntered // the sweeper holds the session lock, mid-pass on eA

	dismissDone := make(chan DismissResult, 1)
	go func() {
		dismissDone <- s.Dismiss(context.Background(), "ws-1", "ses-1", "eB") // defers to the lock
	}()
	close(releaseProbe)
	<-sweepDone
	require.Equal(t, DismissRemoved, <-dismissDone, "dismiss lands once the sweep releases")

	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	ids := []string{}
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	assert.ElementsMatch(t, []string{"eC"}, ids,
		"eA completed by the sweep, eB dismissed after deferring, victim intact exactly once (S3)")
}

// TestSweepParkedErrors_FailedMasksAdmittedPlusOne (review r3 finding
// 2): a FAILED row at Attempts must not hide an ADMITTED in-flight row
// at Attempts+1 — for both the ordinary and the unverifiable park
// class (the forever-stay shape).
func TestSweepParkedErrors_FailedMasksAdmittedPlusOne(t *testing.T) {
	tests := []struct {
		name    string
		lastErr string
	}{
		{"ordinary park", "context deadline exceeded"},
		{"unverifiable park", lastErrUnverifiable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestService(t)
			probe, _ := probeFunc(t, map[string]string{
				"e1|2": LedgerStateFailed,
				"e1|3": LedgerStateAdmitted, // masked admission behind the failure
			})
			s.SetLedgerProbe(probe)
			var delivered atomic.Int32
			s.SetOnDelivered(func(ws, ses string, e Entry) { delivered.Add(1) })
			seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 2, tt.lastErr)

			n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
			require.NoError(t, err)
			assert.Equal(t, 1, n, "the masked admission completes — never a forever-stay")
			assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
			assert.Equal(t, int32(1), delivered.Load())
		})
	}
}

// TestParkGuard_FailedMasksAdmittedPlusOne: the same masking shape at
// the park threshold — the guard completes instead of parking.
func TestParkGuard_FailedMasksAdmittedPlusOne(t *testing.T) {
	s, _ := newTestService(t)
	probe, _ := probeFunc(t, map[string]string{
		"e1|5": LedgerStateFailed,
		"e1|6": LedgerStateAdmitted,
	})
	s.SetLedgerProbe(probe)
	var delivered atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { delivered.Add(1) })

	// Seed an ambiguous in-flight row at 6 with the entry at Attempts=5
	// pre-increment: the failure branch increments to 5... model the
	// post-increment shape directly: driven row FAILED@5, in-flight row
	// ADMITTED@6 — the guard probes both.
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusPending, Attempts: MaxAttempts - 1}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())

	d := func(ctx context.Context, ws, ses string, e Entry) error {
		return errors.New("context deadline exceeded")
	}
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1", d))
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"), "masked admission completed at the park write")
	assert.Equal(t, int32(1), delivered.Load())
}

// TestRetryContendedIsBusyNotMissing (review r3 finding 4): a contended
// retry reports busy — never 404-shaped absence.
func TestRetryContendedIsBusyNotMissing(t *testing.T) {
	s, _ := newTestService(t)
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	oldEvery, oldBudget := sweepLockRetryEvery, sweepLockRetryBudget
	sweepLockRetryEvery, sweepLockRetryBudget = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sweepLockRetryEvery, sweepLockRetryBudget = oldEvery, oldBudget })

	token, ok := s.acquireLock(context.Background(), "ws-1", "ses-1")
	require.True(t, ok)
	defer s.releaseLock(context.Background(), "ws-1", "ses-1", token)

	assert.Equal(t, RetryBusy, s.Retry(context.Background(), "ws-1", "ses-1", "e1"),
		"contention is busy, not not-found")
}

// TestRecoverVsSweepBootOverlap (review r5 finding 1, the REAL
// reproducer): at boot, Run's Recover (head-LPush, shifts indices) can
// overlap the seed transition's parked sweep on the same replica. The
// corrupting mutation is the sweep's REARM branch — its snapshot-index
// LSet after Recover's LPush lands on the wrong entry (the r4 test
// pinned only the index-immune completed-LRem shape). eA re-arms
// (FAILED row, budget remaining); Recover runs concurrently and must
// defer to the session lock; every entry survives exactly once.
func TestRecoverVsSweepBootOverlap(t *testing.T) {
	s, _ := newTestService(t)
	probeEntered := make(chan struct{})
	releaseProbe := make(chan struct{})
	var enteredOnce sync.Once
	s.SetLedgerProbe(func(ctx context.Context, ws, ses, id string, attempt uint32) (string, error) {
		if id == "eA" && attempt == 2 {
			enteredOnce.Do(func() { close(probeEntered) })
			<-releaseProbe
			return LedgerStateFailed, nil // budget remains -> REARM (snapshot-index LSet)
		}
		return LedgerStateFailed, nil
	})

	// Crash leftovers: sX staged (Recover will requeue it verifying),
	// eA parked error at attempts=2 (sweep re-arms it in place), eC
	// pending (the victim between the LSet and the LPush).
	staged := Entry{ID: "sX", ClientMessageID: "cmid-sX", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusDelivering}
	require.NoError(t, s.client.RPush(context.Background(), dKey("ws-1", "ses-1"), string(mustMarshal(staged))).Err())
	seedParkedEntry(t, s, "ws-1", "ses-1", "eA", 2, "context deadline exceeded")
	seedQueueEntry(t, s, "ws-1", "ses-1", "eC", StatusPending)

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		_, _ = s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	}()
	<-probeEntered // the sweep holds the session lock, mid-pass on eA

	recoverDone := make(chan int, 1)
	go func() { recoverDone <- s.Recover(context.Background()) }() // defers to the lock

	// Deterministic interleaving: at a lock-free Recover (the pre-fix
	// head) the requeue push lands HERE — while the sweep's snapshot is
	// taken and its rearm LSet is still pending — so wait until the
	// staged ID is observable at the list head before releasing the
	// probe. At a locked Recover the push never comes mid-pass; the
	// bounded deadline expires and the release proceeds (serialized).
	pushedMidPass := false
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		head, err := s.client.LIndex(context.Background(), qKey("ws-1", "ses-1"), 0).Result()
		require.NoError(t, err)
		if strings.Contains(head, "\"sX\"") {
			pushedMidPass = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(releaseProbe)
	<-sweepDone
	n := <-recoverDone
	require.Equal(t, 1, n, "the staged leftover requeued")
	require.False(t, pushedMidPass, "locked Recover never mutates mid-pass (red at any head that pushes here)")

	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	ids := []string{}
	for _, e := range entries {
		ids = append(ids, e.ID)
	}
	assert.ElementsMatch(t, []string{"eA", "sX", "eC"}, ids,
		"re-armed eA, requeued sX, and the victim each survive exactly once (S3)")
	byID := map[string]Entry{}
	for _, e := range entries {
		byID[e.ID] = e
	}
	assert.Equal(t, StatusPending, byID["eA"].Status, "eA re-armed")
	assert.Equal(t, StatusVerifying, byID["sX"].Status, "sX requeued verifying")
	stagedVals, err := s.client.LRange(context.Background(), dKey("ws-1", "ses-1"), 0, -1).Result()
	require.NoError(t, err)
	assert.Empty(t, stagedVals, "staging drained")
}

// TestRecoverDefersWhenLockHeldPastBudget (r5 finding 3): the deferral
// branch's documented behavior — a lock held past the retry budget
// skips the session (staging stays, visible as delivering; the next
// boot requeues), never corrupts.
func TestRecoverDefersWhenLockHeldPastBudget(t *testing.T) {
	s, _ := newTestService(t)
	staged := Entry{ID: "sX", ClientMessageID: "cmid-sX", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusDelivering}
	require.NoError(t, s.client.RPush(context.Background(), dKey("ws-1", "ses-1"), string(mustMarshal(staged))).Err())

	oldEvery, oldBudget := sweepLockRetryEvery, sweepLockRetryBudget
	sweepLockRetryEvery, sweepLockRetryBudget = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sweepLockRetryEvery, sweepLockRetryBudget = oldEvery, oldBudget })

	token, ok := s.acquireLock(context.Background(), "ws-1", "ses-1")
	require.True(t, ok)
	n := s.Recover(context.Background())
	assert.Equal(t, 0, n, "lock held past budget: session deferred")
	s.releaseLock(context.Background(), "ws-1", "ses-1", token)

	stagedVals, err := s.client.LRange(context.Background(), dKey("ws-1", "ses-1"), 0, -1).Result()
	require.NoError(t, err)
	assert.Len(t, stagedVals, 1, "staging intact for the next boot's requeue")
	assert.Equal(t, 1, s.Recover(context.Background()), "next pass (lock free) requeues")
}

// TestDismissContendedIsBusyNotMissing (review r4 finding 1): a
// contended dismiss reports busy — never a 404-shaped absence, and the
// entry survives.
func TestDismissContendedIsBusyNotMissing(t *testing.T) {
	s, _ := newTestService(t)
	seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")

	oldEvery, oldBudget := sweepLockRetryEvery, sweepLockRetryBudget
	sweepLockRetryEvery, sweepLockRetryBudget = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { sweepLockRetryEvery, sweepLockRetryBudget = oldEvery, oldBudget })

	token, ok := s.acquireLock(context.Background(), "ws-1", "ses-1")
	require.True(t, ok)
	defer s.releaseLock(context.Background(), "ws-1", "ses-1", token)

	assert.Equal(t, DismissBusy, s.Dismiss(context.Background(), "ws-1", "ses-1", "e1"),
		"contention is busy, not not-found")
	require.Len(t, readQueueEntries(t, s, "ws-1", "ses-1"), 1, "nothing silently dropped")
}

// TestLoopLiveness_NamePinnedOnScrapeSurface (r1 finding 2): the API's
// registration of the shared family must expose the exact pkg/obs name
// with the `loop` label — a typo'd Name string would otherwise ship
// green while silently detaching the exporter (pattern:
// auth_attempts_metric_test.go).
func TestLoopLiveness_NamePinnedOnScrapeSurface(t *testing.T) {
	// Self-sufficient: materialize the child (a vec with no children is
	// invisible to Gather) instead of relying on an earlier test's stamp.
	obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper).Set(0)
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	found := false
	for _, mf := range mfs {
		if mf.GetName() != obs.LoopLivenessMetric {
			continue
		}
		found = true
		for _, m := range mf.GetMetric() {
			labels := m.GetLabel()
			require.Len(t, labels, 1, "the family carries exactly the loop label")
			assert.Equal(t, obs.LoopLivenessLabel, labels[0].GetName())
			assert.Equal(t, obs.LoopOutboxParkedSweeper, labels[0].GetValue())
		}
	}
	assert.True(t, found, "llmsafespaces_loop_last_run_timestamp_seconds is exposed on the API scrape surface")
}

// TestRun_LoopLivenessStampsInAdapterMode (r2 finding): the stamp is
// UNCONDITIONAL — the gauge measures Run-loop goroutine liveness in both
// regimes, so dead-loop detection works in adapter mode too (the series
// materializes on the first pass; an absent()-style alert may treat a
// never-materialized series as "outbox not wired").
func TestRun_LoopLivenessStampsInAdapterMode(t *testing.T) {
	s, _ := newTestService(t) // no probe wired — adapter mode
	require.Nil(t, s.ledgerProbeForTest())
	before := promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper))
	if before == 0 {
		obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper).Set(1) // materialize; baseline 1
		before = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.Run(ctx, func(ctx context.Context, ws, ses string, e Entry) error { return nil }, 10*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper)) > before {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	assert.Greater(t, promtestutil.ToFloat64(obs.LoopLastRun().WithLabelValues(obs.LoopOutboxParkedSweeper)), before,
		"adapter mode stamps too — loop liveness is regime-independent")
}

// TestVerifyOne_CompleteFiresExactlyOnceUnderLockLoss (r1): the
// verifyOne park-guard site must not double-fire when a peer completes
// the entry during our verifier/probe window (the lock-loss two-replica
// interleaving) — only the LRem winner fires.
func TestVerifyOne_CompleteFiresExactlyOnceUnderLockLoss(t *testing.T) {
	s, _ := newTestService(t)
	// Attempts is 0 — the guard probes e1|0/e1|1, NOT e1|5 (r2: the
	// e1|5 key made this test vacuous; the one-character fix makes it
	// the real red->green pin).
	probe, _ := probeFunc(t, map[string]string{"e1|0": LedgerStateAdmitted})
	s.SetLedgerProbe(probe)

	var fired atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusVerifying, VerifyAttempts: MaxVerifyAttempts - 1}
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())

	// The inconclusive verifier is where the interleaving lives: on each
	// call (the probe window), a "peer" removes the entry — the shape of
	// replica B completing while replica A's verifyOne sits between its
	// snapshot and its completion write.
	s.SetVerifier(func(ctx context.Context, ws, ses string, e Entry) Verdict {
		s.client.LRem(ctx, qKey("ws-1", "ses-1"), 1, mustMarshal(e))
		return VerdictInconclusive
	})
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1",
		func(ctx context.Context, ws, ses string, e Entry) error { return nil }))
	assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
	assert.Equal(t, int32(0), fired.Load(),
		"the peer's completion owns the hook — our no-op LRem must not fire it")
}

// TestCompleteSites_LoserSuppression (r2 missing test 2): every
// completion site fires the hook exactly once when a peer (simulated by
// a pre-removal) beats us to the entry — delete any site's n>0 gate and
// one of these rows fails.
func TestCompleteSites_LoserSuppression(t *testing.T) {
	preRemove := func(t *testing.T, s *Service) {
		t.Helper()
		vals, err := s.client.LRange(context.Background(), qKey("ws-1", "ses-1"), 0, -1).Result()
		require.NoError(t, err)
		for _, v := range vals {
			s.client.LRem(context.Background(), qKey("ws-1", "ses-1"), 1, v)
		}
	}
	t.Run("deliverOne inline success", func(t *testing.T) {
		s, _ := newTestService(t)
		var fired atomic.Int32
		s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
		seedQueueEntry(t, s, "ws-1", "ses-1", "e1", StatusPending)
		// The peer drains the STAGED copy while our deliverer runs (the
		// entry leaves main for staging before the deliverer is called).
		ok := s.DeliverOnce(context.Background(), "ws-1", "ses-1",
			func(ctx context.Context, ws, ses string, e Entry) error {
				staged, err := s.client.LRange(ctx, dKey(ws, ses), 0, -1).Result()
				require.NoError(t, err)
				for _, v := range staged {
					s.client.LRem(ctx, dKey(ws, ses), 1, v)
				}
				return nil
			})
		require.True(t, ok)
		assert.Equal(t, int32(0), fired.Load(), "loser's no-op LRem fires nothing")
	})
	t.Run("sweeper completed", func(t *testing.T) {
		s, _ := newTestService(t)
		var fired atomic.Int32
		s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
		seedParkedEntry(t, s, "ws-1", "ses-1", "e1", 5, "context deadline exceeded")
		// The peer removes the entry INSIDE the probe window: the
		// sweeper's snapshot holds it, its completion LRem removes 0
		// (the r3 shape — pre-removal emptied the queue and the SCAN
		// found no session, making the row vacuous).
		s.SetLedgerProbe(func(ctx context.Context, ws, ses, id string, attempt uint32) (string, error) {
			vals, err := s.client.LRange(ctx, qKey(ws, ses), 0, -1).Result()
			if err == nil {
				for _, v := range vals {
					s.client.LRem(ctx, qKey(ws, ses), 1, v)
				}
			}
			return LedgerStateAdmitted, nil
		})
		n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
		require.NoError(t, err)
		assert.Equal(t, 0, n)
		assert.Empty(t, readQueueEntries(t, s, "ws-1", "ses-1"))
		assert.Equal(t, int32(0), fired.Load(), "the peer's removal owns the hook")
	})
	t.Run("guard disposition completes arm", func(t *testing.T) {
		s, _ := newTestService(t)
		var fired atomic.Int32
		s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
		// Park-threshold entry: the failure branch's guard probes, the
		// probe plays the peer draining the STAGED copy, returns
		// ADMITTED → the completes arm's staging LRem removes 0.
		e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
			AcceptedAt: time.Now().UTC(), Status: StatusPending, Attempts: MaxAttempts - 1}
		raw, err := json.Marshal(e)
		require.NoError(t, err)
		require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), string(raw)).Err())
		s.SetLedgerProbe(func(ctx context.Context, ws, ses, id string, attempt uint32) (string, error) {
			vals, derr := s.client.LRange(ctx, dKey(ws, ses), 0, -1).Result()
			if derr == nil {
				for _, v := range vals {
					s.client.LRem(ctx, dKey(ws, ses), 1, v)
				}
			}
			return LedgerStateAdmitted, nil
		})
		require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1",
			func(ctx context.Context, ws, ses string, e Entry) error {
				return errors.New("context deadline exceeded")
			}))
		assert.Equal(t, int32(0), fired.Load(), "loser's staging LRem fires nothing")
		staged, err := s.client.LRange(context.Background(), dKey("ws-1", "ses-1"), 0, -1).Result()
		require.NoError(t, err)
		assert.Empty(t, staged, "staging drained (by the peer)")
	})
	t.Run("verifyOne delivered arm", func(t *testing.T) {
		s, _ := newTestService(t)
		var fired atomic.Int32
		s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
		seedQueueEntry(t, s, "ws-1", "ses-1", "e1", StatusVerifying)
		s.SetVerifier(func(ctx context.Context, ws, ses string, e Entry) Verdict {
			preRemove(t, s)
			return VerdictDelivered
		})
		require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1",
			func(ctx context.Context, ws, ses string, e Entry) error { return nil }))
		assert.Equal(t, int32(0), fired.Load())
	})
}

// TestCompleteSites_BothCopiesWindow (the storm's rare interleaving,
// main-red 2026-09-12 00:00Z): the entry legitimately sits in BOTH lists
// (the stage-out crash window) — two completers each winning their own
// single-list LRem must still produce exactly ONE hook fire. The claim
// is atomic across both lists, so the second completer removes nothing.
func TestCompleteSites_BothCopiesWindow(t *testing.T) {
	s, _ := newTestService(t)
	var fired atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusVerifying}
	raw := string(mustMarshal(e))
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), raw).Err())
	// The crash window: the copy ALSO sits in staging.
	require.NoError(t, s.client.RPush(context.Background(), dKey("ws-1", "ses-1"), raw).Err())

	// Completer A (verify path) claims atomically across both lists.
	s.SetVerifier(func(ctx context.Context, ws, ses string, e Entry) Verdict { return VerdictDelivered })
	require.True(t, s.DeliverOnce(context.Background(), "ws-1", "ses-1",
		func(ctx context.Context, ws, ses string, e Entry) error { return nil }))
	assert.Equal(t, int32(1), fired.Load(), "the winner fires once — both copies claimed atomically")

	// Completer B (sweeper shape) finds nothing to claim.
	probe, _ := probeFunc(t, map[string]string{"e1|0": LedgerStateAdmitted})
	s.SetLedgerProbe(probe)
	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, int32(1), fired.Load(), "no second fire — both lists are empty")

	main, _ := s.client.LRange(context.Background(), qKey("ws-1", "ses-1"), 0, -1).Result()
	staged, _ := s.client.LRange(context.Background(), dKey("ws-1", "ses-1"), 0, -1).Result()
	assert.Empty(t, main)
	assert.Empty(t, staged)
}

// TestClaimDelivered_ErrorArmNoFireEntryRetained (r1): a claim-time
// Redis failure produces NO fire and leaves the entry claimable — the
// at-least-once direction.
func TestClaimDelivered_ErrorArmNoFireEntryRetained(t *testing.T) {
	s, mr := newTestService(t)
	var fired atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusVerifying}
	raw := string(mustMarshal(e))
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), raw).Err())

	mr.SetError("claim-time outage")
	got := s.claimDelivered(context.Background(), "ws-1", "ses-1", raw)
	assert.EqualValues(t, 0, got, "failed claim returns 0 — no fire")
	assert.EqualValues(t, 0, fired.Load())
	mr.SetError("")

	vals, err := s.client.LRange(context.Background(), qKey("ws-1", "ses-1"), 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, vals, 1, "the entry is retained — claimable again")
	got = s.claimDelivered(context.Background(), "ws-1", "ses-1", raw)
	assert.Equal(t, int64(1), got, "the recovered claim wins and reports the copy")
}

// TestClaimDelivered_DrainsDuplicateCopies (r1 finding 1): the dual-stage
// race leaves TWO byte-identical staging copies — the count-0 LRem drains
// them all, so next boot's Recover finds no residual to re-fire.
func TestClaimDelivered_DrainsDuplicateCopies(t *testing.T) {
	s, _ := newTestService(t)
	var fired atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
	e := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusVerifying}
	raw := string(mustMarshal(e))
	// main holds one copy (the crash window), staging holds TWO (the
	// dual-stage race re-staged it).
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), raw).Err())
	require.NoError(t, s.client.RPush(context.Background(), dKey("ws-1", "ses-1"), raw).Err())
	require.NoError(t, s.client.RPush(context.Background(), dKey("ws-1", "ses-1"), raw).Err())

	got := s.claimDelivered(context.Background(), "ws-1", "ses-1", raw)
	assert.EqualValues(t, 3, got, "every byte-identical copy claimed atomically")
	assert.EqualValues(t, 0, fired.Load(), "the claim itself never fires — the winning CALLER does")

	main, _ := s.client.LRange(context.Background(), qKey("ws-1", "ses-1"), 0, -1).Result()
	staged, _ := s.client.LRange(context.Background(), dKey("ws-1", "ses-1"), 0, -1).Result()
	assert.Empty(t, main, "no residual for the next boot's Recover to re-fire")
	assert.Empty(t, staged)
}

// TestCompleteSites_SecondCompleterStaleSnapshot (r2): the REAL stale-
// snapshot window is inside the sweep itself (LRange snapshot → probes
// → claim). Completer B's sweep snapshots e1 (ADMITTED truth), blocks in
// its probe; completer A claims e1 meanwhile; B resumes and its claim on
// the snapshot value removes nothing — no second fire. Probe activity is
// asserted (non-vacuous).
func TestCompleteSites_SecondCompleterStaleSnapshot(t *testing.T) {
	s, _ := newTestService(t)
	var fired atomic.Int32
	s.SetOnDelivered(func(ws, ses string, e Entry) { fired.Add(1) })
	e1 := Entry{ID: "e1", ClientMessageID: "cmid-e1", UserID: "u1", Text: "hi",
		AcceptedAt: time.Now().UTC(), Status: StatusError, Attempts: 5, LastError: "context deadline exceeded"}
	raw := string(mustMarshal(e1))
	require.NoError(t, s.client.RPush(context.Background(), qKey("ws-1", "ses-1"), raw).Err())

	probeEntered := make(chan struct{})
	releaseProbe := make(chan struct{})
	var probeCalls atomic.Int32
	var enteredOnce sync.Once
	s.SetLedgerProbe(func(ctx context.Context, ws, ses, id string, attempt uint32) (string, error) {
		probeCalls.Add(1)
		if id == "e1" {
			enteredOnce.Do(func() { close(probeEntered) })
			<-releaseProbe
			return LedgerStateAdmitted, nil
		}
		return LedgerStateFailed, nil
	})

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		_, _ = s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	}()
	<-probeEntered // B holds e1 in its snapshot, mid-probe

	// A claims e1 (drains every copy) while B is parked in the probe.
	require.EqualValues(t, 1, s.claimDelivered(context.Background(), "ws-1", "ses-1", raw))
	close(releaseProbe)
	<-sweepDone

	assert.Greater(t, probeCalls.Load(), int32(0), "the sweep genuinely probed (non-vacuous)")
	assert.Equal(t, int32(0), fired.Load(),
		"B's claim on its stale snapshot value removed NOTHING — only A (the claimer) would fire, and direct claims don't; no site double-fired")
	entries := readQueueEntries(t, s, "ws-1", "ses-1")
	assert.Empty(t, entries, "e1 gone: A's claim drained it; B's stale LSet-free path re-added nothing")
}
