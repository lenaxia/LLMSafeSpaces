// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// WorkspacePassword implements interfaces.WorkspacePasswordProvider (US-46.11).
func (h *ProxyHandler) WorkspacePassword(ctx context.Context, workspaceID string) (string, error) {
	return h.getPassword(ctx, workspaceID)
}

// GetWithBearers GETs an admin-mux endpoint trying each Bearer credential
// in order; 401 advances to the next candidate (#887 D5.1 mixed fleet:
// distinct admin token first, workspace password fallback). Non-401
// responses (including errors) return immediately. Empty candidates means
// one unauthenticated attempt (pre-#887 behavior for Secret-less dev
// environments).
// GetWithBearers is the exported form used by app.relayChecker (same
// package would be fine, but the checker lives in app).
func GetWithBearers(ctx context.Context, client *http.Client, url string, bearers []string) (*http.Response, error) {
	if len(bearers) == 0 {
		// No candidates — one unauthenticated attempt (pre-#887
		// behavior for Secret-less dev environments).
		bearers = []string{""}
	}
	for _, b := range bearers {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		if b != "" {
			req.Header.Set("Authorization", "Bearer "+b)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		_ = resp.Body.Close()
	}
	return nil, fmt.Errorf("all %d bearer candidates rejected (401) for %s", len(bearers), url)
}

// adminBearerCandidates returns credentials to try against a workspace
// pod's admin mux, in order: the DISTINCT admin token (#887 D5.1
// file-delivery pods; read from the Secret's admin-token key, uncached —
// these calls are reconnect/poll-path, not hot) then the supplied
// fallback (the workspace password, for legacy pods). Tolerates a nil k8s
// client (unit-test handlers) by returning just the fallback.
func (h *ProxyHandler) adminBearerCandidates(ctx context.Context, workspaceID, fallbackPassword string) []string {
	candidates := []string{}
	if h.k8sClient != nil {
		secretName := fmt.Sprintf("workspace-pw-%s", workspaceID)
		secret, err := h.k8sClient.Clientset().CoreV1().Secrets(h.namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			if tok := strings.TrimSpace(string(secret.Data["admin-token"])); tok != "" {
				candidates = append(candidates, tok)
			}
		}
	}
	if fallbackPassword != "" {
		candidates = append(candidates, fallbackPassword)
	}
	return candidates
}

func (h *ProxyHandler) getPassword(ctx context.Context, workspaceID string) (string, error) {
	// Cache-only lookup against the state store; the K8s Secret fetch
	// fallback stays local so the store remains pure-state with no I/O
	// dependencies. This separation is what allows US-45.4 to swap the
	// cache layer to Redis without dragging a K8s client into the store.
	if pw, ok := h.state().GetCachedPassword(ctx, workspaceID); ok {
		return pw, nil
	}

	secretName := fmt.Sprintf("workspace-pw-%s", workspaceID)
	secret, err := h.k8sClient.Clientset().CoreV1().Secrets(h.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading password secret %s: %w", secretName, err)
	}

	pw := string(secret.Data["password"])
	if pw == "" {
		return "", fmt.Errorf("password secret %s has empty password key", secretName)
	}

	h.state().SetCachedPassword(ctx, workspaceID, pw)
	return pw, nil
}

func (h *ProxyHandler) checkAndAddActiveSession(ctx context.Context, workspaceID, sessionID string, maxSessions int) bool {
	return h.state().CheckAndAddActiveSession(ctx, workspaceID, sessionID, maxSessions)
}

func (h *ProxyHandler) removeActiveSession(ctx context.Context, workspaceID, sessionID string) {
	h.state().RemoveActiveSession(ctx, workspaceID, sessionID)
}

func (h *ProxyHandler) isSessionActive(ctx context.Context, workspaceID, sessionID string) bool {
	return h.state().IsSessionActive(ctx, workspaceID, sessionID)
}

func (h *ProxyHandler) activeSessionCount(ctx context.Context, workspaceID string) int {
	return h.state().ActiveSessionCount(ctx, workspaceID)
}

func (h *ProxyHandler) acquireConnection(workspaceID string) bool {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.connCount[workspaceID] >= maxConnectionsPerWorkspace {
		return false
	}
	h.connCount[workspaceID]++
	return true
}

func (h *ProxyHandler) releaseConnection(workspaceID string) {
	h.connMu.Lock()
	defer h.connMu.Unlock()
	if h.connCount[workspaceID] > 0 {
		h.connCount[workspaceID]--
	}
	if h.connCount[workspaceID] == 0 {
		delete(h.connCount, workspaceID)
	}
}

func (h *ProxyHandler) connectionCount(workspaceID string) int {
	h.connMu.RLock()
	defer h.connMu.RUnlock()
	return h.connCount[workspaceID]
}

// invalidateCaches clears all per-workspace state on a phase transition
// that makes the cached state stale (suspend / terminate / fail). The
// connCount is intentionally NOT cleared — it represents in-flight HTTP
// connections that must finish naturally; clearing it would leak the
// connection-tracking accounting for live requests.
func (h *ProxyHandler) invalidateCaches(ctx context.Context, workspaceID string) {
	h.state().InvalidateAll(ctx, workspaceID)

	if h.sessionParents != nil {
		h.sessionParents.invalidate(workspaceID)
	}
}

// GetActiveSessions returns the IDs of all sessions currently marked
// active for the workspace. Public because it is called from outside
// the handlers package (admin tooling, canary checks).
func (h *ProxyHandler) GetActiveSessions(ctx context.Context, workspaceID string) []string {
	return h.state().GetActiveSessions(ctx, workspaceID)
}

// GetAuthoritativeActiveSessions queries the workspace pod's /v1/statusz
// for ground-truth busy/idle status (#792 Pattern 1). This eliminates
// the stale-forever window of the in-memory activeSess map: when the
// SSE stream drops or the API restarts mid-turn, the in-memory map
// retains stale "active" entries that make sessions appear stuck busy
// forever.
//
// Falls back to the in-memory activeSess map if the workspace is not
// Active (pod restarting, suspended). If the workspace IS Active but
// statusz fails (agentd crashed), returns empty — conservative: better
// to allow a new turn than to block the user on stale state.
//
// As a side effect, reconciles stale in-memory activeSess entries:
// sessions that statusz reports as idle are removed from the in-memory
// set. This self-heals the write-side gating path.
func (h *ProxyHandler) GetAuthoritativeActiveSessions(ctx context.Context, workspaceID string) map[string]bool {
	// If we can't reach K8s or the workspace pod, fall back to the
	// in-memory activeSess map. This handles test fixtures, non-Active
	// workspaces, and transient failures.
	v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
	if v1Err != nil || v1Client == nil {
		return h.fallbackActiveSessions(ctx, workspaceID)
	}
	ws, wsErr := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if wsErr != nil || ws == nil || ws.Status.Phase != phaseActive || ws.Status.PodIP == "" {
		return h.fallbackActiveSessions(ctx, workspaceID)
	}

	password, pwErr := h.getPassword(ctx, workspaceID)
	if pwErr != nil {
		return h.fallbackActiveSessions(ctx, workspaceID)
	}

	statuszCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	url := fmt.Sprintf("http://%s:%d/v1/statusz", ws.Status.PodIP, agentd.AgentdAdminPort) //nolint:gosec // G107: internal pod
	bearers := h.adminBearerCandidates(statuszCtx, workspaceID, password)
	resp, err := GetWithBearers(statuszCtx, h.httpClient, url, bearers)
	if err != nil {
		h.logger.Debug("GetAuthoritativeActiveSessions: statusz unavailable",
			"workspaceID", workspaceID, "error", err)
		return map[string]bool{}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return map[string]bool{}
	}

	var statusz agentd.StatuszResponse
	// 1 MB cap (was 16 KB). A workspace with many sessions produces a
	// statusz body well over 16 KB — each session entry is ~300 bytes, so
	// ~55 sessions exceeds the old cap. When the decode failed,
	// GetAuthoritativeActiveSessions silently returned an empty set,
	// causing the stuck-busy self-heal to stop working for heavy users.
	// 1 MB accommodates ~3,500 sessions while still bounding a malicious
	// or runaway upstream.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&statusz); err != nil {
		h.logger.Debug("GetAuthoritativeActiveSessions: failed to decode statusz",
			"workspaceID", workspaceID, "error", err)
		return map[string]bool{}
	}

	busySessions := make(map[string]bool)
	for _, sess := range statusz.Sessions {
		if sess.Status != "idle" {
			busySessions[sess.ID] = true
		}
	}

	// Self-heal: reconcile stale in-memory activeSess entries.
	for _, sess := range statusz.Sessions {
		if sess.Status == "idle" && h.isSessionActive(ctx, workspaceID, sess.ID) {
			h.removeActiveSession(ctx, workspaceID, sess.ID)
		}
	}

	return busySessions
}

func (h *ProxyHandler) fallbackActiveSessions(ctx context.Context, workspaceID string) map[string]bool {
	activeIDs := h.GetActiveSessions(ctx, workspaceID)
	if len(activeIDs) == 0 {
		return map[string]bool{}
	}
	result := make(map[string]bool, len(activeIDs))
	for _, id := range activeIDs {
		result[id] = true
	}
	return result
}

// SetActiveSessionsForTest seeds the active-session set for the workspace.
// Test-only — production callers must use CheckAndAddActiveSession so the
// maxSessions limit is enforced atomically. Kept as a public method on
// ProxyHandler so existing tests that poked the activeSess map can be
// migrated with a one-line change.
//
// Contract: the maxSessions argument passed to CheckAndAddActiveSession
// is `len(sessionIDs)+1` so all seeds succeed regardless of duplicate
// IDs in the input. Tests that need to exercise oversubscribe handling
// (i.e. seed a state that violates the maxSessions invariant) must call
// CheckAndAddActiveSession directly, not this helper.
func (h *ProxyHandler) SetActiveSessionsForTest(workspaceID string, sessionIDs []string) {
	ctx := context.Background()
	h.state().ClearActiveSessions(ctx, workspaceID)
	for _, sid := range sessionIDs {
		h.state().CheckAndAddActiveSession(ctx, workspaceID, sid, len(sessionIDs)+1)
	}
}

// HasActiveWorkspaceForTest reports whether the workspace currently has
// any active sessions (i.e. an active set was created and is non-empty).
// Used by tests asserting that the per-workspace entry is cleaned up
// after the last session is removed.
func (h *ProxyHandler) HasActiveWorkspaceForTest(workspaceID string) bool {
	return h.state().ActiveSessionCount(context.Background(), workspaceID) > 0
}

// --- Test helpers for state that was previously poked via map fields ---
//
// These mirror the existing SetActiveSessionsForTest pattern: a typed
// helper on ProxyHandler that delegates to the underlying store. Tests
// use these helpers; production code MUST use the production methods
// above. The helpers intentionally have a `ForTest` suffix so reviewers
// can grep for misuse in production code.

// SetCachedPasswordForTest seeds the password cache for a workspace.
func (h *ProxyHandler) SetCachedPasswordForTest(workspaceID, password string) {
	h.state().SetCachedPassword(context.Background(), workspaceID, password)
}

// GetCachedPasswordForTest returns whether a password is cached for the
// workspace (used by tests asserting cache invalidation).
func (h *ProxyHandler) GetCachedPasswordForTest(workspaceID string) (string, bool) {
	return h.state().GetCachedPassword(context.Background(), workspaceID)
}

// SetWorkspaceConfigForTest seeds the workspace-config cache.
func (h *ProxyHandler) SetWorkspaceConfigForTest(workspaceID string, cfg wsstate.Config) {
	h.state().SetWorkspaceConfig(context.Background(), workspaceID, cfg)
}

// GetWorkspaceConfigForTest returns the cached config for the workspace.
func (h *ProxyHandler) GetWorkspaceConfigForTest(workspaceID string) (wsstate.Config, bool) {
	return h.state().GetWorkspaceConfig(context.Background(), workspaceID)
}

// SetPriorPhaseForTest seeds the prior-phase entry.
func (h *ProxyHandler) SetPriorPhaseForTest(workspaceID, phase string) {
	h.state().SetPriorPhase(context.Background(), workspaceID, phase)
}

// GetPriorPhaseForTest returns the prior-phase entry if present.
func (h *ProxyHandler) GetPriorPhaseForTest(workspaceID string) (string, bool) {
	return h.state().GetPriorPhase(context.Background(), workspaceID)
}

// SetParentBackfilledForTest seeds the parent-backfill marker.
func (h *ProxyHandler) SetParentBackfilledForTest(workspaceID string) {
	h.state().SetParentBackfilled(context.Background(), workspaceID)
}

// MarkSessionDeletedForTest seeds a deleted-session tombstone.
func (h *ProxyHandler) MarkSessionDeletedForTest(workspaceID, sessionID string) {
	h.state().MarkSessionDeleted(context.Background(), workspaceID, sessionID)
}

// state returns the per-workspace state store, initializing it lazily.
// Tests that construct ProxyHandler via `&ProxyHandler{...}` literal
// bypass NewProxyHandler; this guard prevents a nil-store dereference.
// Production code goes through NewProxyHandler which initializes the
// store unconditionally, so the lazy path is never taken in production.
func (h *ProxyHandler) state() wsstate.Store {
	if h.stateStore == nil {
		h.stateStore = wsstate.NewInMemoryStore()
	}
	return h.stateStore
}

// --- Adapter resolver bridges (US-65.4 infrastructure) ---
//
// ProxyHandler already resolves pod IPs and passwords for its legacy
// proxyToWorkspace path. These thin wrappers expose that infrastructure
// as plain Go function/interface types so app.go can construct the
// Agent Adapter without duplicating the K8s + Secret lookup logic.
//
// Returns generic types (not opencode.PasswordResolver / PodIPResolver)
// to avoid importing pkg/agent/opencode from api/internal/handlers/,
// which would violate the agent-import boundary (US-65.6). app.go is
// in the allowed construction layer (api/internal/app/) and performs
// the type assertion to the opencode-specific resolver interfaces.

// AdapterPasswordResolver returns a function that resolves workspace
// passwords via ProxyHandler's existing getPassword method.
func (h *ProxyHandler) AdapterPasswordResolver() func(ctx context.Context, workspaceID string) (string, error) {
	return h.getPassword
}

// AdapterPodIPResolver returns an interface that resolves workspace pod
// IPs from the K8s CRD status. The userID parameter is accepted per the
// Adapter's interface contract but not used — the K8s workspace lookup
// is namespace-scoped, not user-scoped.
func (h *ProxyHandler) AdapterPodIPResolver() WorkspacePodIPResolver {
	return &proxyPodIPResolver{h: h}
}

// WorkspacePodIPResolver is the agent-generic pod IP resolver interface.
// app.go constructs the opencode Adapter with this value; Go's
// structural typing satisfies opencode.PodIPResolver (same method set)
// without an explicit cast.
type WorkspacePodIPResolver interface {
	GetWorkspacePodIP(ctx context.Context, userID, workspaceID string) (string, error)
}

type proxyPodIPResolver struct{ h *ProxyHandler }

func (r *proxyPodIPResolver) GetWorkspacePodIP(ctx context.Context, _, workspaceID string) (string, error) {
	v1Client, err := r.h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return "", fmt.Errorf("get K8s client: %w", err)
	}
	ws, err := v1Client.Workspaces(r.h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get workspace %s: %w", workspaceID, err)
	}
	if ws.Status.Phase != phaseActive || ws.Status.PodIP == "" {
		return "", nil
	}
	return ws.Status.PodIP, nil
}
