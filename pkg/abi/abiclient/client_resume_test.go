// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abiclient_test

import (
	"context"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// client_resume_test.go — the design-0069 consumer half of the replay
// ring: the client persists its fold (and the last applied seq) across
// connections and presents it as EventsRequest.since on every reconnect.
// The pins here are the resume contract's own invariants:
//
//   - a served cursor replays the missed frames through the same discard
//     rule — no gap, no duplicate application, no snapshot frame
//     (the WithResumed seam fires, WithResynced does not);
//   - a cursor below the ring's low-water mark degrades to today's
//     snapshot-first protocol (WithResynced fires; byte-identical
//     behavior for consumers that never opted into resume);
//   - a ReseedNotice inside the replayed range redials for a fresh
//     snapshot (no redial loop, no post-reseed deltas onto the stale
//     fold) while frames replayed BEFORE the notice stay applied;
//   - the property fuzz: randomized disconnect/publish/reconnect cycles
//     on ONE client keep the applied-seq multiset exactly-once and
//     recover every MESSAGE_END lost in a disconnect window.

// newSurfaceRing is newSurface with replay-ring knobs and FastCursor
// (durability across kills is P1a's suite; resume semantics don't need
// the per-event fsync).
func newSurfaceRing(t *testing.T, ringFrames, ringBytes int) (*sessionstate.Authority, *httptest.Server) {
	t.Helper()
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      &scriptedParser{},
		Store:       &countingStore{},
		Passwords:   []string{"pw"},
		FastCursor:  true,
	}
	if ringFrames > 0 {
		cfg.ReplayRingFrames = ringFrames
	}
	if ringBytes > 0 {
		cfg.ReplayRingBytes = ringBytes
	}
	a, err := sessionstate.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return a, ts
}

// appliedRecorder collects the applied-events callback's (seq, type)
// stream plus the resync/resume connection markers, mutex-guarded so
// -race sees one discipline across the fold goroutine and the test.
type appliedRecorder struct {
	mu       sync.Mutex
	applied  []uint64
	types    map[uint64]abiv1.EventType
	resynced int
	resumed  int
}

func newAppliedRecorder() *appliedRecorder {
	return &appliedRecorder{types: map[uint64]abiv1.EventType{}}
}

func (r *appliedRecorder) opts() []abiclient.StreamOption {
	return []abiclient.StreamOption{
		abiclient.WithAppliedEvents(func(evt *abiv1.Event, seq uint64) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.applied = append(r.applied, seq)
			r.types[seq] = evt.GetType()
		}),
		abiclient.WithResynced(func() {
			r.mu.Lock()
			r.resynced++
			r.mu.Unlock()
		}),
		abiclient.WithResumed(func() {
			r.mu.Lock()
			r.resumed++
			r.mu.Unlock()
		}),
	}
}

func (r *appliedRecorder) snapshot() (applied []uint64, resynced, resumed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint64(nil), r.applied...), r.resynced, r.resumed
}

func (r *appliedRecorder) messageEnds() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []uint64
	for _, seq := range r.applied {
		if r.types[seq] == abiv1.EventType_EVENT_TYPE_MESSAGE_END {
			out = append(out, seq)
		}
	}
	return out
}

// duplicates returns the seqs appearing more than once (empty = the
// exactly-once property held).
func duplicates(seqs []uint64) []uint64 {
	seen := map[uint64]int{}
	var dup []uint64
	for _, s := range seqs {
		seen[s]++
		if seen[s] == 2 {
			dup = append(dup, s)
		}
	}
	return dup
}

func messageEndEvt(sid, mid string, tokens int64) *abiv1.Event {
	return &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_MESSAGE_END,
		SessionId: sid,
		MessageId: mid,
		Message: &abiv1.Message{
			Id: mid, SessionId: sid, Type: abiv1.MessageType_MESSAGE_TYPE_ASSISTANT,
			Model: &abiv1.ModelRef{Id: "glm-5.3", Provider: "opencode"},
			Cost:  &abiv1.Cost{InputTokens: tokens, OutputTokens: tokens / 4},
		},
	}
}

// streamRun is one live Stream call under the test's control. stop is
// SYNCHRONOUS: the fold goroutine has fully returned before it does —
// the client's one-goroutine contract holds across back-to-back runs.
type streamRun struct {
	cancel func()
	done   chan struct{}
}

func startStream(t *testing.T, c *abiclient.Client, rec *appliedRecorder) *streamRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Stream(ctx, func(*abiclient.SessionState) {}, rec.opts()...)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	})
	return &streamRun{cancel: cancel, done: done}
}

func (r *streamRun) stop() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
	}
}

// waitApplied polls until the target holds over the recorder's applied
// seqs (outcome-deterministic; the deadline only bites on breakage).
func waitApplied(t *testing.T, rec *appliedRecorder, target func(applied []uint64) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		applied, _, _ := rec.snapshot()
		if target(applied) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: applied %v never satisfied the target", what, applied)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitSeeded waits for the connection's OPENING marker: the snapshot
// (fresh fold or fallback) or the first replayed frame (resume). Only
// meaningful BEFORE publishing on a fresh client's first connection —
// where the snapshot stamp race lives.
func waitSeeded(t *testing.T, rec *appliedRecorder, what string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, resynced, resumed := rec.snapshot()
		if resynced+resumed > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: connection never opened (no snapshot, no resume)", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStreamResumeReplaysGapExactlyOnce: the core contract. A client
// folds 1..S, disconnects; events S+1.. (including a billable
// MESSAGE_END) publish while it is gone; a NEW Stream call on the SAME
// client resumes from the ring — every missed event is applied exactly
// once, no snapshot lands (resumed fires, resynced does not).
func TestStreamResumeReplaysGapExactlyOnce(t *testing.T) {
	a, ts := newSurfaceRing(t, 0, 0)
	c := clientFor(ts)

	// Connection 1: snapshot@0, then fold 1..2 live.
	rec1 := newAppliedRecorder()
	run1 := startStream(t, c, rec1)
	waitSeeded(t, rec1, "connection 1 never snapshotted")
	a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY)) // seq 1
	a.IngestForTest(inputEvt("q1", "s1"))                                     // seq 2
	waitApplied(t, rec1, func(ap []uint64) bool { return len(ap) == 2 }, "connection 1 never folded the live prefix")
	_, r1, m1 := rec1.snapshot()
	require.Equal(t, 1, r1, "the initial connect seeds once (snapshot)")
	require.Zero(t, m1, "the initial connect does not resume")
	run1.stop()

	// The loss window: publish while disconnected.
	a.IngestForTest(messageEndEvt("s1", "m1", 100))                           // seq 3 — the billable frame
	a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE)) // seq 4

	// Connection 2 (same client): resume from seq 2, apply 3..4 exactly once.
	rec2 := newAppliedRecorder()
	run2 := startStream(t, c, rec2)
	defer run2.stop()
	waitApplied(t, rec2, func(ap []uint64) bool { return len(ap) == 2 }, "the resumed connection never replayed the gap")
	applied, resynced, resumed := rec2.snapshot()
	require.Equal(t, []uint64{3, 4}, applied, "the replay must deliver exactly the missed seqs, in order")
	require.Empty(t, duplicates(applied), "no duplicate application across the resume")
	require.Equal(t, 1, resumed, "the served cursor resumes (WithResumed fires once)")
	require.Zero(t, resynced, "a served resume must NOT re-snapshot")

	// The ring confirms the cursor was servable.
	low, high := a.ReplayWindow()
	require.Equal(t, uint64(4), high)
	require.LessOrEqual(t, low, uint64(2), "the ring must still cover the presented cursor")
}

// TestStreamResumeFallbackBelowLowWater: a cursor the ring cannot serve
// (events published past a 2-frame ring while disconnected) degrades to
// today's protocol — snapshot frame first (resynced fires, resumed does
// not), the stamp re-seeds the fold, live frames continue past the
// stamp. Consumers that never opted into resume see byte-identical
// behavior.
func TestStreamResumeFallbackBelowLowWater(t *testing.T) {
	a, ts := newSurfaceRing(t, 2, 0) // tiny ring: the cursor will fall below low-water
	c := clientFor(ts)

	rec1 := newAppliedRecorder()
	run1 := startStream(t, c, rec1)
	waitSeeded(t, rec1, "connection 1 never snapshotted")
	a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY)) // seq 1
	waitApplied(t, rec1, func(ap []uint64) bool { return len(ap) == 1 }, "connection 1 never folded")
	run1.stop()

	// Publish past the 2-frame ring: the cursor (seq 1) falls below
	// low-water (seq 3). The missed MESSAGE_END is NOT replayed — the
	// fallback snapshot carries state, not cost (the honest degrade).
	a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE)) // seq 2
	a.IngestForTest(inputEvt("q1", "s1"))                                     // seq 3
	a.IngestForTest(messageEndEvt("s1", "m1", 100))                           // seq 4 (retained, but subsumed by the stamp)

	rec2 := newAppliedRecorder()
	run2 := startStream(t, c, rec2)
	defer run2.stop()
	waitSeeded(t, rec2, "the fallback connection never opened")
	_, resynced, resumed := rec2.snapshot()
	require.Equal(t, 1, resynced, "below low-water the connection must degrade to snapshot-first")
	require.Zero(t, resumed, "the fallback must not report a resume")
	// Snapshot stamp = 4: the subsumed 2..4 never fire applied-events —
	// today's semantics, unchanged.
	applied, _, _ := rec2.snapshot()
	require.Empty(t, applied, "the fallback snapshot subsumes the missed events (no applied-events for seqs ≤ stamp)")

	// Live frames continue from stamp+1.
	a.IngestForTest(statusEvt("s2", abiv1.SessionStatus_SESSION_STATUS_BUSY)) // seq 5
	waitApplied(t, rec2, func(ap []uint64) bool { return len(ap) == 1 }, "post-snapshot live event never applied")
	applied, _, _ = rec2.snapshot()
	require.Equal(t, []uint64{5}, applied, "post-snapshot live events apply from stamp+1")
}

// TestStreamReseedInsideReplayRedialsSnapshot: a resume whose replay
// range contains a ReseedNotice delivers the notice in seq position:
// frames before it stay applied (the billing window the resume closes),
// the notice triggers the mandatory re-snapshot (resynced fires), and
// the redial does NOT loop — the fresh snapshot's stamp moves the
// cursor past the notice.
func TestStreamReseedInsideReplayRedialsSnapshot(t *testing.T) {
	store := &countingStore{seed: map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	}}
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(), Parser: &scriptedParser{}, Store: store,
		Passwords: []string{"pw"}, FastCursor: true,
	}
	a, err := sessionstate.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	c := clientFor(ts)

	// Connection 1 folds the pre-reseed events 1..2 (incl. a billable).
	rec1 := newAppliedRecorder()
	run1 := startStream(t, c, rec1)
	waitSeeded(t, rec1, "connection 1 never snapshotted")
	a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY)) // seq 1
	a.IngestForTest(messageEndEvt("s1", "m1", 100))                           // seq 2 — billable, pre-reseed
	waitApplied(t, rec1, func(ap []uint64) bool { return len(ap) == 2 }, "connection 1 never folded the pre-reseed events")
	require.Equal(t, []uint64{2}, rec1.messageEnds(), "the pre-reseed MESSAGE_END was applied (billed) live")
	run1.stop()

	// While disconnected: reseed (notice consumes seq 3) then a
	// post-reseed event (seq 4, subsumed by the redial's snapshot).
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))
	a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE))

	// Connection 2: resume from seq 2 → replay opens with the notice →
	// mandatory re-snapshot redial (no since) → live continues.
	rec2 := newAppliedRecorder()
	run2 := startStream(t, c, rec2)
	defer run2.stop()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, resynced, resumed := rec2.snapshot(); resumed == 1 && resynced >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, resynced, resumed := rec2.snapshot()
	require.Equal(t, 1, resumed, "the cursor (seq 2) is served — the replay opens resumed")
	require.GreaterOrEqual(t, resynced, 1, "the replayed ReseedNotice must force the re-snapshot redial")
	require.Empty(t, rec2.appliedSnapshot(), "no post-notice frame applied onto the stale fold (seq 4 arrives via the snapshot)")

	// The redial settles: the fold is current, a live event applies once.
	a.IngestForTest(inputEvt("q9", "s1")) // seq 5
	waitApplied(t, rec2, func(ap []uint64) bool { return len(ap) == 1 }, "post-reseed live event never applied after the redial")
	applied, _, _ := rec2.snapshot()
	require.Equal(t, []uint64{5}, applied)
}

// appliedSnapshot is a nil-safe read for tests that assert emptiness.
func (r *appliedRecorder) appliedSnapshot() []uint64 {
	applied, _, _ := r.snapshot()
	return applied
}

// TestStreamResumePropertyFuzz — the discard-rule property fuzz
// (client_test.go:TestDiscardRulePropertyFuzz) extended to the resume
// path: randomized disconnect/publish/reconnect cycles on ONE client
// against the real authority. Invariants across the WHOLE run:
//
//   - the applied-seq multiset contains no duplicate (the discard rule
//   - since make any redelivery inert);
//   - every published event is applied exactly once (count equality —
//     no gap survives the cycles);
//   - every MESSAGE_END published inside a disconnect window — the
//     billing frames — is recovered (applied at least once; combined
//     with no-duplicates, exactly once).
func TestStreamResumePropertyFuzz(t *testing.T) {
	for iter := 0; iter < 25; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			t.Parallel()
			rng := rand.New(rand.NewSource(int64(iter) + 1900)) // per-iteration source (parallel-safe)
			a, ts := newSurfaceRing(t, 0, 0)
			c := clientFor(ts)

			type cycleRec struct {
				rec *appliedRecorder
			}
			var recs []*cycleRec
			published := uint64(0)
			var wantMessageEnds []uint64

			nextSeq := func() uint64 { return a.State().Seq + 1 }

			cycles := 1 + rng.Intn(4)
			for i := 0; i <= cycles; i++ {
				cr := &cycleRec{rec: newAppliedRecorder()}
				recs = append(recs, cr)
				run := startStream(t, c, cr.rec)
				waitSeeded(t, cr.rec, fmt.Sprintf("cycle %d never opened", i))

				// Live traffic during the connection (the first cycle's
				// snapshot landed; later cycles resume from the cursor —
				// either way, published events are post-opening).
				live := 1 + rng.Intn(3)
				for j := 0; j < live; j++ {
					a.IngestForTest(statusEvt(fmt.Sprintf("s%d", rng.Intn(2)), abiv1.SessionStatus_SESSION_STATUS_BUSY))
					published++
				}
				target := a.State().Seq
				waitApplied(t, cr.rec, func(ap []uint64) bool {
					return len(ap) > 0 && ap[len(ap)-1] >= target
				}, fmt.Sprintf("cycle %d never folded its live traffic", i))
				run.stop()

				// The loss window: publish while disconnected (the last
				// cycle's window exists only to prove stop() leaks
				// nothing — the loop's next iteration is the final drain).
				gap := 1 + rng.Intn(4)
				for j := 0; j < gap && i < cycles; j++ {
					if rng.Intn(2) == 0 {
						want := nextSeq()
						a.IngestForTest(messageEndEvt("s0", fmt.Sprintf("m%d", want), int64(10+want)))
						wantMessageEnds = append(wantMessageEnds, want)
					} else {
						a.IngestForTest(inputEvt(fmt.Sprintf("q%d", nextSeq()), "s0"))
					}
					published++
				}
			}

			// Aggregate across every recorder (each cycle's connection).
			var allApplied, allMessageEnds []uint64
			var resynced, resumed int
			for _, cr := range recs {
				ap, r1, m1 := cr.rec.snapshot()
				allApplied = append(allApplied, ap...)
				allMessageEnds = append(allMessageEnds, cr.rec.messageEnds()...)
				resynced += r1
				resumed += m1
			}
			require.Empty(t, duplicates(allApplied), "iter %d: duplicate application across resume cycles", iter)
			require.Equal(t, published, uint64(len(allApplied)),
				"iter %d: every published event applied exactly once (got %d of %d)", iter, len(allApplied), published)
			require.ElementsMatch(t, wantMessageEnds, allMessageEnds,
				"iter %d: every MESSAGE_END lost in a disconnect window was recovered by the replay", iter)
			require.Equal(t, 1, resynced, "iter %d: exactly one snapshot connection (the initial); every reconnect resumed", iter)
			require.Equal(t, cycles, resumed, "iter %d: every reconnect resumed (got %d resynced / %d resumed)", iter, resynced, resumed)
		})
	}
}
