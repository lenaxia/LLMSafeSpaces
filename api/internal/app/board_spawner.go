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
	"log/slog"
	"strings"
	"time"

	apiwf "github.com/lenaxia/llmsafespaces/api/internal/workflows"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// boardCharterPrompt builds the fresh worker's charter (ruling R2: the
// description "seeds the worker charter"; criteria + addressing + token
// ride along). A claim carrying a Persona (the stage-persona
// claim-then-spawn mechanism) renders the persona preamble — name,
// stage, optional instructions — before the shared task body; a
// persona-less claim renders today's charter byte-identically.
func boardCharterPrompt(c *board.Claim) string {
	var b strings.Builder
	stage := c.Stage
	if stage == "" {
		stage = board.StageInProgress
	}
	if c.Persona != nil {
		fmt.Fprintf(&b, "You are the '%s' persona for task %s at stage %s (claim token: %s).\n\n", c.Persona.Name, c.ShortID, stage, c.ClaimToken)
		if c.Persona.Instructions != "" {
			fmt.Fprintf(&b, "%s\n\n", c.Persona.Instructions)
		}
	} else {
		fmt.Fprintf(&b, "You are the worker for task %s (claim token: %s).\n\n", c.ShortID, c.ClaimToken)
	}
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
	// #1832 item 5 (2/2): prior review verdicts ride the charter — the
	// iteration history after a fail-bounce (never a failure count).
	// #1959: the block is board.RenderVerdictChain — the SAME renderer
	// the bounce re-dispatch message uses, so the original implementer
	// sees byte-equivalent findings whether re-dispatched or replaced.
	b.WriteString(board.RenderVerdictChain(c.VerdictChain))
	// #1947 (probe 2, live evidence): a persona-bearing charter closes
	// on the PERSONA's outcome — never the implementer loop. The shipped
	// trailer ("Work the task to completion … move the task to in_review
	// when the work is done") rode the reviewer persona's charter too,
	// and the spawned reviewer executed the task's Round 1 and called
	// board_move_stage instead of recording its verdict. The task body
	// above is the persona's working context; the persona-less charter
	// keeps today's trailer byte-identically.
	if c.Persona != nil {
		fmt.Fprintf(&b, "Deliver the '%s' persona's outcome for stage %s per its instructions above — the task\n", c.Persona.Name, stage)
		b.WriteString("body is your working context, not an implementer loop to run. Report progress via board\n")
		b.WriteString("heartbeats. The claim token above authorizes your in-flight writes for this task only.\n")
	} else {
		b.WriteString("Work the task to completion. Report progress via board heartbeats; move the\n")
		b.WriteString("task to in_review when the work is done, attaching evidence. The claim token\n")
		b.WriteString("above authorizes your in-flight writes for this task only.\n")
	}
	return b.String()
}

// boardRunID derives the workflow-agent RunID for a claim's dispatch.
// #1957: EVERY episode must be unique — agentd's workflow-agent mux keys
// charter delivery on the (workflow, run) pair and silently drops a
// re-used key, so a plain worker re-claim (bounce, cooldown re-arm,
// lapse re-feed) whose RunID is the bare task id spawns a session whose
// charter is never delivered (the live stillborn class: zero tokens,
// 15-min lease churn). The persona arm always suffixed; the worker arm
// now does too.
func boardRunID(c *board.Claim) string {
	stageTag := c.Stage
	if stageTag == "" || stageTag == board.StageInProgress {
		stageTag = "w"
	}
	// The claim token is minted fresh per episode — uniqueness by
	// construction, no clock resolution dependence (a wall-clock
	// suffix collides for same-second re-claims; the pin catches it).
	if c.ClaimToken != "" {
		return fmt.Sprintf("%s-%s-%s", c.TaskID, stageTag, c.ClaimToken)
	}
	return fmt.Sprintf("%s-%s-%d", c.TaskID, stageTag, time.Now().UnixNano())
}

// boardSpawnRequest builds the dispatch the spawner wires into
// executor.Execute — extracted so the regression site itself (#1957:
// the RunID reaching the wire) is pinnable without the k8s/HTTP seams.
// The RunID has NO parameter: it derives from boardRunID(c) here;
// a bare-task-id re-inline anywhere in the dispatch path reds the
// dispatch pin (TestBoardSpawner_DispatchCarriesEpisodeRunID).
func boardSpawnRequest(c *board.Claim) *apiwf.NodeExecRequest {
	spec, _ := json.Marshal(map[string]any{
		"prompt":  boardCharterPrompt(c),
		"session": "new", // fresh-charter, preserved: the worker's session outlives the dispatch turn (R9)
	})
	return &apiwf.NodeExecRequest{
		NodeID: "board-worker", NodeType: "agent",
		WorkflowID: "board:" + c.ProjectID, RunID: boardRunID(c),
		Spec: spec, Timeout: "30m",
	}
}

// newBoardSpawner builds the production spawn seam: resolve the target
// workspace's pod, dispatch a fresh-session charter through the agentd
// user mux, and return the minted session id.
// The spawn seam as interfaces (#1957 r4): the production concretes
// (apiwf.K8sWorkspaceActivator, apiwf.HTTPAgentExecutor) satisfy both —
// the constructor's callers are unchanged — while the dispatch closure
// itself becomes pinnable end-to-end with fakes (the reviewer's bypass
// mutation: re-inlining a bare-task-id RunID in the closure must RED).
type spawnActivator interface {
	EnsureActive(ctx context.Context, workspaceID string, timeout time.Duration) (string, error)
}

type spawnDispatcher interface {
	Execute(ctx context.Context, workspaceID, podIP string, req *apiwf.NodeExecRequest) (*apiwf.NodeExecResponse, error)
}

// spawnOriginRecorder is the #1966 narrow seam the spawner marks
// through — the same two verbs as apiwf.SessionOriginStore (satisfied
// structurally by the workflows store). Nil ⇒ marking skipped.
type spawnOriginRecorder interface {
	RecordSessionOrigin(ctx context.Context, row *wf.SessionOriginRow) error
	DeleteSessionOrigin(ctx context.Context, sessionID string) error
}

func newBoardSpawner(activator spawnActivator, executor spawnDispatcher, origins spawnOriginRecorder, retain func(ctx context.Context, workspaceID, podIP, keepSessionID string)) board.WorkerSpawner {
	return func(ctx context.Context, c *board.Claim) (string, error) {
		if c.WorkspaceID == "" {
			return "", fmt.Errorf("board: spawn: claim carries no workspace (task %s)", c.TaskID)
		}
		podIP, err := activator.EnsureActive(ctx, c.WorkspaceID, 2*time.Minute)
		if err != nil {
			return "", fmt.Errorf("board: spawn: activate workspace %s: %w", c.WorkspaceID, err)
		}
		// #1957: boardSpawnRequest derives the RunID via boardRunID
		// internally; drift here (bypassing the builder) reds
		// TestBoardSpawner_DispatchCarriesEpisodeRunID.
		resp, err := executor.Execute(ctx, c.WorkspaceID, podIP, boardSpawnRequest(c))
		if err != nil {
			return "", fmt.Errorf("board: spawn dispatch: %w", err)
		}
		if resp.SessionID == "" {
			return "", fmt.Errorf("board: spawn: agentd returned no session id (errorCode=%s detail=%s)",
				resp.ErrorCode, resp.Detail)
		}
		// #1966: stamp the worker-class origin mark so the sidebar's
		// Workers folder can classify this session (presentation-only
		// grouping). The #1915 best-effort discipline: a failed write
		// logs and the spawn still succeeds — the mark feeds display,
		// not the spawn's outcome. kind='session': the worker session
		// survives its dispatch (R9) and must never be reap-admitted
		// as a carrier.
		if origins != nil {
			title := c.ShortID
			if title == "" {
				title = c.TaskID
			}
			if err := origins.RecordSessionOrigin(ctx, &wf.SessionOriginRow{
				SessionID:   resp.SessionID,
				WorkspaceID: c.WorkspaceID,
				Origin:      types.SessionOriginWorker,
				Kind:        types.SessionOriginKindSession,
				Title:       title,
				CreatedAt:   time.Now().UTC(),
			}); err != nil {
				slog.ErrorContext(ctx, "board: spawn: failed to record worker origin mark",
					"error", err.Error(), "workspaceID", c.WorkspaceID, "sessionId", resp.SessionID, "taskId", c.TaskID)
			}
		}
		// #1966 owner ruling: enforce the worker-session hard cap after
		// the create (on-create prune of the tail). Detached — the
		// retention deletes never block or fail the spawn; the podIP is
		// this spawn's own activation (the delete leg runs against a
		// warm target), and keepSessionID shields the just-created
		// worker (with the grace floor as the second guard).
		if retain != nil {
			go func(workspaceID, podIP, keep string) {
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
				defer cancel()
				retain(rctx, workspaceID, podIP, keep)
			}(c.WorkspaceID, podIP, resp.SessionID)
		}
		return resp.SessionID, nil
	}
}

// The stage-persona claim-then-spawn mechanism (owner directive
// 2026-10-24) absorbed the auto-assigned reviewer's dedicated dispatch
// (#1832 item 5, part 2 — newReviewAssigner/reviewerCharterPrompt): a
// persona-bearing stage entry now rides the SAME spawner above with a
// Persona-carrying Claim (see boardCharterPrompt); the reviewer
// persona's instructions live in pkg/board's DefaultReviewerPersona
// (the legacy reviewColumn=auto + autoAssignReviewer composition) or
// in the stagePersonas config.
