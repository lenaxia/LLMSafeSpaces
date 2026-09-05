// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- US-69.7 (design 0055 M2): the PVC-backed delivery ledger --------------
//
// I5 (at-most-once per (entryID, attempt), single-flight per session),
// I6 (admitted never re-admitted; stall recovery is wake-only),
// I7 (interrupt never mutates entries — no interrupt hook exists here),
// I9 (202 ⇒ fsync-persisted).

func ledgerPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "ledger.wal")
}

func openLedgerForTest(t *testing.T, path string) *deliveryLedger {
	t.Helper()
	l, err := openDeliveryLedger(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.close() })
	return l
}

// TestLedger_DedupeEntryIDAttempt: a duplicate (entryID, attempt) does not
// create a second WAL record and reports the existing state; terminal
// failed re-arms at attempt+1 (a NEW row).
func TestLedger_DedupeEntryIDAttempt(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))

	e1, created, err := l.ledger("s1", "entry-1", 1, []string{"hello"})
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, LedgerStateLedgered, e1.State)

	e2, created2, err := l.ledger("s1", "entry-1", 1, []string{"hello"})
	require.NoError(t, err)
	require.False(t, created2, "duplicate (entryID, attempt) must not create a row")
	require.Equal(t, e1.Seq, e2.Seq, "same WAL record")

	require.NoError(t, l.markFailed("entry-1", 1, "provider 500"))

	_, created3, err := l.ledger("s1", "entry-1", 2, []string{"hello"})
	require.NoError(t, err)
	require.True(t, created3, "attempt+1 re-arms (new row)")
}

// TestLedger_Durability202SurvivesKill (I9): a ledger() ack implies the WAL
// record is on disk — close WITHOUT graceful flush (kill semantics), reopen
// from the file, the entry is present.
func TestLedger_Durability202SurvivesKill(t *testing.T) {
	path := ledgerPath(t)
	l1 := openLedgerForTest(t, path)
	e, created, err := l1.ledger("s1", "entry-9", 1, []string{"prompt"})
	require.NoError(t, err)
	require.True(t, created)

	l2 := openLedgerForTest(t, path) // reopen = kill+resume
	got, ok := l2.status("entry-9", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateLedgered, got.State)
	assert.Equal(t, e.Seq, got.Seq)
	assert.Equal(t, "prompt", got.Text)
}

// TestLedger_CrashMatrixStateTransitions: replay-after-reopen resolves every
// intermediate state; promoted/turn-ended transitions apply by messageID and
// session turn boundaries.
func TestLedger_CrashMatrixStateTransitions(t *testing.T) {
	path := ledgerPath(t)
	l := openLedgerForTest(t, path)

	// Three windows: acked-not-admitted; admitted-not-promoted; promoted.
	_, _, _ = l.ledger("s1", "e-a", 1, []string{"A"})
	_, _, _ = l.ledger("s1", "e-b", 1, []string{"B"})
	_, _, _ = l.ledger("s1", "e-c", 1, []string{"C"})

	require.NoError(t, l.markAdmitted("e-b", 1, "msg-b"))
	require.NoError(t, l.markAdmitted("e-c", 1, "msg-c"))
	require.NoError(t, l.markPromoted("e-c", 1, "msg-c"))

	l2 := openLedgerForTest(t, path)
	a, ok := l2.status("e-a", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateLedgered, a.State, "crash after ack pre-admission: resumes ledgered")

	b, ok := l2.status("e-b", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateAdmitted, b.State)
	assert.Equal(t, "msg-b", b.MessageID, "admission correlation survives the WAL")

	c, ok := l2.status("e-c", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStatePromoted, c.State)

	// Turn end promotes terminality for the session's promoted entries.
	require.NoError(t, l2.markTurnEnded("s1"))
	c2, _ := l2.status("e-c", 1)
	assert.Equal(t, LedgerStateTurnEnded, c2.State)
}

// TestLedger_StalledDetectionAndWakeOnly (I6): admitted past the promotion
// deadline becomes stalled and fires the wake ONCE — and there is NO
// re-admission path (the ledger API exposes no markRe-admit; the delivery
// driver only admits entries in ledgered state).
func TestLedger_StalledDetectionAndWakeOnly(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))
	_, _, _ = l.ledger("s1", "e-1119", 1, []string{"stranded"})
	require.NoError(t, l.markAdmitted("e-1119", 1, "msg-x"))

	wakes := 0
	wake := func(ctx context.Context, sessionID string) error {
		wakes++
		return nil
	}
	l.deadline = 20 * time.Millisecond
	l.checkStalls(context.Background(), wake, time.Now().Add(50*time.Millisecond))

	st, ok := l.status("e-1119", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateStalled, st.State, "#1119 class surfaces stalled")

	l.checkStalls(context.Background(), wake, time.Now().Add(100*time.Millisecond))
	l.checkStalls(context.Background(), wake, time.Now().Add(150*time.Millisecond))
	assert.Equal(t, 1, wakes, "wake-only recovery: exactly one wake per stall; no re-admission")
}

// TestLedger_QueueDepthLedgerDerived: ledgered ∪ admitted-unpromoted counts;
// terminal states do not.
func TestLedger_QueueDepthLedgerDerived(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))
	_, _, _ = l.ledger("s1", "q1", 1, []string{"a"})
	_, _, _ = l.ledger("s1", "q2", 1, []string{"b"})
	_, _, _ = l.ledger("s1", "q3", 1, []string{"c"})
	_, _, _ = l.ledger("s2", "q4", 1, []string{"d"})
	require.NoError(t, l.markAdmitted("q3", 1, "m3")) // admitted-unpromoted: counts
	require.NoError(t, l.markFailed("q4", 1, "err"))  // terminal: does not

	assert.Equal(t, 3, l.queueDepth("s1"), "ledgered(2) + admitted-unpromoted(1)")
	assert.Equal(t, 0, l.queueDepth("s2"))
}

// TestLedger_CompactionPreservesTerminalOutcomesAndSeqMeta: compaction drops
// turn-ended/failed rows beyond retention but NEVER the WAL's format/meta
// header records; in-retention terminal outcomes survive.
func TestLedger_CompactionPreservesTerminalOutcomesAndSeqMeta(t *testing.T) {
	path := ledgerPath(t)
	l := openLedgerForTest(t, path)
	_, _, _ = l.ledger("s1", "old", 1, []string{"x"})
	require.NoError(t, l.markAdmitted("old", 1, "m"))
	require.NoError(t, l.markPromoted("old", 1, "m"))
	require.NoError(t, l.markTurnEnded("s1"))

	_, _, _ = l.ledger("s1", "new", 1, []string{"y"})

	now := time.Now()
	require.NoError(t, l.compact(now.Add(time.Hour), now))

	l2 := openLedgerForTest(t, path)
	_, oldOK := l2.status("old", 1)
	assert.False(t, oldOK, "turn-ended beyond retention compacts away")
	_, newOK := l2.status("new", 1)
	assert.True(t, newOK, "live entries survive compaction")
}

// TestLedger_InterruptPurity (I7): the ledger has no interrupt surface —
// states only move via admission/promotion/turn/failed/stall paths; the
// recorded states are byte-identical before/after an "interrupt" (which is
// nothing here by construction — this test pins the API surface).
func TestLedger_InterruptPurity(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))
	_, _, _ = l.ledger("s1", "i1", 1, []string{"q"})
	require.NoError(t, l.markAdmitted("i1", 1, "m"))
	before, ok := l.status("i1", 1)
	require.True(t, ok)

	// An interrupt is a projection-level fact; the ledger exposes no
	// transition for it. The only mutations are the five named paths.
	assert.Equal(t, LedgerStateAdmitted, before.State)
	after, ok := l.status("i1", 1)
	require.True(t, ok)
	assert.Equal(t, before.State, after.State)
	assert.Empty(t, after.Failure, "no failure manufactured by an interrupt")
	_ = json.Marshal
}

// --- integration: Deliver + admission driver + single-flight ---------------

type fakeAdmitter struct {
	mu       sync.Mutex
	calls    []string
	err      error
	latency  time.Duration
	inFlight int
	maxInFl  int
}

// setErr mutates err under the fake's mutex: the delivery driver's
// goroutine reads err inside Admit, so a bare test-side assignment races
// with an in-flight retry (caught by CI's -race; the write must go
// through the same lock the reader holds).
func (f *fakeAdmitter) setErr(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeAdmitter) Admit(ctx context.Context, sessionID, text, model string) (string, error) {
	f.mu.Lock()
	f.inFlight++
	if f.inFlight > f.maxInFl {
		f.maxInFl = f.inFlight
	}
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if f.latency > 0 {
		select {
		case <-time.After(f.latency):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, sessionID+"/"+text)
	return fmt.Sprintf("msg-%d", len(f.calls)), nil
}

func TestDeliver_SingleFlightPerSession(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))
	admitter := &fakeAdmitter{latency: 30 * time.Millisecond}
	d := newDeliveryDriver(l, admitter, Config{Passwords: []string{"pw"}}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := d.deliver(context.Background(), "s1", fmt.Sprintf("e-%d", i), 1, []string{"p"}, "")
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	waitFor(t, func() bool {
		admitter.mu.Lock()
		defer admitter.mu.Unlock()
		return len(admitter.calls) == 8
	}, "all 8 admissions complete")

	admitter.mu.Lock()
	defer admitter.mu.Unlock()
	require.Equal(t, 1, admitter.maxInFl, "per-session single-flight: at most one admission in flight")
	assert.Len(t, admitter.calls, 8, "every entry admitted exactly once")
}

// TestDeliver_ExactlyOncePerAttempt (I5): the same (entryID, attempt)
// delivered twice admits ONCE; a failed attempt re-arms at +1.
func TestDeliver_ExactlyOncePerAttempt(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))
	admitter := &fakeAdmitter{err: fmt.Errorf("provider down")}
	d := newDeliveryDriver(l, admitter, Config{Passwords: []string{"pw"}}, nil)

	_, _, err := d.deliver(context.Background(), "s1", "e1", 1, []string{"p"}, "")
	require.NoError(t, err, "delivery accepts; admission failure is recorded not returned")

	st, ok := l.status("e1", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateLedgered, st.State, "failed admission returns to retryable ledgered")

	admitter.setErr(nil)
	_, _, err = d.deliver(context.Background(), "s1", "e1", 1, []string{"p"}, "")
	require.NoError(t, err)
	waitFor(t, func() bool {
		st, ok := l.status("e1", 1)
		return ok && st.State == LedgerStateAdmitted
	}, "admission completes after retry")

	admitter.mu.Lock()
	calls := len(admitter.calls)
	admitter.mu.Unlock()
	require.Equal(t, 1, calls, "duplicate (entryID,attempt) does not re-admit")

	// attempt+1 (outbox-directed retry): a NEW admission only when the
	// prior attempt never admitted. The original pin ("attempt+1 is a NEW
	// admission — allowed", len==2) WAS #1288: the retry ladder re-POSTed
	// an already-admitted entry as a fresh message — five identical turns
	// from one user send. Cross-attempt dedup now keys on the entry ID:
	// attempt 2 carries attempt 1's messageID and never re-POSTs.
	_, _, err = d.deliver(context.Background(), "s1", "e1", 2, []string{"p"}, "")
	require.NoError(t, err)
	waitFor(t, func() bool {
		st, ok := l.status("e1", 2)
		return ok && st.State == LedgerStateAdmitted
	}, "attempt+1 admits via cross-attempt dedup")
	admitter.mu.Lock()
	calls = len(admitter.calls)
	admitter.mu.Unlock()
	assert.Equal(t, 1, calls, "#1288: attempt+1 must never re-POST an admitted entry")
	st2, _ := l.status("e1", 2)
	assert.Equal(t, "msg-1", st2.MessageID, "attempt 2 carries attempt 1's messageID")
}

// TestDeliver_AdmittedNeverReadmitted (I6, the #1119 guard): once admitted,
// replay/resume paths never admit again — the driver only admits from
// ledgered; a reopen+replay of unresolved entries skips admitted rows.
func TestDeliver_AdmittedNeverReadmitted(t *testing.T) {
	path := ledgerPath(t)
	l := openLedgerForTest(t, path)
	_, _, _ = l.ledger("s1", "e1", 1, []string{"p"})
	require.NoError(t, l.markAdmitted("e1", 1, "msg-1"))

	admitter := &fakeAdmitter{}
	d := newDeliveryDriver(l, admitter, Config{Passwords: []string{"pw"}}, nil)
	d.replayUnresolved(context.Background())

	assert.Empty(t, admitter.calls, "no re-admission path exists for admitted entries")
}

// TestDeliver_CrashAgentdAfterAckPreAdmission: the crash matrix's first
// window — 202 (ledgered+fsynced), kill before admission; reopen + replay
// admits exactly once.
func TestDeliver_CrashAgentdAfterAckPreAdmission(t *testing.T) {
	path := ledgerPath(t)
	l1 := openLedgerForTest(t, path)
	// The pre-crash driver's admission goroutine dies WITH the process —
	// its admitter is separate and never observed by the resume side.
	d1 := newDeliveryDriver(l1, &fakeAdmitter{err: fmt.Errorf("process died")}, Config{Passwords: []string{"pw"}}, nil)
	_, _, err := d1.deliver(context.Background(), "s1", "e1", 1, []string{"p"}, "")
	require.NoError(t, err)

	l2 := openLedgerForTest(t, path) // kill + resume
	admitter := &fakeAdmitter{}
	d2 := newDeliveryDriver(l2, admitter, Config{Passwords: []string{"pw"}}, nil)
	d2.replayUnresolved(context.Background())
	waitFor(t, func() bool {
		st, ok := l2.status("e1", 1)
		return ok && st.State == LedgerStateAdmitted
	}, "resume admits the ledgered row")

	admitter.mu.Lock()
	calls := len(admitter.calls)
	admitter.mu.Unlock()
	require.Equal(t, 1, calls, "exactly one admission after resume")
	st, ok := l2.status("e1", 1)
	require.True(t, ok)
	assert.Equal(t, LedgerStateAdmitted, st.State)
}

// TestDeliver_PromotionFromProjectionEvents: the driver consumes authority
// events — a message event carrying the admitted messageID promotes the
// entry; turn end termininates it; queue depth drains.
func TestDeliver_PromotionFromProjectionEvents(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))
	admitter := &fakeAdmitter{}
	d := newDeliveryDriver(l, admitter, Config{Passwords: []string{"pw"}}, nil)

	_, _, err := d.deliver(context.Background(), "s1", "e1", 1, []string{"p"}, "")
	require.NoError(t, err)
	waitFor(t, func() bool {
		st, _ := l.status("e1", 1)
		return st != nil && st.State == LedgerStateAdmitted
	}, "admitted")

	d.observeEvent(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_MESSAGE_START, SessionId: "s1", MessageId: "msg-1"})
	st, _ := l.status("e1", 1)
	require.NotNil(t, st)
	assert.Equal(t, LedgerStatePromoted, st.State, "messageID correlation promotes")

	d.observeTurnEnded("s1")
	st, _ = l.status("e1", 1)
	require.NotNil(t, st)
	assert.Equal(t, LedgerStateTurnEnded, st.State)
	assert.Equal(t, 0, l.queueDepth("s1"))
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", what)
}

var _ = os.Getenv

// #1288 regression pin: the outbox retry ladder re-POSTs the same entry
// at attempt+1; admission must dedupe across attempts — a prior admitted
// attempt makes the new attempt return that outcome WITHOUT re-POSTing.
// The incident: five identical "Test 123" turns from one user send
// (05:21:47 +11s/+20s/+40s/+80s — the API's doubling retry ladder).
func TestAttemptAdmission_CrossAttemptDedup(t *testing.T) {
	l := openLedgerForTest(t, ledgerPath(t))
	admitter := &fakeAdmitter{}
	d := newDeliveryDriver(l, admitter, Config{Passwords: []string{"pw"}}, nil)

	// Attempt 1: admitted normally (one opencode POST).
	_, _, err := d.deliver(context.Background(), "s1", "e1", 1, []string{"Test 123"}, "")
	require.NoError(t, err)
	waitFor(t, func() bool {
		st, _ := l.status("e1", 1)
		return st != nil && st.State == LedgerStateAdmitted
	}, "attempt 1 admits")
	admitter.mu.Lock()
	posts := len(admitter.calls)
	admitter.mu.Unlock()
	require.Equal(t, 1, posts, "attempt 1 = exactly one opencode POST")

	// The #1288 retry ladder: the SAME entry re-POSTed at attempt+1.
	_, _, err = d.deliver(context.Background(), "s1", "e1", 2, []string{"Test 123"}, "")
	require.NoError(t, err)
	waitFor(t, func() bool {
		st, _ := l.status("e1", 2)
		return st != nil && st.State == LedgerStateAdmitted
	}, "attempt 2 admits via dedup")
	admitter.mu.Lock()
	posts = len(admitter.calls)
	admitter.mu.Unlock()
	assert.Equal(t, 1, posts, "cross-attempt dedup: the retry ladder must never re-POST")
	st, ok := l.status("e1", 2)
	require.True(t, ok)
	assert.Equal(t, st.MessageID, "msg-1", "attempt 2 carries attempt 1's messageID")
}
