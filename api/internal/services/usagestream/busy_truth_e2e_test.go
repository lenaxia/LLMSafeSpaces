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

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// busy_truth_e2e_test.go — #1602's acceptance pin at the level that
// matters: the REAL sessionstate authority (the #1574 derivation, the
// BusyComponents, GetSnapshot with its lease gather), the REAL
// abiclient fold over the REAL connect HTTP surface, and the REAL
// consumer — bridging onto the recorded bridge exactly as
// usageBridge.SessionStatus publishes session.status SSE to the user
// broker. The incident sequence end to end: the harness's idle while a
// bash tool executes must publish BUSY; only the true end of work
// publishes IDLE.

// jsonEventParser feeds the authority by protojson-encoded events
// (Ingest takes raw harness bytes; the parser seam translates).
type jsonEventParser struct{}

func (jsonEventParser) Parse(raw []byte) (*abiv1.Event, bool, error) {
	evt := &abiv1.Event{}
	if err := protojson.Unmarshal(raw, evt); err != nil {
		return nil, true, err
	}
	return evt, true, nil
}

// basicTransport injects the §D1 agentd credential (the same discipline
// as newUsageStreamClient's transport).
type basicTransport struct{ password string }

func (t basicTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(agentd.AuthUsername, t.password)
	return http.DefaultTransport.RoundTrip(r)
}

// syncBridge records bridge calls under a mutex (the consumer
// goroutine calls back while the test polls).
type syncBridge struct {
	mu       sync.Mutex
	statuses []string // "ws:sid:busy|idle"
}

func (b *syncBridge) SessionStatus(workspaceID, sessionID string, busy bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.statuses = append(b.statuses, workspaceID+":"+sessionID+":"+boolWord(busy))
}
func (b *syncBridge) InputRequested(workspaceID string, req *abiv1.InputRequest) {}
func (b *syncBridge) InputResolved(workspaceID, sessionID, inputID string)       {}
func (b *syncBridge) SessionTitle(workspaceID, sessionID, title string)          {}
func (b *syncBridge) ContextUsed(workspaceID, sessionID string, used int64)      {}
func (b *syncBridge) AgentDied(workspaceID string)                               {}

func (b *syncBridge) snapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.statuses...)
}

// e2eStore is the authority's store-reader seam: scriptable seeds for
// the reseed path (Reseed rebuilds the projection from store truth).
type e2eStore struct {
	mu    sync.Mutex
	seed  map[string]sessionstate.SessionSeed
	calls int
}

func (s *e2eStore) set(seeds map[string]sessionstate.SessionSeed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed = seeds
}

func (s *e2eStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	out := make(map[string]sessionstate.SessionSeed, len(s.seed))
	for k, v := range s.seed {
		out[k] = v
	}
	return out, nil
}

func (s *e2eStore) MessagePresence(ctx context.Context, sessionID string, messageIDs []string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (s *e2eStore) PendingInputs(ctx context.Context) (map[string][]*abiv1.InputRequest, error) {
	return map[string][]*abiv1.InputRequest{}, nil
}

func newBusyTruthE2E(t *testing.T) (*sessionstate.Authority, *Consumer, *syncBridge) {
	t.Helper()
	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      jsonEventParser{},
		Store:       &e2eStore{},
		Passwords:   []string{"pw"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })

	_, h := auth.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	br := &syncBridge{}
	c := newBusyTruthConsumer(t, ts.URL, br, time.Hour)
	c.Open("ws1")

	// The gate connects asynchronously and the stream has no replay:
	// wait until the subscription is registered before feeding events.
	require.Eventually(t, func() bool { return auth.Metrics().Subscribers == 1 },
		10*time.Second, 10*time.Millisecond, "consumer never subscribed to the authority stream")
	return auth, c, br
}

// newBusyTruthConsumer wires the REAL abiclient over the authority's
// HTTP surface (idleDrop tunable: most tests park it at an hour and
// tear down via CloseAll; the gate-lifecycle rows need it short).
func newBusyTruthConsumer(t *testing.T, baseURL string, br Bridge, idleDrop time.Duration) *Consumer {
	t.Helper()
	c := New(Config{
		Resolve: func(ctx context.Context, workspaceID string) (string, string, error) {
			return baseURL, "pw", nil
		},
		NewClient: func(baseURL, password string) Client {
			return abiclient.New(&http.Client{Transport: basicTransport{password: password}}, baseURL)
		},
		Bridge:   br,
		IdleDrop: idleDrop,
	})
	t.Cleanup(c.CloseAll)
	return c
}

func ingest(t *testing.T, auth *sessionstate.Authority, evt *abiv1.Event) {
	t.Helper()
	raw, err := protojson.Marshal(evt)
	require.NoError(t, err)
	auth.Ingest(raw)
}

func waitForStatuses(t *testing.T, br *syncBridge, want []string) {
	t.Helper()
	require.Eventually(t, func() bool {
		got := br.snapshot()
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}, 10*time.Second, 25*time.Millisecond, "bridge statuses never reached %v", want)
}

func runningToolPart(id string) *abiv1.Part {
	return &abiv1.Part{
		Id:   id,
		Type: abiv1.PartType_PART_TYPE_TOOL,
		Payload: &abiv1.Part_Tool{Tool: &abiv1.ToolPart{
			CallId: "call_" + id,
			Name:   "bash",
			Input:  []byte(`{"command":"sleep 30"}`),
			State:  &abiv1.ToolState{Status: abiv1.ToolStatus_TOOL_STATUS_RUNNING},
		}},
	}
}

func completedToolPart(id string) *abiv1.Part {
	p := runningToolPart(id)
	p.GetTool().State.Status = abiv1.ToolStatus_TOOL_STATUS_COMPLETED
	return p
}

// TestBusyTruthE2E_RunningBashShowsBusy: the running-bash row, end to
// end through every production component. The harness reports IDLE
// mid-turn while the bash tool part is in flight — the SSE bridge must
// carry the authority's BUSY. The turn's true end (part terminal,
// nothing queued) publishes IDLE.
func TestBusyTruthE2E_RunningBashShowsBusy(t *testing.T) {
	auth, _, br := newBusyTruthE2E(t)

	// Turn opens: the harness marks busy (streaming).
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	waitForStatuses(t, br, []string{"ws1:s1:busy"})

	// Streaming ends (the harness's ONLY signal): at this instant
	// nothing else is in flight — the truthful answer is idle.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	waitForStatuses(t, br, []string{"ws1:s1:busy", "ws1:s1:idle"})

	// THE ROW: the bash tool starts — no status event fires, only the
	// part. The flip-driven consult must publish busy.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1", Part: runningToolPart("p1")})
	waitForStatuses(t, br, []string{"ws1:s1:busy", "ws1:s1:idle", "ws1:s1:busy"})

	// The tool completes but the turn continues autonomously (results
	// feed the next segment; the part's busy-mark holds until the
	// harness's status word) — busy HOLDS; no emission.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_END, SessionId: "s1", PartId: "p1", Part: completedToolPart("p1")})
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, []string{"ws1:s1:busy", "ws1:s1:idle", "ws1:s1:busy"}, br.snapshot(),
		"between tool completion and the harness's next word, autonomous progress is still pending — busy holds")

	// The turn's true end: the harness's final idle lands with nothing
	// in flight — the bridge finally publishes idle.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	waitForStatuses(t, br, []string{"ws1:s1:busy", "ws1:s1:idle", "ws1:s1:busy", "ws1:s1:idle"})
}

// TestBusyTruthE2E_CompactingRendersBusy: the COMPACTING leg through
// the same wiring — compaction is autonomous progress; the derived
// truth (fixed in the projection by this change) must reach the bridge
// as busy, not regress to idle via the raw overlay.
func TestBusyTruthE2E_CompactingRendersBusy(t *testing.T) {
	auth, _, br := newBusyTruthE2E(t)

	ingest(t, auth, &abiv1.Event{
		Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1",
		Status: abiv1.SessionStatus_SESSION_STATUS_COMPACTING,
	})

	waitForStatuses(t, br, []string{"ws1:s1:busy"})
}

// TestBusyTruthE2E_PermissionWaitIsNotBusy: the carve-out leg — a
// pending QUESTION with nothing else in flight bridges idle even
// though the harness last said busy (the #1573 divergence direction).
func TestBusyTruthE2E_PermissionWaitIsNotBusy(t *testing.T) {
	auth, _, br := newBusyTruthE2E(t)

	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_BUSY})
	waitForStatuses(t, br, []string{"ws1:s1:busy"})

	// The ask arrives mid-turn; streaming then ends. The ONLY thing
	// left in flight is the user-input wait — the carve-out.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_INPUT_REQUEST, SessionId: "s1",
		Input: &abiv1.InputRequest{Id: "q1", SessionId: "s1", Kind: abiv1.InputKind_INPUT_KIND_QUESTION, Question: "Proceed?"}})
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	waitForStatuses(t, br, []string{"ws1:s1:busy", "ws1:s1:idle"})

	// The carve-out holds: the pending ask alone never flips busy — no
	// further emission while the user is the only thing being waited
	// on. (A FRESH busy status event legitimately re-marks streaming
	// busy in the authority — that is not the carve-out's leg; the
	// stale-busy overlay direction is pinned in the consumer tests.)
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, []string{"ws1:s1:busy", "ws1:s1:idle"}, br.snapshot(),
		"waiting on the user is not busy — no flip, no emission")
}

// TestBusyTruthE2E_InStreamReseedReconciles: the review-r1 row — a
// generation-change reseed (the harness died; the store rebuilds from
// truth) happens INSIDE the client's stream (redial, no return). The
// consumer's derived baseline must rebuild from the fresh snapshot: the
// gate releases when the reseeded truth is idle, preserving
// scale-to-zero through the wedge-recovery path.
func TestBusyTruthE2E_InStreamReseedReconciles(t *testing.T) {
	// The store is wired at construction; its (mutex-guarded) seed is
	// swapped for the reseed. SetStoreForTest is NOT used: it swaps the
	// store pointer on a live-served authority, racing the consumer's
	// concurrent GetSnapshot consults.
	store := &e2eStore{}
	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      jsonEventParser{},
		Store:       store,
		Passwords:   []string{"pw"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })
	_, h := auth.Handler()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	// A short settle window: this row asserts the gate DROPS once the
	// reseeded truth is idle.
	br := &syncBridge{}
	c := newBusyTruthConsumer(t, ts.URL, br, 100*time.Millisecond)
	c.Open("ws1")
	require.Eventually(t, func() bool { return auth.Metrics().Subscribers >= 1 },
		10*time.Second, 10*time.Millisecond, "consumer never subscribed")

	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1", Part: runningToolPart("p1")})
	waitForStatuses(t, br, []string{"ws1:s1:busy"})
	require.Equal(t, 1, c.Gates(), "precondition: the running tool holds the gate")

	// The harness dies; the store's truth says idle. The reseed is an
	// IN-STREAM frame — the client redials and resnapshots without the
	// consumer's run loop ever reconnecting.
	store.set(map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	})
	require.NoError(t, auth.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))

	require.Equal(t, 1, c.Gates(), "the gate lives through the reseed itself")
	require.Eventually(t, func() bool { return c.Gates() == 0 },
		15*time.Second, 50*time.Millisecond,
		"the in-stream reseed's snapshot must rebuild the derived baseline — the gate must drop")
}
