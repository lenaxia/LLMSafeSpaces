// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
	k8smocks "github.com/lenaxia/llmsafespaces/mocks/kubernetes"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

// flipTestEnv wires the AuthorityFlipHandler against a fake outbox
// (via a real outbox.Service on miniredis) and a fake statusz backend.
func newFlipTestEnv(t *testing.T) (*AuthorityFlipHandler, *ProxyHandler, *httptest.Server, *int64) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var inFlight int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/statusz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(agentd.StatuszResponse{Healthy: true, InFlightDeliveries: inFlight})
	}))
	t.Cleanup(backend.Close)

	k8sMock := k8smocks.NewMockKubernetesClient()
	llmMock := k8smocks.NewMockLLMSafespacesV1Interface()
	wsMock := k8smocks.NewMockWorkspaceInterface()
	k8sMock.On("LlmsafespacesV1").Return(llmMock, nil)
	llmMock.On("Workspaces", "default").Return(wsMock)
	wsCR := &v1.Workspace{}
	wsCR.Name = "ws-1"
	wsCR.Status.Phase = v1.WorkspacePhaseActive
	wsCR.Status.PodIP = "10.0.0.1"
	wsMock.On("Get", mock.Anything, mock.Anything, mock.Anything).Return(wsCR, nil)
	k8sMock.On("Clientset").Return(k8sfake.NewSimpleClientset())
	h, err := NewProxyHandler(k8sMock, &testLogger{}, "default", &http.Client{Transport: &urlRewriteTransport{target: backend.URL}}, nil)
	require.NoError(t, err)
	h.userBroker = eventbroker.NewUserEventBroker()
	h.userBroker.RecordWorkspaceOwner("ws-1", "user-1")
	mr, merr := miniredis.Run()
	require.NoError(t, merr)
	t.Cleanup(mr.Close)
	h.SetOutbox(outbox.New(redis.NewClient(&redis.Options{Addr: mr.Addr()})))
	t.Cleanup(stubUsageStream())

	return NewAuthorityFlipHandler(h, &testLogger{}), h, backend, &inFlight
}

func postJSON(t *testing.T, h gin.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	h(c)
	return w
}

// TestFlipGate_ParkWithReason (flipgate_park_with_reason, US-69.13):
// the gate parks a workspace's in-flight entries with the explicit
// mode_transition reason — visible, never auto-re-sent.
func TestFlipGate_ParkWithReason(t *testing.T) {
	flip, h, _, _ := newFlipTestEnv(t)

	// Seed via direct queue write (Accept would deliver immediately).
	raw, _ := json.Marshal(map[string]any{"id": "e1", "clientMessageID": "cm-e1", "userID": "u1", "text": "hi", "acceptedAt": time.Now().UTC(), "status": "pending"})
	require.NoError(t, h.outbox.SeedEntryForTest(context.Background(), "ws-1", "ses-1", string(raw)))

	w := postJSON(t, flip.Park, parkRequest{WorkspaceID: "ws-1", Reason: "authority flip"})
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct{ Parked int }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Parked)

	// Unpark round-trips (the rollback drain).
	w = postJSON(t, flip.Unpark, parkRequest{WorkspaceID: "ws-1", Reason: "rollback"})
	require.Equal(t, http.StatusOK, w.Code)
	var resp2 struct{ Unparked int }
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp2))
	assert.Equal(t, 1, resp2.Unparked)
}

func TestFlipGate_ParkValidation(t *testing.T) {
	flip, _, _, _ := newFlipTestEnv(t)
	w := postJSON(t, flip.Park, parkRequest{WorkspaceID: ""})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = postJSON(t, flip.Park, parkRequest{WorkspaceID: "ws-1"})
	assert.Equal(t, http.StatusBadRequest, w.Code, "reason is required — the park is always explicit")
}

// TestFlipGate_InFlight: the drain signal reads the pod's
// ledger_in_flight off statusz.
func TestFlipGate_InFlight(t *testing.T) {
	flip, h, _, inFlight := newFlipTestEnv(t)
	*inFlight = 3

	// The K8s mock resolves ws-1 to an Active pod at 10.0.0.1; the
	// rewriting transport sends the statusz GET to the fake backend.
	require.NotNil(t, h)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	c.Params = gin.Params{{Key: "workspaceId", Value: "ws-1"}}
	flip.InFlight(c)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		InFlight int64 `json:"inFlight"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, int64(3), resp.InFlight)
}
