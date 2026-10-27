// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package sessionstate_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// replayConfig is an authority with the fsync'd cursor disabled (the
// scenario-harness FastCursor precedent — the kill-9 suite covers
// durability) and injectable ring budgets.
func replayConfig(t *testing.T, ringFrames, ringBytes int) sessionstate.Config {
	t.Helper()
	cfg := testConfig(t, t.TempDir(), &fixtureParser{}, &mapStore{})
	cfg.FastCursor = true
	if ringFrames > 0 {
		cfg.ReplayRingFrames = ringFrames
	}
	if ringBytes > 0 {
		cfg.ReplayRingBytes = ringBytes
	}
	return cfg
}

// deltaEvent builds a contract event of a chosen serialized size
// (PART_DELTA text — the realistic large-frame class).
func deltaEvent(sid string, size int) *abiv1.Event {
	return &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_PART_DELTA,
		SessionId: sid,
		PartId:    "p1",
		Delta:     string(make([]byte, size)),
	}
}

// seqOf extracts a frame's consuming seq (event or reseed notice).
func seqOf(f *abiv1.StreamFrame) uint64 {
	if e := f.GetEvent(); e != nil {
		return e.GetSeq()
	}
	if n := f.GetReseeded(); n != nil {
		return n.GetSeq()
	}
	return 0
}

// drainUntil collects frames from ch until a frame with the target seq
// arrives (inclusive), failing on ctx deadline.
func drainUntil(t *testing.T, ch <-chan *abiv1.StreamFrame, target uint64) []*abiv1.StreamFrame {
	t.Helper()
	deadline := time.After(10 * time.Second)
	var out []*abiv1.StreamFrame
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before target seq %d (got %d frames, last seq %d)", target, len(out), seqOf(out[len(out)-1]))
			}
			out = append(out, f)
			if seqOf(f) == target {
				return out
			}
		case <-deadline:
			t.Fatalf("timed out draining to seq %d (got %d frames)", target, len(out))
		}
	}
}

// assertContiguousSeqs fails unless every frame carries a seq exactly
// one above the previous — the no-gap/no-duplicate property on one
// connection. Snapshot frames are skipped (they stamp, not consume).
func assertContiguousSeqs(t *testing.T, frames []*abiv1.StreamFrame, firstWant uint64) uint64 {
	t.Helper()
	last := uint64(0)
	expect := firstWant
	for i, f := range frames {
		if f.GetSnapshot() != nil {
			t.Fatalf("frame %d is a snapshot frame inside a resumed replay (resume must not open with one)", i)
		}
		s := seqOf(f)
		if s == 0 {
			t.Fatalf("frame %d carries no seq: %+v", i, f.Frame)
		}
		if last != 0 && s != last+1 {
			t.Fatalf("seq gap/duplicate on resumed stream: %d after %d", s, last)
		}
		if i == 0 && s != firstWant {
			t.Fatalf("resume opened at seq %d, want %d (since+1)", s, firstWant)
		}
		last = s
		_ = expect
	}
	return last
}

// TestReplayResumeGaplessProperty: randomized disconnect/resume cycles —
// the discard-rule fuzz precedent applied to the ring. Invariant: a
// consumer that has applied exactly 1..S and reconnects with since=S
// receives exactly S+1, S+2, … with no snapshot frame, no gap, no
// duplicate, regardless of how much published before the reconnect and
// how much lands live after it.
func TestReplayResumeGaplessProperty(t *testing.T) {
	for iter := 0; iter < 60; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter%d", iter), func(t *testing.T) {
			t.Parallel()
			// Per-iteration source: subtests run in parallel, and a
			// shared *rand.Rand is itself a race.
			rng := rand.New(rand.NewSource(int64(iter) + 1))
			a := newAuthority(t, replayConfig(t, 0, 0)) // default budgets
			sid := fmt.Sprintf("s%d", rng.Intn(3))

			// Phase 1: connect, consume a random prefix live.
			frames1, cancel1, err := a.Stream(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			pre := 1 + rng.Intn(8)
			for i := 0; i < pre; i++ {
				a.IngestForTest(deltaEvent(sid, 8))
			}
			got1 := drainUntil(t, frames1, uint64(pre))
			if f := got1[0]; f.GetSnapshot() == nil {
				t.Fatalf("plain Stream must open with a snapshot frame")
			}
			applied := uint64(pre) // the consumer's last applied seq

			// Disconnect; publish MORE while disconnected (the loss
			// window this whole feature exists to close).
			cancel1()
			gap := 1 + rng.Intn(6)
			for i := 0; i < gap; i++ {
				a.IngestForTest(deltaEvent(sid, 8))
			}

			// Phase 2: resume with since=applied.
			frames2, resumed, cancel2, err := a.StreamSince(context.Background(), applied)
			if err != nil {
				t.Fatal(err)
			}
			defer cancel2()
			if !resumed {
				t.Fatalf("resume refused (since=%d) though the default ring retains everything", applied)
			}

			// Publish live frames DURING the replay drain — the
			// replay→live boundary must stay gapless.
			live := 1 + rng.Intn(5)
			published := make(chan struct{})
			go func() {
				defer close(published)
				for i := 0; i < live; i++ {
					a.IngestForTest(deltaEvent(sid, 8))
				}
			}()
			final := uint64(pre + gap + live)
			got2 := drainUntil(t, frames2, final)
			<-published

			last := assertContiguousSeqs(t, got2, applied+1)
			if last != final {
				t.Fatalf("resumed stream ended at %d, want %d", last, final)
			}
			low, high := a.ReplayWindow()
			if high != final {
				t.Fatalf("high-water %d != final seq %d", high, final)
			}
			if low > applied {
				t.Fatalf("low-water %d rose past the served cursor %d — eviction too aggressive for a retained ring", low, applied)
			}
		})
	}
}

// TestReplayFullHistoryFromZero: since=0 (explicitly present) on a ring
// that retains everything replays the ENTIRE history with no snapshot
// frame — the "cold consumer that still wants events, not state" path.
func TestReplayFullHistoryFromZero(t *testing.T) {
	a := newAuthority(t, replayConfig(t, 0, 0))
	for i := 0; i < 5; i++ {
		a.IngestForTest(deltaEvent("s1", 8))
	}
	frames, resumed, cancel, err := a.StreamSince(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !resumed {
		t.Fatal("since=0 must resume when the ring retains from seq 1")
	}
	got := drainUntil(t, frames, 5)
	if last := assertContiguousSeqs(t, got, 1); last != 5 {
		t.Fatalf("full replay ended at %d, want 5", last)
	}
}

// TestReplayFallbackBelowLowWater: cursor below the ring's low-water
// mark degrades to TODAY'S protocol — resumed=false, snapshot frame
// first, subsequent event seqs > stamp (the honest fallback, not an
// error).
func TestReplayFallbackBelowLowWater(t *testing.T) {
	a := newAuthority(t, replayConfig(t, 4, 0)) // tiny ring: 4 frames
	for i := 0; i < 10; i++ {
		a.IngestForTest(deltaEvent("s1", 8))
	}
	low, high := a.ReplayWindow()
	if low != high-4 || high != 10 {
		t.Fatalf("water marks: low=%d high=%d, want low=6 high=10 (4-frame ring)", low, high)
	}

	frames, resumed, cancel, err := a.StreamSince(context.Background(), low-1)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if resumed {
		t.Fatal("since below low-water must degrade to snapshot-first, not resume")
	}
	first := <-frames
	snap := first.GetSnapshot()
	if snap == nil {
		t.Fatalf("fallback must open with a snapshot frame, got %+v", first.Frame)
	}
	if snap.AtSeq != 10 {
		t.Fatalf("fallback stamp = %d, want current seq 10", snap.AtSeq)
	}
	// Today's behavior continues: post-snapshot events strictly > stamp.
	a.IngestForTest(deltaEvent("s1", 8))
	next := <-frames
	if s := seqOf(next); s <= snap.AtSeq {
		t.Fatalf("post-snapshot event seq %d ≤ stamp %d — I2 gap/overlap", s, snap.AtSeq)
	}
	if got := a.Metrics().ReplayFallbacks; got != 1 {
		t.Fatalf("ReplayFallbacks = %d, want 1", got)
	}
}

// TestRingEvictionBoundary: the count cap evicts oldest-first and the
// low-water mark tracks the eviction frontier exactly; a resume at
// exactly low-water serves from the oldest RETAINED frame, one below it
// falls back.
func TestRingEvictionBoundary(t *testing.T) {
	const cap = 8
	const total = 20
	a := newAuthority(t, replayConfig(t, cap, 0))
	for i := 0; i < total; i++ {
		a.IngestForTest(deltaEvent("s1", 8))
	}
	m := a.Metrics()
	if m.ReplayRingFrames != cap {
		t.Fatalf("ring holds %d frames, want the cap %d", m.ReplayRingFrames, cap)
	}
	low, _ := a.ReplayWindow()
	if want := uint64(total - cap); low != want { // newest 8 retained (13..20), low = 12
		t.Fatalf("low-water = %d, want %d", low, want)
	}

	// Resume AT the boundary: since=low replays low+1..total.
	frames, resumed, cancel, err := a.StreamSince(context.Background(), low)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !resumed {
		t.Fatalf("since=low (%d) must resume — low-water is inclusive", low)
	}
	got := drainUntil(t, frames, total)
	if last := assertContiguousSeqs(t, got, low+1); last != total {
		t.Fatalf("boundary replay ended at %d, want %d", last, total)
	}

	// One below the boundary: fallback.
	_, resumed2, cancel2, err := a.StreamSince(context.Background(), low-1)
	if err != nil {
		t.Fatal(err)
	}
	cancel2()
	if resumed2 {
		t.Fatalf("since=low-1 (%d) must fall back", low-1)
	}
}

// TestRingByteBudgetEviction: the byte budget evicts independently of
// the count cap — oldest first, until the budget holds (or only one
// frame remains). The low-water mark stays exact for whatever survived.
func TestRingByteBudgetEviction(t *testing.T) {
	// Frames of ~64-byte deltas; budget 512 bytes holds a handful
	// regardless of the huge count cap.
	a := newAuthority(t, replayConfig(t, 100000, 512))
	const total = 40
	for i := 0; i < total; i++ {
		a.IngestForTest(deltaEvent("s1", 64))
	}
	m := a.Metrics()
	if m.ReplayRingFrames >= total {
		t.Fatalf("byte budget did not evict: %d frames retained", m.ReplayRingFrames)
	}
	if m.ReplayRingBytes > 512 {
		t.Fatalf("ring bytes %d exceed the 512 budget with %d frames retained (multi-frame overshoot)", m.ReplayRingBytes, m.ReplayRingFrames)
	}
	low, _ := a.ReplayWindow()
	if want := uint64(total - m.ReplayRingFrames); low != want { // retained seqs (total-n+1)..total, low = total-n
		t.Fatalf("low-water %d != eviction frontier %d (retained %d of %d)", low, want, m.ReplayRingFrames, total)
	}

	// Whatever survived still serves an exact resume.
	frames, resumed, cancel, err := a.StreamSince(context.Background(), low)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !resumed {
		t.Fatal("since=low must resume after byte-budget eviction")
	}
	got := drainUntil(t, frames, total)
	if last := assertContiguousSeqs(t, got, low+1); last != total {
		t.Fatalf("post-eviction replay ended at %d, want %d", last, total)
	}
}

// TestRingSingleOversizedFrameAlwaysRetained: one frame larger than the
// whole byte budget is retained alone — the ring never advances the
// low-water past a seq with nothing retained for cause, and resume from
// immediately before it still works.
func TestRingSingleOversizedFrameAlwaysRetained(t *testing.T) {
	a := newAuthority(t, replayConfig(t, 100000, 64))
	a.IngestForTest(deltaEvent("s1", 8)) // small frame
	a.IngestForTest(deltaEvent("s1", 4096))
	m := a.Metrics()
	if m.ReplayRingFrames != 1 {
		t.Fatalf("oversized-frame ring holds %d frames, want exactly 1 (the newest)", m.ReplayRingFrames)
	}
	if m.ReplayRingBytes <= 64 {
		t.Fatalf("the oversized frame itself was dropped — newest must always be retained")
	}
	low, _ := a.ReplayWindow()
	if low != 1 {
		t.Fatalf("low-water = %d, want 1 (frame 2 retained; frame 1 evicted for cause)", low)
	}
	frames, resumed, cancel, err := a.StreamSince(context.Background(), low)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !resumed {
		t.Fatal("resume from low-water must hold with only the oversized frame retained")
	}
	got := drainUntil(t, frames, 2)
	if last := assertContiguousSeqs(t, got, 2); last != 2 {
		t.Fatalf("oversized-frame replay ended at %d, want 2", last)
	}
}

// TestReplayAcrossReseedNotice: the reseed notice consumes a seq and is
// ringed with everything else — a resume from BEFORE the reseed
// delivers it in seq position (the client's re-snapshot trigger
// survives replay; skipping it would silently diverge the fold).
func TestReplayAcrossReseedNotice(t *testing.T) {
	store := &mapStore{m: map[string]abiv1.SessionStatus{"s1": abiv1.SessionStatus_SESSION_STATUS_IDLE}}
	cfg := replayConfig(t, 0, 0)
	cfg.Store = store
	a := newAuthority(t, cfg)

	for i := 0; i < 3; i++ {
		a.IngestForTest(deltaEvent("s1", 8))
	}
	if err := a.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		a.IngestForTest(deltaEvent("s1", 8))
	}
	// seqs: 1,2,3 events · 4 reseed notice · 5,6 events.

	frames, resumed, cancel, err := a.StreamSince(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if !resumed {
		t.Fatal("resume from seq 2 must hold (default ring retains all)")
	}
	got := drainUntil(t, frames, 6)
	if last := assertContiguousSeqs(t, got, 3); last != 6 {
		t.Fatalf("resume across reseed ended at %d, want 6", last)
	}
	sawNotice := false
	for _, f := range got {
		if n := f.GetReseeded(); n != nil {
			sawNotice = true
			if n.Seq != 4 {
				t.Fatalf("reseed notice replayed out of position: seq %d, want 4", n.Seq)
			}
		}
	}
	if !sawNotice {
		t.Fatal("resume across a reseed did not deliver the reseed notice — clients would miss the mandatory re-snapshot signal")
	}
}

// TestReplayAfterKill9FallsBackHonest: kill-window fault injection (the
// TestSeqMonotonicAcrossKill9 precedent): the ring is process memory —
// after a hard kill the reopened authority's ring is empty, so an old
// cursor is honestly refused (snapshot-first), never guessed at.
func TestReplayAfterKill9FallsBackHonest(t *testing.T) {
	dir := t.TempDir()
	cfg := replayConfig(t, 0, 0)
	cfg.PlatformDir = dir
	cfg.FastCursor = false // real fsync'd cursor across the kill

	a1, err := sessionstate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		a1.IngestForTest(deltaEvent("s1", 8))
	}
	time.Sleep(20 * time.Millisecond) // let publishes settle
	a1.KillForTest()                  // the kill — no Close

	a2, err := sessionstate.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a2.Close() }()
	low, high := a2.ReplayWindow()
	if high < 10 {
		t.Fatalf("durable cursor lost seq across kill: high=%d", high)
	}
	if low != high {
		t.Fatalf("fresh authority's ring must be empty (low==high=%d), got low=%d", high, low)
	}

	frames, resumed, cancel, err := a2.StreamSince(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if resumed {
		t.Fatal("resume must be refused after a process restart — the ring died with the process")
	}
	first := <-frames
	if first.GetSnapshot() == nil {
		t.Fatalf("post-restart resume fallback must open with a snapshot frame, got %+v", first.Frame)
	}
}

// TestReplayResumeUnderConcurrentFloodRace: -race pin — ingest flood
// overlapping resume registration: the one-lock registration cut must
// keep every resumed stream contiguous (replay tail then live frames,
// no gap at the seam).
func TestReplayResumeUnderConcurrentFloodRace(t *testing.T) {
	a := newAuthority(t, replayConfig(t, 0, 0))
	a.IngestForTest(deltaEvent("s1", 8))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // flood writer
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				a.IngestForTest(deltaEvent(fmt.Sprintf("s%d", i%3), 16))
			}
		}
	}()

	// Repeatedly resume from the seq the previous cycle reached.
	applied := uint64(1)
	for cycle := 0; cycle < 8; cycle++ {
		target := applied + 20
		frames, resumed, cancel, err := a.StreamSince(context.Background(), applied)
		if err != nil {
			t.Fatal(err)
		}
		if !resumed {
			cancel()
			t.Fatalf("cycle %d: resume refused at since=%d under a retained default ring", cycle, applied)
		}
		got := drainUntil(t, frames, target)
		applied = assertContiguousSeqs(t, got, applied+1)
		cancel()
	}
	close(stop)
	wg.Wait()
}

// TestEventsSinceOnWire: the ABI handler end-to-end — absent since
// opens with a snapshot frame (today's behavior, pinned); present
// since (served) opens with the replayed event seq=since+1 and NO
// snapshot; present since (below low-water) opens with a snapshot.
func TestEventsSinceOnWire(t *testing.T) {
	a, ts := testServer(t, func(c *sessionstate.Config) {
		c.FastCursor = true
		c.ReplayRingFrames = 4
	})
	c := authedClient(ts.URL, "agentd-password")

	// Absent since: snapshot-first (exactly today's behavior).
	s0, err := c.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if !s0.Receive() || s0.Msg().GetSnapshot() == nil {
		t.Fatal("absent since must open with a snapshot frame — today's behavior changed")
	}
	_ = s0.Close()

	for i := 0; i < 10; i++ {
		a.IngestForTest(deltaEvent("s1", 8))
	}

	since := uint64(7)
	s1, err := c.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{Since: &since}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s1.Close() }()
	if !s1.Receive() {
		t.Fatal(s1.Err())
	}
	if e := s1.Msg().GetEvent(); e == nil || e.Seq != since+1 {
		t.Fatalf("resumed wire stream must open with event seq %d, got %+v", since+1, s1.Msg().Frame)
	}
	last := seqOf(s1.Msg())
	for last < 10 && s1.Receive() {
		if s := seqOf(s1.Msg()); s > 0 {
			if s != last+1 {
				t.Fatalf("wire resume gap: %d after %d", s, last)
			}
			last = s
		}
	}
	if last != 10 {
		t.Fatalf("wire resume drained to %d, want 10", last)
	}

	// Below low-water (ring holds 7..10, low=6): since=2 → snapshot.
	below := uint64(2)
	s2, err := c.Events(context.Background(), connect.NewRequest(&abiv1.EventsRequest{Since: &below}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.Receive() || s2.Msg().GetSnapshot() == nil {
		t.Fatal("below-low-water since must open with a snapshot frame (honest fallback)")
	}
}
