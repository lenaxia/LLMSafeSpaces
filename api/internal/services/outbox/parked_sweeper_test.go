// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

	// The sweeper still sees the row: LEDGERED at attempt 5 → stays (no
	// park, no loss), and once agentd admits it, the same probe
	// dispositions it — never stranded by a phantom attempt number.
	before := probeCalls.Load()
	n, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, 0, n, "LEDGERED stays this pass")
	assert.GreaterOrEqual(t, probeCalls.Load(), before, "the row remains visible to the sweeper")
}

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
	lastRunBefore := promtestutil.ToFloat64(parkedSweepLastRun)
	_, err := s.SweepWorkspaceParkedErrors(context.Background(), "ws-1")
	require.NoError(t, err)
	assert.Equal(t, float64(1), promtestutil.ToFloat64(parkedSweepOutcomes.WithLabelValues("completed"))-completedBefore)
	assert.Greater(t, promtestutil.ToFloat64(parkedSweepLastRun), lastRunBefore)
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
	assert.Greater(t, promtestutil.ToFloat64(parkedSweepLastRun), float64(0))
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
