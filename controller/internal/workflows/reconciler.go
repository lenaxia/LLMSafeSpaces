// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Epic 64: Workflow reconciler — the load-bearing execution engine.
// Implements manager.Runnable + NeedLeaderElection so only one controller
// replica drives runs. Claims queued runs, ensures workspace Active, drives
// nodes in DAG order, persists after each transition.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// WorkflowReconciler drives workflow runs from queued to terminal.
type WorkflowReconciler struct {
	Store         ReconcilerStore
	AgentdClient  AgentdExecutor
	K8sClient     WorkspaceActivator
	Logger        ReconcilerLogger
	MaxConcurrent int
	TickInterval  time.Duration

	cancelMu     sync.Mutex
	canceledRuns map[string]struct{}
}

type ReconcilerStore interface {
	ClaimQueuedRuns(ctx context.Context, limit int) ([]*wf.WorkflowRunRow, error)
	UpdateWorkflowRunStatus(ctx context.Context, runID, status string, errorCode *string, errMsg json.RawMessage, output json.RawMessage) error
	CreateNodeRun(ctx context.Context, row *wf.WorkflowNodeRunRow) error
	UpdateNodeRunStatus(ctx context.Context, nodeRunID, status string, output json.RawMessage, branch *string, errorCode *string, errMsg json.RawMessage) error
	IncrementTriggerFailures(ctx context.Context, triggerID string) (int, error)
	ResetTriggerFailures(ctx context.Context, triggerID string) error
}

type AgentdExecutor interface {
	Execute(ctx context.Context, podIP string, req *NodeExecRequest) (*NodeExecResponse, error)
}

type NodeExecRequest struct {
	NodeID   string          `json:"nodeId"`
	NodeType string          `json:"nodeType"`
	Spec     json.RawMessage `json:"spec"`
	Input    json.RawMessage `json:"input"`
	Timeout  string          `json:"timeout,omitempty"`
}

type NodeExecResponse struct {
	Output    json.RawMessage `json:"output,omitempty"`
	Branch    string          `json:"branch,omitempty"`
	ErrorCode string          `json:"errorCode,omitempty"`
	Detail    string          `json:"detail,omitempty"`
}

type WorkspaceActivator interface {
	EnsureActive(ctx context.Context, workspaceID string, timeout time.Duration) (podIP string, err error)
}

type ReconcilerLogger interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}

func (r *WorkflowReconciler) Start(ctx context.Context) error {
	logger := r.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	if r.canceledRuns == nil {
		r.canceledRuns = make(map[string]struct{})
	}

	maxConc := r.MaxConcurrent
	if maxConc <= 0 {
		maxConc = 10
	}
	tick := r.TickInterval
	if tick <= 0 {
		tick = 10 * time.Second
	}

	sem := make(chan struct{}, maxConc)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	r.claimAndDispatch(ctx, logger, sem)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.claimAndDispatch(ctx, logger, sem)
		}
	}
}

func (r *WorkflowReconciler) NeedLeaderElection() bool { return true }

func (r *WorkflowReconciler) claimAndDispatch(ctx context.Context, logger ReconcilerLogger, sem chan struct{}) {
	runs, err := r.Store.ClaimQueuedRuns(ctx, cap(sem))
	if err != nil {
		logger.Error(err, "failed to claim queued runs")
		return
	}
	for _, run := range runs {
		select {
		case sem <- struct{}{}:
			go func(run *wf.WorkflowRunRow) {
				defer func() { <-sem }()
				r.executeRun(ctx, logger, run)
			}(run)
		case <-ctx.Done():
			return
		}
	}
}

func (r *WorkflowReconciler) executeRun(ctx context.Context, logger ReconcilerLogger, run *wf.WorkflowRunRow) {
	podIP, err := r.K8sClient.EnsureActive(ctx, run.WorkspaceID, 120*time.Second)
	if err != nil {
		r.failRun(ctx, logger, run, types.RunErrorCodeWorkspaceUnavailable, fmt.Sprintf("workspace activation failed: %v", err))
		return
	}

	var spec wf.Spec
	if err := json.Unmarshal(run.SpecSnapshot, &spec); err != nil {
		r.failRun(ctx, logger, run, types.RunErrorCodeValidationError, fmt.Sprintf("cannot parse spec: %v", err))
		return
	}

	nodeOrder := topoSort(&spec)
	if len(nodeOrder) == 0 {
		r.failRun(ctx, logger, run, types.RunErrorCodeValidationError, "spec has no valid topological order")
		return
	}

	currentInput := run.Input
	for _, idx := range nodeOrder {
		node := &spec.Nodes[idx]

		if r.isCanceled(run.ID) {
			r.cancelRun(ctx, logger, run)
			return
		}

		output, branch, err := r.executeNode(ctx, logger, run, podIP, node, currentInput)
		if err != nil {
			r.failRun(ctx, logger, run, types.RunErrorCodeNodeFailed, fmt.Sprintf("node %s failed: %v", node.ID, err))
			return
		}

		// Condition nodes pass their INPUT through (not output).
		if node.Type != types.NodeTypeCondition {
			currentInput = output
		}

		// For condition nodes, adjust traversal to follow the matched branch.
		if node.Type == types.NodeTypeCondition && branch != "" && branch != "otherwise" {
			_ = findBranchTarget(&spec, node.ID, branch) // v1: condition traversal via topo order
		}
	}

	if err := r.Store.UpdateWorkflowRunStatus(ctx, run.ID, types.RunStatusSucceeded, nil, nil, currentInput); err != nil {
		logger.Error(err, "failed to mark run succeeded", "runId", run.ID)
	}
	if run.TriggerID != nil && *run.TriggerID != "" {
		_ = r.Store.ResetTriggerFailures(ctx, *run.TriggerID)
	}
	logger.Info("workflow run succeeded", "runId", run.ID)
}

func (r *WorkflowReconciler) executeNode(ctx context.Context, logger ReconcilerLogger, run *wf.WorkflowRunRow, podIP string, node *wf.SpecNode, input json.RawMessage) (json.RawMessage, string, error) {
	maxAttempts := node.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		nodeRunID := fmt.Sprintf("%s-%s-%d", run.ID, node.ID, attempt)
		_ = r.Store.CreateNodeRun(ctx, &wf.WorkflowNodeRunRow{
			ID: nodeRunID, WorkflowRunID: run.ID,
			NodeID: node.ID, NodeType: node.Type,
			Status: types.NodeRunStatusRunning, Attempt: attempt, Input: input,
			StartedAt: time.Now().UTC(),
		})

		req := &NodeExecRequest{
			NodeID: node.ID, NodeType: node.Type,
			Spec: node.Data, Input: input, Timeout: node.Timeout,
		}

		resp, err := r.AgentdClient.Execute(ctx, podIP, req)
		if err != nil {
			errMsg, _ := json.Marshal(map[string]string{"error": err.Error()})
			ec := types.RunErrorCodeNodeFailed
			_ = r.Store.UpdateNodeRunStatus(ctx, nodeRunID, types.NodeRunStatusFailed, nil, nil, &ec, errMsg)
			lastErr = err
		} else if resp.ErrorCode != "" {
			errMsg, _ := json.Marshal(map[string]string{"error": resp.Detail, "code": resp.ErrorCode})
			ec := resp.ErrorCode
			_ = r.Store.UpdateNodeRunStatus(ctx, nodeRunID, types.NodeRunStatusFailed, nil, nil, &ec, errMsg)
			lastErr = fmt.Errorf("%s: %s", resp.ErrorCode, resp.Detail)
		} else {
			var branchPtr *string
			if resp.Branch != "" {
				branchPtr = &resp.Branch
			}
			_ = r.Store.UpdateNodeRunStatus(ctx, nodeRunID, types.NodeRunStatusSucceeded, resp.Output, branchPtr, nil, nil)
			return resp.Output, resp.Branch, nil
		}

		if attempt < maxAttempts {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, "", ctx.Err()
			}
		}
	}
	return nil, "", lastErr
}

func (r *WorkflowReconciler) failRun(ctx context.Context, logger ReconcilerLogger, run *wf.WorkflowRunRow, errorCode, detail string) {
	errMsg, _ := json.Marshal(map[string]string{"error": detail, "code": errorCode})
	ec := errorCode
	_ = r.Store.UpdateWorkflowRunStatus(ctx, run.ID, types.RunStatusFailed, &ec, errMsg, nil)
	if run.TriggerID != nil && *run.TriggerID != "" {
		n, _ := r.Store.IncrementTriggerFailures(ctx, *run.TriggerID)
		logger.Info("incremented trigger failures", "triggerId", *run.TriggerID, "count", n)
	}
}

func (r *WorkflowReconciler) cancelRun(ctx context.Context, logger ReconcilerLogger, run *wf.WorkflowRunRow) {
	ec := types.RunErrorCodeCanceled
	_ = r.Store.UpdateWorkflowRunStatus(ctx, run.ID, types.RunStatusCanceled, &ec, nil, nil)
	r.clearCanceled(run.ID)
}

func (r *WorkflowReconciler) Cancel(runID string) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	if r.canceledRuns == nil {
		r.canceledRuns = make(map[string]struct{})
	}
	r.canceledRuns[runID] = struct{}{}
}

func (r *WorkflowReconciler) isCanceled(runID string) bool {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	_, ok := r.canceledRuns[runID]
	return ok
}

func (r *WorkflowReconciler) clearCanceled(runID string) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	delete(r.canceledRuns, runID)
}

func topoSort(spec *wf.Spec) []int {
	inDeg := make(map[string]int, len(spec.Nodes))
	idx := make(map[string]int, len(spec.Nodes))
	for i := range spec.Nodes {
		inDeg[spec.Nodes[i].ID] = 0
		idx[spec.Nodes[i].ID] = i
	}
	for _, e := range spec.Edges {
		inDeg[e.Target]++
	}
	var queue []int
	for i := range spec.Nodes {
		if inDeg[spec.Nodes[i].ID] == 0 {
			queue = append(queue, i)
		}
	}
	var order []int
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		for _, e := range spec.Edges {
			if e.Source == spec.Nodes[n].ID {
				inDeg[e.Target]--
				if inDeg[e.Target] == 0 {
					queue = append(queue, idx[e.Target])
				}
			}
		}
	}
	return order
}

func findBranchTarget(spec *wf.Spec, sourceID, handle string) int {
	for _, e := range spec.Edges {
		if e.Source == sourceID && e.SourceHandle == handle {
			for i := range spec.Nodes {
				if spec.Nodes[i].ID == e.Target {
					return i
				}
			}
		}
	}
	return -1
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Error(error, string, ...any) {}

// HTTPAgentdExecutor calls agentd via HTTP.
type HTTPAgentdExecutor struct {
	Port   int
	Client *http.Client
}

func (e *HTTPAgentdExecutor) Execute(ctx context.Context, podIP string, req *NodeExecRequest) (*NodeExecResponse, error) {
	body, _ := json.Marshal(req)
	port := e.Port
	if port == 0 {
		port = 4098
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://%s:%d/v1/workflow/node/execute", podIP, port),
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var nodeResp NodeExecResponse
	if err := json.NewDecoder(resp.Body).Decode(&nodeResp); err != nil {
		return nil, err
	}
	return &nodeResp, nil
}
