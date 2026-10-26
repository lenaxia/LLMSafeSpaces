// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_spawner_origin_test.go — #1966 pins: every successful
// board/engine fresh-worker spawn records a session_origins mark
// (origin='worker', kind='session') so the sidebar can folder worker
// sessions without any behavior change to the session itself. The mark
// is best-effort (#1915 discipline): a failed write logs and the spawn
// still succeeds; a nil recorder (unwired/embedded) skips marking.

import (
	"context"
	"errors"
	"testing"
	"time"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	"github.com/lenaxia/llmsafespaces/pkg/workflows"
)

type stubOriginRecorder struct {
	rows     []*workflows.SessionOriginRow
	calls    int
	failWith error
}

func (s *stubOriginRecorder) RecordSessionOrigin(ctx context.Context, row *workflows.SessionOriginRow) error {
	s.calls++
	if s.failWith != nil {
		return s.failWith
	}
	cp := *row
	s.rows = append(s.rows, &cp)
	return nil
}

func (s *stubOriginRecorder) DeleteSessionOrigin(ctx context.Context, sessionID string) error {
	return nil
}

func originTestClaim() *board.Claim {
	return &board.Claim{
		TaskID: "3eec1981-b775-4934-bd52-90e9baca03da", Stage: "",
		ProjectID:   "e2fa7e6a-6216-406f-9ff6-45ef337fb155",
		WorkspaceID: "ws-1", ClaimToken: "tok-orig", ShortID: "tsk-amber-fern-99",
		Task: &board.Task{Description: "probe charter"},
	}
}

// TestBoardSpawner_RecordsWorkerOriginMark: the spawn's happy path
// must leave a persistent worker-class origin row — the exact signal
// the #1966 sidebar folder classifies on.
func TestBoardSpawner_RecordsWorkerOriginMark(t *testing.T) {
	act := stubActivator{ip: "10.42.0.7"}
	var captured *apiwf.NodeExecRequest
	exe := stubDispatcher{capture: &captured, session: "sess-fresh-1"}
	rec := &stubOriginRecorder{}
	spawner := newBoardSpawner(&act, &exe, rec, nil)

	id, err := spawner(context.Background(), originTestClaim())
	if err != nil || id != "sess-fresh-1" {
		t.Fatalf("spawn failed: id=%s err=%v", id, err)
	}
	if rec.calls != 1 {
		t.Fatalf("origin mark not recorded exactly once: calls=%d", rec.calls)
	}
	row := rec.rows[0]
	if row.SessionID != "sess-fresh-1" {
		t.Fatalf("mark session id mismatch: %s", row.SessionID)
	}
	if row.WorkspaceID != "ws-1" {
		t.Fatalf("mark workspace mismatch: %s", row.WorkspaceID)
	}
	if row.Origin != types.SessionOriginWorker {
		t.Fatalf("mark origin not the worker class: %q", row.Origin)
	}
	if row.Kind != types.SessionOriginKindSession {
		t.Fatalf("mark kind must be session (the worker session survives): %q", row.Kind)
	}
	if row.Title == "" {
		t.Fatal("mark title empty — the folder row loses its task context")
	}
	if row.CreatedAt.IsZero() || time.Since(row.CreatedAt) > time.Minute {
		t.Fatalf("mark CreatedAt not stamped now: %v", row.CreatedAt)
	}
}

// TestBoardSpawner_OriginMarkFailureStillSpawns: the #1915
// best-effort discipline — a failed mark write must NOT fail the
// spawn (the worker session is alive regardless; the mark only feeds
// presentation).
func TestBoardSpawner_OriginMarkFailureStillSpawns(t *testing.T) {
	act := stubActivator{ip: "10.42.0.7"}
	var captured *apiwf.NodeExecRequest
	exe := stubDispatcher{capture: &captured, session: "sess-fresh-2"}
	rec := &stubOriginRecorder{failWith: errors.New("db down")}
	spawner := newBoardSpawner(&act, &exe, rec, nil)

	id, err := spawner(context.Background(), originTestClaim())
	if err != nil || id != "sess-fresh-2" {
		t.Fatalf("spawn must succeed despite mark failure: id=%s err=%v", id, err)
	}
}

// TestBoardSpawner_NilOriginRecorderSkipsMarking: unwired/embedded
// builds (nil recorder) keep the pre-#1966 behavior — no panic, the
// spawn still lands.
func TestBoardSpawner_NilOriginRecorderSkipsMarking(t *testing.T) {
	act := stubActivator{ip: "10.42.0.7"}
	var captured *apiwf.NodeExecRequest
	exe := stubDispatcher{capture: &captured, session: "sess-fresh-3"}
	spawner := newBoardSpawner(&act, &exe, nil, nil)

	id, err := spawner(context.Background(), originTestClaim())
	if err != nil || id != "sess-fresh-3" {
		t.Fatalf("nil-recorder spawn failed: id=%s err=%v", id, err)
	}
}

// TestBoardSpawner_FailedSpawnRecordsNothing: a dispatch that errors
// must not leave a worker origin row — the mark exists to classify a
// LIVE worker session only.
func TestBoardSpawner_FailedSpawnRecordsNothing(t *testing.T) {
	act := stubActivator{ip: "10.42.0.7"}
	exe := failingDispatcher{}
	rec := &stubOriginRecorder{}
	spawner := newBoardSpawner(&act, &exe, rec, nil)

	if _, err := spawner(context.Background(), originTestClaim()); err == nil {
		t.Fatal("expected spawn error")
	}
	if rec.calls != 0 {
		t.Fatalf("failed spawn must not record an origin mark: calls=%d", rec.calls)
	}
}

// TestBoardSpawner_TriggeredRetentionPrunesOnCreate (#1966 owner
// ruling): a successful spawn fires the retention prune exactly once,
// with the spawn's own activation IP and the fresh session as the keep
// id — detached from the spawn's return.
func TestBoardSpawner_TriggeredRetentionPrunesOnCreate(t *testing.T) {
	act := stubActivator{ip: "10.42.0.7"}
	var captured *apiwf.NodeExecRequest
	exe := stubDispatcher{capture: &captured, session: "sess-fresh-4"}
	rec := &stubOriginRecorder{}
	pruned := make(chan []string, 4)
	retain := func(ctx context.Context, workspaceID, podIP, keepSessionID string) {
		pruned <- []string{workspaceID, podIP, keepSessionID}
	}
	spawner := newBoardSpawner(&act, &exe, rec, retain)

	id, err := spawner(context.Background(), originTestClaim())
	if err != nil || id != "sess-fresh-4" {
		t.Fatalf("spawn failed: id=%s err=%v", id, err)
	}
	select {
	case got := <-pruned:
		if got[0] != "ws-1" || got[1] != "10.42.0.7" || got[2] != "sess-fresh-4" {
			t.Fatalf("retention prune called with wrong args: %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retention prune never ran after a successful spawn")
	}
}

type failingDispatcher struct{}

func (failingDispatcher) Execute(ctx context.Context, workspaceID, podIP string, req *apiwf.NodeExecRequest) (*apiwf.NodeExecResponse, error) {
	return nil, errors.New("agentd unreachable")
}
