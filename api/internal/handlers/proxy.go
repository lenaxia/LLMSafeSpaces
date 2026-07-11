// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sse"
	"github.com/lenaxia/llmsafespaces/api/internal/services/workspace"
	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	"github.com/lenaxia/llmsafespaces/pkg/agent"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

const (
	defaultMaxActiveSessions   = 5
	maxConnectionsPerWorkspace = 10
	opencodePort               = agentd.AgentPort
	retryAfterSec              = 10

	phaseActive      = v1.WorkspacePhaseActive
	phaseSuspending  = "Suspending"
	phaseSuspended   = "Suspended"
	phaseTerminating = "Terminating"
	phaseTerminated  = "Terminated"
)

type ProxyHandler struct {
	k8sClient         pkginterfaces.KubernetesClient
	httpClient        *http.Client
	logger            pkginterfaces.LoggerInterface
	namespace         string
	dialect           agent.Dialect
	agentStateChecker AgentStateChecker

	// stateStore holds the per-workspace state that was previously kept
	// in process-local maps on ProxyHandler (activeSess, deletedSessions,
	// pwCache, wsConfig, priorPhase, parentBackfilled). Externalizing it
	// via an interface is the foundation for moving the state to a
	// shared Redis backend in subsequent Epic 45 stories, which
	// eliminates the multi-replica drift that caused the 2026-06-16
	// stuck-session incident. The InMemoryStore used today preserves
	// single-replica behavior exactly.
	stateStore wsstate.Store

	// connCount is intentionally NOT in stateStore — it represents a
	// per-replica resource (HTTP file descriptors, memory) that must
	// remain local even after the Redis migration. See US-45 design.
	connCount map[string]int
	connMu    sync.RWMutex

	activityTracker *activity.ActivityTracker
	watcher         *workspace.Watcher
	sseTracker      *sse.Tracker
	sessionIndex    interfaces.SessionIndexService
	userBroker      *eventbroker.UserEventBroker
	sessionParents  *sessionParentCache

	meteringSvc interfaces.MeteringService

	// versionSyncCb is the callback wired into the CRD watcher to persist
	// runtime version info (imageTag) to the DB whenever a workspace becomes
	// Active. Set via SetVersionSyncCallback before Start().
	versionSyncCb workspace.VersionSyncCallback

	// workspaceUpdateCb is invoked on every Added/Modified event for
	// any Workspace CRD (worklog 0591). Powers the watcher-driven
	// auto-push of user-DEK secrets after pod recreation. Set via
	// SetWorkspaceUpdateCallback before Start().
	workspaceUpdateCb workspace.WorkspaceUpdateCallback

	queueSvc interfaces.MessageQueueService

	// sweepInterval overrides the default queueSweepInterval for testing.
	// Zero means use the default (30s). Set via SetSweepInterval before Start().
	sweepInterval time.Duration

	// requestBuffer parks POST /message requests during an opencode restart
	// (connection-refused window) so users do not see 503s. See US-44.10.
	requestBuffer *requestBuffer

	startOnce sync.Once
	stopOnce  sync.Once
	// started is set true inside startOnce.Do. Used by SetStateStore to
	// panic if called after Start — request goroutines read stateStore
	// without synchronization, so a late swap would race.
	started bool

	// stopCh is closed by Stop() to signal background goroutines
	// (e.g. the stranded-queue sweep) to shut down.
	stopCh chan struct{}
}

func NewProxyHandler(
	k8sClient pkginterfaces.KubernetesClient,
	logger pkginterfaces.LoggerInterface,
	namespace string,
	httpClient *http.Client,
	dialect agent.Dialect,
) (*ProxyHandler, error) {
	if k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client cannot be nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}
	if namespace == "" {
		namespace = "default"
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 60 * time.Second,
			},
		}
	}
	return &ProxyHandler{
		k8sClient:     k8sClient,
		httpClient:    httpClient,
		logger:        logger,
		namespace:     namespace,
		dialect:       dialect,
		stateStore:    wsstate.NewInMemoryStore(),
		connCount:     make(map[string]int),
		requestBuffer: newRequestBuffer(defaultBufferMaxSize, defaultBufferTimeout, defaultBufferPollInterval, logger),
	}, nil
}

// SetStateStore overrides the per-workspace state store. By default the
// ProxyHandler uses an InMemoryStore (single-replica); app.go swaps in a
// RedisStore when a Redis/Valkey client is available so multi-replica
// deployments share active-session state. Panics if called after Start()
// — request goroutines read stateStore without synchronization, so a
// late swap would race.
func (h *ProxyHandler) SetStateStore(store wsstate.Store) {
	if store == nil {
		return
	}
	if h.started {
		panic("SetStateStore called after Start — request goroutines may already be reading stateStore")
	}
	h.stateStore = store
}

func (h *ProxyHandler) proxyToWorkspace(c *gin.Context, targetPath string, isWriteOp bool, sessionID string) {
	h.proxyToWorkspaceWithErrBody(c, targetPath, isWriteOp, sessionID, nil, false)
}

// proxyToWorkspaceWithErrBody behaves like proxyToWorkspace but optionally
// rewrites the response body on 4xx/5xx. When onErrorBody is non-nil and the
// upstream returns status >= 400, the response body is buffered (up to
// chatErrorBufferCap bytes), passed through onErrorBody, and the transformed
// bytes are written to the client. Used by SendMessage (US-27b.5) to inject
// the agentNeedsRefresh / hint fields when the agent fails with staged
// credentials pending. 2xx responses stream as before (no buffering).
//
// When bufferable is true and the forward fails with a connection error
// (opencode restarting), the request is parked in the per-workspace request
// buffer and retried until the upstream recovers or the buffer timeout elapses,
// instead of returning 503 immediately. Only SendMessage sets bufferable.
//
//nolint:gocyclo // proxy path has many independent guard clauses; complexity is inherent
func (h *ProxyHandler) proxyToWorkspaceWithErrBody(
	c *gin.Context,
	targetPath string,
	isWriteOp bool,
	sessionID string,
	onErrorBody func(statusCode int, body []byte) []byte,
	bufferable bool,
) {
	workspaceID := c.Param("id")
	if workspaceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace ID required"})
		return
	}

	var workspace *v1.Workspace
	if cached, exists := c.Get("workspace"); exists {
		if sb, ok := cached.(*v1.Workspace); ok {
			workspace = sb
		}
	}
	if workspace == nil {
		v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
		if v1Err != nil {
			h.logger.Error("Failed to get LLMSafespacesV1 client", v1Err, "workspaceID", workspaceID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}
		var err error
		workspace, err = v1Client.Workspaces(h.namespace).Get(c.Request.Context(), workspaceID, metav1.GetOptions{})
		if err != nil {
			h.logger.Error("Failed to get workspace CRD", err, "workspaceID", workspaceID)
			c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
			return
		}
	}

	if workspace.Status.Phase != phaseActive || workspace.Status.PodIP == "" {
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error":      "workspace not ready",
			"phase":      workspace.Status.Phase,
			"retryAfter": retryAfterSec,
		})
		return
	}

	password, err := h.getPassword(c.Request.Context(), workspaceID)
	if err != nil {
		h.logger.Error("Failed to get workspace password", err, "workspaceID", workspaceID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve workspace credentials"})
		return
	}

	maxSessions := int(workspace.Spec.MaxActiveSessions)
	if maxSessions <= 0 {
		maxSessions = defaultMaxActiveSessions
	}

	if !h.acquireConnection(workspaceID) {
		c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":      "connection limit reached",
			"retryAfter": retryAfterSec,
		})
		return
	}
	slotReleased := false
	defer func() {
		if !slotReleased {
			h.releaseConnection(workspaceID)
		}
	}()

	if isWriteOp && sessionID != "" {
		if !h.checkAndAddActiveSession(c.Request.Context(), workspaceID, sessionID, maxSessions) {
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":             "active session limit reached",
				"maxActiveSessions": maxSessions,
				"retryAfter":        retryAfterSec,
			})
			return
		}
	}

	if isWriteOp && sessionID != "" && h.sseTracker != nil {
		h.sseTracker.EnsureWatching(workspaceID)
	}

	var bodyBytes []byte
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		limited := http.MaxBytesReader(nil, c.Request.Body, 10*1024*1024)
		bodyBytes, err = io.ReadAll(limited)
		_ = c.Request.Body.Close()
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body exceeds 10 MB limit"})
				return
			}
			h.logger.Error("Failed to read request body", err, "workspaceID", workspaceID)
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
			return
		}
	}

	podIP := workspace.Status.PodIP

	if h.meteringSvc != nil && workspaceID != "" {
		if !h.checkProxyQuota(c, workspace) {
			return
		}
	}

	proxyErr := h.doProxy(c, podIP, targetPath, password, bodyBytes, onErrorBody)

	if proxyErr != nil && isConnectionError(proxyErr) && !c.Writer.Written() {
		freshWS, getErr := func() (*v1.Workspace, error) {
			v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
			if v1Err != nil {
				return nil, v1Err
			}
			return v1Client.Workspaces(h.namespace).Get(c.Request.Context(), workspaceID, metav1.GetOptions{})
		}()
		if getErr == nil && freshWS.Status.PodIP != "" && freshWS.Status.PodIP != podIP && freshWS.Status.Phase == phaseActive {
			h.logger.Info("Retrying proxy with fresh pod IP", "workspaceID", workspaceID, "oldIP", podIP, "newIP", freshWS.Status.PodIP)
			proxyErr = h.doProxy(c, freshWS.Status.PodIP, targetPath, password, bodyBytes, onErrorBody)
		}
	}

	if proxyErr != nil && isConnectionError(proxyErr) && !c.Writer.Written() && bufferable &&
		h.requestBuffer != nil && h.requestBuffer.maxSize > 0 {
		// podIP is stable for in-place opencode restarts (same pod, agentd
		// SIGTERMs and restarts opencode in place); pod-recreating restarts
		// (suspend/resume) go through the not-Active 503 path above, never the
		// buffer. So re-forwarding the captured podIP is correct for the
		// restart window this buffer exists to smooth over.
		bufReq := &bufferedRequest{
			forward: func() error {
				if !h.acquireConnection(workspaceID) {
					return errBufferRetryLater
				}
				defer h.releaseConnection(workspaceID)
				err := h.doProxy(c, podIP, targetPath, password, bodyBytes, onErrorBody)
				if err != nil && c.Writer.Written() {
					return errBufferCommitted
				}
				return err
			},
			result:   make(chan error, 1),
			deadline: time.Now().Add(h.requestBuffer.timeout),
			cancelCh: make(chan struct{}),
			// C5: account the body bytes against the global buffer memory cap.
			bodySize: len(bodyBytes),
		}
		if !h.requestBuffer.tryEnqueue(workspaceID, bufReq) {
			metrics.RecordRequestBufferFull(workspaceID)
			if isWriteOp && sessionID != "" {
				h.removeActiveSession(c.Request.Context(), workspaceID, sessionID)
			}
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "Too many requests during restart, please try again"})
			return
		}
		// A parked request holds no upstream socket, so release the connection
		// slot acquired on entry; forward re-acquires (briefly) per attempt.
		// slotReleased=true neutralizes the top-level deferred release so
		// connCount is decremented exactly once for this request.
		h.releaseConnection(workspaceID)
		slotReleased = true
		startWait := time.Now()
		// Always learn the drainer's terminal outcome: even if the client
		// disconnects, block for the drainer's deliver so a success that
		// raced with ctx.Done is not silently dropped (which would skip
		// metering and wrongly remove the active session).
		var ferr error
		select {
		case ferr = <-bufReq.result:
		case <-c.Request.Context().Done():
			close(bufReq.cancelCh)
			ferr = <-bufReq.result
		}
		metrics.RecordRequestBufferWait(workspaceID, time.Since(startWait))
		if ferr == nil {
			proxyErr = nil
		} else {
			if errors.Is(ferr, errBufferTimeout) {
				metrics.RecordRequestBufferTimeout(workspaceID)
			}
			if isWriteOp && sessionID != "" {
				h.removeActiveSession(c.Request.Context(), workspaceID, sessionID)
			}
			if !c.Writer.Written() && c.Request.Context().Err() == nil {
				if errors.Is(ferr, errBufferTimeout) {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Workspace is restarting, please try again in a moment"})
				} else {
					c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "workspace connection failed", "retryAfter": retryAfterSec})
				}
			}
			return
		}
	}

	if proxyErr != nil {
		h.logger.Error("Proxy request failed", proxyErr, "workspaceID", workspaceID)
		if isWriteOp && sessionID != "" {
			h.removeActiveSession(c.Request.Context(), workspaceID, sessionID)
		}
		if !c.Writer.Written() {
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
			c.JSON(http.StatusServiceUnavailable, gin.H{
				"error":      "workspace connection failed",
				"retryAfter": retryAfterSec,
			})
		}
		return
	}

	if h.activityTracker != nil {
		h.activityTracker.Record(workspaceID)
	}

	if h.sessionIndex != nil && sessionID != "" && isWriteOp {
		h.sessionIndex.RecordMessage(workspaceID, sessionID, "", time.Now())
	}

	if h.meteringSvc != nil && workspaceID != "" {
		userID, _ := extractAuth(c)
		if userID != "" && workspace.Labels["llmsafespaces.dev/canary"] != "true" {
			h.meteringSvc.Record(types.UsageEvent{
				IdempotencyKey: fmt.Sprintf("llmreq:%s:%d", workspaceID, time.Now().UnixNano()),
				Owner:          types.BillingOwner{ID: userID, Type: types.OwnerTypeUser},
				ActorID:        userID,
				WorkspaceID:    workspaceID,
				EventType:      "llm_request",
				EventSubtype:   "message",
				Quantity:       1,
				Source:         "api",
				EventTime:      time.Now(),
				RequestContext: map[string]any{
					"ip":         c.ClientIP(),
					"request_id": c.GetString("request_id"),
					"session_id": sessionID,
				},
			})
		}
	}
}

// chatErrorBufferCap bounds the amount of upstream body buffered when an
// onErrorBody transform is supplied. Chat error responses are small JSON
// payloads (~1 KB); a runaway upstream must not consume unbounded memory.
// Truncation is handled by EnrichChatErrorBody (non-JSON wraps to a 1024-byte
// "message" field), so anything above this cap is dropped on the floor.
const chatErrorBufferCap = 64 * 1024

// doProxy sends the request to the sandbox and writes the response back to
// the client. Streaming endpoints (events, prompt_async) are streamed
// directly to the client with flushed writes.
//
// When onErrorBody is non-nil and the upstream returns status >= 400, the
// response body is buffered (up to chatErrorBufferCap), passed through
// onErrorBody, and the transformed bytes are written. This is the US-27b.5
// path that lets SendMessage enrich chat errors with agentNeedsRefresh / hint
// fields. 2xx responses always stream chunk-by-chunk.
func (h *ProxyHandler) doProxy(c *gin.Context, podIP, targetPath, password string, body []byte, onErrorBody func(int, []byte) []byte) error {
	targetURL := fmt.Sprintf("http://%s:%d%s", podIP, opencodePort, targetPath)
	if forwardedQuery := stripVerboseQuery(c.Request.URL.RawQuery); forwardedQuery != "" {
		targetURL += "?" + forwardedQuery
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, targetURL, bodyReader)
	if err != nil {
		return fmt.Errorf("creating proxy request: %w", err)
	}

	// G34: forward only an explicit allowlist of client headers. The caller's
	// Authorization, Cookie, Origin, Referer, X-Forwarded-* and arbitrary
	// custom headers describe the caller's relationship with this API server,
	// not with the tenant pod, and must not reach untrusted agent code.
	// Authorization is set below via SetBasicAuth; X-Forwarded-For after that.
	copyRequestHeaders(c.Request.Header, req.Header)
	req.SetBasicAuth("opencode", password)
	req.Header.Set("X-Forwarded-For", c.ClientIP())

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("proxy request to workspace: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// LLMSafeSpaces#488: log + count upstream 5xx as early as possible so
	// operators have a signal in Prometheus and logs even for streaming
	// responses (where the body is not buffered and preview will be empty).
	// The 401 branch below still does its own log — different semantic and
	// pre-dates this instrumentation. See recordUpstream5xx for path
	// sanitization.
	if resp.StatusCode >= 500 {
		wsID := c.Param("id")
		recordUpstream5xx(h.logger, wsID, targetPath, resp.StatusCode, nil)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		wsID := c.Param("id")
		h.invalidateCaches(c.Request.Context(), wsID)
		h.logger.Warn("Upstream auth failed; password cache invalidated",
			"workspaceID", wsID, "path", targetPath)
		c.JSON(http.StatusBadGateway, gin.H{
			"error":       "upstream authentication failed; please retry",
			"workspaceID": wsID,
		})
		return nil
	}

	copyResponseHeaders(resp.Header, c.Writer.Header())
	c.Writer.Header().Set("X-Accel-Buffering", "no")

	// US-27b.5: when an error-body transform is supplied AND the upstream
	// returned an error status, buffer the body (bounded), transform, write.
	// 2xx / 3xx always stream chunk-by-chunk regardless of onErrorBody.
	if onErrorBody != nil && resp.StatusCode >= 400 {
		buf := make([]byte, 0, 4*1024)
		tmp := make([]byte, 32*1024)
		for {
			n, readErr := resp.Body.Read(tmp)
			if n > 0 {
				if len(buf)+n > chatErrorBufferCap {
					buf = append(buf, tmp[:chatErrorBufferCap-len(buf)]...)
				} else {
					buf = append(buf, tmp[:n]...)
				}
			}
			if readErr != nil {
				break
			}
			if len(buf) >= chatErrorBufferCap {
				break
			}
		}
		transformed := onErrorBody(resp.StatusCode, buf)
		// Content-Length is now potentially wrong; drop it and let the writer
		// send chunked encoding or fixate on the new length.
		c.Writer.Header().Del("Content-Length")
		c.Writer.WriteHeader(resp.StatusCode)
		_, _ = c.Writer.Write(transformed)
		return nil
	}

	c.Writer.WriteHeader(resp.StatusCode)

	flusher, canFlush := c.Writer.(http.Flusher)
	buf := make([]byte, 32*1024)
	// US-44.1: terminal event on agent death. Scope: SSE responses only
	// (Content-Type: text/event-stream). On EOF after data on an SSE
	// stream, the agent process disappeared (OOM/SIGTERM/crash); emit a
	// terminal `agent_died` event so clients can surface it instead of
	// seeing a silent close. Non-SSE responses legitimately EOF after
	// data (normal HTTP), so the heuristic MUST be SSE-scoped or JSON
	// parsers downstream would be corrupted.
	isSSEStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	var bytesReceived int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			bytesReceived += int64(n)
			_, _ = c.Writer.Write(buf[:n])
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				if isSSEStream && bytesReceived > 0 {
					const agentDiedEvent = "event: error\ndata: {\"type\":\"agent_died\",\"reason\":\"unknown\"}\n\n"
					_, _ = c.Writer.Write([]byte(agentDiedEvent))
					if canFlush {
						flusher.Flush()
					}
				}
				break
			}
			// Epic 25 B2: non-EOF errors are network-level failures
			// (TCP RST, timeout). Keep the existing wire format — it is
			// intentionally distinct from agent_died so clients can
			// distinguish "network problem" from "process gone". Both
			// shapes are pinned by TestProxy_US44_1_ErrorShapesAreDocumented
			// and TestProxy_B2_MidStreamReadError_WritesSSEErrorEvent.
			const sseErrEvent = "event: error\ndata: {\"error\":\"upstream connection lost\"}\n\n"
			_, _ = c.Writer.Write([]byte(sseErrEvent))
			if canFlush {
				flusher.Flush()
			}
			return fmt.Errorf("upstream stream cut short: %w", readErr)
		}
	}

	return nil
}

// checkProxyQuota verifies the caller has not exceeded their LLM request
// quota. Returns true if the request should proceed, false if it was
// rejected (quota exceeded — 429 already written to the response).
// Quota check failures (DB errors) are logged and the request is allowed
// (fail-open) so a transient DB issue doesn't block all traffic.
func (h *ProxyHandler) checkProxyQuota(c *gin.Context, workspace *v1.Workspace) bool {
	if h.meteringSvc == nil {
		return true
	}
	userID, _ := extractAuth(c)
	if userID == "" {
		return true
	}
	if workspace.Labels["llmsafespaces.dev/canary"] == "true" {
		return true
	}
	owner := types.BillingOwner{ID: userID, Type: types.OwnerTypeUser}
	allowed, _, qerr := h.meteringSvc.CheckQuota(c.Request.Context(), owner, "llm_request")
	if qerr != nil {
		h.logger.Warn("Quota check failed, allowing request", "error", qerr, "user_id", userID)
		return true
	}
	if !allowed {
		metrics.RecordQuotaExceeded("llm_request")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "quota exceeded", "event_type": "llm_request"})
		return false
	}
	return true
}
