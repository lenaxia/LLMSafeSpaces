// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
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
	// bodyByPrefix overrides the response body for a path prefix
	// (longest prefix wins; absent prefixes serve {}).
	bodyByPrefix map[string]string
}

func (s *stubHarness) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.mu.Lock()
		s.requests = append(s.requests, recordedRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		status := 200
		respBody := "{}"
		for prefix, st := range s.statusByPrefix {
			if strings.HasPrefix(r.URL.Path, prefix) {
				status = st
			}
		}
		for prefix, b := range s.bodyByPrefix {
			if strings.HasPrefix(r.URL.Path, prefix) {
				respBody = b
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
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

	// switch_model → V2 model {"model":{"id","providerID"}} (the >=1.18.15 golden, #1293 r1)
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
	assert.JSONEq(t, `{"model":{"id":"m1","providerID":"p1"}}`, reqs[1].Body) // providerID: the >=1.18.15 golden (#1293 r1)
	assert.Equal(t, "/api/session/s1/switchAgent", reqs[2].Path)
	assert.JSONEq(t, `{"agentID":"plan"}`, reqs[2].Body)
	assert.Equal(t, "/question/q1/reply", reqs[3].Path)
	assert.JSONEq(t, `{"answers":[["Go","notes"]]}`, reqs[3].Body, "options and custom text ride one answer array (the frontend's input contract)")
	assert.Equal(t, "/api/session/s1/compact", reqs[4].Path)
}

// TestOpencodeActor_AnswerPermissionFallback: a 404 on the question
// route for an UNPREFIXED id (agent-agnostic callers) means the input
// is a permission — the reply shape switches. Prefixed ids never probe
// cross-kind (r6: the harness prefix-validates).
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

// TestOpencodeActor_AnswerQuestionIDNeverProbesPermission (r6): a que_
// id's dead ask surfaces the 404 (absence signal) — the old cross-kind
// fallback would have 400'd on the real harness and stranded the record.
func TestOpencodeActor_AnswerQuestionIDNeverProbesPermission(t *testing.T) {
	stub := withStubHarness(t, map[string]int{"/question/": http.StatusNotFound})
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "que_dead", OptionIds: []string{"Yes"}},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 404")

	for _, r := range stub.recorded() {
		assert.NotContains(t, r.Path, "/permission/", "cross-kind posts are forbidden")
	}
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
	// #1372: the unconditional declaration grew from the pinned trio to
	// trio + the four sessions verbs; the probed pair rides on top.
	require.Len(t, actions, 9)

	withStubHarness(t, map[string]int{"/api/session/": http.StatusNoContent})
	_, actions = opencodeActionSurface(probeClient(), "pw")
	require.Len(t, actions, 7, "the unconditional trio + sessions verbs survive absent V2 routes")
}

// TestOpencodeActor_ReplyRoutesDirectlyToPermission (#1310 slice A /
// #1302 contract delta): a set `reply` (permission vocabulary) goes
// straight to /permission/{id}/reply — no question-first probe, no
// lossy option_ids encoding.
func TestOpencodeActor_ReplyRoutesDirectlyToPermission(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "p1", Reply: strPtr("always")},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 1, "no question-first probe on the reply form")
	assert.Equal(t, "/permission/p1/reply", reqs[0].Path)
	assert.JSONEq(t, `{"reply":"always"}`, reqs[0].Body)
}

// TestOpencodeActor_LegacyFormsUnchanged: option_ids/custom_text keep
// the question-first contract (regression pin for the delta).
func TestOpencodeActor_LegacyFormsUnchanged(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "q1", OptionIds: []string{"Go"}, CustomText: strPtr("notes")},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/question/q1/reply", reqs[0].Path)
	assert.JSONEq(t, `{"answers":[["Go","notes"]]}`, reqs[0].Body)
}

func strPtr(s string) *string { return &s }

// TestOpencodeActor_ReplyRejectIsTheDismissExit (4a D1 + r6): reply
// ="reject" on a que_-prefixed id routes to the question REJECT
// endpoint ONLY — the harness prefix-validates (a que_ id on a
// permission endpoint is a 400 Params error, never a 404; captured in
// ask_terminal_states_1_18_15.json), so no cross-kind fallback exists.
// The 404 from the ask's own kind surfaces as the typed NotFound the
// authority's resolve-by-absence folds (S6).
func TestOpencodeActor_ReplyRejectIsTheDismissExit(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "que_1", Reply: sp("reject")},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 1, "reject on a question id must hit exactly the question reject endpoint")
	assert.Equal(t, "/question/que_1/reject", reqs[0].Path)
	assert.JSONEq(t, `{}`, reqs[0].Body)
}

// TestOpencodeActor_ReplyRejectDeadQuestionSurfacesNotFound (r6): the
// dead-ask reject on a que_ id returns the typed NotFound — the absence
// signal for the authority's fold — and NEVER posts cross-kind.
func TestOpencodeActor_ReplyRejectDeadQuestionSurfacesNotFound(t *testing.T) {
	stub := withStubHarness(t, map[string]int{"/question/": http.StatusNotFound})
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "que_dead", Reply: sp("reject")},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status 404", "the 404 from the ask's own kind is the absence signal")

	reqs := stub.recorded()
	for _, r := range reqs {
		assert.NotContains(t, r.Path, "/permission/", "cross-kind posts are forbidden (the harness 400s them)")
	}
}

// TestOpencodeActor_ReplyRejectPermissionDirect (r6): a per_-prefixed
// id rejects straight through the permission reply — no question probe.
func TestOpencodeActor_ReplyRejectPermissionDirect(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "per_1", Reply: sp("reject")},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/permission/per_1/reply", reqs[0].Path)
	assert.JSONEq(t, `{"reply":"reject"}`, reqs[0].Body)
}

// TestOpencodeActor_PermissionReplyMessageForwarded (4a D2): the deny
// feedback rides the permission reply body verbatim.
func TestOpencodeActor_PermissionReplyMessageForwarded(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_AnswerQuestion{
		AnswerQuestion: &abiv1.AnswerInputAction{InputId: "p1", Reply: sp("once"), Message: sp("looks fine")},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/permission/p1/reply", reqs[0].Path)
	assert.JSONEq(t, `{"reply":"once","message":"looks fine"}`, reqs[0].Body, "message preserves the raw passthrough's deny feedback (#1302)")
}

// --- #1372: the sessions-cluster verbs (create/send/delete/rename) ------
//
// The actor mirrors the adapter path's harness wire EXACTLY (the REST
// responses are byte-identical across regimes); the fixtures are the
// opencode seam's own captured testdata (schema_helper_test.go's relative
// -path convention).

func mustReadTestdata(t *testing.T, rel ...string) string {
	t.Helper()
	parts := append([]string{"..", "..", "pkg", "agent", "opencode", "testdata"}, rel...)
	b, err := os.ReadFile(filepath.Join(parts...))
	require.NoError(t, err)
	return string(b)
}

// TestOpencodeActor_SessionVerbs_WireShapes: each verb hits the adapter
// path's own route with the identical body — create POSTs /session with
// the optional title, send POSTs the V1 message route with the parts +
// model object wire, delete DELETEs, rename PATCHes the title.
func TestOpencodeActor_SessionVerbs_WireShapes(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}
	ctx := context.Background()

	_, err := actor.Act(ctx, "", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_CreateSession{
		CreateSession: &abiv1.CreateSessionAction{Title: "New chat"},
	}})
	require.NoError(t, err)

	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_Send{
		Send: &abiv1.SendAction{Text: "hi", Model: &abiv1.ModelRef{Id: "glm-5.3", Provider: "thekaocloud"}},
	}})
	require.NoError(t, err)

	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_Send{
		Send: &abiv1.SendAction{Text: "plain"},
	}})
	require.NoError(t, err)

	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_DeleteSession{}})
	require.NoError(t, err)

	_, err = actor.Act(ctx, "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_RenameSession{
		RenameSession: &abiv1.RenameSessionAction{Title: "Renamed"},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 5)
	assert.Equal(t, "POST", reqs[0].Method)
	assert.Equal(t, "/session", reqs[0].Path)
	assert.JSONEq(t, `{"title":"New chat"}`, reqs[0].Body)

	assert.Equal(t, "POST", reqs[1].Method)
	assert.Equal(t, "/session/s1/message", reqs[1].Path)
	assert.JSONEq(t, `{"parts":[{"type":"text","text":"hi"}],"model":{"modelID":"glm-5.3","providerID":"thekaocloud"}}`, reqs[1].Body,
		"the per-prompt model object wire — the same form Adapter.Send builds")

	assert.Equal(t, "POST", reqs[2].Method)
	assert.Equal(t, "/session/s1/message", reqs[2].Path)
	assert.JSONEq(t, `{"parts":[{"type":"text","text":"plain"}]}`, reqs[2].Body,
		"no model field when the ref is absent (session default applies)")

	assert.Equal(t, "DELETE", reqs[3].Method)
	assert.Equal(t, "/session/s1", reqs[3].Path)
	assert.Empty(t, reqs[3].Body)

	assert.Equal(t, "PATCH", reqs[4].Method)
	assert.Equal(t, "/session/s1", reqs[4].Path)
	assert.JSONEq(t, `{"title":"Renamed"}`, reqs[4].Body,
		"PATCH, not POST — the pinned agent ignores POST bodies (adapter comment, 2026-09-13 loopback L2 leg)")

	// create with NO title POSTs an empty object (adapter parity)
	_, err = actor.Act(ctx, "", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_CreateSession{}})
	require.NoError(t, err)
	reqs = stub.recorded()
	assert.Equal(t, "/session", reqs[5].Path)
	assert.JSONEq(t, `{}`, reqs[5].Body)
}

// TestOpencodeActor_CreateSession_TranslatesFixture: the captured 1.18.10
// session body round-trips into CreateSessionResult with the contract
// fields the REST route serves (id/title/model/cost/time — the same
// translation ParseSessionWire applies on the adapter path).
func TestOpencodeActor_CreateSession_TranslatesFixture(t *testing.T) {
	stub := withStubHarness(t, nil)
	stub.bodyByPrefix = map[string]string{"/session": mustReadTestdata(t, "session_get_1_18_10.json")}
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	res, err := actor.Act(context.Background(), "", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_CreateSession{}})
	require.NoError(t, err)

	s := res.GetCreateSession().GetSession()
	require.NotNil(t, s, "create_session result carries the session")
	assert.Equal(t, "ses_test01KKKKKKKKKKKKKKKKKK", s.GetId())
	assert.Equal(t, "Test Session", s.GetTitle())
	require.NotNil(t, s.GetModel())
	assert.Equal(t, "glm-5.2", s.GetModel().GetId())
	assert.Equal(t, "thekaocloud", s.GetModel().GetProvider())
	require.NotNil(t, s.GetCost())
	assert.Equal(t, int64(4868893), s.GetCost().GetInputTokens())
	assert.Equal(t, int64(761649152), s.GetCost().GetCacheReadTokens())
	require.NotNil(t, s.GetTime())
	assert.NotNil(t, s.GetTime().GetStartedAt())
}

// TestOpencodeActor_Send_TranslatesFixture: the captured 1.18.10 flat-tool
// message body round-trips into SendResult with the contract message (the
// same translation ParseMessageWire applies on the adapter path).
func TestOpencodeActor_Send_TranslatesFixture(t *testing.T) {
	fixture := mustReadTestdata(t, "history_1_18_10_flat_tool.json")
	var arr []json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(fixture), &arr))
	require.GreaterOrEqual(t, len(arr), 2)

	stub := withStubHarness(t, nil)
	stub.bodyByPrefix = map[string]string{"/session/s1/message": string(arr[1])}
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	res, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_Send{
		Send: &abiv1.SendAction{Text: "hi"},
	}})
	require.NoError(t, err)

	m := res.GetSend().GetMessage()
	require.NotNil(t, m, "send result carries the assistant message")
	assert.Equal(t, "msg_fec3d8058001kyxP8bMldMDn42", m.GetId())
	assert.Equal(t, "ses_015c5a29effeVY00SdXAQIuRIL", m.GetSessionId())
	assert.Equal(t, abiv1.MessageType_MESSAGE_TYPE_ASSISTANT, m.GetType())
	require.Len(t, m.GetParts(), 3)
	assert.Equal(t, abiv1.PartType_PART_TYPE_TEXT, m.GetParts()[0].GetType())
	assert.Equal(t, "I'll clone the repository now.", m.GetParts()[0].GetText())
	assert.Equal(t, abiv1.PartType_PART_TYPE_REASONING, m.GetParts()[1].GetType())
	assert.Equal(t, abiv1.PartType_PART_TYPE_TOOL, m.GetParts()[2].GetType())
	assert.Equal(t, "bash", m.GetParts()[2].GetTool().GetName())
	require.NotNil(t, m.GetCreatedAt())
}

// TestOpencodeActor_DeleteSession_NotFoundTyped: a missing session
// surfaces as connect NotFound — the pass-through the REST route maps to
// its existing 502 body (parity), never a swallowed success.
func TestOpencodeActor_DeleteSession_NotFoundTyped(t *testing.T) {
	withStubHarness(t, map[string]int{"/session/s1": http.StatusNotFound})
	actor := opencodeActor{password: "pw", agentKey: "agentID"}

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_DeleteSession{}})
	require.Error(t, err)
	cerr := new(connect.Error)
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, connect.CodeNotFound, cerr.Code())
}

// TestOpencodeActionSurface_SessionVerbsDeclared: the sessions verbs are
// declared unconditionally — their harness routes are the production
// adapter path's own V1 routes (D3).
func TestOpencodeActionSurface_SessionVerbsDeclared(t *testing.T) {
	withStubHarness(t, map[string]int{"/api/session/": http.StatusNoContent})
	_, actions := opencodeActionSurface(probeClient(), "pw")
	want := map[abiv1.ActionType]bool{
		abiv1.ActionType_ACTION_TYPE_CREATE_SESSION: false,
		abiv1.ActionType_ACTION_TYPE_SEND:           false,
		abiv1.ActionType_ACTION_TYPE_DELETE_SESSION: false,
		abiv1.ActionType_ACTION_TYPE_RENAME_SESSION: false,
	}
	for _, a := range actions {
		if _, listed := want[a]; listed {
			want[a] = true
		}
	}
	for at, seen := range want {
		assert.True(t, seen, "action %s must be declared", at)
	}
}

// --- r1 f3: the disk-pressure notice rides the agentd send seams -------
//
// #944's injector moved seams twice and was orphaned twice; the Act
// migration would orphan it a third time (the API-side Wrap decorates the
// ADAPTER, which the authority regime's sends bypass). The authority
// regime's message writes both funnel through agentd — the actor's send
// (sync Act) and the admitter's Admit (outbox Deliver) — so the notice is
// injected there, pod-local (statfs of the workspace volume — fresher
// than the CRD status the API-side reader consumes), fail-open by
// construction, same systemnotices text as the API-side injector.

func withStubbedPodDiskUsage(t *testing.T, used, total uint64, err error) {
	t.Helper()
	orig := podDiskUsage
	t.Cleanup(func() { podDiskUsage = orig })
	podDiskUsage = func() (uint64, uint64, error) { return used, total, err }
}

// TestWithDiskNotice mirrors systemnotices' tiers with the pod-local reader.
func TestWithDiskNotice(t *testing.T) {
	t.Run("warning injects the platform notice", func(t *testing.T) {
		withStubbedPodDiskUsage(t, 92, 100, nil) // 92%: the warning tier (0.90 ≤ r < 0.95)
		got := withDiskNotice("hello")
		assert.Contains(t, got, "hello", "the user text survives")
		assert.NotEqual(t, "hello", got, "the notice is prepended at/above the warning tier")
		assert.True(t, strings.HasPrefix(got, "System notice:"), "platform-authored notice prefix (systemnotices.Notice)")
	})
	t.Run("below threshold is unchanged", func(t *testing.T) {
		withStubbedPodDiskUsage(t, 10, 100, nil)
		assert.Equal(t, "hello", withDiskNotice("hello"))
	})
	t.Run("usage read error fails open", func(t *testing.T) {
		withStubbedPodDiskUsage(t, 0, 0, errors.New("statfs: no such volume"))
		assert.Equal(t, "hello", withDiskNotice("hello"), "a usage read failure must never block the send")
	})
	t.Run("unknown total fails open", func(t *testing.T) {
		withStubbedPodDiskUsage(t, 10, 0, nil)
		assert.Equal(t, "hello", withDiskNotice("hello"))
	})
}

// TestOpencodeActor_Send_InjectsDiskNotice: the sync Act send carries the
// notice on the wire.
func TestOpencodeActor_Send_InjectsDiskNotice(t *testing.T) {
	stub := withStubHarness(t, nil)
	actor := opencodeActor{password: "pw", agentKey: "agentID"}
	withStubbedPodDiskUsage(t, 96, 100, nil)

	_, err := actor.Act(context.Background(), "s1", &abiv1.ActionRequest{Action: &abiv1.ActionRequest_Send{
		Send: &abiv1.SendAction{Text: "hello"},
	}})
	require.NoError(t, err)

	reqs := stub.recorded()
	require.Len(t, reqs, 1)
	var body struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	}
	require.NoError(t, json.Unmarshal([]byte(reqs[0].Body), &body))
	require.Len(t, body.Parts, 1)
	assert.True(t, strings.HasPrefix(body.Parts[0].Text, "System notice:"),
		"the disk notice rides the Act send text: %q", body.Parts[0].Text)
	assert.Contains(t, body.Parts[0].Text, "hello")
}

// TestOpencodeAdmitter_Admit_InjectsDiskNotice: the outbox Deliver path
// carries the same notice.
func TestOpencodeAdmitter_Admit_InjectsDiskNotice(t *testing.T) {
	var gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Parts) > 0 {
			gotText = body.Parts[0].Text
		}
		_, _ = w.Write([]byte(`{"info":{"id":"msg_x"}}`))
	}))
	t.Cleanup(srv.Close)
	orig := agentAddrAtomic.Load()
	t.Cleanup(func() { agentAddrAtomic.Store(orig) })
	agentAddrAtomic.Store(srv.URL)
	withStubbedPodDiskUsage(t, 96, 100, nil)

	a := opencodeAdmitter{password: "pw"}
	_, err := a.Admit(context.Background(), "ses_1", "msg_ob_1", "hello", "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(gotText, "System notice:"),
		"the disk notice rides the delivered text: %q", gotText)
	assert.Contains(t, gotText, "hello")
}
