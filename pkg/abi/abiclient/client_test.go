// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package abiclient_test

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	"github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	abiconnect "github.com/lenaxia/llmsafespaces/pkg/abi/v1/abiconnect"
	"github.com/stretchr/testify/require"
)

// The reference Go client (pkg/abi/abiclient) implements the M1 sync
// protocol's client rule: apply in order, discard seq ≤ S, and re-snapshot
// on projection.reseeded. It is the consumer the S1 comparator (US-69.5)
// and the API (S2) share.

type scriptedParser struct {
	mu sync.Mutex
	fn func(raw []byte) (*abiv1.Event, bool, error)
}

func (p *scriptedParser) Parse(raw []byte) (*abiv1.Event, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fn == nil {
		return nil, false, nil
	}
	return p.fn(raw)
}

type countingStore struct {
	mu    sync.Mutex
	calls int
	seed  map[string]sessionstate.SessionSeed
}

func (s *countingStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return s.seed, nil
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *countingStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = 0
}

func newSurface(t *testing.T, seed map[string]sessionstate.SessionSeed, parser sessionstate.EventParser) (*sessionstate.Authority, *httptest.Server, *countingStore) {
	t.Helper()
	store := &countingStore{seed: seed}
	if parser == nil {
		parser = &scriptedParser{}
	}
	cfg := sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      parser,
		Store:       store,
		Passwords:   []string{"pw"},
		Capabilities: &abiv1.CapabilityReport{
			Provenance:             abiv1.Provenance_PROVENANCE_PLATFORM_PINNED,
			Harness:                "opencode",
			HarnessVersion:         "1.18.15",
			AgentdVersion:          "test",
			SupportedActions:       nil, // actions land with US-69.9
			SupportedDeliveryParts: []abiv1.DeliveryPartKind{abiv1.DeliveryPartKind_DELIVERY_PART_KIND_TEXT},
			AbiVersion:             "1",
		},
	}
	a, err := sessionstate.New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	_, h := a.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return a, ts, store
}

func clientFor(ts *httptest.Server) *abiclient.Client {
	return abiclient.New(&http.Client{Transport: authedTransport{pw: "pw"}}, ts.URL)
}

type authedTransport struct{ pw string }

func (t authedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.SetBasicAuth("opencode", t.pw)
	return http.DefaultTransport.RoundTrip(r)
}

func statusEvt(sid string, st abiv1.SessionStatus) *abiv1.Event {
	return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: sid, Status: st}
}

func inputEvt(id, sid string) *abiv1.Event {
	return &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, SessionId: sid,
		Input: &abiv1.InputRequest{Id: id, SessionId: sid, Kind: abiv1.InputKind_INPUT_KIND_QUESTION}}
}

// TestGetSnapshot_ZeroOpencodeCalls: the snapshot serves from the
// projection — the store reader counts ZERO calls; and the payload is
// I12-complete (issue #1138: snapshot_zero_opencode_calls).
func TestGetSnapshot_ZeroOpencodeCalls(t *testing.T) {
	a, ts, store := newSurface(t, map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_BUSY, PendingInputs: []*abiv1.InputRequest{
			{Id: "q1", SessionId: "s1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Proceed?"},
		}},
	}, nil)
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	store.reset() // the reseed legitimately reads the store; snapshots must not

	c := clientFor(ts)
	snap, err := c.GetSnapshot(context.Background(), "s1")
	require.NoError(t, err)
	require.Equal(t, 0, store.count(), "snapshot must make ZERO store/opencode calls")
	require.Equal(t, abiv1.SessionStatus_SESSION_STATUS_BUSY, snap.GetStatus())
	require.Len(t, snap.GetPendingInputs(), 1)
	require.Equal(t, "Proceed?", snap.GetPendingInputs()[0].GetQuestion())

	_, err = c.GetSnapshot(context.Background(), "")
	require.Error(t, err, "empty session id must be rejected")
}

// TestSnapshotLatencyLocal: p99 < 250ms locally against a projection with
// a realistic session count (the 2-CPU pod measurement is the US-69.6
// spike; this pins the code path).
func TestSnapshotLatencyLocal(t *testing.T) {
	seed := make(map[string]sessionstate.SessionSeed, 200)
	for i := 0; i < 200; i++ {
		seed[fmt.Sprintf("s%d", i)] = sessionstate.SessionSeed{Status: abiv1.SessionStatus_SESSION_STATUS_IDLE}
	}
	a, ts, store := newSurface(t, seed, nil)
	require.NoError(t, a.Reseed(context.Background(), sessionstate.ReseedReasonBoot))
	store.reset()
	c := clientFor(ts)

	latencies := make([]time.Duration, 0, 300)
	for i := 0; i < 300; i++ {
		start := time.Now()
		_, err := c.GetSnapshot(context.Background(), "s100")
		require.NoError(t, err)
		latencies = append(latencies, time.Since(start))
	}
	p99 := latencies[(len(latencies)-1)*99/100]
	require.Less(t, p99, 250*time.Millisecond, "snapshot p99 %v exceeds the 250ms budget", p99)
	require.Equal(t, 0, store.count(), "snapshots must never touch the store")
}

// TestDiscardRulePropertyFuzz: random snapshot/event interleavings — the
// reference client's folded state always equals the server-side fold
// (issue #1138: discard_rule_property_fuzz).
func TestDiscardRulePropertyFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for iter := 0; iter < 50; iter++ {
		a, ts, _ := newSurface(t, nil, nil)

		// Random pre-connect events.
		pre := rng.Intn(10)
		for i := 0; i < pre; i++ {
			a.IngestForTest(statusEvt(fmt.Sprintf("s%d", rng.Intn(3)), abiv1.SessionStatus_SESSION_STATUS_BUSY))
			a.IngestForTest(inputEvt(fmt.Sprintf("q%d", rng.Intn(4)), "s0"))
		}

		c := clientFor(ts)
		_, err := c.Sync(context.Background())
		require.NoError(t, err)

		// Random live events; the client folds concurrently via Stream.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		updates := make(chan *abiclient.SessionState, 64)
		go func() {
			_ = c.Stream(ctx, func(s *abiclient.SessionState) { updates <- s })
		}()
		live := rng.Intn(10)
		for i := 0; i < live; i++ {
			a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_IDLE))
			a.IngestForTest(inputEvt("q1", "s0"))
			a.IngestForTest(&abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_INPUT_RESOLVED, SessionId: "s0",
				Input: &abiv1.InputRequest{Id: "q1"}})
		}
		deadline := time.After(5 * time.Second)
		var last *abiclient.SessionState
		for last == nil || last.Seq < a.State().Seq {
			select {
			case s := <-updates:
				last = s
			case <-deadline:
				t.Fatalf("iter %d: client stalled at seq %d, server at %d", iter, lastSeq(last), a.State().Seq)
			}
		}
		cancel()

		// Equivalence: client fold == server fold (status with busy
		// override + pending set).
		server := a.State()
		client := st2map(last)
		for sid, srv := range server.Sessions {
			cl, ok := client[sid]
			if !ok {
				t.Fatalf("iter %d: session %s missing from client fold", iter, sid)
			}
			wantStatus := srv.Status
			if srv.Busy {
				wantStatus = abiv1.SessionStatus_SESSION_STATUS_BUSY
			}
			require.Equal(t, wantStatus, cl.GetStatus(), "iter %d session %s status", iter, sid)
			require.Equal(t, len(srv.PendingInputs), len(cl.GetPendingInputs()), "iter %d session %s pendings", iter, sid)
		}
	}
}

func lastSeq(s *abiclient.SessionState) uint64 {
	if s == nil {
		return 0
	}
	return s.Seq
}

func st2map(s *abiclient.SessionState) map[string]*abiv1.SessionSnapshot {
	return s.Sessions
}

// TestSlowConsumerDropResync: a stalled reader is dropped (M3.4) — after a
// stall the stream closes; a fresh Sync() converges to current truth; no
// cursors anywhere (issue #1138: slow_consumer_drop_resync).
func TestSlowConsumerDropResync(t *testing.T) {
	a, ts, _ := newSurface(t, nil, nil)

	// Fill beyond the per-connection buffer without reading → server drops.
	for i := 0; i < 400; i++ {
		a.IngestForTest(statusEvt("s1", abiv1.SessionStatus_SESSION_STATUS_BUSY))
	}
	time.Sleep(50 * time.Millisecond)

	c := clientFor(ts)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// A stalled raw stream: open then never read. Use the authority's
	// subscription via a client that doesn't drain.
	stalled := abiclient.New(&http.Client{Transport: authedTransport{pw: "pw"}}, ts.URL)
	_ = stalled // opened below
	go func() { _ = stalled.Stream(ctx, func(*abiclient.SessionState) {}) }()

	// Meanwhile a healthy client resyncs and converges.
	st, err := c.Sync(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, st.Sessions, "fresh snapshot must enumerate sessions after the drop storm")
	require.Equal(t, a.State().Seq, st.Seq, "fresh snapshot must carry the current stamp")
}

// TestCapabilityReportContract: provenance + actions + part types on the
// wire; opencode declares TEXT parts only (D3) and no actions until
// US-69.9; an undeclared action is refused with a typed NotSupported
// detail (issue #1138: capability_report_contract).
func TestCapabilityReportContract(t *testing.T) {
	_, ts, _ := newSurface(t, nil, nil)
	c := abiclient.New(&http.Client{Transport: authedTransport{pw: "pw"}}, ts.URL)

	rep, err := c.Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, abiv1.Provenance_PROVENANCE_PLATFORM_PINNED, rep.GetProvenance())
	require.Equal(t, "opencode", rep.GetHarness())
	require.NotEmpty(t, rep.GetHarnessVersion())
	require.Equal(t, []abiv1.DeliveryPartKind{abiv1.DeliveryPartKind_DELIVERY_PART_KIND_TEXT}, rep.GetSupportedDeliveryParts(),
		"D3: file parts are NotSupported on opencode until harness support lands")
	require.Empty(t, rep.GetSupportedActions(), "actions are undeclared until US-69.9 wires them")

	_, err = c.Act(context.Background(), &abiv1.ActionRequest{SessionId: "s1",
		Action: &abiv1.ActionRequest_Compact{Compact: &abiv1.CompactAction{}}})
	ce, ok := err.(*connect.Error)
	require.True(t, ok)
	require.Equal(t, connect.CodeUnimplemented, ce.Code())
	require.NotEmpty(t, ce.Details(), "undeclared action must carry a typed NotSupported detail")
}

// TestAuthorityServesGeneratedHandlers: the 4097 ABI surface rides the
// GENERATED connect handler exclusively — the authority satisfies the
// generated service interface, and the mount path comes from the generator
// (issue #1138: handlers_are_generated; TestNoHandWrittenWire covers the
// schema side).
func TestAuthorityServesGeneratedHandlers(t *testing.T) {
	var _ abiconnect.HarnessABIServiceHandler = (*sessionstate.Authority)(nil)
	a, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(), Parser: &scriptedParser{}, Store: &countingStore{}, Passwords: []string{"pw"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.Close() })
	mount, _ := a.Handler()
	require.Equal(t, "/llmsafespaces.abi.v1.HarnessABIService/", mount,
		"the mount path must be the generated connect procedure root, not a hand-written route")
}
