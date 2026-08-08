// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workflows

// Epic 64: Workflow engine — reconciler + scheduler running in the API server.
//
// The API already has the pgxpool, K8s client, and HTTP connectivity to workspace
// pods. Background goroutines (jwtSessionJanitor, pendingOrgCleaner) are the
// established pattern. FOR UPDATE SKIP LOCKED provides multi-replica safety
// without leader election.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	k8stypes "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkgk8s "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- Shared types ---

type Logger interface {
	Info(msg string, keysAndValues ...any)
	Error(err error, msg string, keysAndValues ...any)
}

type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Error(error, string, ...any) {}

// --- ReconcilerStore interface (same as controller version) ---

type ReconcilerStore interface {
	ClaimQueuedRuns(ctx context.Context, limit int) ([]*wf.WorkflowRunRow, error)
	UpdateWorkflowRunStatus(ctx context.Context, runID, status string, errorCode *string, errMsg json.RawMessage, output json.RawMessage) error
	CreateNodeRun(ctx context.Context, row *wf.WorkflowNodeRunRow) error
	UpdateNodeRunStatus(ctx context.Context, nodeRunID, status string, output json.RawMessage, branch *string, errorCode *string, errMsg json.RawMessage) error
	IncrementTriggerFailures(ctx context.Context, triggerID string) (int, error)
	ResetTriggerFailures(ctx context.Context, triggerID string) error
}

// --- SchedulerStore interface ---

type SchedulerStore interface {
	ListDueCronTriggers(ctx context.Context, now time.Time, limit int) ([]*wf.TriggerRow, error)
	GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error)
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

// --- K8sWorkspaceActivator uses the API's existing K8s client ---

type K8sWorkspaceActivator struct {
	K8sClient pkgk8s.KubernetesClient
	Namespace string
}

func (a *K8sWorkspaceActivator) EnsureActive(ctx context.Context, workspaceID string, timeout time.Duration) (string, error) {
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
			crd.Spec.Suspend = &suspendFalse
			_, err := v1Client.Workspaces(a.Namespace).Update(checkCtx, crd)
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
	Store         ReconcilerStore
	AgentdClient  AgentdExecutor
	Activator     WorkspaceActivator
	Logger        Logger
	MaxConcurrent int
	TickInterval  time.Duration

	cancelMu     mutex
	canceledRuns map[string]struct{}
}

type mutex struct{ mu chan struct{} }

func newMutex() mutex { return mutex{mu: make(chan struct{}, 1)} }

func (m mutex) Lock()   { m.mu <- struct{}{} }
func (m mutex) Unlock() { <-m.mu }

func (r *Reconciler) Start(ctx context.Context) error {
	logger := r.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	if r.canceledRuns == nil {
		r.canceledRuns = make(map[string]struct{})
		r.cancelMu = newMutex()
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
			pruneInactiveBranches(&spec, node.ID, branch, nodeOrder, activeNodes, idx)
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
	BatchLimit   int
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
	limit := s.BatchLimit
	if limit <= 0 {
		limit = 50
	}

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
	var targetCfg map[string]any
	_ = json.Unmarshal(trigger.TargetConfig, &targetCfg)
	workflowID, _ := targetCfg["workflowId"].(string)
	if workflowID == "" {
		return
	}

	wfRow, err := s.Store.GetWorkflow(ctx, trigger.OwnerType, trigger.OwnerID, workflowID)
	if err != nil {
		return
	}

	inputForRun := json.RawMessage(envelopeJSON)
	if tmpl, ok := targetCfg["inputTemplate"].(map[string]any); ok && len(tmpl) > 0 {
		rendered := make(map[string]any)
		for k, v := range tmpl {
			rendered[k] = v
		}
		inputForRun, _ = json.Marshal(rendered)
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
		ID: runID, WorkflowID: workflowID, SpecSnapshot: wfRow.SpecJSON,
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

	fields := splitFields(cfg.Expr)
	if len(fields) != 5 {
		return now.Add(time.Hour)
	}

	minuteField := fields[0]
	hourField := fields[1]

	if hasPrefix(minuteField, "*/") {
		if n := atoiSafe(minuteField[2:]); n > 0 {
			return now.Add(time.Duration(n) * time.Minute)
		}
	}

	if minuteField == "0" && hourField == "*" {
		return now.Add(time.Hour)
	}

	minute := atoiSafe(minuteField)
	hour := atoiSafe(hourField)

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

// --- DAG helpers ---

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

func pruneInactiveBranches(spec *wf.Spec, condNodeID, matchedBranch string, nodeOrder []int, active map[int]bool, _ int) {
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

// --- string helpers (avoid importing strings/strconv in hot path) ---

func splitFields(s string) []string {
	var out []string
	start := 0
	for i, c := range s {
		if c == ' ' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
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
