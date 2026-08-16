// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session"
)

// #817 self-healing send recovery: when adapter.Send fails but the turn
// completed server-side (session idle, completion timestamp after the
// submit), the handler must return the assistant's message as a normal
// 200 instead of a 502 — the production signature was exactly this:
// turn persisted (messageCount 129→134), response lost, 502 after 2m5s.

func newRecoveryTestContext(t *testing.T, method, path string, body string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{
		{Key: "id", Value: "ws-1"},
		{Key: "sessionId", Value: "ses_1"},
	}
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	c.Request = httptest.NewRequest(method, path, rd)
	return w, c
}

func idleCompletedSession(completedAgo time.Duration) *session.Session {
	completed := time.Now().Add(-completedAgo)
	return &session.Session{
		ID:     "ses_1",
		Status: session.StatusIdle,
		Time:   &session.TimeRange{StartedAt: completed.Add(-time.Minute), CompletedAt: &completed},
	}
}

func TestSendMessage_Recovery_TurnCompletedServerSide_Returns200(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			// The #817 signature: transport loses the response of an
			// in-flight send long after submission.
			return nil, fmt.Errorf("context deadline exceeded (Post \"http://10.0.0.1:4097/session/ses_1/message\": context deadline exceeded)")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			// Completed at "now" — i.e. after the handler captured
			// submittedAt (the mock chain runs after the send attempt),
			// matching the production shape: turn completed after the
			// prompt was submitted.
			return idleCompletedSession(0), nil
		},
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return []session.Message{
				{ID: "msg_user", Type: session.MessageUser, Parts: []session.Part{{Type: session.PartText, Text: "hi"}}},
				{ID: "msg_asst", Type: session.MessageAssistant, Parts: []session.Part{{Type: session.PartText, Text: "recovered response"}}},
			}, nil
		},
	}

	w, c := newRecoveryTestContext(t, http.MethodPost, "/", `{"parts":[{"type":"text","text":"hi"}]}`)
	h.SendMessage(c)

	require.Equal(t, http.StatusOK, w.Code, "completed turn must be recovered, not 502'd")
	var msg session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msg))
	assert.Equal(t, "msg_asst", msg.ID)
	assert.Equal(t, session.MessageAssistant, msg.Type)
}

func TestSendPromptAsync_Recovery_TurnCompletedServerSide_Returns200(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("connection reset by peer")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return idleCompletedSession(time.Second), nil
		},
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return []session.Message{
				{ID: "msg_asst_async", Type: session.MessageAssistant, Parts: []session.Part{{Type: session.PartText, Text: "async recovered"}}},
			}, nil
		},
	}

	w, c := newRecoveryTestContext(t, http.MethodPost, "/", `{"parts":[{"type":"text","text":"hi"}]}`)
	h.SendPromptAsync(c)

	require.Equal(t, http.StatusOK, w.Code, "the browser /prompt path — the exact #817 endpoint — must recover")
	var msg session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msg))
	assert.Equal(t, "msg_asst_async", msg.ID)
}

func TestSendMessage_Recovery_SessionStillBusy_Keeps502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return &session.Session{ID: "ses_1", Status: session.StatusBusy}, nil
		},
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			t.Fatal("history must not be fetched when the session is busy — the turn is still in flight")
			return nil, nil
		},
	}

	w, c := newRecoveryTestContext(t, http.MethodPost, "/", `{"parts":[{"type":"text","text":"hi"}]}`)
	h.SendMessage(c)
	assert.Equal(t, http.StatusBadGateway, w.Code, "busy session means the turn may still be running — keep the 502")
}

func TestSendMessage_Recovery_StaleCompletion_Keeps502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			// Idle, but completed well BEFORE this prompt was submitted —
			// a previous turn. Must not be mistaken for this turn's result.
			return idleCompletedSession(10 * time.Minute), nil
		},
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			t.Fatal("history must not be fetched for a stale completion")
			return nil, nil
		},
	}

	w, c := newRecoveryTestContext(t, http.MethodPost, "/", `{"parts":[{"type":"text","text":"hi"}]}`)
	h.SendMessage(c)
	assert.Equal(t, http.StatusBadGateway, w.Code, "stale completion is not this turn's response")
}

func TestSendMessage_Recovery_HistoryFetchFails_Keeps502(t *testing.T) {
	h := newProxyHandlerForAdapterTest(t)
	h.adapter = &mockAdapter{
		sendFn: func(_ context.Context, _, _, _, _ string, _ session.SendOpts) (*session.Message, error) {
			return nil, fmt.Errorf("pod unreachable")
		},
		getSessionFn: func(_ context.Context, _, _, _ string) (*session.Session, error) {
			return idleCompletedSession(time.Second), nil
		},
		getHistoryFn: func(_ context.Context, _, _, _ string) ([]session.Message, error) {
			return nil, fmt.Errorf("history fetch failed")
		},
	}

	w, c := newRecoveryTestContext(t, http.MethodPost, "/", `{"parts":[{"type":"text","text":"hi"}]}`)
	h.SendMessage(c)
	assert.Equal(t, http.StatusBadGateway, w.Code, "no fabricated responses — if the message cannot be fetched, fail honestly")
}

// E2E through the real opencode adapter: the backend reproduces the
// production incident — the send POST fails, but the session shows idle
// with a completion timestamp after the submit, and history holds the
// assistant message. The handler must return it as a 200.
func TestE2E_Adapter_SendMessage_FailedSendCompletedTurn_Recovers(t *testing.T) {
	turnCompleted := time.Now().Add(3 * time.Second) // "future" vs submit is irrelevant; must be after it
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message"):
			// The lost-response leg: upstream wedged / errored.
			http.Error(w, `{"error":"upstream wedged"}`, http.StatusBadGateway)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ses_1"):
			// The recovery probe: session idle, completed moments ago
			// (modern opencode wire shape: time.created/updated, ms epoch).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "ses_1",
				"status": "idle",
				"time": map[string]any{
					"created": turnCompleted.Add(-time.Minute).UnixMilli(),
					"updated": turnCompleted.UnixMilli(),
				},
			})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/ses_1/message"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"info": map[string]any{"id": "msg_user", "role": "user", "time": map[string]any{"created": turnCompleted.Add(-2 * time.Second).UnixMilli()}},
					"parts": []map[string]any{
						{"type": "text", "text": "production repro"},
					},
				},
				{
					"info": map[string]any{"id": "msg_asst_e2e", "role": "assistant", "time": map[string]any{"created": turnCompleted.UnixMilli()}},
					"parts": []map[string]any{
						{"type": "text", "text": "recovered via probe"},
					},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)

	env := newE2EEnv(t, backend)

	w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/message",
		strings.NewReader(`{"parts":[{"type":"text","text":"production repro"}]}`))

	require.Equal(t, http.StatusOK, w.Code,
		"send failed but the turn completed server-side — the handler must recover the response")
	var msg session.Message
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &msg))
	assert.Equal(t, "msg_asst_e2e", msg.ID)
	assert.Equal(t, session.MessageAssistant, msg.Type)
	require.Len(t, msg.Parts, 1)
	assert.Equal(t, "recovered via probe", msg.Parts[0].Text)
}
