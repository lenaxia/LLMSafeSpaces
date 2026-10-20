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
	mu   sync.Mutex
	seed map[string]sessionstate.SessionSeed
}

func (s *e2eStore) set(seeds map[string]sessionstate.SessionSeed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seed = seeds
}

func (s *e2eStore) SessionStates(ctx context.Context) (map[string]sessionstate.SessionSeed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// #1532: 30s bound — the old 10s was a wall-clock guess that lost
	// to multi-second runner stalls (the test's 7+ CI hits); the wait
	// itself is outcome-deterministic, the deadline only bites on real
	// breakage.
	require.Eventually(t, func() bool { return auth.Metrics().Subscribers == 1 },
		30*time.Second, 10*time.Millisecond, "consumer never subscribed to the authority stream")
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
	// #1532: 30s bound (was 10s) — Eventually only waits as long as it
	// needs; the budget matters solely when the chain is genuinely
	// broken, and 10s was small enough for a stalled runner to trip.
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
	}, 30*time.Second, 25*time.Millisecond, "bridge statuses never reached %v", want)
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
// bridge converges onto the reseeded idle truth, and a gate armed onto
// that world releases — scale-to-zero preserved through the
// wedge-recovery path.
//
// #1779 determinism design. The pre-fix shape armed the gate with
// IdleDrop=100ms and then raced its own setup against the idle
// watchdog: Open starts the settle clock (consumer.go arms idleSince
// at gate creation), and the busy truth that resets it must travel
// HTTP subscribe → two ingests → SSE delivery → consult → fold. Every
// ingest runs the seq-cursor persist (two fsyncs; observed 31–133ms
// for one ingest on CI-class disks), so a single slow fsync detonated
// the 100ms window, tore the stream down mid-arm, and the replay-less
// stream lost the events — the bridge's first emission never landed
// and the wait burned its whole 30s budget. Ingesting IDLE and
// PART_START back-to-back added a second race: the consumer's
// IDLE-frame consult could answer before the part folded, emitting an
// idle the exact-history assert can never match. Both lose only under
// load — five green runs locally said nothing.
//
// The fix follows the package's own e2e discipline (bridge_reconcile
// rows park IdleDrop at an hour so nothing but the test drives the
// gate): the busy-arm and reseed legs run on an hour-parked holder
// with every emission awaited before the next input, and the
// gate-release leg runs on a second, short-window consumer opened
// AFTER the reseed — onto a world holding no busy truth, its drop is
// outcome-certain (nothing can defuse it; scheduling delays only
// postpone it into the Eventually budget).
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

	br := &syncBridge{}
	holder := newBusyTruthConsumer(t, ts.URL, br, time.Hour)
	holder.Open("ws1")
	require.Eventually(t, func() bool { return auth.Metrics().Subscribers >= 1 },
		30*time.Second, 10*time.Millisecond, "consumer never subscribed")

	// The turn's streaming ends (the harness's ONLY signal) — awaited
	// BEFORE the tool starts, so the emission order never races the
	// delivery fold.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_SESSION_STATUS, SessionId: "s1", Status: abiv1.SessionStatus_SESSION_STATUS_IDLE})
	waitForStatuses(t, br, []string{"ws1:s1:idle"})

	// The bash tool starts — no status event fires, only the part. The
	// flip-driven consult must publish busy.
	ingest(t, auth, &abiv1.Event{Type: abiv1.EventType_EVENT_TYPE_PART_START, SessionId: "s1", PartId: "p1", Part: runningToolPart("p1")})
	waitForStatuses(t, br, []string{"ws1:s1:idle", "ws1:s1:busy"})
	require.Equal(t, 1, holder.Gates(), "precondition: the running tool holds the gate")

	// The harness dies; the store's truth says idle. The reseed is an
	// IN-STREAM frame — the client redials and resnapshots without the
	// consumer's run loop ever reconnecting. The resync's seed fold is
	// the authority's post-reseed answer (the redial's snapshot is
	// taken under the reseed's lock hold, so it can never observe the
	// pre-reseed world): the derived baseline rebuilds from it and the
	// bridge must be corrected onto the idle truth — the stale-busy
	// emission is cured, not left dangling.
	store.set(map[string]sessionstate.SessionSeed{
		"s1": {Status: abiv1.SessionStatus_SESSION_STATUS_IDLE},
	})
	require.NoError(t, auth.Reseed(context.Background(), sessionstate.ReseedReasonGenerationChange))
	waitForStatuses(t, br, []string{"ws1:s1:idle", "ws1:s1:busy", "ws1:s1:idle"})

	// Scale-to-zero through the wedge recovery: a gate armed onto the
	// reseeded world must release once its settle window elapses. A
	// wedged-busy reseed (the regression this row exists for) holds
	// this gate open forever — the wait below fails. The window is
	// safe only because nothing busy exists post-reseed: the drop is
	// the certain outcome, so the budget absorbs any runner stall.
	dropper := newBusyTruthConsumer(t, ts.URL, br, 100*time.Millisecond)
	dropper.Open("ws1")
	require.Eventually(t, func() bool { return dropper.Gates() == 0 },
		45*time.Second, 50*time.Millisecond,
		"a gate armed onto the reseeded idle truth must release — the in-stream reseed's snapshot must rebuild the derived baseline")
}
