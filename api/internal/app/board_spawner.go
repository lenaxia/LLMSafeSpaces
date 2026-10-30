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
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/board"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// boardCharterPrompt builds the fresh worker's charter (ruling R2: the
// description "seeds the worker charter"; criteria + addressing + token
// ride along). A claim carrying a Persona (the stage-persona
// claim-then-spawn mechanism) renders the ROLE preamble — one line
// naming the referenced agent role; the role's own system prompt (its
// model/tools/behavior, including the reviewer's verdict-channel
// charter) rides the dispatch itself (see boardSpawnRequest), never
// the charter: the board layer composes no behavior (owner ruling
// 2026-10-29, role-only). A persona-less claim renders today's charter
// byte-identically.
func boardCharterPrompt(c *board.Claim) string {
	var b strings.Builder
	stage := c.Stage
	if stage == "" {
		stage = board.StageInProgress
	}
	if c.Persona != nil {
		fmt.Fprintf(&b, "You are the '%s' agent role for task %s at stage %s (claim token: %s).\n\n",
			c.Persona.Label(), c.ShortID, stage, c.ClaimToken)
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
		fmt.Fprintf(&b, "Deliver the '%s' agent role's outcome for stage %s per the role's charter — the task\n", c.Persona.Label(), stage)
		b.WriteString("body is your working context, not an implementer loop to run. If a BOARD LEASE NUDGE\n")
		b.WriteString("message arrives, call board_heartbeat ONCE immediately, then continue. The claim token above\n")
		b.WriteString("authorizes your in-flight writes for this task only.\n")
	} else {
		b.WriteString("Work the task to completion. If a BOARD LEASE NUDGE message arrives (your claim has gone\n")
		b.WriteString("quiet): FIRST call board_heartbeat once to confirm you are active, then continue working if\n")
		b.WriteString("there is more work to do; otherwise move the task to in_review with evidence. The claim token\n")
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
//
// Owner ruling 2026-10-29 (role-only): a persona IS its role
// reference, and spec["agent"] = <role> is the ONLY persona dispatch
// shape (the opencode agentID — the same seam the workflow engine's
// routine fires use, engine.go's spec["agent"]=trigger.Agent). The
// referenced agent role is the single source of truth for the spawned
// worker's model, tool surface, and system prompt; the charter prompt
// is the board's layered context and composes no behavior. A claim
// with no resolvable role (persona-less, or the defensive empty-role
// shape) dispatches with NO agent key — byte-identical to pre-persona.
func boardSpawnRequest(c *board.Claim) *apiwf.NodeExecRequest {
	spec := map[string]any{
		"prompt":  boardCharterPrompt(c),
		"session": "new", // fresh-charter, preserved: the worker's session outlives the dispatch turn (R9)
	}
	if c.Persona != nil && c.Persona.RoleID != "" {
		spec["agent"] = c.Persona.RoleID
	}
	specJSON, _ := json.Marshal(spec)
	return &apiwf.NodeExecRequest{
		NodeID: "board-worker", NodeType: "agent",
		WorkflowID: "board:" + c.ProjectID, RunID: boardRunID(c),
		Spec: specJSON, Timeout: "30m",
	}
}

// boardSpawnRequestPersonalessRetry builds the DEGRADED re-dispatch for
// a spawn the pod rejected with agentd's A8 agent_not_found (the
// referenced role never materialized in that pod's opencode registry —
// a role created after the pod's bootstrap, or a stale tmpfs). The
// claim renders PERSONA-LESS (the charter's persona arm and the
// agent key both drop — byte-identical to a pre-persona spawn), and
// the RunID carries a fresh "-np" suffix: #1957's mux keys charter
// delivery on the (workflow, run) pair and silently DROPS a re-used
// key, so the degraded dispatch must never re-use the rejected
// episode's RunID. One-shot by construction — the degraded spec
// carries no agent key, so it cannot agent_not_found again.
func boardSpawnRequestPersonalessRetry(c *board.Claim) *apiwf.NodeExecRequest {
	degraded := *c
	degraded.Persona = nil
	req := boardSpawnRequest(&degraded)
	req.RunID += "-np"
	return req
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
		// A8 degrade (owner ruling posture: never a silently role-less
		// worker): the pod's opencode registry lacks the referenced
		// role's agent (stale bootstrap — the role vocabulary is
		// delivered at pod boot), and agentd's pre-validation refused
		// the dispatch instead of letting opencode silently fall back
		// to the build agent. Degrade ONCE to the persona-less
		// dispatch (warn-logged; the stored ref stays visible in the
		// config view; the next pod bootstrap materializes the role
		// and the next episode heals). Without this the episode would
		// churn: re-arm re-claims, re-resolves the persona api-side
		// (the role exists there), and re-dispatches into the same
		// rejection forever — the reviewerless-review trap.
		if resp.SessionID == "" && resp.ErrorCode == agentd.ErrorCodeAgentNotFound && c.Persona != nil {
			slog.WarnContext(ctx, "board: spawn: role agent not materialized in the workspace pod (A8 agent_not_found) — degrading to a persona-less dispatch",
				"workspaceID", c.WorkspaceID, "taskId", c.TaskID, "roleId", c.Persona.RoleID, "detail", resp.Detail)
			resp, err = executor.Execute(ctx, c.WorkspaceID, podIP, boardSpawnRequestPersonalessRetry(c))
			if err != nil {
				return "", fmt.Errorf("board: spawn dispatch: %w", err)
			}
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
// Persona-carrying Claim (see boardCharterPrompt). The default
// reviewer composition is the seeded built-in `reviewer` platform
// agent role (board.DefaultReviewerRoleSlug — its system prompt
// carries the verdict-channel charter; the api seeds it at startup),
// and an org/user override points in_review at their own role: either
// way the BEHAVIOR lives in the referenced role (materialized into
// the workspace pod's opencode registry at bootstrap), never in the
// board layer.
