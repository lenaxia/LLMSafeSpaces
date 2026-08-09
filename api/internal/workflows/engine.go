// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Epic 64: Workflow engine — reconciler + scheduler running in the API server.
//
// The API already has the pgxpool, K8s client, and HTTP connectivity to workspace
// pods. Background goroutines (jwtSessionJanitor, pendingOrgCleaner) are the
// established pattern. FOR UPDATE SKIP LOCKED provides multi-replica safety
// using FOR UPDATE SKIP LOCKED (no leader election needed).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	k8stypes "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkgk8s "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sapiTypes "k8s.io/apimachinery/pkg/types"
)

// --- Shared types ---

type Logger interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Error(error, string, ...any) {}

// --- ReconcilerStore interface ---

type ReconcilerStore interface {
	ClaimQueuedRuns(ctx context.Context, limit int) ([]*wf.WorkflowRunRow, error)
	UpdateWorkflowRunStatus(ctx context.Context, runID, status string, errorCode *string, errMsg json.RawMessage, output json.RawMessage) error
	CreateNodeRun(ctx context.Context, row *wf.WorkflowNodeRunRow) error
	UpdateNodeRunStatus(ctx context.Context, nodeRunID, status string, output json.RawMessage, branch *string, errorCode *string, errMsg json.RawMessage) error
	IncrementTriggerFailures(ctx context.Context, triggerID string) (int, error)
	ResetTriggerFailures(ctx context.Context, triggerID string) error
	GetWorkflowPolicy(ctx context.Context, workflowID string) (onMissing, ownerType, ownerID string, err error)
	UpdateRunWorkspace(ctx context.Context, runID, workspaceID string) error
}

// --- SchedulerStore interface ---

type SchedulerStore interface {
	ListDueCronTriggers(ctx context.Context, now time.Time, limit int) ([]*wf.TriggerRow, error)
	ListPendingRoutineFires(ctx context.Context, limit int) ([]*wf.TriggerFireRow, error)
	GetTriggerByID(ctx context.Context, triggerID string) (*wf.TriggerRow, error)
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
	CreateWorkflowRunWithFire(ctx context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error
	CreateTriggerFire(ctx context.Context, row *wf.TriggerFireRow) error
	UpdateTriggerFireResult(ctx context.Context, fireID string, result json.RawMessage, status string) error
	GetLastRoutineResult(ctx context.Context, triggerID string) (json.RawMessage, error)
	GetRecentRoutineResults(ctx context.Context, triggerID string, limit int) ([]json.RawMessage, error)
	UpdateTriggerFireTimestamps(ctx context.Context, triggerID string, lastFiredAt time.Time, nextFireAt *time.Time) error
	IncrementTriggerFailures(ctx context.Context, triggerID string) (int, error)
	ResetTriggerFailures(ctx context.Context, triggerID string) error
	DisableTrigger(ctx context.Context, triggerID string) error
}

// --- AgentdExecutor interface ---

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

// --- WorkspaceActivator interface ---

type WorkspaceActivator interface {
	EnsureActive(ctx context.Context, workspaceID string, timeout time.Duration) (podIP string, err error)
}

// --- WorkspaceCreator interface (on-missing-create policy) ---

type WorkspaceCreator interface {
	CreateWorkspace(ctx context.Context, workflowID, ownerType, ownerID string) (workspaceID string, err error)
}

// --- K8sWorkspaceActivator uses the API's existing K8s client ---

type K8sWorkspaceActivator struct {
	K8sClient pkgk8s.KubernetesClient
	Namespace string
}

func (a *K8sWorkspaceActivator) EnsureActive(ctx context.Context, workspaceID string, timeout time.Duration) (string, error) {
	if a.K8sClient == nil {
		return "", fmt.Errorf("K8sClient is nil — workspace activator not configured")
	}
	v1Client, err := a.K8sClient.LlmsafespacesV1()
	if err != nil {
		return "", fmt.Errorf("get k8s client: %w", err)
	}

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		crd, err := v1Client.Workspaces(a.Namespace).Get(checkCtx, workspaceID, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return "", fmt.Errorf("workspace %s not found", workspaceID)
			}
			return "", err
		}

		if crd.Status.Phase == k8stypes.WorkspacePhaseActive && crd.Status.PodIP != "" {
			return crd.Status.PodIP, nil
		}

		if crd.Status.Phase == k8stypes.WorkspacePhaseSuspended || crd.Status.Phase == k8stypes.WorkspacePhaseFailed {
			suspendFalse := false
			patchBody := map[string]any{"spec": map[string]any{"suspend": suspendFalse}}
			patchBytes, _ := json.Marshal(patchBody)
			_, err := v1Client.Workspaces(a.Namespace).Patch(checkCtx, workspaceID, k8sapiTypes.MergePatchType, patchBytes, metav1.PatchOptions{})
			if err != nil {
				return "", fmt.Errorf("failed to activate workspace: %w", err)
			}
		}

		select {
		case <-checkCtx.Done():
			return "", fmt.Errorf("workspace activation timed out after %s (phase: %s)", timeout, crd.Status.Phase)
		case <-time.After(2 * time.Second):
		}
	}
}

// --- Reconciler ---

type Reconciler struct {
	Store            ReconcilerStore
	AgentdClient     AgentdExecutor
	Activator        WorkspaceActivator
	WorkspaceCreator WorkspaceCreator
	Logger           Logger
	MaxConcurrent    int
	TickInterval     time.Duration

	cancelMu     sync.Mutex
	canceledRuns map[string]struct{}
}

func (r *Reconciler) Start(ctx context.Context) error {
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

func (r *Reconciler) claimAndDispatch(ctx context.Context, logger Logger, sem chan struct{}) {
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

func (r *Reconciler) executeRun(ctx context.Context, logger Logger, run *wf.WorkflowRunRow) {
	podIP, err := r.Activator.EnsureActive(ctx, run.WorkspaceID, 120*time.Second)
	if err != nil {
		policy, ownerType, ownerID, policyErr := r.Store.GetWorkflowPolicy(ctx, run.WorkflowID)
		if policyErr == nil && policy == types.OnMissingCreate && r.WorkspaceCreator != nil {
			newWSID, createErr := r.WorkspaceCreator.CreateWorkspace(ctx, run.WorkflowID, ownerType, ownerID)
			if createErr != nil {
				r.failRun(ctx, logger, run, types.RunErrorCodeWorkspaceUnavailable, fmt.Sprintf("workspace creation failed: %v", createErr))
				return
			}
			_ = r.Store.UpdateRunWorkspace(ctx, run.ID, newWSID)
			run.WorkspaceID = newWSID
			podIP, err = r.Activator.EnsureActive(ctx, run.WorkspaceID, 120*time.Second)
		}
		if err != nil {
			r.failRun(ctx, logger, run, types.RunErrorCodeWorkspaceUnavailable, fmt.Sprintf("workspace activation failed: %v", err))
			return
		}
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

	activeNodes := make(map[int]bool, len(nodeOrder))
	for _, idx := range nodeOrder {
		activeNodes[idx] = true
	}

	currentInput := run.Input
	for _, idx := range nodeOrder {
		if !activeNodes[idx] {
			continue
		}
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

		if node.Type != types.NodeTypeCondition {
			currentInput = output
		}

		if node.Type == types.NodeTypeCondition {
			pruneInactiveBranches(&spec, node.ID, branch, activeNodes)
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

func (r *Reconciler) executeNode(ctx context.Context, logger Logger, run *wf.WorkflowRunRow, podIP string, node *wf.SpecNode, input json.RawMessage) (json.RawMessage, string, error) {
	maxAttempts := node.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		nodeRunID := uuid.New().String()
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

func (r *Reconciler) failRun(ctx context.Context, logger Logger, run *wf.WorkflowRunRow, errorCode, detail string) {
	errMsg, _ := json.Marshal(map[string]string{"error": detail, "code": errorCode})
	ec := errorCode
	_ = r.Store.UpdateWorkflowRunStatus(ctx, run.ID, types.RunStatusFailed, &ec, errMsg, nil)
	if run.TriggerID != nil && *run.TriggerID != "" {
		n, _ := r.Store.IncrementTriggerFailures(ctx, *run.TriggerID)
		logger.Info("incremented trigger failures", "triggerId", *run.TriggerID, "count", n)
	}
}

func (r *Reconciler) cancelRun(ctx context.Context, logger Logger, run *wf.WorkflowRunRow) {
	ec := types.RunErrorCodeCanceled
	_ = r.Store.UpdateWorkflowRunStatus(ctx, run.ID, types.RunStatusCanceled, &ec, nil, nil)
	r.clearCanceled(run.ID)
}

func (r *Reconciler) Cancel(runID string) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	if r.canceledRuns == nil {
		r.canceledRuns = make(map[string]struct{})
	}
	r.canceledRuns[runID] = struct{}{}
}

func (r *Reconciler) isCanceled(runID string) bool {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	_, ok := r.canceledRuns[runID]
	return ok
}

func (r *Reconciler) clearCanceled(runID string) {
	r.cancelMu.Lock()
	defer r.cancelMu.Unlock()
	delete(r.canceledRuns, runID)
}

// --- Scheduler ---

type Scheduler struct {
	Store        SchedulerStore
	Activator    WorkspaceActivator
	AgentdClient AgentdExecutor
	Logger       Logger
	TickInterval time.Duration
}

func (s *Scheduler) Start(ctx context.Context) error {
	logger := s.Logger
	if logger == nil {
		logger = noopLogger{}
	}

	interval := s.TickInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	limit := 50

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	s.tick(ctx, logger, limit)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			s.tick(ctx, logger, limit)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context, logger Logger, limit int) {
	now := time.Now().UTC()
	triggers, err := s.Store.ListDueCronTriggers(ctx, now, limit)
	if err != nil {
		logger.Error(err, "scheduler: failed to list due cron triggers")
		return
	}
	tickInterval := s.TickInterval
	if tickInterval <= 0 {
		tickInterval = 30 * time.Second
	}
	for _, trigger := range triggers {
		s.fireTrigger(ctx, logger, trigger, now, tickInterval)
	}

	pendingFires, err := s.Store.ListPendingRoutineFires(ctx, limit)
	if err != nil {
		logger.Error(err, "scheduler: failed to list pending routine fires")
		return
	}
	for _, fire := range pendingFires {
		s.processPendingRoutineFire(ctx, logger, fire)
	}
}

func (s *Scheduler) fireTrigger(ctx context.Context, logger Logger, trigger *wf.TriggerRow, now time.Time, tickInterval time.Duration) {
	if trigger.NextFireAt != nil && now.Sub(*trigger.NextFireAt) > tickInterval {
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID:        uuid.New().String(),
			TriggerID: trigger.ID, SourceType: "cron",
			ActionType: "routine", Status: "skipped",
			FiredAt: *trigger.NextFireAt, CompletedAt: &now,
		})
		nextFire := computeNextFire(trigger, now)
		_ = s.Store.UpdateTriggerFireTimestamps(ctx, trigger.ID, now, &nextFire)
		return
	}

	envelope := map[string]any{
		"source":      map[string]any{"type": "cron", "id": trigger.ID},
		"received_at": now.Format(time.RFC3339),
	}
	envelopeJSON, _ := json.Marshal(envelope)

	if trigger.WorkflowID != nil && *trigger.WorkflowID != "" {
		s.fireWorkflowTarget(ctx, logger, trigger, envelopeJSON, now)
	} else {
		s.fireRoutineTarget(ctx, logger, trigger, envelopeJSON, now)
	}

	nextFire := computeNextFire(trigger, now)
	_ = s.Store.UpdateTriggerFireTimestamps(ctx, trigger.ID, now, &nextFire)
}

func (s *Scheduler) fireWorkflowTarget(ctx context.Context, logger Logger, trigger *wf.TriggerRow, envelopeJSON []byte, now time.Time) {
	if trigger.WorkflowID == nil || *trigger.WorkflowID == "" {
		return
	}
	workflowID := *trigger.WorkflowID

	wfRow, err := s.Store.GetWorkflow(ctx, trigger.OwnerType, trigger.OwnerID, workflowID)
	if err != nil {
		return
	}

	inputForRun := json.RawMessage(envelopeJSON)

	fireID := uuid.New().String()
	runID := uuid.New().String()

	workspaceID := ""
	if wfRow.TargetWorkspaceID != nil {
		workspaceID = *wfRow.TargetWorkspaceID
	}

	fire := &wf.TriggerFireRow{
		ID: fireID, TriggerID: trigger.ID, SourceType: "cron",
		InputEnvelope: envelopeJSON, ActionType: "run_workflow",
		Status: "fired", FiredAt: now,
	}
	run := &wf.WorkflowRunRow{
		ID: runID, WorkflowID: workflowID, SpecSnapshot: wfRow.SpecJSON,
		Input: inputForRun, Status: "queued", TriggerID: &trigger.ID,
		WorkspaceID: workspaceID, CreatedAt: now, UpdatedAt: now,
	}

	err = s.Store.CreateWorkflowRunWithFire(ctx, fire, run)
	if err != nil {
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID: uuid.New().String(), TriggerID: trigger.ID, SourceType: "cron",
			InputEnvelope: envelopeJSON, ActionType: "run_workflow",
			ActionResult: json.RawMessage(`{"reason":"already_running"}`),
			Status:       "skipped", FiredAt: now, CompletedAt: &now,
		})
		return
	}
	logger.Info("scheduler: fired cron trigger", "triggerId", trigger.ID, "runId", runID)
}

func (s *Scheduler) fireRoutineTarget(ctx context.Context, logger Logger, trigger *wf.TriggerRow, envelopeJSON []byte, now time.Time) {
	if trigger.WorkspaceID == nil || *trigger.WorkspaceID == "" {
		logger.Error(fmt.Errorf("routine trigger has no workspace"), "trigger has no workspace_id", "triggerId", trigger.ID)
		return
	}

	fireID := uuid.New().String()

	fire := &wf.TriggerFireRow{
		ID: fireID, TriggerID: trigger.ID, SourceType: trigger.SourceType,
		InputEnvelope: envelopeJSON, ActionType: "routine",
		Status: "fired", FiredAt: now,
	}
	if err := s.Store.CreateTriggerFire(ctx, fire); err != nil {
		logger.Error(err, "routine: failed to create fire row", "triggerId", trigger.ID)
		return
	}

	s.executeRoutine(ctx, logger, trigger, fire)
}

func (s *Scheduler) executeRoutine(ctx context.Context, logger Logger, trigger *wf.TriggerRow, fire *wf.TriggerFireRow) {
	workspaceID := *trigger.WorkspaceID
	fireID := fire.ID
	envelopeJSON := fire.InputEnvelope
	if envelopeJSON == nil {
		envelopeJSON = json.RawMessage(`{}`)
	}

	var resultData json.RawMessage
	var resultStatus string

	podIP, err := s.Activator.EnsureActive(ctx, workspaceID, 120*time.Second)
	if err != nil {
		errMsg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("workspace activation failed: %v", err)})
		resultData = errMsg
		resultStatus = "failed"
		_ = s.Store.UpdateTriggerFireResult(ctx, fireID, resultData, resultStatus)
		if n, _ := s.Store.IncrementTriggerFailures(ctx, trigger.ID); n >= trigger.AutoDisableAfter {
			_ = s.Store.DisableTrigger(ctx, trigger.ID)
		}
		return
	}

	prompt := trigger.Prompt
	if trigger.MemoryMode == types.MemoryLastResult {
		maxRuns := trigger.MemoryMaxRuns
		if maxRuns <= 0 {
			maxRuns = 1
		}
		if maxRuns == 1 {
			if prevResult, err := s.Store.GetLastRoutineResult(ctx, trigger.ID); err == nil && len(prevResult) > 0 {
				prompt = strings.ReplaceAll(prompt, "{{.prevResult}}", string(prevResult))
			}
		} else {
			if results, err := s.Store.GetRecentRoutineResults(ctx, trigger.ID, maxRuns); err == nil && len(results) > 0 {
				combined := make([]string, len(results))
				for i, r := range results {
					combined[i] = string(r)
				}
				prompt = strings.ReplaceAll(prompt, "{{.prevResult}}", strings.Join(combined, "\n---\n"))
			}
		}
	}

	if trigger.ScriptPath != "" {
		scriptReq := &NodeExecRequest{
			NodeID: "routine-script", NodeType: "script",
			Spec: buildRoutineScriptSpec(trigger), Input: envelopeJSON,
			Timeout: "5m",
		}
		scriptResp, err := s.AgentdClient.Execute(ctx, podIP, scriptReq)
		if err != nil {
			errMsg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("script failed: %v", err)})
			resultData = errMsg
			resultStatus = "failed"
			_ = s.Store.UpdateTriggerFireResult(ctx, fireID, resultData, resultStatus)
			if n, _ := s.Store.IncrementTriggerFailures(ctx, trigger.ID); n >= trigger.AutoDisableAfter {
				_ = s.Store.DisableTrigger(ctx, trigger.ID)
			}
			return
		}
		if scriptResp.Output != nil {
			prompt = strings.ReplaceAll(prompt, "{{.scriptResult}}", string(scriptResp.Output))
		}
	}
	prompt = strings.ReplaceAll(prompt, "{{.input}}", string(envelopeJSON))

	agentReq := &NodeExecRequest{
		NodeID: "routine-agent", NodeType: "agent",
		Spec: buildRoutineAgentSpec(trigger, prompt), Input: envelopeJSON,
		Timeout: "10m",
	}
	agentResp, err := s.AgentdClient.Execute(ctx, podIP, agentReq)
	if err != nil {
		errMsg, _ := json.Marshal(map[string]string{"error": fmt.Sprintf("agent call failed: %v", err)})
		resultData = errMsg
		resultStatus = "failed"
	} else if agentResp.ErrorCode != "" {
		errMsg, _ := json.Marshal(map[string]string{"error": agentResp.Detail, "code": agentResp.ErrorCode})
		resultData = errMsg
		resultStatus = "failed"
	} else {
		resultStatus = "delivered"
		if trigger.CaptureMode == types.CaptureFull {
			resultData = agentResp.Output
		}
		if trigger.PreserveSession == types.PreserveOnFailure {
			var agentOutput map[string]any
			if json.Unmarshal(agentResp.Output, &agentOutput) == nil {
				if sessionID, ok := agentOutput["session_id"].(string); ok && sessionID != "" {
					deleteReq, _ := http.NewRequestWithContext(ctx, "DELETE",
						fmt.Sprintf("http://%s:%d/v1/workflow/session/delete?sessionId=%s", podIP, agentdExecPort(), sessionID), nil)
					deleteResp, err := httpClient().Do(deleteReq)
					if err == nil {
						_ = deleteResp.Body.Close()
					}
				}
			}
		}
	}

	_ = s.Store.UpdateTriggerFireResult(ctx, fireID, resultData, resultStatus)

	if resultStatus == "failed" {
		if n, _ := s.Store.IncrementTriggerFailures(ctx, trigger.ID); n >= trigger.AutoDisableAfter {
			_ = s.Store.DisableTrigger(ctx, trigger.ID)
		}
	} else {
		_ = s.Store.ResetTriggerFailures(ctx, trigger.ID)
	}

	logger.Info("routine executed", "triggerId", trigger.ID, "fireId", fireID, "status", resultStatus)
}

func buildRoutineScriptSpec(trigger *wf.TriggerRow) json.RawMessage {
	envMap := map[string]string{}
	if len(trigger.ScriptEnv) > 0 {
		_ = json.Unmarshal(trigger.ScriptEnv, &envMap)
	}
	envLines := ""
	for k, v := range envMap {
		envLines += fmt.Sprintf("    os.environ[%q] = %q\n", k, v)
	}
	argsStr := ""
	for _, a := range trigger.ScriptArgs {
		argsStr += fmt.Sprintf("%q, ", a)
	}
	handler := fmt.Sprintf(`import subprocess, json, os
%s
def handler(input):
    result = subprocess.run([%q, %s], capture_output=True, text=True, timeout=300)
    return {"exitCode": result.returncode, "stdout": result.stdout, "stderr": result.stderr}
`, envLines, trigger.ScriptPath, argsStr)
	spec := map[string]any{
		"language": "python",
		"handler":  handler,
	}
	out, _ := json.Marshal(spec)
	return out
}

func buildRoutineAgentSpec(trigger *wf.TriggerRow, prompt string) json.RawMessage {
	spec := map[string]any{
		"prompt": prompt,
	}
	if trigger.PreserveSession == types.PreserveAlways || trigger.PreserveSession == types.PreserveOnFailure {
		spec["session"] = "new"
	} else {
		spec["session"] = "ephemeral"
	}
	if trigger.Agent != "" {
		spec["agent"] = trigger.Agent
	}
	out, _ := json.Marshal(spec)
	return out
}

func agentdExecPort() int { return 4097 }

func httpClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

func (s *Scheduler) processPendingRoutineFire(ctx context.Context, logger Logger, fire *wf.TriggerFireRow) {
	trigger, err := s.Store.GetTriggerByID(ctx, fire.TriggerID)
	if err != nil {
		logger.Error(err, "routine: failed to get trigger for pending fire", "fireId", fire.ID)
		errMsg, _ := json.Marshal(map[string]string{"error": "trigger not found"})
		_ = s.Store.UpdateTriggerFireResult(ctx, fire.ID, errMsg, "failed")
		return
	}
	if trigger.WorkspaceID == nil || *trigger.WorkspaceID == "" {
		logger.Error(fmt.Errorf("no workspace"), "routine: trigger has no workspace", "triggerId", trigger.ID)
		errMsg, _ := json.Marshal(map[string]string{"error": "trigger has no workspace_id"})
		_ = s.Store.UpdateTriggerFireResult(ctx, fire.ID, errMsg, "failed")
		return
	}

	s.executeRoutine(ctx, logger, trigger, fire)
}

func computeNextFire(trigger *wf.TriggerRow, now time.Time) time.Time {
	var cfg types.CronSourceConfig
	_ = json.Unmarshal(trigger.SourceConfig, &cfg)
	if cfg.Expr == "" {
		return now.Add(time.Hour)
	}

	loc := time.UTC
	if cfg.TZ != "" {
		if parsed, err := time.LoadLocation(cfg.TZ); err == nil {
			loc = parsed
		}
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	sched, err := parser.Parse(cfg.Expr)
	if err != nil {
		return now.Add(time.Hour)
	}
	return sched.Next(now.In(loc)).UTC()
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

func pruneInactiveBranches(spec *wf.Spec, condNodeID, matchedBranch string, active map[int]bool) {
	var matchedTargets, unmatchedTargets []string
	for _, e := range spec.Edges {
		if e.Source != condNodeID {
			continue
		}
		if e.SourceHandle == matchedBranch {
			matchedTargets = append(matchedTargets, e.Target)
		} else {
			unmatchedTargets = append(unmatchedTargets, e.Target)
		}
	}

	matchedReachable := make(map[string]bool)
	for _, t := range matchedTargets {
		for n := range bfsReachableByNodeID(spec, t) {
			matchedReachable[n] = true
		}
	}

	for _, t := range unmatchedTargets {
		for n := range bfsReachableByNodeID(spec, t) {
			if !matchedReachable[n] {
				if i, ok := findNodeIndexByID(spec, n); ok {
					active[i] = false
				}
			}
		}
	}
}

func bfsReachableByNodeID(spec *wf.Spec, startID string) map[string]bool {
	adj := make(map[string][]string)
	for _, e := range spec.Edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}
	reachable := map[string]bool{startID: true}
	queue := []string{startID}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range adj[node] {
			if !reachable[next] {
				reachable[next] = true
				queue = append(queue, next)
			}
		}
	}
	return reachable
}

func findNodeIndexByID(spec *wf.Spec, id string) (int, bool) {
	for i := range spec.Nodes {
		if spec.Nodes[i].ID == id {
			return i, true
		}
	}
	return -1, false
}

// --- HTTPAgentExecutor ---

// HTTPAgentExecutor calls agentd on the workspace pod via HTTP.
// Uses context for cancellation and timeout propagation.
type HTTPAgentExecutor struct {
	Port   int
	Client *http.Client
}

func (e *HTTPAgentExecutor) Execute(ctx context.Context, podIP string, req *NodeExecRequest) (*NodeExecResponse, error) {
	body, _ := json.Marshal(req)
	port := e.Port
	if port == 0 {
		port = 4097
	}
	url := fmt.Sprintf("http://%s:%d/v1/workflow/node/execute", podIP, port)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Minute}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}

	var nodeResp NodeExecResponse
	if err := json.Unmarshal(respBody, &nodeResp); err != nil {
		return nil, err
	}
	return &nodeResp, nil
}

// AppEngineLogger adapts an external logger to the engine's Logger interface.
// The concrete type is created by the caller (app.go) — this struct is just
// a type definition that the caller wraps around its own logger.
type AppEngineLogger struct {
	LogFn func(msg string, kv ...any)
	ErrFn func(err error, msg string, kv ...any)
}

func (l *AppEngineLogger) Info(msg string, kv ...any) {
	if l.LogFn != nil {
		l.LogFn(msg, kv...)
	}
}

func (l *AppEngineLogger) Error(err error, msg string, kv ...any) {
	if l.ErrFn != nil {
		l.ErrFn(err, msg, kv...)
	}
}
