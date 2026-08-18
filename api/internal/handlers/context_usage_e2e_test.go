package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/session"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type contextUsedSessionIndex struct {
	mu          sync.Mutex
	contextUsed map[string]int64
	records     map[string]types.SessionListItem
}

func newContextUsedSessionIndex() *contextUsedSessionIndex {
	return &contextUsedSessionIndex{
		contextUsed: make(map[string]int64),
		records:     make(map[string]types.SessionListItem),
	}
}

func (s *contextUsedSessionIndex) RecordMessage(_, _, _ string, _ time.Time) {}
func (s *contextUsedSessionIndex) DeleteByWorkspace(_ context.Context, _ string) error {
	return nil
}
func (s *contextUsedSessionIndex) DeleteSession(_ context.Context, _, _ string) error {
	return nil
}
func (s *contextUsedSessionIndex) UpsertTitle(_ context.Context, workspaceID, sessionID, title string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := workspaceID + "/" + sessionID
	rec := s.records[key]
	rec.ID = sessionID
	rec.Title = title
	s.records[key] = rec
	return nil
}
func (s *contextUsedSessionIndex) UpsertParent(_ context.Context, _, _, _ string) error {
	return nil
}
func (s *contextUsedSessionIndex) UpdateLastSeen(_ context.Context, _, _ string) error {
	return nil
}
func (s *contextUsedSessionIndex) Start() error { return nil }
func (s *contextUsedSessionIndex) Stop() error  { return nil }

func (s *contextUsedSessionIndex) UpsertContextUsed(_ context.Context, workspaceID, sessionID string, contextUsed int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contextUsed[workspaceID+"/"+sessionID] = contextUsed
	return nil
}

func (s *contextUsedSessionIndex) ListByWorkspace(_ context.Context, workspaceID string) ([]types.SessionListItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var items []types.SessionListItem
	for key, rec := range s.records {
		if len(key) > len(workspaceID)+1 && key[:len(workspaceID)] == workspaceID && key[len(workspaceID)] == '/' {
			if v, ok := s.contextUsed[key]; ok {
				rec.ContextUsed = &v
			}
			items = append(items, rec)
		}
	}
	return items, nil
}

func (s *contextUsedSessionIndex) get(workspaceID, sessionID string) (int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.contextUsed[workspaceID+"/"+sessionID]
	return v, ok
}

func TestE2E_StepEndedEvent_PersistsContextUsed(t *testing.T) {
	si := newContextUsedSessionIndex()
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.handler.SetSessionIndex(si)
	env.handler.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_abc": 1050}))
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	sub, _ := env.handler.userBroker.SubscribeWorkspace("ws-1")
	defer env.handler.userBroker.UnsubscribeWorkspace("ws-1", sub)

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_abc",
			"tokens": {
				"input": 800,
				"output": 400,
				"reasoning": 100,
				"cache": {"read": 200, "write": 50}
			}
		}
	}`)

	v, ok := si.get("ws-1", "ses_abc")
	assert.True(t, ok, "UpsertContextUsed must be called for ses_abc")
	assert.Equal(t, int64(1050), v, "contextUsed = input + cacheRead + cacheWrite = 800+200+50")

	select {
	case evt := <-sub.Ch:
		assert.Equal(t, "agent.event", evt.Type)
		assert.Equal(t, "session.next.step.ended", evt.EventType)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event through broker")
	}
}

func TestE2E_StepEndedEvent_MultipleSessions_TrackedIndependently(t *testing.T) {
	si := newContextUsedSessionIndex()
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.handler.SetSessionIndex(si)
	env.handler.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_1": 5000, "ses_2": 80000}))
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_1",
			"tokens": {"input": 5000, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}}
		}
	}`)
	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_2",
			"tokens": {"input": 80000, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}}
		}
	}`)

	v1, ok1 := si.get("ws-1", "ses_1")
	v2, ok2 := si.get("ws-1", "ses_2")
	assert.True(t, ok1)
	assert.Equal(t, int64(5000), v1, "ses_1 contextUsed")
	assert.True(t, ok2)
	assert.Equal(t, int64(80000), v2, "ses_2 contextUsed")
}

func TestE2E_StepEndedEvent_OverwritesPreviousValue(t *testing.T) {
	si := newContextUsedSessionIndex()
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.handler.SetSessionIndex(si)
	env.handler.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_1": 61500}))
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_1",
			"tokens": {"input": 50000, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}}
		}
	}`)
	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_1",
			"tokens": {"input": 60000, "output": 0, "reasoning": 0, "cache": {"read": 1000, "write": 500}}
		}
	}`)

	v, ok := si.get("ws-1", "ses_1")
	assert.True(t, ok)
	assert.Equal(t, int64(61500), v, "latest step.ended overwrites previous contextUsed")
}

func TestE2E_StepEndedEvent_MissingTokens_NoPersistence(t *testing.T) {
	si := newContextUsedSessionIndex()
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.handler.SetSessionIndex(si)
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	// No stub mapping matches ses_abc without tokens: the adapter reports
	// no usage (ok=false) — matching the real adapter's drift behavior.
	env.handler.SetAdapter(newUsageStubAdapter(nil))

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_abc"
		}
	}`)

	_, ok := si.get("ws-1", "ses_abc")
	assert.False(t, ok, "missing tokens → UpsertContextUsed must not be called")
}

func TestE2E_StepEndedEvent_EmptySessionID_NoPersistence(t *testing.T) {
	si := newContextUsedSessionIndex()
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.handler.SetSessionIndex(si)
	env.handler.SetAdapter(&mockAdapter{contextUsageFn: func(string, string) (string, *session.ContextUsage, bool) {
		return "", &session.ContextUsage{Used: 100}, true // adapter decoded usage but no sessionID
	}})

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "",
			"tokens": {"input": 100, "output": 0, "reasoning": 0, "cache": {"read": 0, "write": 0}}
		}
	}`)

	assert.Empty(t, si.contextUsed, "empty sessionID → no persistence")
}

func TestE2E_ContextUsed_JSONWireFormatThroughRouter(t *testing.T) {
	gin.SetMode(gin.TestMode)

	si := newContextUsedSessionIndex()
	env := newTestEnvWithBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	env.handler.SetSessionIndex(si)
	env.handler.SetAdapter(newUsageStubAdapter(map[string]int64{"ses_rt": 45000}))
	env.handler.userBroker = eventbroker.NewUserEventBroker()
	env.setupWorkspacePodWithT(t, "ws-1", "10.0.0.1", string(v1.WorkspacePhaseActive), "ws-1")
	env.setupPasswordWithT(t, "ws-1", "test-password")
	env.setupWorkspaceWithT(t, "ws-1", 5)

	env.handler.onRawEvent("ws-1", "session.next.step.ended", `{
		"type": "session.next.step.ended",
		"properties": {
			"sessionID": "ses_rt",
			"tokens": {"input": 42000, "output": 0, "reasoning": 0, "cache": {"read": 3000, "write": 0}}
		}
	}`)

	v, ok := si.get("ws-1", "ses_rt")
	require.True(t, ok, "contextUsed persisted")
	require.Equal(t, int64(45000), v, "42000 + 3000 cache read = 45000")

	statusResult := &types.WorkspaceStatusResult{
		Phase:        "Active",
		ContextUsed:  45000,
		ContextTotal: 200000,
		Sessions: []types.SessionStatusItem{
			{ID: "ses_rt", Status: "idle", ContextUsed: 45000},
		},
	}

	raw, err := json.Marshal(statusResult)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"contextUsed":45000`, "contextUsed must survive JSON round-trip")
	assert.Contains(t, string(raw), `"contextTotal":200000`, "contextTotal must survive JSON round-trip")
	assert.Contains(t, string(raw), `"contextUsed":45000`, "per-session contextUsed must survive JSON round-trip")

	var decoded types.WorkspaceStatusResult
	require.NoError(t, json.Unmarshal(raw, &decoded))
	assert.Equal(t, int64(45000), decoded.ContextUsed)
	assert.Equal(t, int64(200000), decoded.ContextTotal)
	require.Len(t, decoded.Sessions, 1)
	assert.Equal(t, int64(45000), decoded.Sessions[0].ContextUsed)
}
