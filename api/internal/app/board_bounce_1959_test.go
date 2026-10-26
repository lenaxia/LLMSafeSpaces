package app

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// board_bounce_1959_test.go — #1959 no-regression pins on the spawn
// seam: the bounce's episode discipline. Every bounce round mints a
// FRESH claim token (the verdict-driven bounce's mint), so the
// fallback fresh-implementer spawn rides #1957's boardRunID with a
// never-reused key across rounds — the re-dispatch arm itself never
// touches the workflow RunID (existing-session SendAsync delivery, no
// session:new dispatch), so a re-dispatched original worker can never
// collide with its own first-round charter either.

// TestBoardSpawnRequest_BounceRoundsDistinctRunIDs: three fail-bounce
// rounds of the same task (each round = a fresh minted claim token for
// the fallback's fresh worker) dispatch DISTINCT episode RunIDs —
// #1958's guarantee holds across the #1959 bounce semantics.
func TestBoardSpawnRequest_BounceRoundsDistinctRunIDs(t *testing.T) {
	const taskID = "07aab2d2-71aa-4a7f-8a2c-626a6e78730e" // the live probe task id
	seen := map[string]bool{}
	for round := 1; round <= 3; round++ {
		c := &board.Claim{
			TaskID: taskID, Stage: board.StageInProgress,
			ProjectID:  "e2fa7e6a-6216-404f-9ff6-45ef337fb155",
			ClaimToken: fmt.Sprintf("tok-bounce-r%d", round), // fresh per bounce mint
			Task:       &board.Task{Description: "bounce round charter"},
		}
		req := boardSpawnRequest(c)
		if req.RunID == taskID {
			t.Fatalf("round %d: bare-task-id RunID on the wire (the #1957 live bug): %s", round, req.RunID)
		}
		if !strings.HasPrefix(req.RunID, taskID+"-w-") {
			t.Fatalf("round %d: worker RunID shape lost: %s", round, req.RunID)
		}
		if seen[req.RunID] {
			t.Fatalf("round %d: RunID reuse across bounce rounds: %s", round, req.RunID)
		}
		seen[req.RunID] = true
	}
}

// TestBoardCharterPrompt_RidesSharedVerdictChain: the fallback's fresh
// charter renders the verdict chain through the SAME
// board.RenderVerdictChain the pkg/board re-dispatch message uses —
// the original implementer (re-dispatched or replaced) sees
// byte-equivalent findings.
func TestBoardCharterPrompt_RidesSharedVerdictChain(t *testing.T) {
	idx := 1
	chain := []board.Verdict{{
		Verdict: board.VerdictFail, Reviewer: "rev-1959",
		Findings: []board.VerdictFinding{{Severity: board.SeverityBlocker, Pointer: "/workspace/bounce-final.txt:1 contains 'TOKEN-INITIAL'", CriterionIndex: &idx}},
	}}
	c := &board.Claim{
		TaskID: "07aab2d2-71aa-4a7f-8a2c-626a6e78730e", ShortID: "tsk-orient-songbird-wiggles",
		Stage: "", ClaimToken: "tok-bounce-r1",
		Task: &board.Task{Description: "bounce round charter",
			AcceptanceCriteria: []board.AcceptanceCriterion{{Text: "/workspace/bounce-final.txt exists containing exactly the line TOKEN-FINAL"}}},
		VerdictChain: chain,
	}
	prompt := boardCharterPrompt(c)
	if block := board.RenderVerdictChain(chain); !strings.Contains(prompt, block) {
		t.Fatalf("charter lost the shared verdict-chain block:\nwant substring:\n%s\ngot:\n%s", block, prompt)
	}
}
