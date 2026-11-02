// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_spawner_a8_degrade_test.go — the A8 degrade pin (review round 2
// of the role-only model, PR #1990 F1): when agentd's pre-validation
// rejects a persona'd dispatch with agent_not_found (the referenced
// role never materialized in that pod's opencode registry — opencode
// itself would have silently fallen back to the build agent), the
// spawner degrades ONCE to the persona-less dispatch with a FRESH
// RunID (the #1957 mux drops a re-used (workflow, run) charter key),
// and any OTHER error code keeps today's spawn-failure contract.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// a8StubDispatcher records every dispatch and answers from a scripted
// queue of responses (one per Execute call).
type a8StubDispatcher struct {
	resps    []*apiwf.NodeExecResponse
	reqs     []*apiwf.NodeExecRequest
	spawnErr error
}

func (d *a8StubDispatcher) Execute(ctx context.Context, workspaceID, podIP string, req *apiwf.NodeExecRequest) (*apiwf.NodeExecResponse, error) {
	d.reqs = append(d.reqs, req)
	if d.spawnErr != nil {
		return nil, d.spawnErr
	}
	if len(d.resps) == 0 {
		panic("a8StubDispatcher: scripted responses exhausted")
	}
	r := d.resps[0]
	d.resps = d.resps[1:]
	return r, nil
}

func a8PersonaClaim() *board.Claim {
	return &board.Claim{
		TaskID: "3eec1981-b775-4934-bd52-90e9baca03da", Stage: board.StageInReview,
		ProjectID:   "e2fa7e6a-6216-406f-9ff6-45ef337fb155",
		WorkspaceID: "ws-a8", ClaimToken: "tok-a8",
		ShortID: "tsk-a8-degrade",
		Persona: &board.PersonaConfig{RoleID: "11111111-2222-3333-4444-555555555555"},
		Task:    &board.Task{Description: "probe charter"},
	}
}

// TestBoardSpawner_A8DegradesPersonalessOnce — the happy degrade: the
// first dispatch (agent = roleID) is refused agent_not_found; the
// spawner re-dispatches persona-less and returns THAT session. The
// degraded dispatch must carry NO agent key, NO persona-arm charter,
// and a RunID DISTINCT from the refused episode's (#1957).
func TestBoardSpawner_A8DegradesPersonalessOnce(t *testing.T) {
	c := a8PersonaClaim()
	exe := &a8StubDispatcher{resps: []*apiwf.NodeExecResponse{
		{ErrorCode: agentd.ErrorCodeAgentNotFound, Detail: "agent \"11111111-2222-3333-4444-555555555555\" not found"},
		{SessionID: "sess-degraded-1"},
	}}
	spawner := newBoardSpawner(&stubActivator{ip: "10.42.0.7"}, exe, nil, nil)
	id, err := spawner(context.Background(), c)
	if err != nil || id != "sess-degraded-1" {
		t.Fatalf("degraded spawn failed: id=%s err=%v", id, err)
	}
	if len(exe.reqs) != 2 {
		t.Fatalf("expected exactly 2 dispatches (refused + degraded), got %d", len(exe.reqs))
	}
	first, second := exe.reqs[0], exe.reqs[1]

	var spec1, spec2 map[string]any
	if err := json.Unmarshal(first.Spec, &spec1); err != nil {
		t.Fatalf("first spec not json: %v", err)
	}
	if err := json.Unmarshal(second.Spec, &spec2); err != nil {
		t.Fatalf("degraded spec not json: %v", err)
	}
	if spec1["agent"] != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("the refused dispatch must carry the role reference: %+v", spec1)
	}
	if _, ok := spec2["agent"]; ok {
		t.Fatalf("the degraded dispatch must carry NO agent key: %+v", spec2)
	}
	if p, _ := spec2["prompt"].(string); strings.Contains(p, "agent role for task") {
		t.Fatalf("the degraded charter must be the persona-less render (no role preamble): %q", p)
	}
	if p, _ := spec2["prompt"].(string); !strings.Contains(p, "Work the task to completion") {
		t.Fatalf("the degraded charter must carry the persona-less trailer: %q", p)
	}
	if second.RunID == first.RunID {
		t.Fatalf("the degraded dispatch re-used the refused episode's RunID (#1957 charter drop): %s", second.RunID)
	}
	if !strings.HasSuffix(second.RunID, "-np") {
		t.Fatalf("the degraded RunID must carry the -np marker: %s", second.RunID)
	}
}

// TestBoardSpawner_A8DegradeIsOneShot — a persona-LESS claim refused
// agent_not_found (cannot happen — no agent key is dispatched) must
// NOT loop or degrade: the spawn fails with today's contract.
func TestBoardSpawner_A8DegradeIsOneShot(t *testing.T) {
	c := a8PersonaClaim()
	c.Persona = nil
	exe := &a8StubDispatcher{resps: []*apiwf.NodeExecResponse{
		{ErrorCode: agentd.ErrorCodeAgentNotFound},
	}}
	spawner := newBoardSpawner(&stubActivator{ip: "10.42.0.7"}, exe, nil, nil)
	id, err := spawner(context.Background(), c)
	if err == nil || id != "" {
		t.Fatalf("a persona-less agent_not_found must fail the spawn (no degrade arm): id=%s err=%v", id, err)
	}
	if len(exe.reqs) != 1 {
		t.Fatalf("expected exactly 1 dispatch (no persona to degrade), got %d", len(exe.reqs))
	}
}

// TestBoardSpawner_OtherErrorCodesKeepTheFailureContract — only the A8
// code degrades; any other errorCode (e.g. agent_list_failed, script
// failures) keeps the spawn-failure contract so transient outages heal
// by lease lapse instead of silently downgrading the worker.
func TestBoardSpawner_OtherErrorCodesKeepTheFailureContract(t *testing.T) {
	for _, code := range []string{"agent_list_failed", "script_failed", "session_create_failed", ""} {
		exe := &a8StubDispatcher{resps: []*apiwf.NodeExecResponse{{ErrorCode: code, Detail: "boom"}}}
		spawner := newBoardSpawner(&stubActivator{ip: "10.42.0.7"}, exe, nil, nil)
		id, err := spawner(context.Background(), a8PersonaClaim())
		if err == nil || id != "" {
			t.Fatalf("errorCode %q must fail the spawn: id=%s err=%v", code, id, err)
		}
		if len(exe.reqs) != 1 {
			t.Fatalf("errorCode %q: expected exactly 1 dispatch, got %d", code, len(exe.reqs))
		}
	}
}

// TestBoardSpawnRequestPersonalessRetry_Shape — the degraded request
// builder pinned directly: persona-less spec + suffixed RunID, derived
// from the SAME claim (nothing else may drift).
func TestBoardSpawnRequestPersonalessRetry_Shape(t *testing.T) {
	c := a8PersonaClaim()
	base := boardSpawnRequest(c)
	degraded := boardSpawnRequestPersonalessRetry(c)
	if degraded.RunID != base.RunID+"-np" {
		t.Fatalf("degraded RunID must be the base + -np: got %s want %s-np", degraded.RunID, base.RunID)
	}
	var spec map[string]any
	if err := json.Unmarshal(degraded.Spec, &spec); err != nil {
		t.Fatalf("degraded spec not json: %v", err)
	}
	if _, ok := spec["agent"]; ok {
		t.Fatalf("degraded spec must carry no agent key: %+v", spec)
	}
	if spec["session"] != "new" {
		t.Fatalf("degraded spec must keep the fresh-session contract: %+v", spec)
	}
	if degraded.WorkflowID != base.WorkflowID || degraded.NodeID != base.NodeID {
		t.Fatalf("degraded request must not drift identity fields: %+v vs %+v", degraded, base)
	}
}
