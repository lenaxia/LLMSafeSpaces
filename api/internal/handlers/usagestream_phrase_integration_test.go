// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// usagestream_phrase_integration_test.go — #1817 piece 3's real-wiring
// acceptance pin. The translated authority stream → the REAL
// usagestream consumer → the REAL usageBridge → the REAL sessionindex
// over Postgres. No fake bridge: the only doubles are the harness's
// authority-side store reader and the event feed (IngestForTest).
//
// Pinned: a phrase-keyed wire frame writes context occupancy to the
// HARNESS row (never a minted phantom), the user-stream event joins on
// the phrase with the harness id additive, the reconnect reconcile
// corrects without minting, and a dead index degrades to the harness id
// without ever attempting a registration for an already-translated frame.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/logger"
	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/api/internal/services/usagestream"
	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/cmd/workspace-agentd/sessionstate"
	abiclient "github.com/lenaxia/llmsafespaces/pkg/abi/abiclient"
	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
)

type phraseIntegrationParser struct{}

func (phraseIntegrationParser) Parse(raw []byte) (*abiv1.Event, bool, error) {
	evt := &abiv1.Event{}
	if err := protojson.Unmarshal(raw, evt); err != nil {
		return nil, true, err
	}
	return evt, true, nil
}

type phraseIntegrationTransport struct{ password string }

func (t phraseIntegrationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.SetBasicAuth(agentd.AuthUsername, t.password)
	return http.DefaultTransport.RoundTrip(r)
}

// countingSessionDB embeds the real database.Service and counts the
// registration seam calls (the only way to observe a mint attempt when
// the DB is deliberately unreachable).
type countingSessionDB struct {
	*database.Service
	ensureCalls int32
}

func (c *countingSessionDB) EnsureSessionRegistered(ctx context.Context, workspaceID, sessionID, harnessType string) (string, error) {
	atomic.AddInt32(&c.ensureCalls, 1)
	return c.Service.EnsureSessionRegistered(ctx, workspaceID, sessionID, harnessType)
}

type phraseIntegrationEnv struct {
	h         *testharness.Harness
	auth      *sessionstate.Authority
	consumer  *usagestream.Consumer
	broker    *eventbroker.UserEventBroker
	sub       *eventbroker.Subscriber
	stream    <-chan *abiv1.StreamFrame
	session   *sessionindex.Service
	db        *countingSessionDB
	workspace string
	owner     string
	harnessID string
	phrase    string
}

func newPhraseIntegrationEnv(t *testing.T) *phraseIntegrationEnv {
	t.Helper()
	h := testharness.New(t)
	env := &phraseIntegrationEnv{
		h:         h,
		db:        &countingSessionDB{Service: &database.Service{DB: h.SQLDB()}},
		workspace: "ws-" + h.ID(),
		owner:     "u-" + h.ID(),
		harnessID: "ses_" + h.ID(),
	}
	env.session = sessionindex.New(env.db, nil)
	ctx := h.NewContext()
	env.phrase = env.session.Register(ctx, env.workspace, env.harnessID, "opencode")
	require.NotEmpty(t, env.phrase, "the harness id must mint a phrase in the real index")

	auth, err := sessionstate.New(sessionstate.Config{
		PlatformDir: t.TempDir(),
		Parser:      phraseIntegrationParser{},
		Passwords:   []string{"pw"},
		IDTranslator: func(harnessID string) string {
			if harnessID == env.harnessID {
				return env.phrase
			}
			return ""
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = auth.Close() })
	env.auth = auth

	_, handler := auth.Handler()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	env.broker = eventbroker.NewUserEventBroker()
	env.broker.RecordWorkspaceOwner(env.workspace, env.owner)
	sub, err := env.broker.SubscribeUser(env.owner)
	require.NoError(t, err)
	t.Cleanup(func() { env.broker.UnsubscribeUser(env.owner, sub) })
	env.sub = sub

	log, _ := logger.NewObserved()
	ph := &ProxyHandler{userBroker: env.broker, sessionIndex: env.session, logger: log}
	env.consumer = usagestream.New(usagestream.Config{
		Resolve: func(context.Context, string) (string, string, error) { return ts.URL, "pw", nil },
		NewClient: func(baseURL, password string) usagestream.Client {
			return abiclient.New(&http.Client{Transport: phraseIntegrationTransport{password: password}}, baseURL)
		},
		Bridge:   &usageBridge{h: ph},
		IdleDrop: time.Hour,
	})
	t.Cleanup(env.consumer.CloseAll)

	frames, cancel, err := auth.Stream(context.Background())
	require.NoError(t, err)
	t.Cleanup(cancel)
	env.stream = frames
	return env
}

func (e *phraseIntegrationEnv) ingest(t *testing.T, evt *abiv1.Event) {
	t.Helper()
	e.auth.IngestForTest(evt)
}

func (e *phraseIntegrationEnv) openAndWait(t *testing.T) {
	t.Helper()
	e.consumer.Open(e.workspace)
	require.Eventually(t, func() bool { return e.auth.Metrics().Subscribers >= 1 },
		30*time.Second, 10*time.Millisecond, "consumer never subscribed to the authority stream")
}

func (e *phraseIntegrationEnv) waitUserEvent(t *testing.T, pred func(apitypes.WorkspaceSSEEvent) bool) apitypes.WorkspaceSSEEvent {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case evt := <-e.sub.Ch:
			if pred(evt) {
				return evt
			}
		case <-deadline:
			t.Fatal("user-stream event never arrived")
		}
	}
}

func waitContractEvent(t *testing.T, frames <-chan *abiv1.StreamFrame, pred func(*abiv1.Event) bool) *abiv1.Event {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case f := <-frames:
			if evt := f.GetEvent().GetEvent(); evt != nil && pred(evt) {
				return evt
			}
		case <-deadline:
			t.Fatal("contract-stream frame never arrived")
		}
	}
}

func (e *phraseIntegrationEnv) sessionRowCount(t *testing.T, sessionID string) int {
	t.Helper()
	var n int
	require.NoError(t, e.h.SQLDB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
		e.workspace, sessionID).Scan(&n))
	return n
}

func TestIntegrationUsageStream_TranslatedFramesJoinPhraseAndWriteHarnessRow(t *testing.T) {
	env := newPhraseIntegrationEnv(t)
	env.openAndWait(t)

	// BUSY emitted by the harness: the authority translates the wire id
	// to the phrase (harness additive); the consumer → real bridge must
	// publish a phrase-keyed user event the frontend can join.
	env.ingest(t, &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: env.harnessID,
		Status:    abiv1.SessionStatus_SESSION_STATUS_BUSY,
	})
	busy := env.waitUserEvent(t, func(evt apitypes.WorkspaceSSEEvent) bool {
		return evt.Type == "session.status" && evt.Status == "busy"
	})
	require.Equal(t, env.phrase, busy.SessionID)
	data, ok := busy.Data.(map[string]any)
	require.True(t, ok, "session.status data must be a map")
	require.Equal(t, env.harnessID, data["internal_session_id"])

	frame := waitContractEvent(t, env.stream, func(evt *abiv1.Event) bool {
		return evt.GetType() == abiv1.EventType_EVENT_TYPE_SESSION_STATUS
	})
	require.Equal(t, env.phrase, frame.GetSessionId())
	require.Equal(t, env.harnessID, frame.GetInternalSessionId())

	// MESSAGE_END cost: input 100 + cacheRead 10 + cacheWrite 5 = 115
	// lands on the HARNESS row, never a phrase-named phantom.
	env.ingest(t, &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_MESSAGE_END,
		SessionId: env.harnessID,
		Message: &abiv1.Message{
			Id:   "msg-" + env.h.ID(),
			Type: abiv1.MessageType_MESSAGE_TYPE_ASSISTANT,
			Cost: &abiv1.Cost{InputTokens: 100, CacheReadTokens: 10, CacheWriteTokens: 5},
		},
	})
	require.Eventually(t, func() bool {
		var used sql.NullInt64
		err := env.h.SQLDB().QueryRowContext(context.Background(),
			`SELECT context_used FROM session_index WHERE workspace_id = $1 AND session_id = $2`,
			env.workspace, env.harnessID).Scan(&used)
		return err == nil && used.Valid && used.Int64 == 115
	}, 30*time.Second, 25*time.Millisecond, "context_used never reached the harness row")

	// IDLE clears the phrase-keyed status.
	env.ingest(t, &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: env.harnessID,
		Status:    abiv1.SessionStatus_SESSION_STATUS_IDLE,
	})
	idle := env.waitUserEvent(t, func(evt apitypes.WorkspaceSSEEvent) bool {
		return evt.Type == "session.status" && evt.Status == "idle"
	})
	require.Equal(t, env.phrase, idle.SessionID)

	// No phantom row keyed by the phrase; exactly one harness row whose
	// short_id is the phrase.
	require.Equal(t, 0, env.sessionRowCount(t, env.phrase), "phrase-named phantom row was minted")
	require.Equal(t, 1, env.sessionRowCount(t, env.harnessID))

	// Three-surface join: list row id, user event id, contract frame id.
	rows, err := env.session.ListByWorkspace(context.Background(), env.workspace)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, env.phrase, rows[0].ID)
	require.Equal(t, env.harnessID, rows[0].InternalID)
	require.Equal(t, env.phrase, rows[0].ShortID)
}

func TestIntegrationUsageStream_ReconnectCorrectionMintsNothing(t *testing.T) {
	env := newPhraseIntegrationEnv(t)
	atomic.StoreInt32(&env.db.ensureCalls, 0)

	// BUSY lands BEFORE the consumer connects: the stream has no replay,
	// so only the snapshot reconcile can deliver it — and that path must
	// stay phrase-keyed without re-registering the phrase.
	env.ingest(t, &abiv1.Event{
		Type:      abiv1.EventType_EVENT_TYPE_SESSION_STATUS,
		SessionId: env.harnessID,
		Status:    abiv1.SessionStatus_SESSION_STATUS_BUSY,
	})
	env.openAndWait(t)

	busy := env.waitUserEvent(t, func(evt apitypes.WorkspaceSSEEvent) bool {
		return evt.Type == "session.status" && evt.Status == "busy" && evt.SessionID == env.phrase
	})
	data, ok := busy.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, env.harnessID, data["internal_session_id"])
	require.Equal(t, int32(0), atomic.LoadInt32(&env.db.ensureCalls),
		"the reconnect correction re-registered the phrase (a mint attempt)")
	require.Equal(t, 0, env.sessionRowCount(t, env.phrase))
	require.Equal(t, 1, env.sessionRowCount(t, env.harnessID))
}

func TestIntegrationUsageStream_DeadIndexDegradesLegacyAndGuardsTranslated(t *testing.T) {
	h := testharness.New(t)

	dsn := h.DSN()
	closedDB, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	require.NoError(t, closedDB.Ping())
	require.NoError(t, closedDB.Close())

	db := &countingSessionDB{Service: &database.Service{DB: closedDB}}
	session := sessionindex.New(db, nil)

	workspace := "ws-" + h.ID()
	owner := "u-" + h.ID()
	harnessID := "ses_" + h.ID()
	phrase := "amber-falcon-glides"

	broker := eventbroker.NewUserEventBroker()
	broker.RecordWorkspaceOwner(workspace, owner)
	sub, err := broker.SubscribeUser(owner)
	require.NoError(t, err)
	t.Cleanup(func() { broker.UnsubscribeUser(owner, sub) })

	log, _ := logger.NewObserved()
	bridge := &usageBridge{h: &ProxyHandler{userBroker: broker, sessionIndex: session, logger: log}}

	// Legacy / heal-window frame: wire id is the harness id, no additive
	// internal. The registration seam IS attempted (once) and fails; the
	// event degrades to the harness id — never worse.
	bridge.SessionStatusWithInternal(workspace, harnessID, "", true)
	legacy := waitForUserEvent(t, sub, func(evt apitypes.WorkspaceSSEEvent) bool {
		return evt.Type == "session.status" && evt.Status == "busy"
	})
	require.Equal(t, harnessID, legacy.SessionID)
	require.Equal(t, int32(1), atomic.LoadInt32(&db.ensureCalls))

	// Already-translated frame (phrase on the wire, harness additive):
	// the guard short-circuits BEFORE the index, so a dead DB never sees
	// a registration attempt.
	bridge.SessionStatusWithInternal(workspace, phrase, harnessID, true)
	translated := waitForUserEvent(t, sub, func(evt apitypes.WorkspaceSSEEvent) bool {
		return evt.Type == "session.status" && evt.Status == "busy" && evt.SessionID == phrase
	})
	data, ok := translated.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, harnessID, data["internal_session_id"])
	require.Equal(t, int32(1), atomic.LoadInt32(&db.ensureCalls),
		"a translated frame attempted a registration against a dead index")
}

func waitForUserEvent(t *testing.T, sub *eventbroker.Subscriber, pred func(apitypes.WorkspaceSSEEvent) bool) apitypes.WorkspaceSSEEvent {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case evt := <-sub.Ch:
			if pred(evt) {
				return evt
			}
		case <-deadline:
			t.Fatal("user-stream event never arrived")
		}
	}
}

var _ interfaces.DatabaseService = (*countingSessionDB)(nil)
