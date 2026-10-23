// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/usagestream"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
)

// proxy_usage_resync_test.go — #1884's handler-tier pins: the pod→api
// usage stream's silent death heals server-side (the #1666 per-session
// corrections), but the USER stream's subscribers never learn frames
// were lost — the browser leg stayed healthy, so its #1646/#1687 heals
// never fire, and latches freeze (the incident signature). The
// usageBridge's StreamResynced (fired by the consumer on every
// non-initial snapshot seed) must run the per-workspace busy-snapshot
// flight — the same authority-backed reconciliation #1848 serves on
// subscribe and the lease recheck — so every subscriber converges onto
// ground truth without a round trip.

// TestUsageBridgeStreamResynced_FiresBusySnapshotFlight: THE #1884
// handler row. A subscribed browser (healthy stream, latched busy) is
// frozen because the pod→api leg silently died and its idle frame was
// lost. The consumer's redial fires StreamResynced — the subscriber
// must receive a full D9/D10 flight (begin → authoritative rows →
// complete ok=true) sourced from statusz.
func TestUsageBridgeStreamResynced_FiresBusySnapshotFlight(t *testing.T) {
	hs := newBusySnapHarness(t, "user-r1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	// A subscribed browser — via the broker directly (no SSE connect,
	// so no connect-path flight noise; exactly the frozen-latch state).
	sub, err := hs.broker.SubscribeUser("user-r1")
	require.NoError(t, err)
	defer hs.broker.UnsubscribeUser("user-r1", sub)

	// The consumer's non-initial-seed cue.
	(&usageBridge{h: hs.handler}).StreamResynced(busySnapWS)

	deadline := time.After(3 * time.Second)
	seq := []string{}
	var snap *apitypes.WorkspaceSSEEvent
	for len(seq) < 3 {
		select {
		case evt := <-sub.Ch:
			switch evt.Type {
			case "session.status.snapshot_begin", "session.status.snapshot_complete":
				seq = append(seq, evt.Type)
			case "session.status.snapshot":
				seq = append(seq, evt.Type)
				e := evt
				snap = &e
			}
		case <-deadline:
			t.Fatalf("resync flight incomplete before deadline; got %v", seq)
		}
	}
	assert.Equal(t, []string{
		"session.status.snapshot_begin",
		"session.status.snapshot",
		"session.status.snapshot_complete",
	}, seq, "the resync cue must run one D9/D10 flight on the user stream")

	// The snapshot frame carries the typed payload on the broker (the
	// SSE wire JSON-decodes it; a direct channel sub sees the struct).
	payload, ok := snap.Data.(busySnapshotPayload)
	require.True(t, ok, "snapshot data must be the typed busySnapshotPayload, got %T", snap.Data)
	var busyRow *busySnapshotRow
	for i := range payload.Sessions {
		if payload.Sessions[i].SessionID == "ses-busy" {
			busyRow = &payload.Sessions[i]
		}
	}
	require.NotNil(t, busyRow, "the flight must carry the authority's busy row — the frozen latch's heal")
	assert.True(t, busyRow.DerivedBusy, "ses-busy must carry derived_busy=true")
}

// TestUsageBridgeStreamResynced_NonBlockingAndCoalesced: the cue fires
// from the consumer's fold goroutine — it must return immediately (the
// flight is detached), and a flapping stream must not storm the
// authority: concurrent cues while one flight is running coalesce into
// it (exactly one statusz fetch), the RequestSessionStatusSnapshot
// discipline.
func TestUsageBridgeStreamResynced_NonBlockingAndCoalesced(t *testing.T) {
	release := make(chan struct{})
	var fetches int32
	hs := newBusySnapHarness(t, "user-r2", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fetches, 1)
		<-release // hold the flight's authority fetch open
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	sub, err := hs.broker.SubscribeUser("user-r2")
	require.NoError(t, err)
	defer hs.broker.UnsubscribeUser("user-r2", sub)

	br := &usageBridge{h: hs.handler}

	// First cue: the authority fetch parks, but the cue itself must
	// return immediately — the fold goroutine must never stall on the
	// flight.
	started := time.Now()
	br.StreamResynced(busySnapWS)
	assert.Less(t, int64(time.Since(started)), int64(500*time.Millisecond),
		"StreamResynced must be non-blocking — the flight is detached from the fold goroutine")

	// The flight is provably running (its authority fetch started, so
	// its coalescing marker is held).
	assert.Eventually(t, func() bool { return atomic.LoadInt32(&fetches) == 1 },
		2*time.Second, 10*time.Millisecond, "the first cue's flight never reached the authority fetch")

	// Rapid re-cues (a flapping stream): while the first flight's
	// marker holds, every further cue is a no-op.
	for i := 0; i < 3; i++ {
		br.StreamResynced(busySnapWS)
	}

	// Let the held flight complete.
	close(release)

	// Exactly ONE authority fetch served all four cues — and none was
	// spawned behind the coalescing marker.
	assert.Eventually(t, func() bool {
		return atomic.LoadInt32(&fetches) == 1
	}, 2*time.Second, 20*time.Millisecond,
		"concurrent resync cues must coalesce to one authority fetch (got %d)", atomic.LoadInt32(&fetches))
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), atomic.LoadInt32(&fetches),
		"a re-cue that raced the marker release must not have spawned a second fetch")

	// Drain the flight's frames so the broker subscriber is idle for
	// the next test's channel reuse (defensive; UnsubscribeUser above
	// is the real cleanup).
	for {
		select {
		case <-sub.Ch:
		default:
			return
		}
	}
}

// killableUsageClient is the usage-stream test fake at the handlers
// tier: snapshot-first (scripted seeds per connection), then parked
// until the test kills the connection — the silent stream death (no
// close frame, no replay). GetSnapshot has no authority scripted (the
// fail-open consult leg; not this pin's subject).
type killableUsageClient struct {
	mu        sync.Mutex
	kill      chan struct{}
	seed      map[string]*abiv1.SessionSnapshot
	connected chan struct{}
	onUpdate  func(*abiclient.SessionState)
	onEvent   func(evt *abiv1.Event, seq uint64)
}

func (f *killableUsageClient) setSeed(sessions map[string]*abiv1.SessionSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seed = sessions
}

func (f *killableUsageClient) Stream(ctx context.Context, onUpdate func(*abiclient.SessionState), opts ...abiclient.StreamOption) error {
	f.mu.Lock()
	f.onUpdate = onUpdate
	f.onEvent = abiclient.AppliedEventsOf(opts)
	if f.kill == nil {
		f.kill = make(chan struct{})
	}
	kill := f.kill
	seed := f.seed
	f.mu.Unlock()
	sessions := map[string]*abiv1.SessionSnapshot{}
	for sid, s := range seed {
		sessions[sid] = s
	}
	onUpdate(&abiclient.SessionState{Seq: 0, Sessions: sessions})
	if f.connected != nil {
		f.connected <- struct{}{}
	}
	select {
	case <-ctx.Done():
		return context.Canceled
	case <-kill:
		f.mu.Lock()
		f.kill = nil
		f.mu.Unlock()
		return errors.New("stream killed by test")
	}
}

// apply drives one ABI event through the captured callbacks as the pod
// stream would.
func (f *killableUsageClient) apply(seq uint64, evt *abiv1.Event) {
	f.mu.Lock()
	onUpdate, onEvent := f.onUpdate, f.onEvent
	f.mu.Unlock()
	if onEvent != nil {
		onEvent(evt, seq)
	}
	if onUpdate != nil {
		onUpdate(&abiclient.SessionState{Seq: seq, Sessions: map[string]*abiv1.SessionSnapshot{
			evt.GetSessionId(): {SessionId: evt.GetSessionId(), Status: evt.GetStatus()},
		}})
	}
}

func (f *killableUsageClient) GetSnapshot(ctx context.Context, sessionID string) (*abiv1.SessionSnapshot, error) {
	return nil, errors.New("no authority snapshot scripted")
}

func (f *killableUsageClient) killStream() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kill != nil {
		close(f.kill)
	}
}

// TestUsageStreamReconnectE2E_ResyncFlightHealsUserStream: THE #1884
// full-stack row — the REAL consumer driving the PRODUCTION usageBridge
// on a real ProxyHandler/user broker. The usage stream dies silently
// mid-turn; the idle frame is lost with the user stream healthy (the
// incident signature). The consumer's redial applies the fresh snapshot
// seed (#1666 corrections for the fold's diffs) AND fires the resync
// cue, whose flight delivers the authoritative busy rows to the
// subscriber — frozen latches heal with no browser round trip.
func TestUsageStreamReconnectE2E_ResyncFlightHealsUserStream(t *testing.T) {
	hs := newBusySnapHarness(t, "user-e2e", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	// The frozen-latch browser: subscribed, healthy stream.
	sub, err := hs.broker.SubscribeUser("user-e2e")
	require.NoError(t, err)
	defer hs.broker.UnsubscribeUser("user-e2e", sub)

	// The real consumer over the killable fake, bridging through the
	// PRODUCTION usageBridge.
	fc := &killableUsageClient{connected: make(chan struct{}, 4)}
	c := usagestream.New(usagestream.Config{
		Resolve:   func(context.Context, string) (string, string, error) { return "http://pod", "pw", nil },
		NewClient: func(string, string) usagestream.Client { return fc },
		Bridge:    &usageBridge{h: hs.handler},
		IdleDrop:  time.Hour,
		Retry:     5 * time.Millisecond,
	})
	t.Cleanup(c.CloseAll)
	c.Open(busySnapWS)
	select {
	case <-fc.connected:
	case <-time.After(2 * time.Second):
		t.Fatal("usage gate never connected")
	}

	// The turn runs: the subscriber latches busy (the bridged baseline
	// the reconnect diff needs).
	fc.apply(1, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "ses-busy", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	require.Eventually(t, func() bool {
		for {
			select {
			case evt := <-sub.Ch:
				if evt.Type == "session.status" && evt.SessionID == "ses-busy" && evt.Status == "busy" {
					return true
				}
			default:
				return false
			}
		}
	}, 3*time.Second, 10*time.Millisecond, "the busy frame never reached the subscriber (precondition)")

	// The turn's idle frame dies with the stream: the authority's truth
	// (idle now) exists only in the NEXT connection's snapshot seed.
	fc.setSeed(map[string]*abiv1.SessionSnapshot{
		"ses-busy": {SessionId: "ses-busy", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	})
	fc.killStream()

	// The redial's corrections AND the resync flight must both reach the
	// subscriber: a session.status idle correction (#1666, the seed's
	// diff against the bridged baseline) and the authoritative D9/D10
	// flight (begin → rows → complete ok=true).
	deadline := time.After(5 * time.Second)
	var sawIdleCorrection, sawBegin, sawComplete bool
	var complete *apitypes.WorkspaceSSEEvent
	for !sawIdleCorrection || !sawBegin || !sawComplete {
		select {
		case evt := <-sub.Ch:
			switch evt.Type {
			case "session.status":
				if evt.SessionID == "ses-busy" && evt.Status == "idle" {
					sawIdleCorrection = true
				}
			case "session.status.snapshot_begin":
				sawBegin = true
			case "session.status.snapshot_complete":
				sawComplete = true
				e := evt
				complete = &e
			}
		case <-deadline:
			t.Fatalf("heal incomplete: idleCorrection=%v begin=%v complete=%v",
				sawIdleCorrection, sawBegin, sawComplete)
		}
	}
	require.NotNil(t, complete.SnapshotOK, "flight complete must carry snapshot_ok")
	assert.True(t, *complete.SnapshotOK, "a healthy statusz read is authoritative (ok=true)")
}

// TestUsageBridgeStreamResynced_AuthorityFailureHoldsUnknown: the
// flight's D10 discipline on the resync path — a statusz failure reads
// ok=false (keep your state), never an authoritative empty set (which
// would clear every latch on a blip).
func TestUsageBridgeStreamResynced_AuthorityFailureHoldsUnknown(t *testing.T) {
	hs := newBusySnapHarness(t, "user-r3", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)

	sub, err := hs.broker.SubscribeUser("user-r3")
	require.NoError(t, err)
	defer hs.broker.UnsubscribeUser("user-r3", sub)

	(&usageBridge{h: hs.handler}).StreamResynced(busySnapWS)

	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-sub.Ch:
			if evt.Type == "session.status.snapshot_complete" {
				require.NotNil(t, evt.SnapshotOK, "complete must carry snapshot_ok")
				assert.False(t, *evt.SnapshotOK,
					"an authority failure on the resync flight must read ok=false — latches keep their state")
				return
			}
			if evt.Type == "session.status.snapshot" {
				t.Fatal("a failed authority read must NOT emit snapshot rows")
			}
		case <-deadline:
			t.Fatal("resync flight never completed")
		}
	}
}
