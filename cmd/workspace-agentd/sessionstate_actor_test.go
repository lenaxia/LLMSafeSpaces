// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	abiv1 "github.com/lenaxia/llmsafespaces/pkg/abi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- US-69.9: the opencode actor's wire shapes + the boot route probe ---

type recordedRequest struct {
	Method string
	Path   string
	Body   string
}

type stubHarness struct {
	mu       sync.Mutex
	requests []recordedRequest
	// statusByPrefix overrides the response status for a path prefix
	// (absent prefixes serve 200 {}).
	statusByPrefix map[string]int
}

func (s *stubHarness) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		status := 200
		for prefix, st := range s.statusByPrefix {
			if strings.HasPrefix(r.URL.Path, prefix) {
				status = st
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	})
}

func (s *stubHarness) recorded() []recordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]recordedRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func withStubHarness(t *testing.T, statusByPrefix map[string]int) *stubHarness {
	t.Helper()
	stub := &stubHarness{statusByPrefix: statusByPrefix}
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)
	orig := getAgentAddr()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	agentAddrAtomic.Store(srv.URL)
	return stub
}

// TestProbeActionRoutes_PresentAndAbsent: a typed 400 declares the route
// (and teaches the switchAgent key); a catch-all 204 means absent — the
// 1.18.10 V2-interrupt precedent.
func TestProbeActionRoutes_PresentAndAbsent(t *testing.T) {
	// Both routes present: 400 with the missing-key pointer.
	withStubHarness(t, map[string]int{"/api/session/": http.StatusBadRequest})
	switchAgent, agentKey, compact := probeActionRoutes(probeClient())
	assert.True(t, switchAgent)
	assert.Equal(t, "agentID", agentKey, "the default key holds when the pointer is absent from the body")
	assert.True(t, compact)

	// Both routes absent: catch-all 204 (the removed-route shape).
	withStubHarness(t, map[string]int{"/api/session/": http.StatusNoContent})
	switchAgent, _, compact = probeActionRoutes(probeClient())
	assert.False(t, switchAgent)
	assert.False(t, compact)
}

func sp(s string) *string { return &s }

// probeClient builds the probe client (postHarnessRaw uses
// http.DefaultClient; the OpenCodeClient only gates nil-ness).
func probeClient() *OpenCodeClient {
	return &OpenCodeClient{password: "pw", client: &http.Client{}}
}

// TestOpencodeActor_WireShapes: every verb hits the pinned route with the
// pinned body shape.
func TestOpencodeActor_WireShapes(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}
	ctx := context.Background()

	// interrupt → V1 abort
	_, err := actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_Interrupt{}})
	require.NoError(t, err)

	// switch_model → V2 model {"model":{"id","provider"}}
	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_SwitchModel{
		SwitchModel: &abiv1.SwitchModelAction{Model: &abiv1.ModelRef{Id: "m1", Provider: "p1"}},
	}})
	require.NoError(t, err)

	// switch_agent → V2 switchAgent with the boot-learned key
	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_SwitchAgent{
		SwitchAgent: &abiv1.SwitchAgentAction{AgentId: "plan"},
	}})
	require.NoError(t, err)

	// answer_question → V1 question reply {"answers":[[...]]}
	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "q1", OptionIds: []string{"Go"}, CustomText: sp("notes")},
	}})
	require.NoError(t, err)

	// compact → V2 compact
	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_Compact{}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 5)
	assert.Equal(t, "/session/s1/abort", reqs[0].Path)
	assert.Equal(t, "/api/session/s1/model", reqs[1].Path)
	assert.JSONEq(t, `{"model":{"id":"m1","provider":"p1"}}`, reqs[1].Body)
	assert.Equal(t, "/api/session/s1/switchAgent", reqs[2].Path)
	assert.JSONEq(t, `{"agentID":"plan"}`, reqs[2].Body)
	assert.Equal(t, "/question/q1/reply", reqs[3].Path)
	assert.JSONEq(t, `{"answers":[["Go","notes"]]}`, reqs[3].Body, "options and custom text ride one answer array (the frontend's input contract)")
	assert.Equal(t, "/api/session/s1/compact", reqs[4].Path)
}

// TestOpencodeActor_AnswerPermissionFallback: a 404 on the question route
// means the input is a permission — the reply shape switches.
func TestOpencodeActor_AnswerPermissionFallback(t *testing.T) {
	stub := withStubHarness(t, map[string]int{"/question/": http.StatusNotFound})
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "p1", OptionIds: []string{"always"}},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 2)
	assert.Equal(t, "/question/p1/reply", reqs[0].Path)
	assert.Equal(t, "/permission/p1/reply", reqs[1].Path)
	assert.JSONEq(t, `{"reply":"always"}`, reqs[1].Body, "the first option rides the permission reply field (once/always/reject)")
}

// TestOpencodeActor_HarnessStatusIsTyped: a harness 4xx surfaces as a
// connect InvalidArgument/NotFound, not a generic 500.
func TestOpencodeActor_HarnessStatusIsTyped(t *testing.T) {
	withStubHarness(t, map[string]int{"/session/": http.StatusBadRequest})
	actor := opencodeActor{password: "pw", agentKey: "agentID"}
	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_Interrupt{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 400")
}

// TestOpencodeActionSurface_Declaration: the surface declares the pinned
// trio + the probed pair; absent routes stay undeclared.
func TestOpencodeActionSurface_Declaration(t *testing.T) {
	withStubHarness(t, map[string]int{"/api/session/": http.StatusBadRequest})
	_, actions := opencodeActionSurface(probeClient(), "pw")
	require.Len(t, actions, 5)

	withStubHarness(t, map[string]int{"/api/session/": http.StatusNoContent})
	_, actions = opencodeActionSurface(probeClient(), "pw")
	require.Len(t, actions, 3, "only the regression-pinned trio when the V2 routes are absent")
}
