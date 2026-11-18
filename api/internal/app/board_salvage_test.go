// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// The salvage probe adapter's unit pins (the board_spawner_test
// posture: fake seams, drive the production closure end-to-end):
//
//   - the dispatch is the sessionless script-node shape (no
//     WorkflowID/RunID — #1957's keyless class; read-only python;
//     lane keys rendered as compile-time constants);
//   - the output mapping (facts → signals; empty/garbage → nil);
//   - every failure leg answers nil (the seam's loud-degradation
//     contract — never a failed claim).

func fakeSalvageScope(ws string, err error) boardScopeResolver {
	return func(_ context.Context, _ string) (string, string, error) {
		if err != nil {
			return "", "", err
		}
		return "user-1", ws, nil
	}
}

type fakeSalvageActivator struct {
	ip  string
	err error
}

func (f *fakeSalvageActivator) EnsureActive(_ context.Context, _ string, _ time.Duration) (string, error) {
	return f.ip, f.err
}

type fakeSalvageExecutor struct {
	req  *apiwf.NodeExecRequest
	resp *apiwf.NodeExecResponse
	err  error
}

func (f *fakeSalvageExecutor) Execute(_ context.Context, _ string, _ string, req *apiwf.NodeExecRequest) (*apiwf.NodeExecResponse, error) {
	f.req = req
	return f.resp, f.err
}

func TestBoardSalvageProbeRequest_Shape(t *testing.T) {
	req := boardSalvageProbeRequest("tsk-retro-textile-picnics", "board/wait-salvage")
	if req.NodeType != "script" {
		t.Fatalf("the probe rides the sessionless script node, got %q", req.NodeType)
	}
	if req.WorkflowID != "" || req.RunID != "" {
		t.Fatalf("the probe carries no harness identity (keyless, sessionless): %q/%q", req.WorkflowID, req.RunID)
	}
	var spec struct {
		Language string `json:"language"`
		Handler  string `json:"handler"`
	}
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		t.Fatalf("spec unmarshal: %v", err)
	}
	if spec.Language != "python" {
		t.Fatalf("language: %q", spec.Language)
	}
	// The lane keys are COMPILE-TIME constants (the declSeed pattern):
	// the pointer's branch and the task's short id render into the
	// script itself — byte-pinnable, no input plumbing.
	for _, needle := range []string{`BRANCH = "board/wait-salvage"`, `SHORT_ID = "tsk-retro-textile-picnics"`} {
		if !strings.Contains(spec.Handler, needle) {
			t.Fatalf("script missing compile-time constant %q", needle)
		}
	}
	// Read-only posture: git inspection + one gh pr list; no write
	// verbs anywhere in the probe.
	for _, verb := range []string{"git push", "git commit", "gh pr create", "gh pr merge", "os.remove", "open("} {
		if strings.Contains(spec.Handler, verb) {
			t.Fatalf("probe script contains a non-read-only verb: %q", verb)
		}
	}
	// The loop-bound discipline is observable in the script: the
	// branch leg (pointer present) resolves ONLY that branch.
	if !strings.Contains(spec.Handler, "refs/heads/") {
		t.Fatal("the branch leg verifies the ref exists")
	}
}

func TestParseBoardSalvageOutput(t *testing.T) {
	sig := parseBoardSalvageOutput(json.RawMessage(`{"branch":"b/x","head":"deadbeef2064","openPr":"2064","uncommitted":true}`))
	if sig == nil {
		t.Fatal("facts must map to signals")
	}
	if sig.Branch != "b/x" || sig.Head != "deadbeef2064" || sig.OpenPR != "2064" || !sig.Uncommitted {
		t.Fatalf("mapping mismatch: %+v", sig)
	}
	if s := parseBoardSalvageOutput(json.RawMessage(`{}`)); s != nil {
		t.Fatal("the empty object is no signals (nothing found)")
	}
	if s := parseBoardSalvageOutput(json.RawMessage(`{"uncommitted":false}`)); s != nil {
		t.Fatal("an all-empty facts set is no signals")
	}
	if s := parseBoardSalvageOutput(json.RawMessage(`not json`)); s != nil {
		t.Fatal("garbage output is no signals")
	}
	if s := parseBoardSalvageOutput(nil); s != nil {
		t.Fatal("absent output is no signals")
	}
}

func salvageTask(branch string) *board.Task {
	t := &board.Task{ID: "task-1", ShortID: "tsk-retro-textile-picnics"}
	if branch != "" {
		t.Salvage = &board.SalvagePointer{Branch: branch, Head: "old", PR: "2000"}
	}
	return t
}

func TestBoardSalvageSource_MapsTheHappyPath(t *testing.T) {
	ex := &fakeSalvageExecutor{resp: &apiwf.NodeExecResponse{
		Output: json.RawMessage(`{"branch":"board/wait-salvage","head":"deadbeef2064","openPr":"2064","uncommitted":false}`),
	}}
	src := newBoardSalvageSource(fakeSalvageScope("ws-1", nil),
		&fakeSalvageActivator{ip: "10.0.0.9"}, ex)

	sig := src.SalvageFor(context.Background(), salvageTask("board/wait-salvage"))
	if sig == nil {
		t.Fatal("the 2064 shape must answer signals")
	}
	if sig.OpenPR != "2064" || sig.Branch != "board/wait-salvage" || sig.Head != "deadbeef2064" {
		t.Fatalf("signals mismatch: %+v", sig)
	}
	if ex.req == nil || ex.req.NodeID != "board-salvage-probe" {
		t.Fatal("the probe dispatched through the node-execution seam")
	}
	// The pointer's branch rendered into the dispatch (the reliable
	// leg — the script probes exactly the recorded lane).
	if !strings.Contains(string(ex.req.Spec), "board/wait-salvage") {
		t.Fatal("the pointer branch rode the dispatch")
	}
}

func TestBoardSalvageSource_FailureLegsAnswerNil(t *testing.T) {
	task := salvageTask("")
	for name, tc := range map[string]struct {
		scope     boardScopeResolver
		activator spawnActivator
		executor  spawnDispatcher
	}{
		"scope resolve fails": {fakeSalvageScope("", errors.New("no row")), &fakeSalvageActivator{ip: "ip"}, &fakeSalvageExecutor{}},
		"activation fails":    {fakeSalvageScope("ws-1", nil), &fakeSalvageActivator{err: errors.New("cold pod")}, &fakeSalvageExecutor{}},
		"dispatch fails":      {fakeSalvageScope("ws-1", nil), &fakeSalvageActivator{ip: "ip"}, &fakeSalvageExecutor{err: errors.New("transport")}},
		"error envelope": {fakeSalvageScope("ws-1", nil), &fakeSalvageActivator{ip: "ip"},
			&fakeSalvageExecutor{resp: &apiwf.NodeExecResponse{ErrorCode: "script_failed", Detail: "boom"}}},
		"empty output": {fakeSalvageScope("ws-1", nil), &fakeSalvageActivator{ip: "ip"},
			&fakeSalvageExecutor{resp: &apiwf.NodeExecResponse{Output: json.RawMessage(`{}`)}}},
	} {
		src := newBoardSalvageSource(tc.scope, tc.activator, tc.executor)
		if sig := src.SalvageFor(context.Background(), task); sig != nil {
			t.Fatalf("%s: failure legs answer nil, got %+v", name, sig)
		}
	}
	// A nil task / nil seams are inert (the zero-value honesty).
	var nilSrc *boardSalvageSource
	if sig := nilSrc.SalvageFor(context.Background(), salvageTask("")); sig != nil {
		t.Fatal("a nil source is inert")
	}
}
