// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"os"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	agentoc "github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// epic-71 / 4a increment 2 (#1302 items 1/3/4): ONE contract shape on
// REST + SSE — session.InputRequest (root-session resolved), with the
// whileAway re-presentation marker riding the SSE ENVELOPE (D3: the ABI
// InputRequest is agent-facing; a browser-only flag does not belong on
// it). Prefix-specific validation moves behind the dialect seam: the
// handlers enforce the generic request-ID contract.

func newOneShapeEnv(t *testing.T, listFn func(ctx context.Context, userID, workspaceID, sessionID string) ([]session.InputRequest, error)) (*ProxyHandler, *inbox.Service, *miniredis.Miniredis) {
	t.Helper()
	h := newProxyHandlerForAdapterTest(t)
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	in := inbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	h.SetInboxStoreForTest(in)
	h.state().SetWorkspaceConfig(context.Background(), "ws-1", wsstate.Config{})
	h.adapter = &mockAdapter{listPendingFn: listFn}
	return h, in, mr
}

func ginTestCtx(t *testing.T, method, target string, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

func TestOneShape_ListQuestionsReturnsInputRequest(t *testing.T) {
	h, _, _ := newOneShapeEnv(t, func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
		return []session.InputRequest{{
			ID: "que_1", SessionID: "ses_1", Kind: session.InputQuestion,
			Question: "Go?", Header: "Pick",
			Options: []session.InputOption{{Label: "Go", Description: "fast"}},
			Custom:  true,
			Tool:    &session.ToolRef{MessageID: "msg_1", CallID: "call_1"},
		}}, nil
	})

	c, w := ginTestCtx(t, http.MethodGet, "/workspaces/ws-1/question", "")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	h.ListQuestions(c)
	require.Equal(t, http.StatusOK, w.Code)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	req := out[0]
	// The contract shape: flattened, camelCase, kind-discriminated.
	assert.Equal(t, "que_1", req["id"])
	assert.Equal(t, "ses_1", req["sessionId"])
	assert.Equal(t, "question", req["kind"])
	assert.Equal(t, "Go?", req["question"])
	assert.Equal(t, "Pick", req["header"])
	assert.Equal(t, true, req["custom"])
	tool, ok := req["tool"].(map[string]any)
	require.True(t, ok, "tool: %v", req["tool"])
	assert.Equal(t, "msg_1", tool["messageId"])
	// The legacy envelope is GONE: no nested questions[] array.
	assert.NotContains(t, req, "questions")
	assert.NotContains(t, req, "session_id")
}

func TestOneShape_ListPermissionsReturnsInputRequest(t *testing.T) {
	h, _, _ := newOneShapeEnv(t, func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
		return []session.InputRequest{{
			ID: "per_1", SessionID: "ses_1", Kind: session.InputPermission,
			Permission: "bash", Patterns: []string{"ls"}, Always: []string{"ls *"},
		}}, nil
	})

	c, w := ginTestCtx(t, http.MethodGet, "/workspaces/ws-1/permission", "")
	c.Params = gin.Params{{Key: "id", Value: "ws-1"}}
	h.ListPermissions(c)
	require.Equal(t, http.StatusOK, w.Code)

	var out []map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	require.Len(t, out, 1)
	assert.Equal(t, "per_1", out[0]["id"])
	assert.Equal(t, "permission", out[0]["kind"])
	assert.Equal(t, "bash", out[0]["permission"])
	assert.Equal(t, []any{"ls"}, out[0]["patterns"])
	assert.NotContains(t, out[0], "session_id")
}

func TestOneShape_SSEEmitsInputRequest_EnvelopeWhileAway(t *testing.T) {
	h, in, _ := newOneShapeEnv(t, func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
		return []session.InputRequest{{
			ID: "que_live", SessionID: "ses_1", Kind: session.InputQuestion,
			Question: "Live?", Options: []session.InputOption{{Label: "A", Description: "a"}},
		}}, nil
	})
	require.NoError(t, in.Record(context.Background(), "ws-1", inbox.Record{
		ID: "que_away", SessionID: "ses_2", Kind: inbox.KindQuestion, Status: inbox.StatusPending,
		Question: "Away?", RecordedAt: time.Now().UTC().Add(-time.Minute),
		Options: []inbox.Option{{Label: "B", Description: "b"}},
	}))

	userSub, _ := h.userBroker.SubscribeUser("user-1")
	defer h.userBroker.UnsubscribeUser("user-1", userSub)

	h.emitPendingInputRequests(context.Background(), "ws-1")

	_ = recvWithTimeout(t, userSub, "agent.input.snapshot_begin")
	var liveEvt, awayEvt *apitypes.WorkspaceSSEEvent
	for i := 0; i < 2; i++ {
		evt := recvWithTimeout(t, userSub, "agent.question")
		if evt.RequestID == "que_live" {
			liveEvt = &evt
		}
		if evt.RequestID == "que_away" {
			awayEvt = &evt
		}
	}
	marker := recvWithTimeout(t, userSub, "agent.input.snapshot_complete")
	require.NotNil(t, marker.SnapshotOK)
	assert.True(t, *marker.SnapshotOK)

	require.NotNil(t, liveEvt, "live event emitted")
	require.NotNil(t, awayEvt, "whileAway event emitted")

	// Live: InputRequest in Data, NO envelope marker.
	b, _ := json.Marshal(liveEvt.Data)
	var liveShape map[string]any
	require.NoError(t, json.Unmarshal(b, &liveShape))
	assert.Equal(t, "question", liveShape["kind"])
	assert.Equal(t, "Live?", liveShape["question"])
	assert.Nil(t, liveEvt.WhileAway, "live asks carry no envelope marker")

	// whileAway: InputRequest in Data, marker on the ENVELOPE (D3).
	b2, _ := json.Marshal(awayEvt.Data)
	var awayShape map[string]any
	require.NoError(t, json.Unmarshal(b2, &awayShape))
	assert.Equal(t, "question", awayShape["kind"])
	assert.Equal(t, "Away?", awayShape["question"])
	assert.NotContains(t, awayShape, "whileAway", "the marker is NOT on the ABI shape")
	require.NotNil(t, awayEvt.WhileAway, "the envelope carries the whileAway marker")
	assert.True(t, *awayEvt.WhileAway)
}

func TestOneShape_GenericRequestIDValidation(t *testing.T) {
	// #1302 item 3: the handler enforces the GENERIC contract — charset
	// [a-zA-Z0-9._-], length ≤128, no "..". Both que_-prefixed and
	// non-prefixed IDs are accepted; the que_/per_ literals live behind
	// the dialect seam only.
	h, _, _ := newOneShapeEnv(t, func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
		return nil, nil
	})
	h.adapter = &mockAdapter{
		listPendingFn: func(_ context.Context, _, _, _ string) ([]session.InputRequest, error) {
			return nil, nil
		},
		rejectInputFn: func(_ context.Context, _, _, _ string) error { return nil },
	}
	cases := []struct {
		id    string
		valid bool
	}{
		{"que_abc123", true},
		{"per_abc_123", true},
		{"req.plain-id_1", true}, // non-prefixed accepted (agent-agnostic)
		{"", false},
		{"has space", false},
		{"has/slash", false},
		{"has..traversal", false},
		{string(make([]byte, 129)), false},
	}
	for _, tc := range cases {
		// pad the long case to a valid charset so ONLY length fails
		id := tc.id
		if len(id) == 129 {
			id = string(make([]byte, 0))
			for i := 0; i < 129; i++ {
				id += "a"
			}
		}
		c, w := ginTestCtx(t, http.MethodPost, "/x", `{}`)
		c.Params = gin.Params{{Key: "id", Value: "ws-1"}, {Key: "requestID", Value: id}}
		h.QuestionReject(c)
		if tc.valid {
			assert.NotEqual(t, http.StatusBadRequest, w.Code, "id %q must pass the generic contract", id)
		} else {
			assert.Equal(t, http.StatusBadRequest, w.Code, "id %q must fail the generic contract", id)
		}
	}
}

func TestOneShape_DialectOwnsPrefixes(t *testing.T) {
	// The que_/per_ literals are behind the seam: the dialect exposes
	// the kind-by-prefix mapping; the handlers do not match prefixes.
	d := &agentoc.Dialect{}
	assert.Equal(t, "question", d.KindByPrefix("que_1"))
	assert.Equal(t, "permission", d.KindByPrefix("per_1"))
	assert.Equal(t, "", d.KindByPrefix("req_other"))
	// And the handlers no longer carry the literals.
	src, err := os.ReadFile("proxy_input.go")
	require.NoError(t, err)
	assert.NotContains(t, string(src), "que_[a-zA-Z0-9]", "prefix regexes stay out of the handler")
}
