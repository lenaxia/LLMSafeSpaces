// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	apitypes "github.com/lenaxia/llmsafespaces/api/internal/types"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// BrokerPublisher is the minimal SSE broker interface needed by the reload handler.
type BrokerPublisher interface {
	PublishToWorkspace(workspaceID string, event apitypes.WorkspaceSSEEvent)
}

// respondWithAPIError maps API errors to HTTP responses. Uses errors.As to
// find an *apierrors.APIError anywhere in the chain (handles wrapped errors),
// then falls back to a duck-typed StatusCode() check, then 500.
func respondWithAPIError(c *gin.Context, err error) {
	var apiErr *apierrors.APIError
	if errors.As(err, &apiErr) {
		c.JSON(apiErr.StatusCode(), gin.H{"error": apiErr.Error()})
		return
	}
	if ae, ok := err.(interface {
		StatusCode() int
		Error() string
	}); ok {
		c.JSON(ae.StatusCode(), gin.H{"error": ae.Error()})
		return
	}
	c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
}

// AgentStateStore is the DB surface needed by the reload handler.
type AgentStateStore interface {
	GetLastCredentialChangedAt(ctx context.Context, workspaceID string) (time.Time, error)
	MarkAgentReloaded(ctx context.Context, tx *sql.Tx, workspaceID string, priorChangedAt time.Time) (time.Time, error)
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// WorkspaceServicer is the minimal workspace service surface for reload.
type WorkspaceServicer interface {
	GetWorkspace(ctx context.Context, userID, workspaceID string) (*types.Workspace, error)
}

// AgentReloadHandler handles POST /api/v1/workspaces/:id/agent/reload.
type AgentReloadHandler struct {
	workspaceSvc         WorkspaceServicer
	db                   AgentStateStore
	podResolver          PodIPResolver
	httpClient           *http.Client
	logger               pkginterfaces.LoggerInterface
	sseTracker           *sse.Tracker
	getPassword          interfaces.WorkspacePasswordProvider
	metricsService       MetricsRecorder
	broker               BrokerPublisher
	statusCheckerFactory func(podIP, password string) SessionStatusChecker
	// agentdPort overrides the dispatch port. Zero → agentd.AgentdPort.
	// Tests point this at an httptest server so the dispatch path (incl.
	// the Basic-auth header, #848) is actually exercised.
	agentdPort int
}

// agentdDispatchPort returns the override or the production default.
func (h *AgentReloadHandler) agentdDispatchPort() int {
	if h.agentdPort != 0 {
		return h.agentdPort
	}
	return agentd.AgentdPort
}

// MetricsRecorder is the minimal metrics interface for reload handlers.
type MetricsRecorder interface {
	RecordAgentReload(result string, durationMs int64, drained bool)
	RecordAgentReloadDrainTimeout(elapsedMs int64)
	RecordAgentReloadBulk(total, succeeded, failed int)
}

// NewAgentReloadHandler constructs the handler with all dependencies.
func NewAgentReloadHandler(
	wsSvc WorkspaceServicer,
	db AgentStateStore,
	podResolver PodIPResolver,
	httpClient *http.Client,
	logger pkginterfaces.LoggerInterface,
) *AgentReloadHandler {
	return &AgentReloadHandler{
		workspaceSvc: wsSvc,
		db:           db,
		podResolver:  podResolver,
		httpClient:   httpClient,
		logger:       logger,
	}
}

// SetStatusCheckerFactory injects the factory that builds a SessionStatusChecker
// from podIP + password. app.go wires this with opencode.NewClient; the handler
// itself never imports the opencode package.
func (h *AgentReloadHandler) SetStatusCheckerFactory(f func(podIP, password string) SessionStatusChecker) {
	h.statusCheckerFactory = f
}

// SetSSETracker injects the tracker for drain mode support.
func (h *AgentReloadHandler) SetSSETracker(t *sse.Tracker) { h.sseTracker = t }

// SetPasswordGetter injects the password getter for drain mode (needs opencode client).
func (h *AgentReloadHandler) SetPasswordGetter(provider interfaces.WorkspacePasswordProvider) {
	h.getPassword = provider
}

// SetMetrics injects the metrics recorder.
func (h *AgentReloadHandler) SetMetrics(m MetricsRecorder) { h.metricsService = m }

// SetBrokerPublisher injects the SSE broker for publishing dismissed events on dispose.
func (h *AgentReloadHandler) SetBrokerPublisher(b BrokerPublisher) { h.broker = b }

// Reload handles POST /api/v1/workspaces/:id/agent/reload.
func (h *AgentReloadHandler) Reload(c *gin.Context) {
	start := time.Now()
	workspaceID := c.Param("id")
	succeeded := false
	drain := false // set below before the defer reads it
	defer func() {
		if h.metricsService != nil {
			result := "error"
			if succeeded {
				result = "success"
			}
			h.metricsService.RecordAgentReload(result, time.Since(start).Milliseconds(), drain)
		}
	}()

	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	ws, err := h.workspaceSvc.GetWorkspace(c.Request.Context(), userID, workspaceID)
	if err != nil {
		respondWithAPIError(c, err)
		return
	}

	if ws.Phase != "Active" {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("cannot reload agent: workspace is in phase %q (must be Active)", ws.Phase),
		})
		return
	}

	podIP, err := h.podResolver.GetWorkspacePodIP(c.Request.Context(), userID, workspaceID)
	if err != nil || podIP == "" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "cannot reload agent: workspace pod is not reachable",
		})
		return
	}

	// Drain mode: wait for all sessions to become idle before disposing.
	drain = c.Query("drain") == "true"
	drainTimeout := 5 * time.Minute
	if v := c.Query("drainTimeoutSeconds"); v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs > 0 && secs <= 600 {
			drainTimeout = time.Duration(secs) * time.Second
		}
	}

	if drain && h.sseTracker != nil && h.getPassword != nil {
		pw, err := h.getPassword.WorkspacePassword(c.Request.Context(), workspaceID)
		if err != nil {
			respondWithAPIError(c, apierrors.NewInternalError("get_opencode_password_failed", err))
			return
		}
		if h.statusCheckerFactory == nil {
			h.logger.Warn("Drain mode requested but statusCheckerFactory not wired; skipping drain")
			return
		}
		statusChecker := h.statusCheckerFactory(
			fmt.Sprintf("%s:%d", podIP, agentd.AgentPort),
			pw,
		)
		if err := WaitUntilIdle(c.Request.Context(), workspaceID, h.sseTracker, statusChecker, drainTimeout); err != nil {
			var drainErr *ErrDrainTimeout
			if errors.As(err, &drainErr) {
				if h.metricsService != nil {
					h.metricsService.RecordAgentReloadDrainTimeout(time.Since(start).Milliseconds())
				}
				c.JSON(http.StatusRequestTimeout, gin.H{
					"error": gin.H{
						"code":           "drain_timeout",
						"message":        fmt.Sprintf("workspace did not become idle within %s", drainTimeout),
						"busySessionIDs": drainErr.BusySessions,
					},
				})
				return
			}
			respondWithAPIError(c, apierrors.NewInternalError("drain_failed", err))
			return
		}
	}

	priorChangedAt, err := h.db.GetLastCredentialChangedAt(c.Request.Context(), workspaceID)
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("agent_state_read_failed", err))
		return
	}

	// Dispatch to agentd (which calls opencode dispose locally).
	// agentd enforces Basic auth on /v1/agent/reload (#848) — resolve
	// the workspace password and send the credential.
	if h.getPassword == nil {
		respondWithAPIError(c, apierrors.NewInternalError("password_getter_not_wired",
			errors.New("agent reload: password getter not configured")))
		return
	}
	agentdPassword, err := h.getPassword.WorkspacePassword(c.Request.Context(), workspaceID)
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("get_opencode_password_failed", err))
		return
	}
	agentdURL := fmt.Sprintf("http://%s:%d/v1/agent/reload", podIP, h.agentdDispatchPort())
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, agentdURL, nil)
	if err != nil {
		// Malformed pod IP (empty, double-port, etc.) must not reach
		// Do(nil) — that panics. Surface as an internal error instead.
		if h.logger != nil {
			h.logger.Error("agent reload: invalid agentd URL", err, "url", agentdURL)
		}
		respondWithAPIError(c, apierrors.NewInternalError("agent_reload_url_invalid", err))
		return
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+agentdPassword)))
	resp, err := h.httpClient.Do(req)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("agent reload: agentd unreachable", err)
		}
		respondWithAPIError(c, apierrors.NewInternalError("agent_unreachable", err))
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		respondWithAPIError(c, apierrors.NewInternalError("dispose_failed",
			fmt.Errorf("agentd returned %d: %s", resp.StatusCode, string(body)),
		))
		return
	}

	// Update agent state.
	tx, err := h.db.BeginTx(c.Request.Context(), nil)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("agent reload: tx begin failed; dispose done, banner may persist", "error", err.Error())
		}
		c.JSON(http.StatusOK, gin.H{
			"disposed":       true,
			"lastDisposedAt": time.Now().UTC().Format(time.RFC3339),
			"warning":        "Agent was reloaded but state could not be updated. The banner may reappear; clicking Reload again is safe.",
		})
		return
	}
	defer func() {
		if tx != nil {
			tx.Rollback() //nolint:errcheck
		}
	}()

	disposedAt, err := h.db.MarkAgentReloaded(c.Request.Context(), tx, workspaceID, priorChangedAt)
	if err != nil {
		if errors.Is(err, apierrors.ErrNoAgentStateRow) {
			respondWithAPIError(c, err)
			return
		}
		if h.logger != nil {
			h.logger.Warn("agent reload: MarkAgentReloaded failed", "error", err.Error())
		}
		c.JSON(http.StatusOK, gin.H{
			"disposed":       true,
			"lastDisposedAt": time.Now().UTC().Format(time.RFC3339),
			"warning":        "Agent was reloaded but state could not be updated. The banner may reappear; clicking Reload again is safe.",
		})
		return
	}
	if tx == nil {
		// A store whose BeginTx returned (nil, nil) (mock stores; a real
		// *sql.DB never does) — dispose succeeded, state row handling
		// already ran in MarkAgentReloaded. Nothing to commit.
		c.JSON(http.StatusOK, gin.H{
			"disposed":       true,
			"lastDisposedAt": time.Now().UTC().Format(time.RFC3339),
		})
		return
	}
	if err := tx.Commit(); err != nil {
		if h.logger != nil {
			h.logger.Warn("agent reload: tx commit failed", "error", err.Error())
		}
		c.JSON(http.StatusOK, gin.H{
			"disposed":       true,
			"lastDisposedAt": time.Now().UTC().Format(time.RFC3339),
			"warning":        "Agent was reloaded but state could not be updated. The banner may reappear; clicking Reload again is safe.",
		})
		return
	}

	succeeded = true
	c.JSON(http.StatusOK, gin.H{
		"disposed":       true,
		"lastDisposedAt": disposedAt.Format(time.RFC3339),
	})
}

// PendingReloadLister lists workspaces with pending credential reload.
type PendingReloadLister interface {
	ListPendingReloadWorkspaces(ctx context.Context, userID string) ([]*types.WorkspaceMetadata, error)
}

// BulkReloadHandler handles POST /api/v1/users/me/agents/reload.
type BulkReloadHandler struct {
	pendingLister        PendingReloadLister
	workspaceSvc         WorkspaceServicer
	db                   AgentStateStore
	podResolver          PodIPResolver
	httpClient           *http.Client
	logger               pkginterfaces.LoggerInterface
	sseTracker           *sse.Tracker
	getPassword          interfaces.WorkspacePasswordProvider
	metricsService       MetricsRecorder
	broker               BrokerPublisher
	statusCheckerFactory func(podIP, password string) SessionStatusChecker
	// agentdPort overrides the dispatch port. Zero → agentd.AgentdPort.
	agentdPort int
}

// agentdDispatchPort returns the override or the production default.
func (h *BulkReloadHandler) agentdDispatchPort() int {
	if h.agentdPort != 0 {
		return h.agentdPort
	}
	return agentd.AgentdPort
}

// NewBulkReloadHandler constructs the bulk reload handler.
func NewBulkReloadHandler(
	pendingLister PendingReloadLister,
	wsSvc WorkspaceServicer,
	db AgentStateStore,
	podResolver PodIPResolver,
	httpClient *http.Client,
	logger pkginterfaces.LoggerInterface,
) *BulkReloadHandler {
	return &BulkReloadHandler{
		pendingLister: pendingLister,
		workspaceSvc:  wsSvc,
		db:            db,
		podResolver:   podResolver,
		httpClient:    httpClient,
		logger:        logger,
	}
}

// SetSSETracker injects the SSE tracker for drain mode.
func (h *BulkReloadHandler) SetSSETracker(t *sse.Tracker) { h.sseTracker = t }

// SetMetrics injects the metrics recorder.
func (h *BulkReloadHandler) SetMetrics(m MetricsRecorder) { h.metricsService = m }

// SetPasswordGetter injects the password getter for drain mode.
func (h *BulkReloadHandler) SetPasswordGetter(provider interfaces.WorkspacePasswordProvider) {
	h.getPassword = provider
}

// SetStatusCheckerFactory injects the factory (same as AgentReloadHandler).
func (h *BulkReloadHandler) SetStatusCheckerFactory(f func(podIP, password string) SessionStatusChecker) {
	h.statusCheckerFactory = f
}

// SetBrokerPublisher injects the SSE broker for publishing dismissed events on dispose.
func (h *BulkReloadHandler) SetBrokerPublisher(b BrokerPublisher) { h.broker = b }

// BulkReload streams per-workspace reload results as NDJSON.
func (h *BulkReloadHandler) BulkReload(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	drain := c.Query("drain") == "true"
	drainTimeout := 5 * time.Minute
	if v := c.Query("drainTimeoutSeconds"); v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs > 0 && secs <= 600 {
			drainTimeout = time.Duration(secs) * time.Second
		}
	}

	pending, err := h.pendingLister.ListPendingReloadWorkspaces(c.Request.Context(), userID)
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("list_pending_failed", err))
		return
	}

	c.Writer.Header().Set("Content-Type", "application/x-ndjson")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)
	flusher, _ := c.Writer.(http.Flusher)

	start := time.Now()

	// Epic 27b US-27b.4: Fan-out with a bounded semaphore (max 5 concurrent
	// reloads). Results are streamed as they complete via a channel so the
	// client gets partial progress even if some workspaces are slow.
	const maxParallel = 5
	type result struct {
		data map[string]any
	}
	resultCh := make(chan result, len(pending))
	sem := make(chan struct{}, maxParallel)

	var wg sync.WaitGroup
	for _, ws := range pending {
		wg.Add(1)
		go func(wsID string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := h.reloadOne(c.Request.Context(), userID, wsID, drain, drainTimeout)
			resultCh <- result{data: r}
		}(ws.ID)
	}

	// Close channel when all goroutines finish.
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	succeeded, failed := 0, 0
	for r := range resultCh {
		_ = json.NewEncoder(c.Writer).Encode(r.data)
		if flusher != nil {
			flusher.Flush()
		}
		if _, ok := r.data["error"]; ok {
			failed++
		} else {
			succeeded++
		}
	}

	// Summary line (always last)
	_ = json.NewEncoder(c.Writer).Encode(map[string]any{
		"summary": map[string]any{
			"total":      len(pending),
			"succeeded":  succeeded,
			"failed":     failed,
			"durationMs": time.Since(start).Milliseconds(),
		},
	})
	if flusher != nil {
		flusher.Flush()
	}
	if h.metricsService != nil {
		h.metricsService.RecordAgentReloadBulk(len(pending), succeeded, failed)
	}
}

func (h *BulkReloadHandler) reloadOne(ctx context.Context, userID, workspaceID string, drain bool, drainTimeout time.Duration) map[string]any {
	ws, err := h.workspaceSvc.GetWorkspace(ctx, userID, workspaceID)
	if err != nil {
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "workspace_error", "message": err.Error()}}
	}
	if ws.Phase != "Active" {
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "phase_not_active", "message": fmt.Sprintf("workspace is in phase %q", ws.Phase)}}
	}

	podIP, err := h.podResolver.GetWorkspacePodIP(ctx, userID, workspaceID)
	if err != nil || podIP == "" {
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "pod_not_reachable", "message": "workspace pod is not reachable"}}
	}

	if drain && h.sseTracker != nil && h.getPassword != nil {
		pw, err := h.getPassword.WorkspacePassword(ctx, workspaceID)
		if err != nil {
			return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "get_password_failed", "message": err.Error()}}
		}
		if h.statusCheckerFactory == nil {
			h.logger.Warn("Drain mode requested but statusCheckerFactory not wired; skipping drain")
			return map[string]any{"workspaceId": workspaceID, "skip_reason": "status_checker_factory_not_wired"}
		}
		statusChecker := h.statusCheckerFactory(fmt.Sprintf("%s:%d", podIP, agentd.AgentPort), pw)
		if err := WaitUntilIdle(ctx, workspaceID, h.sseTracker, statusChecker, drainTimeout); err != nil {
			var drainErr *ErrDrainTimeout
			if errors.As(err, &drainErr) {
				return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "drain_timeout", "busySessionIDs": drainErr.BusySessions}}
			}
			return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "drain_failed", "message": err.Error()}}
		}
	}

	priorChangedAt, err := h.db.GetLastCredentialChangedAt(ctx, workspaceID)
	if err != nil {
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "state_read_failed", "message": err.Error()}}
	}

	// agentd enforces Basic auth on /v1/agent/reload (#848).
	if h.getPassword == nil {
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "password_getter_not_wired", "message": "password getter not configured"}}
	}
	bulkPassword, err := h.getPassword.WorkspacePassword(ctx, workspaceID)
	if err != nil {
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "get_password_failed", "message": err.Error()}}
	}
	agentdURL := fmt.Sprintf("http://%s:%d/v1/agent/reload", podIP, h.agentdDispatchPort())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agentdURL, nil)
	if err != nil {
		// Malformed pod IP must not reach Do(nil) — that panics (same
		// class as the single-reload fix above).
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "agent_reload_url_invalid", "message": err.Error()}}
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+bulkPassword)))
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "agent_unreachable", "message": err.Error()}}
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return map[string]any{"workspaceId": workspaceID, "error": map[string]any{"code": "dispose_failed", "message": fmt.Sprintf("agentd returned %d: %s", resp.StatusCode, string(body))}}
	}

	// Dispose succeeded — update state.
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return map[string]any{"workspaceId": workspaceID, "disposed": true, "warning": "state could not be updated"}
	}
	defer func() {
		if tx != nil {
			tx.Rollback() //nolint:errcheck
		}
	}()

	disposedAt, err := h.db.MarkAgentReloaded(ctx, tx, workspaceID, priorChangedAt)
	if err != nil {
		return map[string]any{"workspaceId": workspaceID, "disposed": true, "warning": "state could not be updated"}
	}
	if err := tx.Commit(); err != nil {
		return map[string]any{"workspaceId": workspaceID, "disposed": true, "warning": "state could not be updated"}
	}

	return map[string]any{"workspaceId": workspaceID, "disposed": true, "drained": drain, "lastDisposedAt": disposedAt.Format(time.RFC3339)}
}
