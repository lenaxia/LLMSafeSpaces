package app

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// TestBoardRunID_EpisodeUnique (#1957): two dispatches of the SAME task
// must never share a RunID — agentd's workflow-agent mux drops duplicate
// (workflow, run) keys, which starved re-claimed tasks of their charter
// (the live stillborn-spawn class).
func TestBoardRunID_EpisodeUnique(t *testing.T) {
	n := 0
	mk := func(stage string) *board.Claim {
		n++
		return &board.Claim{TaskID: "3eec1981-b775-4934-bd52-90e9baca03da", Stage: stage,
			ClaimToken: fmt.Sprintf("tok-%d", n)}
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		for _, stage := range []string{"", board.StageInProgress, board.StageInReview} {
			id := boardRunID(mk(stage))
			if seen[id] {
				t.Fatalf("RunID reuse across episodes: %s", id)
			}
			seen[id] = true
			if !strings.HasPrefix(id, "3eec1981-b775-4934-bd52-90e9baca03da-") {
				t.Fatalf("RunID lost its task prefix: %s", id)
			}
		}
	}
	// worker arms ("" and in_progress) share the "w" tag family; review arms tag their stage.
	if !regexp.MustCompile(`^3eec1981-b775-4934-bd52-90e9baca03da-w-tok-`).MatchString(boardRunID(mk(""))) {
		t.Fatalf("worker RunID shape unexpected: %s", boardRunID(mk("")))
	}
	if !strings.Contains(boardRunID(mk(board.StageInReview)), "-in_review-") {
		t.Fatalf("review RunID shape unexpected: %s", boardRunID(mk(board.StageInReview)))
	}
}

// TestBoardSpawnRequest_WiresTheRunID (#1957 r1): the regression SITE —
// the RunID that actually reaches executor.Execute — carries
// boardRunID's episode-unique value, the fresh-session shape, and the
// charter prompt verbatim.
func TestBoardSpawnRequest_WiresTheRunID(t *testing.T) {
	c := &board.Claim{
		TaskID: "3eec1981-b775-4934-bd52-90e9baca03da", Stage: "",
		ProjectID:  "e2fa7e6a-6216-406f-9ff6-45ef337fb155",
		ClaimToken: "tok-wire", Task: &board.Task{Description: "probe charter"},
	}
	req := boardSpawnRequest(c)
	if req.RunID != boardRunID(c) { // same c — divergence means the site drifted from the pinned helper
		t.Fatalf("dispatch-site RunID drifted from boardRunID: %s", req.RunID)
	}
	if req.RunID != "3eec1981-b775-4934-bd52-90e9baca03da-w-tok-wire" {
		t.Fatalf("RunID not wired verbatim: %s", req.RunID)
	}
	if req.NodeID != "board-worker" || req.NodeType != "agent" {
		t.Fatalf("node addressing changed: %+v", req)
	}
	if req.WorkflowID != "board:e2fa7e6a-6216-406f-9ff6-45ef337fb155" {
		t.Fatalf("workflow key changed: %s", req.WorkflowID)
	}
	var spec map[string]any
	if err := json.Unmarshal(req.Spec, &spec); err != nil {
		t.Fatalf("spec not json: %v", err)
	}
	if spec["session"] != "new" {
		t.Fatalf("fresh-charter session shape lost: %+v", spec)
	}
	if p, _ := spec["prompt"].(string); !strings.Contains(p, "probe charter") {
		t.Fatalf("charter prompt not carried: %q", p)
	}
}

// TestBoardSpawner_DispatchCarriesEpisodeRunID (#1957 r4): THE dispatch
// closure, end-to-end over fakes — the RunID that actually reaches the
// executor must be boardRunID's episode-unique value. The reviewer's
// bypass mutation (re-inlining bare-task-id construction in the
// closure, skipping the builder) reds this pin.
func TestBoardSpawner_DispatchCarriesEpisodeRunID(t *testing.T) {
	c := &board.Claim{
		TaskID: "3eec1981-b775-4934-bd52-90e9baca03da", Stage: "",
		ProjectID:   "e2fa7e6a-6216-406f-9ff6-45ef337fb155",
		WorkspaceID: "ws-1", ClaimToken: "tok-ep1",
		Task: &board.Task{Description: "probe charter"},
	}
	var captured *apiwf.NodeExecRequest
	act := stubActivator{ip: "10.42.0.7"}
	exe := stubDispatcher{capture: &captured, session: "sess-fresh-1"}
	spawner := newBoardSpawner(&act, &exe, nil)
	id, err := spawner(context.Background(), c)
	if err != nil || id != "sess-fresh-1" {
		t.Fatalf("spawn failed: id=%s err=%v", id, err)
	}
	if captured == nil {
		t.Fatal("no dispatch reached the executor")
	}
	if want := boardRunID(c); captured.RunID != want {
		t.Fatalf("dispatch RunID drifted from boardRunID: got %s want %s", captured.RunID, want)
	}
	if captured.RunID == c.TaskID {
		t.Fatalf("regression live: bare-task-id RunID on the wire: %s", captured.RunID)
	}
	// a second episode of the SAME task must dispatch a DIFFERENT RunID
	c2 := *c
	c2.ClaimToken = "tok-ep2"
	captured = nil
	if _, err := spawner(context.Background(), &c2); err != nil {
		t.Fatalf("second-episode spawn failed: %v", err)
	}
	if captured.RunID == boardRunID(c) {
		t.Fatalf("episode collision: second dispatch re-used %s", captured.RunID)
	}
}

type stubActivator struct{ ip string }

func (a *stubActivator) EnsureActive(ctx context.Context, workspaceID string, timeout time.Duration) (string, error) {
	return a.ip, nil
}

type stubDispatcher struct {
	capture **apiwf.NodeExecRequest
	session string
}

func (d *stubDispatcher) Execute(ctx context.Context, workspaceID, podIP string, req *apiwf.NodeExecRequest) (*apiwf.NodeExecResponse, error) {
	*d.capture = req
	return &apiwf.NodeExecResponse{SessionID: d.session}, nil
}

// TestBoardRunID_TokenlessFallbackShape (#1957 r4): the fallback arm
// (no claim token — does not occur in production paths) keeps the
// task-prefix + worker-tag shape with nanosecond uniqueness.
func TestBoardRunID_TokenlessFallbackShape(t *testing.T) {
	c := &board.Claim{TaskID: "tkn", Stage: ""}
	a, b := boardRunID(c), boardRunID(c)
	if !strings.HasPrefix(a, "tkn-w-") || !strings.HasPrefix(b, "tkn-w-") {
		t.Fatalf("fallback shape lost: %s / %s", a, b)
	}
}
