package app

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

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
