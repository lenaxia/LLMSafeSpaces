// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package usagestream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
)

// billing_replay_test.go — design 0069's acceptance rows: the L3 billing
// windows CLOSE. Billing fires on MESSAGE_END applied-events; the DB
// write is idempotent under deterministic (workspace, message, seq)
// keys (proxy_usagestream.go recordStepUsage — key determinism pinned by
// TestRecordStepUsage_DeterministicKeys), so redelivery is always SAFE;
// what was missing was a way to REDeliver. The replay ring + the
// client's resume cursor are that mechanism. The fault-injection rows
// (kill-window suite precedent, sessionstate/authority_test.go):
//
//   - window 1 (agentd-side slow-consumer drop): the gate's subscriber
//     is evicted while the API's fold is stalled; MESSAGE_ENDs published
//     in the drop window are recovered by the resume replay — billed
//     exactly once;
//   - window 2 (transport/reseed gap): frames published while the
//     transport is dead are recovered the same way through the RUN-LOOP
//     redial (the reused client's cursor), with NO #1884 resync flight
//     (a served resume lost nothing — the no-double-fire pin);
//   - duplicate redelivery: the idempotency key absorbs it (already
//     billed, no double row).
//
// Window 3 (the gate-closed window — a turn starting entirely inside
// the pod arms no gate; MESSAGE_ENDs before the ≤60s reconciler re-arm
// are unbilled) is measured-but-unaddressed by design: the SSE
// investigation's recommendation is to measure the sub-tick miss rate
// before building the agentd→API "gate poke" coupling.

// billingGate is a Billing seam whose FIRST calls block until opened —
// the stall that turns the fold goroutine into a slow consumer and
// forces the authority's M3.4 drop (fanoutLocked evicts the subscriber
// once its 256-frame channel fills).
type billingGate struct {
	mu      sync.Mutex
	records []recordedUsage
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBillingGate(t *testing.T) *billingGate {
	t.Helper()
	g := &billingGate{entered: make(chan struct{}, 1024), release: make(chan struct{})}
	// Fail-safe: whatever aborts the test, the fold goroutine must not
	// be left parked in RecordUsage (CloseAll waits on it).
	t.Cleanup(g.open)
	return g
}

func (b *billingGate) RecordUsage(workspaceID string, u Usage) {
	b.entered <- struct{}{}
	<-b.release
	b.mu.Lock()
	b.records = append(b.records, recordedUsage{workspaceID: workspaceID, usage: u})
	b.mu.Unlock()
}

func (b *billingGate) open() {
	b.once.Do(func() { close(b.release) })
}

func (b *billingGate) snapshot() []recordedUsage {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recordedUsage(nil), b.records...)
}

// evictingServer is the deterministic M3.4 slow-consumer drop at the
// wire: it mounts the REAL authority's generated ABI surface but ends
// an Events connection CLEANLY (returns nil, subscription canceled) at
// the next pulled frame once armed — byte-for-byte the client-visible
// shape of fanoutLocked's eviction (the subscriber's channel closes,
// the generated handler's read gets ok=false, it returns nil). A real
// flood-based eviction depends on kernel buffer sizes (the CI box's
// loopback can absorb tens of MB); the armed wrapper pins the DROP
// ITSELF deterministically while everything else — authority, ring,
// connect transport, redial — stays production.
type evictingServer struct {
	abiconnect.HarnessABIServiceHandler
	auth  *sessionstate.Authority
	mu    sync.Mutex
	armed bool
}

func (s *evictingServer) armDrop() {
	s.mu.Lock()
	s.armed = true
	s.mu.Unlock()
}

// dropArmed consumes the arm one-shot: true exactly once, at the
// chosen frame.
func (s *evictingServer) dropArmed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed {
		s.armed = false
		return true
	}
	return false
}

// Events mirrors the authority's own Events decision (absent since ⇒
// snapshot-first via Stream; present since ⇒ StreamSince resume), with
// the armed eviction check before each Send.
func (s *evictingServer) Events(ctx context.Context, req *connect.Request[abiv1.EventsRequest], stream *connect.ServerStream[abiv1.StreamFrame]) error {
	var frames <-chan *abiv1.StreamFrame
	var cancel func()
	if since := req.Msg.Since; since != nil {
		f, _, c, err := s.auth.StreamSince(ctx, *since)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		frames, cancel = f, c
	} else {
		f, c, err := s.auth.Stream(ctx)
		if err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
		frames, cancel = f, c
	}
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case f, ok := <-frames:
			if !ok {
				return nil
			}
			if s.dropArmed() {
				// The M3.4 drop: the frame is PULLED (published) but
				// never sent — lost to this subscriber, retained by the
				// ring. Clean end: the client redials with its cursor.
				return nil
			}
			if err := stream.Send(f); err != nil {
				return err
			}
		}
	}
}

func newEvictingHarness(t *testing.T) (*sessionstate.Authority, *evictingServer, *httptest.Server) {
	t.Helper()
	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      jsonEventParser{},
		Store:       &e2eStore{},
		Passwords:   []string{"pw"},
		FastCursor:  true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })
	ev := &evictingServer{HarnessABIServiceHandler: auth, auth: auth}
	path, svc := abiconnect.NewHarnessABIServiceHandler(ev)
	mux := http.NewServeMux()
	mux.Handle(path, svc)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return auth, ev, ts
}

func newBillingConsumer(t *testing.T, baseURL string, br Bridge, bill Billing, retry time.Duration) *Consumer {
	t.Helper()
	c := New(Config{
		Resolve: func(ctx context.Context, workspaceID string) (string, string, error) {
			return baseURL, "pw", nil
		},
		NewClient: func(baseURL, password string) Client {
			return abiclient.New(&http.Client{Transport: basicTransport{password: password}}, baseURL)
		},
		Bridge:   br,
		Billing:  bill,
		IdleDrop: time.Hour,
		Retry:    retry,
	})
	t.Cleanup(c.CloseAll)
	return c
}

func msgEndEvent(sid, mid string, tokens int64) *abiv1.Event {
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

func deltaEvent(sid string, size int) *abiv1.Event {
	return &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_PART_DELTA,
		SessionId: sid,
		PartId:    "p1",
		Delta:     string(make([]byte, size)),
	}
}

// billedSeqs extracts the billing seq multiset (duplicates visible).
func billedSeqs(recs []recordedUsage) []uint64 {
	out := make([]uint64, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.usage.Seq)
	}
	return out
}

// TestBillingClosedByReplay_SlowConsumerDropWindow: window 1. The fold
// stalls inside RecordUsage (the billing callback is synchronous — the
// slow consumer), the authority's fanout evicts the gate's subscriber,
// and MESSAGE_ENDs published in the drop window are unbilled by any
// snapshot (the snapshot carries no cost data). With the resume cursor
// the internal redial replays the window from the ring and bills every
// MESSAGE_END exactly once.
func TestBillingClosedByReplay_SlowConsumerDropWindow(t *testing.T) {
	auth, ev, ts := newEvictingHarness(t)
	gate := newBillingGate(t)
	br := &syncBridge{}
	c := newBillingConsumer(t, ts.URL, br, gate, 50*time.Millisecond)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	// The first MESSAGE_END lands and STALLS the fold inside billing —
	// the slow-consumer condition, deterministic by the gate.
	ingest(t, auth, msgEndEvent("s1", "m0", 100)) // seq 1
	select {
	case <-gate.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the fold never reached billing — the stall never armed")
	}

	// The server drops the stalled subscriber (M3.4): the armed
	// eviction fires at the next published frame — clean end, no error.
	ev.armDrop()
	ingest(t, auth, deltaEvent("s1", 1024)) // seq 2 — the eviction trigger (never sent)
	waitSubscribers(t, auth, 0, "the stalled fold was never dropped")

	// The drop window: billables (and traffic) published while the
	// subscription is gone — invisible to any snapshot's cost data.
	var wantSeqs []uint64
	for i := 0; i < 12; i++ {
		ingest(t, auth, deltaEvent("s1", 1024))
		want := auth.State().Seq + 1
		ingest(t, auth, msgEndEvent("s1", "m", int64(100+want)))
		wantSeqs = append(wantSeqs, want)
	}

	// Release the stall: the fold finishes seq 1's billing, observes
	// the clean end, and the INTERNAL redial (same Stream call, same
	// client) resumes from seq 1 — the ring replays the window.
	gate.open()
	waitSubscribers(t, auth, 1, "the consumer never resumed after the slow-consumer drop")

	// Billed exactly once: seq 1 (the stall's own frame) + every
	// dropped-window MESSAGE_END, no duplicates, no gaps.
	wantAll := append([]uint64{1}, wantSeqs...)
	require.Eventually(t, func() bool {
		return len(gate.snapshot()) == len(wantAll)
	}, 45*time.Second, 25*time.Millisecond, "billing never closed: got %d of %d frames", len(gate.snapshot()), len(wantAll))
	got := billedSeqs(gate.snapshot())
	require.ElementsMatch(t, wantAll, got,
		"every MESSAGE_END in the drop window recovered — exactly the emitted set")
	for _, seq := range got {
		require.Equal(t, 1, countOf(got, seq), "seq %d billed more than once — redelivery leaked past the idempotency key", seq)
	}
}

// TestBillingClosedByReplay_TransportGap: window 2. The transport dies
// mid-flight (server-side write abort — Stream RETURNS, so the recovery
// rides the consumer's run-loop redial with the REUSED client and its
// resume cursor). MESSAGE_ENDs published while dead are recovered and
// billed exactly once; the busy frame lost with the transport converges
// via the REPLAYED event (not a snapshot seed); and — the
// no-double-fire pin — NO #1884 StreamResynced fires: a served resume
// lost nothing, so the authority flight must stay parked.
func TestBillingClosedByReplay_TransportGap(t *testing.T) {
	auth, ts, kill := newKillingAuthorityRing(t, 0) // default ring: serves the gap
	gate := newBillingGate(t)
	gate.open() // no stall on this row
	br := &resyncBridge{}
	c := newBillingConsumer(t, ts.URL, br, gate, 50*time.Millisecond)
	c.Open("ws1")
	waitSubscribers(t, auth, 1, "consumer never subscribed")

	// Live billing before the gap (the baseline).
	ingest(t, auth, msgEndEvent("s1", "m1", 100)) // seq 1
	require.Eventually(t, func() bool { return len(gate.snapshot()) == 1 },
		30*time.Second, 10*time.Millisecond, "the live MESSAGE_END never billed")

	// The transport dies AT the next billable frame (write abort, no
	// close frame); more billables publish while dead.
	kill.arm()
	ingest(t, auth, msgEndEvent("s1", "m2", 200)) // seq 2 — dies on the wire
	waitSubscribers(t, auth, 0, "the killed transport never dropped its subscription")
	ingest(t, auth, msgEndEvent("s1", "m3", 300))                                                                                                    // seq 3 — published into the gap
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY}) // seq 4

	// The run-loop redial reuses the client: the cursor resumes from the
	// ring, replays the gap, bills exactly once.
	waitSubscribers(t, auth, 1, "the consumer never redialed after the transport death")
	require.Eventually(t, func() bool { return len(gate.snapshot()) == 3 },
		30*time.Second, 10*time.Millisecond, "the gap's MESSAGE_ENDs never billed (replay did not close the window)")
	got := billedSeqs(gate.snapshot())
	require.ElementsMatch(t, []uint64{1, 2, 3}, got, "exactly the three emitted frames, each once")
	for _, seq := range got {
		require.Equal(t, 1, countOf(got, seq), "seq %d billed more than once", seq)
	}

	// The lost busy frame converges via the REPLAYED event (the consult
	// path), not a snapshot seed — and the resync cue never fired: the
	// no-double-fire pin for #1884 under replay.
	waitForStatuses(t, &br.syncBridge, []string{"ws1:s1:busy"})
	require.Zero(t, br.resyncCount(),
		"a served resume lost nothing — the #1884 authority flight must NOT fire (the resync flight is the below-low-water fallback)")
}

// TestBillingDuplicateRedelivery_IdempotencyKeyAbsorbs: redelivery is
// safe by construction — the SAME MESSAGE_END event delivered twice
// produces byte-identical Usage values, so the deterministic
// (workspace, message, seq) idempotency keys (pinned at
// TestRecordStepUsage_DeterministicKeys) collide and the DB's unique
// constraint turns the race into an at-most-once insert: already
// billed, no double row.
func TestBillingDuplicateRedelivery_IdempotencyKeyAbsorbs(t *testing.T) {
	c, fc, usage, _ := newTestConsumer(t)
	requireOpen(t, c, fc)

	evt := &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_MESSAGE_END,
		SessionId: "s1",
		MessageId: "msg_dup",
		Message: &abiv1.Message{
			Id: "msg_dup", SessionId: "s1", Type: abiv1.MessageType_MESSAGE_TYPE_ASSISTANT,
			Model: &abiv1.ModelRef{Id: "glm-5.3", Provider: "opencode"},
			Cost:  &abiv1.Cost{InputTokens: 100, OutputTokens: 40, CostUsd: 0.002},
		},
	}
	fc.apply(7, evt)
	fc.apply(7, evt) // the duplicate redelivery

	require.Len(t, *usage, 2, "the consumer reports each delivery (dedupe is the DB's, via the key)")
	require.Equal(t, (*usage)[0].usage, (*usage)[1].usage,
		"a redelivered frame must produce a byte-identical Usage — anything varying breaks the deterministic key and double-bills")

	// The DB's unique constraint, modeled: one row per idempotency key.
	rows := map[string]int{}
	for _, r := range *usage {
		rows[r.workspaceID+":"+r.usage.MessageID+":"+fmtU(r.usage.Seq)]++
	}
	require.Len(t, rows, 1, "already-billed — the key absorbs the redelivery (one row)")
}

func fmtU(u uint64) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}

func countOf(seqs []uint64, want uint64) int {
	n := 0
	for _, s := range seqs {
		if s == want {
			n++
		}
	}
	return n
}
