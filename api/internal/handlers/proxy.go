// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
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
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
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
	// phaseSource is how the SSE watch reconciler (#902) enumerates
	// Active workspaces; production wires the CRD watcher
	// (GetAllKnownPhases). Interface so tests can inject phases without
	// a k8s informer.
	phaseSource interface {
		GetAllKnownPhases() map[string]string
	}
	sseTracker     *sse.Tracker
	sessionIndex   interfaces.SessionIndexService
	userBroker     *eventbroker.UserEventBroker
	sessionParents *sessionParentCache

	meteringSvc interfaces.MeteringService

	// tokenSeenStore persists per-session cumulative usage dedup state
	// across tracker restarts (#759); nil = in-memory only (dev/test).
	tokenSeenStore sse.TokenSeenStore

	// versionSyncCb is the callback wired into the CRD watcher to persist
	// runtime version info (imageTag) to the DB whenever a workspace becomes
	// Active. Set via SetVersionSyncCallback before Start().
	versionSyncCb workspace.VersionSyncCallback

	// workspaceUpdateCb is invoked on every Added/Modified event for
	// any Workspace CRD (worklog 0591). Powers the watcher-driven
	// auto-push of user-DEK secrets after pod recreation. Set via
	// SetWorkspaceUpdateCallback before Start().
	workspaceUpdateCb workspace.WorkspaceUpdateCallback

	// v2ClientFactory overrides V2 client construction. nil in production
	// (v2Client resolves pod IP + password and builds the default client).
	// Tests inject a factory pointing at a dynamic-port httptest.Server,
	// eliminating the port 4096 dependency that caused non-deterministic
	// CI failures.
	v2ClientFactory V2ClientFactory

	// v2ClientConcreteFactory builds a V2SessionClient from a baseURL +
	// password. Set during wiring (app.go) with opencode.NewClient; this
	// file does not import pkg/agent/opencode.
	v2ClientConcreteFactory func(baseURL, password string) (agent.V2SessionClient, error)

	// v2Pending tracks sessions with undrained V2 queue-delivered input.
	// Used by US-63.9 (stranded-input recovery) to identify sessions
	// needing a wake after pod restart. Initialized in NewProxyHandler
	// (in-memory); app.go swaps in Redis-backed when a client is available
	// so multi-replica deployments share the pending set.
	v2Pending v2PendingTracker

	// v2Shadow is a Redis-backed view-cache of pending V2 queue messages
	// per session for fresh-load pill visibility (US-63.10). nil when no
	// Redis client is available (shadow disabled; ListQueue returns empty
	// under V2). Set via SetV2QueueShadow before Start().
	v2Shadow *V2QueueShadow

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

	// busyAlerts / busyAlertsMu back the D6 (#998) escalation cooldown.
	busyAlerts   map[string]time.Time
	busyAlertsMu sync.Mutex

	// sessionAlerts persists D6 (#998) escalations (nil = dev/test:
	// SSE-only, no durability). Wired via SetSessionAlerts before Start.
	sessionAlerts interfaces.SessionAlertsService

	// outboxCancel stops the outbox delivery worker on Stop().
	outboxCancel context.CancelFunc

	// outbox is the D3 durable-prompt outbox (design 0050, #907). nil
	// means the outbox is disabled (dev/test) and the legacy synchronous
	// send path is used; when set, POST /prompt accepts into the outbox
	// and a detached worker delivers via the adapter.
	outbox *outbox.Service

	// adapter is the US-65.3 Agent Adapter seam. nil means the handler
	// uses the legacy dialect + proxyToWorkspace path (every handler
	// today). US-65.4 migrates handlers one-by-one to call adapter
	// methods instead; each migration checks `if h.adapter != nil` and
	// takes the new path, falling back to the legacy path when nil.
	// Set via SetAdapter before Start(). Once all handlers migrate,
	// the dialect field retires and this becomes required.
	adapter agent.Adapter

	// modelPolicyChecker enforces org allowed-models/allowed-providers on
	// explicit per-prompt model overrides (2026-08-16 follow-up: policy was
	// enforced only by hiding models in ListModels). nil = no enforcement
	// (personal deployments). Read on the prompt path after Start, so it is
	// set once via SetModelPolicyChecker before Start — same invariant as
	// SetAdapter.
	modelPolicyChecker OrgPolicyChecker

	// Epic 67 US-67.2 upload overrides. Zero → env-derived defaults
	// (UPLOAD_MAX_BYTES / UPLOAD_TIMEOUT_MS); set via SetUploadLimitsForTest.
	// Written only before Start (test wiring), read on the upload path.
	uploadMaxBytesOverride      int64
	uploadStreamTimeoutOverride time.Duration
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
		busyAlerts:    make(map[string]time.Time),
		requestBuffer: newRequestBuffer(defaultBufferMaxSize, defaultBufferTimeout, defaultBufferPollInterval, logger),
		v2Pending:     newV2PendingSessions(),
	}, nil
}

// SetAdapter wires the US-65.3 Agent Adapter. Once set, handlers that
// have been migrated check `h.adapter != nil` and take the Adapter path;
// unmigrated handlers continue through the legacy dialect path. Set
// before Start(); nil leaves the handler in legacy mode (today's
// behavior). Panics if called after Start() — same invariant as
// SetStateStore, preventing a data race on the interface field once
// handler goroutines begin reading h.adapter.
func (h *ProxyHandler) SetAdapter(a agent.Adapter) {
	if a == nil {
		return
	}
	if h.started {
		panic("SetAdapter called after Start — request goroutines may already be reading h.adapter")
	}
	h.adapter = a
}

// SetOutbox wires the D3 durable-prompt outbox (design 0050 §D3, #907).
// The worker launches in Start() when the outbox is set (nil = legacy
// synchronous send path, dev/test).
func (h *ProxyHandler) SetOutbox(o *outbox.Service) {
	if o == nil {
		return
	}
	if h.started {
		panic("SetOutbox called after Start — request goroutines may already be reading h.outbox")
	}
	h.outbox = o
}

// GetOutboxForTest exposes the outbox for assertions.
func (h *ProxyHandler) GetOutboxForTest() *outbox.Service {
	return h.outbox
}

// DeliverOutboxOnceForTest drives one delivery tick through the
// production bridge (h.outboxDeliver) for one session.
func (h *ProxyHandler) DeliverOutboxOnceForTest(ws, ses string) bool {
	if h.outbox == nil {
		return false
	}
	return h.outbox.DeliverOnce(context.Background(), ws, ses, h.outboxDeliver)
}

// SetOutboxForTest wires the outbox after Start (tests only; production
// wires via SetOutbox before Start).
func (h *ProxyHandler) SetOutboxForTest(o *outbox.Service) {
	h.outbox = o
}

// SetUserBrokerForTest wires the workspace SSE broker without running
// Start (tests only; production creates the broker inside Start). The
// mcp-router integration gate (api/internal/server) drives StreamEvents
// through the production router and publishes events on this broker.
func (h *ProxyHandler) SetUserBrokerForTest(b *eventbroker.UserEventBroker) {
	h.userBroker = b
}

// SetModelPolicyChecker wires the org-policy checker for per-prompt model
// override enforcement. Optional (nil = unenforced). Panics after Start for
// the same race-safety reason as SetAdapter.
func (h *ProxyHandler) SetModelPolicyChecker(p OrgPolicyChecker) {
	if p == nil {
		return
	}
	if h.started {
		panic("SetModelPolicyChecker called after Start — request goroutines may already be reading h.modelPolicyChecker")
	}
	h.modelPolicyChecker = p
}

// HasModelPolicyChecker reports whether org-policy enforcement is wired on
// the prompt paths. Exists for wiring tests (app-level): the enforcement is
// fail-open when nil, so an unwired deployment looks identical to a
// disabled one at runtime — only an explicit probe distinguishes them
// (the #912 round-2 review found exactly this gap).
func (h *ProxyHandler) HasModelPolicyChecker() bool {
	return h.modelPolicyChecker != nil
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

// SetTokenSeenStore wires the persistent session-usage dedup store
// (#759). The SSE tracker consumes it at construction; panics after
// Start for the same race-safety reason as SetStateStore.
func (h *ProxyHandler) SetTokenSeenStore(store sse.TokenSeenStore) {
	if store == nil {
		return
	}
	if h.started {
		panic("SetTokenSeenStore called after Start — the tracker may already be reading it")
	}
	h.tokenSeenStore = store
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
			"code":       "service_unavailable",
			"reason":     "not_ready",
			"phase":      workspace.Status.Phase,
			"retryAfter": retryAfterSec,
			"message":    fmt.Sprintf("Workspace is %s. This usually takes a few seconds.", strings.ToLower(string(workspace.Status.Phase))),
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

	// Disk-pressure injection: when the workspace disk is >=90% full,
	// prepend a notice part to LLM-bound requests (POST /message)
	// so the agent nudges the user to free up space; >=95% escalates to
	// safe-cleanup guidance (build artifacts + caches only, logs last).
	// The ratio comes from the Workspace CRD status (controller-mirrored
	// from agentd statusz, ~60s freshness — the same data the frontend
	// shows as a %). Fail-open: no injection on unknown disk state.
	if len(bodyBytes) > 0 && isLLMPromptPath(targetPath) {
		bodyBytes = injectDiskPressureNotice(bodyBytes,
			diskPressureRatio(workspace.Status.DiskUsedBytes, workspace.Status.DiskTotalBytes))
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
					c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error":      "Workspace is restarting, please try again in a moment",
						"code":       "service_unavailable",
						"reason":     "agent_restarting",
						"retryAfter": retryAfterSec,
						"message":    "The agent is restarting (credential change, OOM, or crash recovery). Your request will work once it's back.",
					})
				} else {
					c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
					c.JSON(http.StatusServiceUnavailable, gin.H{
						"error":      "workspace connection failed",
						"code":       "service_unavailable",
						"reason":     "agent_unreachable",
						"retryAfter": retryAfterSec,
						"message":    "The agent is not responding. It may be restarting or recovering — please try again in a moment.",
					})
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
				"code":       "service_unavailable",
				"reason":     "agent_unreachable",
				"retryAfter": retryAfterSec,
				"message":    "The agent is not responding. It may be restarting or recovering — please try again in a moment.",
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
// the client. Streaming endpoints (events) are streamed
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
	req.SetBasicAuth(agentd.AuthUsername, password)
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
					const agentDiedEvent = "event: error\ndata: {\"type\":\"agent_died\",\"reason\":\"unknown\",\"message\":\"The agent stopped responding (OOM, crash, or restart). Reconnecting…\"}\n\n"
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
			const sseErrEvent = "event: error\ndata: {\"error\":\"upstream connection lost\",\"message\":\"Connection to the agent was lost. Reconnecting…\"}\n\n"
			_, _ = c.Writer.Write([]byte(sseErrEvent))
			if canFlush {
				flusher.Flush()
			}
			return fmt.Errorf("upstream stream cut short: %w", readErr)
		}
	}

	return nil
}

// checkProxyQuota gates a proxied request on the caller's quotas.
// Returns true if the request should proceed, false if it was rejected
// (429 quota exceeded or 503 check unavailable — already written to the
// response).
//
// Two gates (#768):
//   - llm_tokens: deny new requests once the period's accumulated token
//     usage is at the limit. Absence of a token limit row means
//     unlimited — deployments that never configured one are unaffected.
//   - llm_request: atomic slot reservation (advisory-locked
//     check-then-insert) — concurrent requests cannot both claim the
//     last free slot.
//
// Quota check failures fail CLOSED (503): a transient DB outage must
// not silently disable enforcement — that was the fail-open gap.
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

	tokensAllowed, _, tokErr := h.meteringSvc.CheckQuota(c.Request.Context(), owner, "llm_tokens")
	if tokErr != nil {
		return h.quotaCheckFailed(c, tokErr, userID, "llm_tokens")
	}
	if !tokensAllowed {
		metrics.RecordQuotaExceeded("llm_tokens")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "quota exceeded", "event_type": "llm_tokens"})
		return false
	}

	allowed, _, rerr := h.meteringSvc.ReserveQuota(c.Request.Context(), owner, "llm_request", 1)
	if rerr != nil {
		return h.quotaCheckFailed(c, rerr, userID, "llm_request")
	}
	if !allowed {
		metrics.RecordQuotaExceeded("llm_request")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "quota exceeded", "event_type": "llm_request"})
		return false
	}
	return true
}

// quotaCheckFailed writes the fail-closed response for a quota gate
// that could not reach its data (#768b). 503 — not 429 — so clients and
// operators can distinguish "quota exhausted" from "enforcement
// unavailable".
func (h *ProxyHandler) quotaCheckFailed(c *gin.Context, err error, userID, eventType string) bool {
	metrics.RecordQuotaCheckFailed(eventType)
	h.logger.Error("Quota check failed, denying request (fail-closed)", err,
		"user_id", userID, "event_type", eventType)
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "quota check unavailable, please retry"})
	return false
}
