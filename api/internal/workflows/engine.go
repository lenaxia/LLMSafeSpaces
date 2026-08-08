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
	"strconv"
	"strings"
	"sync"
	"time"

	k8stypes "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkgk8s "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
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
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
	GetOrCreateScriptWorkflow(ctx context.Context, trigger *wf.TriggerRow, specJSON json.RawMessage, workspaceID string) (*wf.WorkflowRow, error)
	CreateWorkflowRunWithFire(ctx context.Context, fire *wf.TriggerFireRow, run *wf.WorkflowRunRow) error
	CreateTriggerFire(ctx context.Context, row *wf.TriggerFireRow) error
	UpdateTriggerFireTimestamps(ctx context.Context, triggerID string, lastFiredAt time.Time, nextFireAt *time.Time) error
	IncrementTriggerFailures(ctx context.Context, triggerID string) (int, error)
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
}

func (s *Scheduler) fireTrigger(ctx context.Context, logger Logger, trigger *wf.TriggerRow, now time.Time, tickInterval time.Duration) {
	if trigger.NextFireAt != nil && now.Sub(*trigger.NextFireAt) > tickInterval {
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID:        fmt.Sprintf("fire-missed-%s-%d", trigger.ID, now.Unix()),
			TriggerID: trigger.ID, SourceType: "cron",
			ActionType: trigger.TargetType, Status: "skipped",
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

	if trigger.TargetType == types.TriggerTargetRunWorkflow {
		s.fireWorkflowTarget(ctx, logger, trigger, envelopeJSON, now)
	} else if trigger.TargetType == types.TriggerTargetRunScript {
		s.fireScriptTarget(ctx, logger, trigger, envelopeJSON, now)
	} else {
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID:        fmt.Sprintf("fire-%s-%d", trigger.ID, now.Unix()),
			TriggerID: trigger.ID, SourceType: "cron",
			InputEnvelope: envelopeJSON, ActionType: trigger.TargetType,
			Status: "delivered", FiredAt: now, CompletedAt: &now,
		})
	}

	nextFire := computeNextFire(trigger, now)
	_ = s.Store.UpdateTriggerFireTimestamps(ctx, trigger.ID, now, &nextFire)
}

func (s *Scheduler) fireWorkflowTarget(ctx context.Context, logger Logger, trigger *wf.TriggerRow, envelopeJSON []byte, now time.Time) {
	var targetCfg types.RunWorkflowTargetConfig
	_ = json.Unmarshal(trigger.TargetConfig, &targetCfg)
	if targetCfg.WorkflowID == "" {
		return
	}

	wfRow, err := s.Store.GetWorkflow(ctx, trigger.OwnerType, trigger.OwnerID, targetCfg.WorkflowID)
	if err != nil {
		return
	}

	inputForRun := json.RawMessage(envelopeJSON)
	if len(targetCfg.InputTemplate) > 0 {
		inputForRun, _ = json.Marshal(targetCfg.InputTemplate)
	}

	fireID := fmt.Sprintf("fire-%s-%d", trigger.ID, now.Unix())
	runID := fmt.Sprintf("run-%s-%d", trigger.ID, now.Unix())

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
		ID: runID, WorkflowID: targetCfg.WorkflowID, SpecSnapshot: wfRow.SpecJSON,
		Input: inputForRun, Status: "queued", TriggerID: &trigger.ID,
		WorkspaceID: workspaceID, CreatedAt: now, UpdatedAt: now,
	}

	err = s.Store.CreateWorkflowRunWithFire(ctx, fire, run)
	if err != nil {
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID: fireID + "-skipped", TriggerID: trigger.ID, SourceType: "cron",
			InputEnvelope: envelopeJSON, ActionType: "run_workflow",
			ActionResult: json.RawMessage(`{"reason":"already_running"}`),
			Status:       "skipped", FiredAt: now, CompletedAt: &now,
		})
		return
	}
	logger.Info("scheduler: fired cron trigger", "triggerId", trigger.ID, "runId", runID)
}

func (s *Scheduler) fireScriptTarget(ctx context.Context, logger Logger, trigger *wf.TriggerRow, envelopeJSON []byte, now time.Time) {
	var targetCfg types.RunScriptTargetConfig
	_ = json.Unmarshal(trigger.TargetConfig, &targetCfg)
	if targetCfg.WorkspaceID == "" {
		return
	}

	specJSON := BuildRunScriptSpec(&targetCfg)
	wfRow, err := s.Store.GetOrCreateScriptWorkflow(ctx, trigger, specJSON, targetCfg.WorkspaceID)
	if err != nil {
		logger.Error(err, "scheduler: failed to get-or-create script workflow", "triggerId", trigger.ID)
		return
	}

	fireID := fmt.Sprintf("fire-%s-%d", trigger.ID, now.Unix())
	runID := fmt.Sprintf("run-%s-%d", trigger.ID, now.Unix())

	fire := &wf.TriggerFireRow{
		ID: fireID, TriggerID: trigger.ID, SourceType: "cron",
		InputEnvelope: envelopeJSON, ActionType: "run_script",
		Status: "fired", FiredAt: now,
	}
	run := &wf.WorkflowRunRow{
		ID: runID, WorkflowID: wfRow.ID, SpecSnapshot: specJSON,
		Input: envelopeJSON, Status: "queued", TriggerID: &trigger.ID,
		WorkspaceID: targetCfg.WorkspaceID, CreatedAt: now, UpdatedAt: now,
	}

	err = s.Store.CreateWorkflowRunWithFire(ctx, fire, run)
	if err != nil {
		_ = s.Store.CreateTriggerFire(ctx, &wf.TriggerFireRow{
			ID: fireID + "-skipped", TriggerID: trigger.ID, SourceType: "cron",
			InputEnvelope: envelopeJSON, ActionType: "run_script",
			ActionResult: json.RawMessage(`{"reason":"already_running"}`),
			Status:       "skipped", FiredAt: now, CompletedAt: &now,
		})
		return
	}
	logger.Info("scheduler: fired run_script trigger", "triggerId", trigger.ID, "runId", runID)
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

	fields := strings.Fields(cfg.Expr)
	if len(fields) != 5 {
		return now.Add(time.Hour)
	}

	minuteField := fields[0]
	hourField := fields[1]

	if strings.HasPrefix(minuteField, "*/") {
		n, err := strconv.Atoi(minuteField[2:])
		if err == nil && n > 0 {
			return now.Add(time.Duration(n) * time.Minute)
		}
	}

	if minuteField == "0" && hourField == "*" {
		return now.Add(time.Hour)
	}

	minute, _ := strconv.Atoi(minuteField)
	hour, _ := strconv.Atoi(hourField)

	nowInTz := now.In(loc)
	next := time.Date(nowInTz.Year(), nowInTz.Month(), nowInTz.Day(), hour, minute, 0, 0, loc)
	if next.Before(nowInTz) || next.Equal(nowInTz) {
		next = next.Add(24 * time.Hour)
	}

	dowField := fields[4]
	if dowField == "1-5" {
		for next.Weekday() == time.Sunday || next.Weekday() == time.Saturday {
			next = next.Add(24 * time.Hour)
		}
	}

	return next.UTC()
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

// BuildRunScriptSpec generates a 2-node DAG (script → agent) from a run_script
// trigger's target config. The script node executes path+args+env; its output
// feeds the agent node which renders the prompt template with the script result.
// If Prompt is empty, only the script node is emitted (no agent follow-up).
func BuildRunScriptSpec(cfg *types.RunScriptTargetConfig) json.RawMessage {
	handler := fmt.Sprintf(`import subprocess, json
def handler(input):
    result = subprocess.run(
        [%q] + %v,
        capture_output=True, text=True, timeout=300
    )
    return {
        "exitCode": result.returncode,
        "stdout": result.stdout,
        "stderr": result.stderr,
    }`, cfg.Path, goQuoteSlice(cfg.Args))

	spec := map[string]any{
		"nodes": []map[string]any{
			{
				"id":   "run-script",
				"type": "script",
				"data": map[string]any{
					"language": "python",
					"handler":  handler,
				},
				"maxAttempts": 1,
				"timeout":     "5m",
			},
		},
		"edges": []map[string]any{},
	}

	if cfg.Prompt != "" {
		spec["nodes"] = append(spec["nodes"].([]map[string]any), map[string]any{
			"id":   "agent-prompt",
			"type": "agent",
			"data": map[string]any{
				"prompt":  cfg.Prompt,
				"session": "ephemeral",
			},
			"maxAttempts": 1,
			"timeout":     "10m",
		})
		spec["edges"] = append(spec["edges"].([]map[string]any), map[string]any{
			"source": "run-script",
			"target": "agent-prompt",
		})
	}

	out, _ := json.Marshal(spec)
	return out
}

func goQuoteSlice(s []string) string {
	parts := make([]string, len(s))
	for i, v := range s {
		parts[i] = fmt.Sprintf("%q", v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
