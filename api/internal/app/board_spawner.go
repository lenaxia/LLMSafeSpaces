// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package app

// board_spawner.go — the production fresh-worker spawn (design 0067 §4.4,
// ruling R9): one fresh-charter session per claimed task, created through
// the SAME agentd node-execution seam the workflow engine uses
// ("spawn rides the existing session-creation seams … engine.go
// agent-node execution class" — the apiwf.HTTPAgentExecutor + pod
// activator pair). The charter prompt carries the task's description
// markdown + acceptance criteria + the claim token + short-ID addressing;
// the session id returned by agentd is stamped onto the task with the
// lease re-armed from stamp time (spawn latency never burns the worker's
// lease window).
//
// Lane 1 caveat (declared in the worklog): the dispatch turn's semantics
// are provisional until Lane 3 lands the agentd board_* tool registry —
// the engine treats the spawn as best-effort throughout (failure or a
// lost goroutine heals by ordinary lease lapse; nothing strands).

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"
	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// boardCharterPrompt builds the fresh worker's charter (ruling R2: the
// description "seeds the worker charter"; criteria + addressing + token
// ride along).
func boardCharterPrompt(c *board.Claim) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the worker for task %s (claim token: %s).\n\n", c.ShortID, c.ClaimToken)
	if c.Task != nil {
		if c.Task.Description != "" {
			fmt.Fprintf(&b, "%s\n\n", c.Task.Description)
		}
		if len(c.Task.AcceptanceCriteria) > 0 {
			b.WriteString("Acceptance criteria (all must be met before done, with evidence):\n")
			for _, crit := range c.Task.AcceptanceCriteria {
				mark := " "
				if crit.Met {
					mark = "x"
				}
				fmt.Fprintf(&b, "  [%s] %s\n", mark, crit.Text)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("Work the task to completion. Report progress via board heartbeats; move the\n")
	b.WriteString("task to in_review when the work is done, attaching evidence. The claim token\n")
	b.WriteString("above authorizes your in-flight writes for this task only.\n")
	return b.String()
}

// newBoardSpawner builds the production spawn seam: resolve the target
// workspace's pod, dispatch a fresh-session charter through the agentd
// user mux, and return the minted session id.
func newBoardSpawner(activator *apiwf.K8sWorkspaceActivator, executor *apiwf.HTTPAgentExecutor) board.WorkerSpawner {
	return func(ctx context.Context, c *board.Claim) (string, error) {
		if c.WorkspaceID == "" {
			return "", fmt.Errorf("board: spawn: claim carries no workspace (task %s)", c.TaskID)
		}
		podIP, err := activator.EnsureActive(ctx, c.WorkspaceID, 2*time.Minute)
		if err != nil {
			return "", fmt.Errorf("board: spawn: activate workspace %s: %w", c.WorkspaceID, err)
		}
		spec, _ := json.Marshal(map[string]any{
			"prompt":  boardCharterPrompt(c),
			"session": "new", // fresh-charter, preserved: the worker's session outlives the dispatch turn (R9)
		})
		resp, err := executor.Execute(ctx, c.WorkspaceID, podIP, &apiwf.NodeExecRequest{
			NodeID: "board-worker", NodeType: "agent",
			WorkflowID: "board:" + c.ProjectID, RunID: c.TaskID,
			Spec: spec, Timeout: "30m",
		})
		if err != nil {
			return "", fmt.Errorf("board: spawn dispatch: %w", err)
		}
		if resp.SessionID == "" {
			return "", fmt.Errorf("board: spawn: agentd returned no session id (errorCode=%s detail=%s)",
				resp.ErrorCode, resp.Detail)
		}
		return resp.SessionID, nil
	}
}
