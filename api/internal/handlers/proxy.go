// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/api/internal/services/activity"
	"github.com/lenaxia/llmsafespaces/api/internal/services/eventbroker"
	"github.com/lenaxia/llmsafespaces/api/internal/services/inbox"
	"github.com/lenaxia/llmsafespaces/api/internal/services/metrics"
	"github.com/lenaxia/llmsafespaces/api/internal/services/outbox"
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
	agentStateChecker AgentStateChecker
	// resolvers owns the shared connection-resolution state (password
	// cache + pod-IP lookup) the Agent Adapter and the handler both
	// consume — see ResolverHost. Lazily initialized for struct-literal
	// constructions.
	resolvers *ResolverHost

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
	sessionIndex   interfaces.SessionIndexService
	userBroker     *eventbroker.UserEventBroker
	sessionParents *sessionParentCache

	meteringSvc interfaces.MeteringService

	// versionSyncCb is the callback wired into the CRD watcher to persist
	// runtime version info (imageTag) to the DB whenever a workspace becomes
	// Active. Set via SetVersionSyncCallback before Start().
	versionSyncCb workspace.VersionSyncCallback

	// v2Delivery routes outbox delivery through the V2 admit-and-return
	// prompt endpoint (design 0052, OPENCODE_V2_DELIVERY). When on, the
	// adapter MUST also read the V2 store (WithV2Store) — delivery
	// completes at admission instead of turn completion.
	v2Delivery bool
	// agentdTerminus (US-69.8): the outbox delivers via the pod's ABI
	// delivery ledger instead of the adapter + verify oracle.
	agentdTerminus bool

	// Usage billing sinks (SetUsageBilling; guarded by usageBillingMu).
	usageBillingMu sync.Mutex
	usageInference func(modelID, providerID string, inputTokens, outputTokens int64, costDollars float64)
	usageMetering  func(types.UsageEvent)

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
	// outboxDone closes when the outbox Run loop has fully returned
	// (workers joined). Stop waits on it (bounded) so no Run goroutine
	// leaks past the handler's lifetime into later tests' package-var
	// tuning — the #1316 review round 2 race.
	outboxDone chan struct{}

	// outbox is the D3 durable-prompt outbox (design 0050, #907). nil
	// means the outbox is disabled (dev/test) and the legacy synchronous
	// send path is used; when set, POST /prompt accepts into the outbox
	// and a detached worker delivers via the adapter.
	outbox *outbox.Service

	// inbox is the unanswered-question inbox (#1313, epic-71 / 3a) — the
	// outbox's sibling for inbound asks. nil means the inbox is disabled
	// and asks revert to fire-and-forget (pre-inbox behavior). Set via
	// SetInboxStore before Start.
	inbox *inbox.Service

	// adapter is the US-65.3 Agent Adapter seam. The migrated
	// session/message cluster is adapter-only since #828 batches 1+2
	// (nil adapter -> adapterUnavailable guard, typed 503); the
	// outbox-backed queue view routes never consult it, and
	// RenameSessionInAgent fails with its own error. The remaining
	// nil-checks are the fail-closed guards themselves (proxy_handlers
	// ×10 incl. the rename helper, input ×8, permissions, session index
	// ×3, parents, stream/user-events flight gates, inbox wiring) and
	// the lifecycle wiring (Start()'s outbox verifier hooks,
	// proxy_lifecycle.go; the phase-change sweep gate, proxy_events.go)
	// — the final #828 batch's required-constructor change collapses
	// them all. The dialect field was retired in batch 4 (zero readers
	// remained; agent.Dialect's interface went with it — the opencode
	// Dialect struct stays, agent-side: the adapter and agentd's store
	// readers consume it, no platform/handler code).
	// Set via SetAdapter before Start().
	adapter agent.Adapter

	// modelPolicyChecker enforces org allowed-models/allowed-providers on
	// explicit per-prompt model overrides (2026-08-16 follow-up: policy was
	// enforced only by hiding models in ListModels). nil = no enforcement
	// (personal deployments). Read on the prompt path after Start, so it is
	// set once via SetModelPolicyChecker before Start — same invariant as
	// SetAdapter.
	modelPolicyChecker OrgPolicyChecker

	// Epic 68 US-68.2 upload overrides. Zero → env-derived defaults
	// (UPLOAD_MAX_BYTES / UPLOAD_TIMEOUT_MS); set via SetUploadLimitsForTest.
	// Written only before Start (test wiring), read on the upload path.
	uploadMaxBytesOverride      int64
	uploadStreamTimeoutOverride time.Duration

	// agentdPortOverride redirects the ABI-surface port (terminus +
	// actions) at a test stub; zero → agentd.AgentdPort.
	agentdPortOverride int
}

func NewProxyHandler(
	k8sClient pkginterfaces.KubernetesClient,
	logger pkginterfaces.LoggerInterface,
	namespace string,
	httpClient *http.Client,
	adapter agent.Adapter,
) (*ProxyHandler, error) {
	if k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client cannot be nil")
	}
	if logger == nil {
		return nil, fmt.Errorf("logger cannot be nil")
	}
	if adapter == nil {
		return nil, fmt.Errorf("agent adapter cannot be nil")
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
		k8sClient:  k8sClient,
		httpClient: httpClient,
		logger:     logger,
		namespace:  namespace,
		connCount:  make(map[string]int),
		busyAlerts: make(map[string]time.Time),
		adapter:    adapter,
	}, nil
}

// SetInboxStore wires the unanswered-question inbox (#1313). nil (or a
// nil service) leaves the inbox disabled — asks revert to
// fire-and-forget.
func (h *ProxyHandler) SetInboxStore(s *inbox.Service) {
	if s == nil {
		return
	}
	if h.started {
		panic("SetInboxStore called after Start — request goroutines may already be reading h.inbox")
	}
	h.inbox = s
}

// SetInboxStoreForTest wires the inbox after Start (tests only;
// production wires via SetInboxStore before Start).
func (h *ProxyHandler) SetInboxStoreForTest(s *inbox.Service) {
	h.inbox = s
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

// GetOutbox exposes the outbox service (admin authority-flip endpoints).
func (h *ProxyHandler) GetOutbox() *outbox.Service {
	return h.outbox
}

// GetOutboxForTest is the historical test alias.
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
// SetAdapterForTest wires the Agent Adapter after construction
// (out-of-package integration tests only; in-package tests assign the
// field directly).
func (h *ProxyHandler) SetAdapterForTest(a agent.Adapter) {
	h.adapter = a
}

func (h *ProxyHandler) SetUserBrokerForTest(b *eventbroker.UserEventBroker) {
	h.userBroker = b
}

// SetModelPolicyChecker wires the org-policy checker for per-prompt model
// override enforcement. Optional (nil = unenforced). Panics after Start for
// the same race-safety reason as SetResolverHost.
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
	h.host().SetStateStore(store)
}

// SetResolverHost adopts a pre-built ResolverHost (app.go constructs the
// host first, builds the Agent Adapter over it, then passes the adapter
// to the handler ctor and adopts the host here — one shared password
// cache + invalidation for both). Panics if called after Start(): same
// pre-Start-only invariant as SetStateStore.
func (h *ProxyHandler) SetResolverHost(host *ResolverHost) {
	if host == nil {
		return
	}
	if h.started {
		panic("SetResolverHost called after Start — request goroutines may already be reading resolvers")
	}
	h.resolvers = host
}

// chatErrorBufferCap bounds the amount of upstream body buffered when an
// onErrorBody transform is supplied. Chat error responses are small JSON
// payloads (~1 KB); a runaway upstream must not consume unbounded memory.
// Truncation is handled by EnrichChatErrorBody (non-JSON wraps to a 1024-byte
// "message" field), so anything above this cap is dropped on the floor.
const chatErrorBufferCap = 64 * 1024

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
