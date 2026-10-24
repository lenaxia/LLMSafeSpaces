// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1821 pieces 2-4 — wire-level tier-1 pins for the busy-snapshot
// handshake on the user-stream connect path (§2 piece 2), the
// snapshot_ok D10 discipline, and the phrase-keyed snapshot rows.
//
// The connect path must emit, per Active workspace, a D9/D10 flight:
//
//	session.status.snapshot_begin {snapshot_id}
//	session.status.snapshot {data: {cursor, sessions: [...]}}
//	session.status.snapshot_complete {snapshot_id, snapshot_ok}
//
// sourced from the pod's statusz (the same authority
// GetAuthoritativeActiveSessions consults). An authority failure must
// read ok=false (keep your state), never an authoritative "all idle".
//
// RED-FIRST: at the time these pins were written the connect path emits
// only workspace.phase + the pending-input anti-entropy — every test
// below fails on the missing flight.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

const busySnapWS = "ws-busy"

// busySnapHarness wires a StreamUserEvents handler whose snapshot path
// sees one Active workspace (busySnapWS) whose statusz answers from the
// given test server. httpClient uses the rewritingTransport so the
// fixed admin port in the statusz URL is routed to the stub regardless.
type busySnapHarness struct {
	broker   *eventbroker.UserEventBroker
	handler  *ProxyHandler
	router   *gin.Engine
	statusz  *httptest.Server
	wsMock   *k8smocks.MockWorkspaceInterface
	statuszN func() int // calls received by the statusz stub
}

func newBusySnapHarness(t *testing.T, userID string, statuszHandler http.HandlerFunc, si interfaces.SessionIndexService) *busySnapHarness {
	t.Helper()

	// The stub is hit from concurrent flight goroutines (an on-demand
	// flight parking on the handler while a joiner's connect-path flight
	// enters — the #1862 CI race fired exactly here); the counter is
	// atomic end-to-end.
	var statuszCalls atomic.Int32
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		statuszCalls.Add(1)
		statuszHandler(w, r)
	})
	srv := httptest.NewServer(wrapped)
	t.Cleanup(srv.Close)

	broker := eventbroker.NewUserEventBroker()

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset()).Maybe()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsMock.On("List", mock.Anything, mock.MatchedBy(func(opts metav1.ListOptions) bool {
		return opts.LabelSelector == labelUserID+"="+userID
	})).Return(&v1.WorkspaceList{
		Items: []v1.Workspace{{ObjectMeta: metav1.ObjectMeta{Name: busySnapWS}}},
	}, nil)
	wsMock.On("Get", mock.Anything, busySnapWS, mock.Anything).Return(&v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: busySnapWS, Namespace: "default"},
		Status: v1.WorkspaceStatus{
			Phase: v1.WorkspacePhaseActive,
			PodIP: "10.0.0.9",
		},
	}, nil).Maybe()

	watcher, err := workspace.NewWatcher(k8sMock, &testLogger{}, "default", func(*v1.Workspace) {})
	require.NoError(t, err)
	watcher.SetKnownPhase(busySnapWS, "Active")

	h := &ProxyHandler{
		k8sClient:  k8sMock,
		logger:     &testLogger{},
		namespace:  "default",
		userBroker: broker,
		watcher:    watcher,
		httpClient: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &rewritingTransport{target: srv.URL},
		},
	}
	if si != nil {
		h.SetSessionIndex(si)
	}
	h.SetCachedPasswordForTest(busySnapWS, "test-password")
	broker.RecordWorkspaceOwner(busySnapWS, userID)

	router := gin.New()
	router.GET("/api/v1/events", func(c *gin.Context) {
		c.Set("userID", userID)
		h.StreamUserEvents(c)
	})

	return &busySnapHarness{
		broker:   broker,
		handler:  h,
		router:   router,
		statusz:  srv,
		wsMock:   wsMock,
		statuszN: func() int { return int(statuszCalls.Load()) },
	}
}

func busyStatuszBody() string {
	return `{"healthy":true,"sessions":[
		{"id":"ses-busy","status":"busy"},
		{"id":"ses-idle","status":"idle"}
	]}`
}

// collectSSE connects (optionally with Last-Event-ID) and collects
// decoded events until predicate returns true or the deadline passes.
// Returns everything observed, in arrival order.
func collectSSE(t *testing.T, hs *busySnapHarness, lastEventID string, done func([]apitypes.WorkspaceSSEEvent) bool) []apitypes.WorkspaceSSEEvent {
	t.Helper()

	srv := httptest.NewServer(hs.router)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/events", nil)
	require.NoError(t, err)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var evts []apitypes.WorkspaceSSEEvent
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-deadline:
			return evts
		default:
		}
		if !scanner.Scan() {
			return evts
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt apitypes.WorkspaceSSEEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
			continue
		}
		evts = append(evts, evt)
		if done(evts) {
			return evts
		}
	}
}

func countType(evts []apitypes.WorkspaceSSEEvent, typ string) int {
	n := 0
	for i := range evts {
		if evts[i].Type == typ {
			n++
		}
	}
	return n
}

func findEvent(evts []apitypes.WorkspaceSSEEvent, typ string) *apitypes.WorkspaceSSEEvent {
	for i := range evts {
		if evts[i].Type == typ {
			return &evts[i]
		}
	}
	return nil
}

func snapshotRows(t *testing.T, evt *apitypes.WorkspaceSSEEvent) ([]map[string]any, float64) {
	t.Helper()
	require.NotNil(t, evt, "session.status.snapshot event missing")
	data, ok := evt.Data.(map[string]any)
	require.True(t, ok, "snapshot data must be a JSON object, got %T", evt.Data)
	cursor, _ := data["cursor"].(float64)
	rawRows, ok := data["sessions"].([]any)
	require.True(t, ok, "snapshot data must carry a sessions array")
	rows := make([]map[string]any, 0, len(rawRows))
	for _, r := range rawRows {
		m, ok := r.(map[string]any)
		require.True(t, ok, "snapshot row must be a JSON object")
		rows = append(rows, m)
	}
	return rows, cursor
}

func rowBySession(rows []map[string]any, sid string) map[string]any {
	for _, r := range rows {
		if r["session_id"] == sid {
			return r
		}
	}
	return nil
}

// T1.1 — subscribe-after-flip: a client connecting AFTER a session went
// busy must receive the busy row in the connect window (class 1: busy
// was delta-only with no snapshot-on-subscribe — red today: nothing
// until the next transition).
func TestStreamUserEvents_BusySnapshotOnConnect_RestoresBusy(t *testing.T) {
	hs := newBusySnapHarness(t, "user-t11", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	// A stream with history: the busy flip happened before this client
	// ever connected (subscribe-after-flip), so the broker has already
	// published at least one event for the user.
	hs.broker.PublishToUser("user-t11", apitypes.WorkspaceSSEEvent{
		Type:        "workspace.phase",
		WorkspaceID: busySnapWS,
		Phase:       "Active",
	})

	evts := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		return findEvent(evts, "session.status.snapshot_complete") != nil
	})

	begin := findEvent(evts, "session.status.snapshot_begin")
	require.NotNil(t, begin, "connect window must open a session.status.snapshot flight (begin marker)")
	complete := findEvent(evts, "session.status.snapshot_complete")
	require.NotNil(t, complete, "connect window must close the flight (complete marker)")
	assert.Equal(t, begin.SnapshotID, complete.SnapshotID, "begin/complete must share the flight id")
	require.NotNil(t, complete.SnapshotOK, "complete must carry snapshot_ok")
	assert.True(t, *complete.SnapshotOK, "a healthy statusz read is authoritative (ok=true)")

	snap := findEvent(evts, "session.status.snapshot")
	rows, cursor := snapshotRows(t, snap)
	assert.Greater(t, cursor, float64(0), "snapshot must carry the broker cursor for delta ordering")

	busyRow := rowBySession(rows, "ses-busy")
	require.NotNil(t, busyRow, "busy session row missing from snapshot")
	derived, _ := busyRow["derived_busy"].(bool)
	assert.True(t, derived, "ses-busy must carry derived_busy=true")
	status, _ := busyRow["status"].(string)
	assert.Equal(t, "busy", status)

	idleRow := rowBySession(rows, "ses-idle")
	require.NotNil(t, idleRow, "idle session row missing from snapshot")
	idleDerived, _ := idleRow["derived_busy"].(bool)
	assert.False(t, idleDerived, "ses-idle must carry derived_busy=false")

	assert.Equal(t, begin.SnapshotID, snap.SnapshotID, "the snapshot frame must carry the flight id for client-side staging")
	assert.Equal(t, busySnapWS, snap.WorkspaceID)
}

// T1.2 — authority failure must read "keep your state" (ok=false),
// never an authoritative empty set (D10 discipline).
func TestStreamUserEvents_BusySnapshotAuthorityDown_HoldsUnknown(t *testing.T) {
	hs := newBusySnapHarness(t, "user-t12", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)

	evts := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		return findEvent(evts, "session.status.snapshot_complete") != nil
	})

	complete := findEvent(evts, "session.status.snapshot_complete")
	require.NotNil(t, complete, "authority failure still completes the flight")
	require.NotNil(t, complete.SnapshotOK, "complete must carry snapshot_ok")
	assert.False(t, *complete.SnapshotOK, "statusz failure must read ok=false (keep your state)")

	assert.Nil(t, findEvent(evts, "session.status.snapshot"),
		"a failed authority read must NOT emit snapshot rows (empty rows would read as authoritative all-idle)")
}

// T1.3 — refresh-mid-busy / disconnect with a replay gap: the client
// reconnects with a cursor the replay ring has already overflowed; the
// broker signals resync, and the connect-path snapshot flight must
// restore the busy row (class 2's refresh half + the per-replica replay
// limitation made benign by state convergence).
func TestStreamUserEvents_BusySnapshotAfterReplayGap_Resync(t *testing.T) {
	hs := newBusySnapHarness(t, "user-t13", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	// Overflow the 128-entry replay ring so Last-Event-ID: 3 is a gap.
	for i := 0; i < 130; i++ {
		hs.broker.PublishToUser("user-t13", apitypes.WorkspaceSSEEvent{
			Type:        "workspace.phase",
			WorkspaceID: busySnapWS,
			Phase:       "Active",
		})
	}

	evts := collectSSE(t, hs, "1", func(evts []apitypes.WorkspaceSSEEvent) bool {
		return findEvent(evts, "session.status.snapshot_complete") != nil
	})

	assert.NotNil(t, findEvent(evts, "resync"), "gap must signal resync (existing behavior)")

	snap := findEvent(evts, "session.status.snapshot")
	rows, _ := snapshotRows(t, snap)
	busyRow := rowBySession(rows, "ses-busy")
	require.NotNil(t, busyRow, "busy row must be restored by the post-resync snapshot flight")
	derived, _ := busyRow["derived_busy"].(bool)
	assert.True(t, derived)
}

// T1.4 — busy→idle→busy on one connection stays exactly-once per
// transition (#1781's emission legs unchanged) while a snapshot flight
// races them concurrently (deltas published the moment the stream is
// live, before the flight's fetch completes): the flight must not
// duplicate the delta surface.
func TestStreamUserEvents_BusySnapshotFlight_DoesNotDuplicateDeltas(t *testing.T) {
	hs := newBusySnapHarness(t, "user-t14", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	// #1848 r3 review note: drive TRUE concurrency — the three flips
	// publish the moment the subscriber registers (the connect-path
	// flight is in flight, its fetch unresolved), racing the flight
	// rather than sequencing after its complete.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hs.broker.UserSubscriberCount("user-t14") >= 1 {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		hs.broker.PublishToUser("user-t14", apitypes.WorkspaceSSEEvent{Type: "session.status", WorkspaceID: busySnapWS, SessionID: "ses-busy", Status: "busy"})
		hs.broker.PublishToUser("user-t14", apitypes.WorkspaceSSEEvent{Type: "session.status", WorkspaceID: busySnapWS, SessionID: "ses-busy", Status: "idle"})
		hs.broker.PublishToUser("user-t14", apitypes.WorkspaceSSEEvent{Type: "session.status", WorkspaceID: busySnapWS, SessionID: "ses-busy", Status: "busy"})
	}()
	evts := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		return findEvent(evts, "session.status.snapshot_complete") != nil && countType(evts, "session.status") >= 3
	})

	statuses := []string{}
	for _, e := range evts {
		if e.Type == "session.status" {
			statuses = append(statuses, e.Status)
		}
	}
	assert.Equal(t, []string{"busy", "idle", "busy"}, statuses,
		"exactly one session.status per transition — the snapshot flight must not emit delta-shaped duplicates")

	snapCount := 0
	for _, e := range evts {
		if e.Type == "session.status.snapshot" {
			snapCount++
		}
	}
	assert.Equal(t, 1, snapCount, "exactly one snapshot frame for the connect flight")
}

// T1.5 — the #1821 piece-1 ID discipline on the new surface: snapshot
// rows are keyed by the PHRASE with the harness id additive; a session
// the index cannot resolve degrades to the harness id, flagged
// deprecated (never worse).
func TestStreamUserEvents_BusySnapshotRows_PhraseKeyed(t *testing.T) {
	si := newMockSessionIndex()
	si.phrases[busySnapWS+"/ses-known"] = "phrase-one"

	hs := newBusySnapHarness(t, "user-t15", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"healthy":true,"sessions":[
			{"id":"ses-known","status":"busy"},
			{"id":"ses-unknown","status":"busy"}
		]}`)
	}, si)

	evts := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		return findEvent(evts, "session.status.snapshot_complete") != nil
	})

	snap := findEvent(evts, "session.status.snapshot")
	rows, _ := snapshotRows(t, snap)

	known := rowBySession(rows, "phrase-one")
	require.NotNil(t, known, "a resolved session must be keyed by its phrase")
	internal, _ := known["internal_session_id"].(string)
	assert.Equal(t, "ses-known", internal, "the harness id rides additively")
	deprecated, _ := known["session_id_deprecated"].(string)
	assert.Empty(t, deprecated, "a resolved row carries no degrade flag")

	unknown := rowBySession(rows, "ses-unknown")
	require.NotNil(t, unknown, "an unresolved session must keep its harness id (never worse)")
	dep, _ := unknown["session_id_deprecated"].(bool)
	assert.True(t, dep, "the degraded row must be flagged session_id_deprecated")
}

// Z7 (#1848 r4 — finding 3's backend half): IDLE rows must register
// into the phrase space exactly like busy rows. The client keys its
// per-session ordering watermarks and removal decisions on the phrase,
// so a harness-keyed idle row neither advances the phrase-keyed
// watermark nor matches the latched entry — an older overlapping busy
// flight then re-latches what the newer idle answer already cleared
// (pinned client-side as Z3b). RED-FIRST: idle rows used to skip the
// Register seam entirely and rode the harness key.
func TestStreamUserEvents_BusySnapshotIdleRows_PhraseKeyed(t *testing.T) {
	si := newMockSessionIndex()
	si.phrases[busySnapWS+"/ses-known"] = "phrase-one"

	hs := newBusySnapHarness(t, "user-t17", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"healthy":true,"sessions":[
			{"id":"ses-known","status":"idle"}
		]}`)
	}, si)

	evts := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		return findEvent(evts, "session.status.snapshot_complete") != nil
	})

	snap := findEvent(evts, "session.status.snapshot")
	require.NotNil(t, snap, "flight must carry the snapshot frame")
	rows, _ := snapshotRows(t, snap)

	idle := rowBySession(rows, "phrase-one")
	require.NotNil(t, idle, "a resolved IDLE session must be keyed by its phrase (#1848 r3 finding 3)")
	internal, _ := idle["internal_session_id"].(string)
	assert.Equal(t, "ses-known", internal, "the harness id rides additively on idle rows too")
	derived, _ := idle["derived_busy"].(bool)
	assert.False(t, derived, "idle row carries derived_busy=false")
	assert.Nil(t, rowBySession(rows, "ses-known"), "no harness-keyed duplicate row")
}

// The frontend lease's recheck surface (#1821 piece 4): POSTing the
// trigger must answer 202 immediately and fire the flight on the user
// stream (RequestInputSnapshot pattern).
func TestRequestSessionStatusSnapshot_FiresFlight(t *testing.T) {
	hs := newBusySnapHarness(t, "user-ep", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	sub, err := hs.broker.SubscribeUser("user-ep")
	require.NoError(t, err)
	defer hs.broker.UnsubscribeUser("user-ep", sub)

	proxy := hs.router.Group("/api/v1/workspaces/:id")
	proxy.POST("/sessions/status-snapshot", hs.handler.RequestSessionStatusSnapshot)

	srv := httptest.NewServer(hs.router)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/v1/workspaces/"+busySnapWS+"/sessions/status-snapshot", "", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusAccepted, resp.StatusCode)

	// The flight lands on the user stream: begin → snapshot → complete.
	deadline := time.After(3 * time.Second)
	seq := []string{}
	for len(seq) < 3 {
		select {
		case evt := <-sub.Ch:
			if evt.Type == "session.status.snapshot_begin" ||
				evt.Type == "session.status.snapshot" ||
				evt.Type == "session.status.snapshot_complete" {
				seq = append(seq, evt.Type)
			}
		case <-deadline:
			t.Fatalf("flight incomplete before deadline; got %v", seq)
		}
	}
	assert.Equal(t, []string{
		"session.status.snapshot_begin",
		"session.status.snapshot",
		"session.status.snapshot_complete",
	}, seq)
}

// F4 (#1848 r0): a statusz 200 whose session listing FAILED (agentd
// swallows ListSessions errors and caches the empty slice —
// session_tracker.go cachedState) must read as a non-authoritative
// busy set, not an authoritative empty set. The sessions_ok signal
// (absent on older pods — treated as ok, never worse) carries it.
func TestStreamUserEvents_BusySnapshot_SessionListFailure_NotAuthoritative(t *testing.T) {
	hs := newBusySnapHarness(t, "user-t16", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"healthy":true,"sessions":[],"sessions_ok":false}`)
	}, nil)

	evts := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		return findEvent(evts, "session.status.snapshot_complete") != nil
	})

	complete := findEvent(evts, "session.status.snapshot_complete")
	require.NotNil(t, complete)
	require.NotNil(t, complete.SnapshotOK)
	assert.False(t, *complete.SnapshotOK,
		"a failed session listing must read ok=false — an empty-row commit would clear latched busy (the D10 failure one layer above HTTP status)")
	assert.Nil(t, findEvent(evts, "session.status.snapshot"))
}

// X6 (#1848 r1): the on-demand trigger coalesces — a second request
// while a flight is running joins it (one statusz fetch), never spawns
// a concurrent flight for the same workspace.
//
// De-flake (the #39914818351 CI red, "got 2"): "nearly simultaneously"
// is not the contract. The guard (busyFlightsInFlight,
// proxy_busy_snapshot.go) coalesces only requests whose guard runs
// WHILE a flight is live; the old body fired two naked POSTs and hoped
// both guards landed inside the first flight's sub-millisecond window
// (the statusz stub answers instantly). Under CI load (-short +
// coverage, 25 parallel packages) the second POST's dial+handler
// latency exceeded that window; the second trigger then LEGITIMATELY
// started its own flight (no flight was running — the first had
// completed and published its markers), the monotonic fetch counter
// read 2, and assert.Eventually(==1) could never recover (2.01s red).
// Not a product race: LoadOrStore is an atomic CAS.
//
// The body below ESTABLISHES the "while a flight is running"
// precondition deterministically instead of hoping for it: the stub
// parks the FIRST authority fetch; the second trigger fires only after
// that fetch is provably parked (and the first accepted), so its guard
// runs strictly inside the first flight's live window — the map entry
// cannot be gone yet (Delete runs only after the fetch returns) and
// the guard MUST coalesce. The second trigger's 202 proves its guard
// ran; a second authority fetch — flagged stub-side, whenever it lands
// while the stub lives — or a pre-release snapshot_complete both prove
// a non-coalesced second flight. (A fetch that has not begun by
// cleanup time is the one microscopic, disclosed escape window; every
// fetch that HAS begun is on the counter at stub-handler entry.)
//
// Deterministic reds under the causal mutations: a neutered guard
// (always spawn) and an early Delete (key released before the flight
// runs) both make the second trigger — arriving strictly inside the
// window — spawn its own flight: fetch #2 trips the stub's t.Errorf
// (and the parked-phase count assert).
//
// Follow-up hardening (#1907 r2): (a) release is closed idempotently
// in a defer — failure paths that fire while flight 1 is parked must
// not strand the harness's stub-Close cleanup for the full go-test
// timeout; (b) the pre-release drain also counts snapshot_begin — a
// second flight's begin is published in-process BEFORE its fetch, so
// begins > 1 catches a begin-only mutant the complete-only drain never
// sees.
func TestRequestSessionStatusSnapshot_Coalesces(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	// Failure-path hardening: if any require/fatal below fires while
	// flight 1 is parked mid-fetch (trigger-2 assert, drain verdict,
	// or the fetch-park wait itself), the harness's
	// t.Cleanup(stub srv.Close) — which waits for outstanding requests
	// — would block forever on the parked fetch, hanging the package
	// until go test's 10m timeout. A DEFER runs before every t.Cleanup
	// (cleanups run after the test function's defers, LIFO within each
	// regime), so idempotently closing release here makes park-release
	// strictly precede the stub Close on every exit path; the happy
	// path closes it inline first and the once makes that final.
	defer releaseOnce.Do(func() { close(release) })
	firstFetch := make(chan struct{})
	var fetches int32
	hs := newBusySnapHarness(t, "user-co", func(w http.ResponseWriter, r *http.Request) {
		if n := atomic.AddInt32(&fetches, 1); n == 1 {
			close(firstFetch)
			<-release // hold the first authority fetch open
		} else {
			// A second fetch is the non-coalescing regression itself —
			// flagged from the stub so it cannot slip past any wait.
			t.Errorf("authority fetch #%d: two triggers with the first flight parked must coalesce to one authority fetch", n)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	// A subscriber makes the flight's markers observable: the
	// pre-release drain below needs the stream, not the POST responses.
	sub, err := hs.broker.SubscribeUser("user-co")
	require.NoError(t, err)
	defer hs.broker.UnsubscribeUser("user-co", sub)

	proxy := hs.router.Group("/api/v1/workspaces/:id")
	proxy.POST("/sessions/status-snapshot", hs.handler.RequestSessionStatusSnapshot)

	srv := httptest.NewServer(hs.router)
	defer srv.Close()

	// Post-settle verdict, run after the test body returns. It does NOT
	// rely on server-drain ordering: the fetch counter increments at
	// stub-handler entry (the first statement of the statusz handler),
	// so any fetch that has begun is already counted here, and any
	// second fetch has already tripped the stub's t.Errorf. In the
	// happy path no second fetch can exist past the flight's completion
	// — both triggers coalesced and no further triggers remain. The one
	// escape window (a fetch that has not yet begun by cleanup time) is
	// the disclosed microscopic residual, not something a Close order
	// would close.
	t.Cleanup(func() {
		assert.Equal(t, int32(1), atomic.LoadInt32(&fetches),
			"final verdict: exactly one authority fetch must serve both triggers (got %d)", atomic.LoadInt32(&fetches))
	})

	client := &http.Client{Timeout: 5 * time.Second}
	postTrigger := func() bool {
		resp, err := client.Post(srv.URL+"/api/v1/workspaces/"+busySnapWS+"/sessions/status-snapshot", "", nil)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusAccepted
	}

	// Trigger 1: accepted, then wait until its flight's authority fetch
	// is parked mid-flight — the coalescing window is now provably open
	// and will stay open until release.
	require.True(t, postTrigger(), "trigger 1 must be accepted (202)")
	select {
	case <-firstFetch:
	case <-time.After(3 * time.Second):
		t.Fatalf("flight 1 never reached its authority fetch (fetches=%d)", atomic.LoadInt32(&fetches))
	}

	// Trigger 2 — strictly inside the open window: the first flight is
	// parked mid-fetch, so its map entry is necessarily present (Delete
	// runs only after the fetch returns). The guard MUST coalesce: the
	// 202 proves the guard ran, and the fetch count must still be 1.
	require.True(t, postTrigger(), "trigger 2 must be accepted (202)")
	require.Equal(t, int32(1), atomic.LoadInt32(&fetches),
		"trigger 2 ran while flight 1 was parked mid-fetch and must coalesce to one authority fetch (got %d)", atomic.LoadInt32(&fetches))

	// No snapshot_complete can exist before release: the only flight is
	// parked mid-fetch. One arriving now proves a second flight ran.
	// The begin count narrows the same verdict: a second flight's begin
	// is published IN-PROCESS before its authority fetch (emit:
	// cursor read → begin → fetch), so begins > 1 catches a second,
	// non-coalesced flight even in the window where its fetch has not
	// yet reached the stub — a mutant the complete-only drain is blind
	// to.
	begins := 0
	for drained := false; !drained; {
		select {
		case evt := <-sub.Ch:
			require.NotEqual(t, "session.status.snapshot_complete", evt.Type,
				"snapshot_complete before the parked flight's fetch was released proves a second, non-coalesced flight ran (fetches=%d)", atomic.LoadInt32(&fetches))
			if evt.Type == "session.status.snapshot_begin" {
				begins++
			}
		default:
			drained = true
		}
	}
	require.Equal(t, 1, begins,
		"exactly one snapshot_begin may exist while the only flight is parked mid-fetch (got %d — a second begin is a second, non-coalesced flight)", begins)

	releaseOnce.Do(func() { close(release) })

	// The coalesced flight completes after release. Seeing its
	// snapshot_complete is the happy-path end state: both triggers
	// coalesced, so no further flight — and no further authority fetch
	// — can exist from here on; the cleanup verdict re-checks the
	// count after the body returns (the counter increments at stub
	// entry, so nothing in flight slips past it uncounted).
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-sub.Ch:
			if evt.Type == "session.status.snapshot_complete" {
				return
			}
		case <-deadline:
			t.Fatalf("the coalesced flight never completed after release (fetches=%d)", atomic.LoadInt32(&fetches))
		}
	}
}

// Y1 (#1848 r2 finding 2): a subscriber connecting while an on-demand
// flight is ALREADY running must still get its OWN connect-path flight
// — coalescing belongs to the on-demand trigger only (a fresh
// subscriber has no backlog; the running flight's markers do not reach
// it as a usable seed).
func TestStreamUserEvents_ConnectPathFlightNotCoalesced(t *testing.T) {
	release := make(chan struct{})
	fetched := make(chan struct{})
	var once sync.Once
	hs := newBusySnapHarness(t, "user-join", func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(fetched) }) // first fetch = the on-demand flight's begin is already published
		<-release                          // hold every flight's fetch
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, busyStatuszBody())
	}, nil)

	// Fire the on-demand trigger; wait until its fetch STARTED — its
	// begin marker is provably already on the wire, so the joiner below
	// cannot see it.
	go hs.handler.requestSessionStatusSnapshotSync(busySnapWS)
	select {
	case <-fetched:
	case <-time.After(2 * time.Second):
		t.Fatal("on-demand flight never reached the authority fetch")
	}

	// A subscriber joins mid-flight. The connect-path flight's fetch
	// also parks on the stub, so the begin the joiner receives can ONLY
	// be its own connect-path flight's — the pre-fix code coalesced the
	// connect path into the running flight and emitted NOTHING for the
	// fresh subscriber.
	evts := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		for _, e := range evts {
			if e.Type == "session.status.snapshot_begin" {
				return true
			}
		}
		return false
	})
	sawBegin := false
	for _, e := range evts {
		if e.Type == "session.status.snapshot_begin" {
			sawBegin = true
		}
	}
	assert.True(t, sawBegin,
		"mid-flight joiner got no begin marker — the connect path must not coalesce into a running on-demand flight")

	// Release the stub: both flights complete; the joiner's own flight
	// delivers its complete (its authoritative seed).
	close(release)
	evts2 := collectSSE(t, hs, "", func(evts []apitypes.WorkspaceSSEEvent) bool {
		for _, e := range evts {
			if e.Type == "session.status.snapshot_complete" {
				return true
			}
		}
		return false
	})
	sawComplete := false
	for _, e := range evts2 {
		if e.Type == "session.status.snapshot_complete" {
			sawComplete = true
		}
	}
	assert.True(t, sawComplete,
		"the joiner's own flight must complete (its authoritative seed, not the running flight's markers)")
}

// Z5 (#1848 r3 missing-test ask): fetchStatuszSessions' unhappy-path
// table — every failure class must read NOT-authoritative (ok=false).
func TestFetchStatuszSessions_UnhappyPaths(t *testing.T) {
	newHandler := func(ws *v1.Workspace, wsErr error) *ProxyHandler {
		k8sMock := k8smocks.NewMockKubernetesClient()
		llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
		wsMock := k8smocks.NewMockWorkspaceInterface()
		k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset()).Maybe()
		k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
		llmMock.On("Workspaces", "default").Return(wsMock)
		wsMock.On("Get", mock.Anything, busySnapWS, mock.Anything).Return(ws, wsErr).Maybe()
		h := &ProxyHandler{
			k8sClient: k8sMock,
			logger:    &testLogger{},
			namespace: "default",
			httpClient: &http.Client{
				Timeout:   5 * time.Second,
				Transport: &rewritingTransport{target: "http://127.0.0.1:1"},
			},
		}
		h.SetCachedPasswordForTest(busySnapWS, "pw")
		return h
	}
	activeWS := func() *v1.Workspace {
		return &v1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: busySnapWS, Namespace: "default"},
			Status:     v1.WorkspaceStatus{Phase: v1.WorkspacePhaseActive, PodIP: "10.0.0.9"},
		}
	}

	cases := []struct {
		name string
		h    *ProxyHandler
	}{
		{"k8s client nil", &ProxyHandler{logger: &testLogger{}, namespace: "default"}},
		{"workspace Get error", newHandler(nil, fmt.Errorf("k8s down"))},
	}
	{
		w := activeWS()
		w.Status.Phase = v1.WorkspacePhaseSuspended
		cases = append(cases, struct {
			name string
			h    *ProxyHandler
		}{"non-Active phase", newHandler(w, nil)})
	}
	{
		w := activeWS()
		w.Status.PodIP = ""
		cases = append(cases, struct {
			name string
			h    *ProxyHandler
		}{"empty PodIP", newHandler(w, nil)})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, ok := tc.h.fetchStatuszSessions(context.Background(), busySnapWS)
			assert.False(t, ok, "%s must read NOT-authoritative", tc.name)
			assert.Empty(t, rows)
		})
	}

	// Non-200 + bad JSON via a real stub.
	for name, stub := range map[string]http.HandlerFunc{
		"non-200":  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"bad JSON": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{not json`)) },
		"oversized body": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"sessions":[` + strings.Repeat(`{"id":"x","status":"idle"},`, 200_000) + `]}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(stub)
			defer srv.Close()
			h := newHandler(activeWS(), nil)
			h.httpClient = &http.Client{Timeout: 10 * time.Second, Transport: &rewritingTransport{target: srv.URL}}
			rows, ok := h.fetchStatuszSessions(context.Background(), busySnapWS)
			assert.False(t, ok, "%s must read NOT-authoritative", name)
			assert.Empty(t, rows)
		})
	}
}
