package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/lenaxia/llmsafespaces/controller/internal/metrics"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (r *WorkspaceReconciler) setCondition(ws *v1.Workspace, condType v1.WorkspaceConditionType, status, reason, message string) {
	for i := range ws.Status.Conditions {
		if ws.Status.Conditions[i].Type == condType {
			if ws.Status.Conditions[i].Status == status && ws.Status.Conditions[i].Reason == reason {
				ws.Status.Conditions[i].Message = message
				return
			}
			ws.Status.Conditions[i].Status = status
			ws.Status.Conditions[i].Reason = reason
			ws.Status.Conditions[i].Message = message
			ws.Status.Conditions[i].LastTransitionTime = metav1.Now()
			return
		}
	}
	ws.Status.Conditions = append(ws.Status.Conditions, v1.WorkspaceCondition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

var (
	healthCheckInterval         = 15 * time.Second
	healthCheckBackoffInterval  = 60 * time.Second
	healthCheckFailureThreshold = int32(3)
	healthCheckGracePeriod      = 30 * time.Second
	agentdPort                  = agentd.AgentdPort
	agentdAdminPort             = agentd.AgentdAdminPort
	// US-22.5/22.6: Deep-status poll interval. /v1/statusz is expensive
	// (multiple opencode calls under mutex). Failures of the deep poll do
	// NOT increment ConsecutiveHealthFailures — they only mark fields stale.
	deepStatusInterval = 60 * time.Second
)

var healthHTTPClient = &http.Client{Timeout: 5 * time.Second}

// US-22.5: Separate client for deep-status with generous timeout (statusz can be slow).
var deepStatusHTTPClient = &http.Client{Timeout: 30 * time.Second}

func (r *WorkspaceReconciler) shouldRunHealthCheck(ws *v1.Workspace) bool {
	if ws.Status.StartTime != nil && time.Since(ws.Status.StartTime.Time) < healthCheckGracePeriod {
		return false
	}
	interval := healthCheckInterval
	if ws.Status.ConsecutiveHealthFailures >= healthCheckFailureThreshold {
		interval = healthCheckBackoffInterval
	}
	if ws.Status.LastHealthCheckAt == nil {
		return true
	}
	return time.Since(ws.Status.LastHealthCheckAt.Time) >= interval
}

func (r *WorkspaceReconciler) checkAgentHealth(ctx context.Context, ws *v1.Workspace) {
	logger := log.FromContext(ctx)

	if ws.Status.PodIP != "" && ws.Status.StartTime != nil && ws.Status.LastHealthCheckAt != nil {
		if ws.Status.LastHealthCheckAt.Before(ws.Status.StartTime) {
			ws.Status.ConsecutiveHealthFailures = 0
			ws.Status.LastHealthCheckAt = nil
		}
	}

	if !r.shouldRunHealthCheck(ws) {
		return
	}
	if ws.Status.PodIP == "" {
		return
	}

	// US-22.5: Liveness check via /v1/healthz (cheap, process-only, never
	// calls opencode). This drives ConsecutiveHealthFailures and pod-restart
	// decisions. Under SSE load, /v1/healthz still responds < 100ms because
	// it has zero opencode dependency (US-22.1).
	endpoint := fmt.Sprintf("http://%s:%d/v1/healthz", ws.Status.PodIP, agentdAdminPort)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return
	}

	resp, err := healthHTTPClient.Do(req)

	now := metav1.Now()
	ws.Status.LastHealthCheckAt = &now

	if err != nil {
		ws.Status.ConsecutiveHealthFailures++
		// Pod unreachable — clear the terminal-verified delivery state
		// (US-70.1): no evidence from a dead pod beats stale evidence.
		ws.Status.SecretsDelivery = nil
		// Same evidence rule for the deferred-apply surface (#1342): a
		// dead pod's deferral state is unknown, not pending.
		removeCondition(ws, v1.WorkspaceConditionCredentialsApplyPending)
		r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "Unknown",
			v1.ReasonHealthCheckFailed, err.Error())
		if ws.Status.ConsecutiveHealthFailures >= healthCheckFailureThreshold {
			logger.Info("Agent unreachable beyond threshold; restarting pod",
				"failures", ws.Status.ConsecutiveHealthFailures, "pod", podName(ws.Name, string(ws.UID)), "lastError", err.Error())
			r.restartAgentPod(ctx, ws)
		}
		return
	}

	defer func() { _ = resp.Body.Close() }()

	var healthResp agentd.HealthzResponse
	if err := json.NewDecoder(resp.Body).Decode(&healthResp); err != nil {
		ws.Status.ConsecutiveHealthFailures++
		// Response undecodable: same reasoning as unreachable — we
		// can't trust any prior value, clear to nil.
		ws.Status.SecretsDelivery = nil
		removeCondition(ws, v1.WorkspaceConditionCredentialsApplyPending)
		r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "Unknown",
			v1.ReasonHealthCheckFailed, "failed to decode healthz response")
		return
	}

	if !healthResp.Healthy {
		ws.Status.ConsecutiveHealthFailures++
		// Agent reports unhealthy: don't trust its delivery signal.
		ws.Status.SecretsDelivery = nil
		removeCondition(ws, v1.WorkspaceConditionCredentialsApplyPending)
		r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "False",
			v1.ReasonAgentUnhealthy, "agent process not responding")
		if ws.Status.ConsecutiveHealthFailures >= healthCheckFailureThreshold {
			logger.Info("Agent unhealthy beyond threshold; restarting pod",
				"failures", ws.Status.ConsecutiveHealthFailures, "pod", podName(ws.Name, string(ws.UID)))
			r.restartAgentPod(ctx, ws)
		}
		return
	}

	// Liveness passed — reset failure counter.
	ws.Status.ConsecutiveHealthFailures = 0
	// US-70.1 (design 0057 I4/I10): mirror the terminal-verified
	// spawn-env delivery state. Set only when the agentd reports it —
	// a healthy scrape from a pre-US-70.1 runtime (mixed fleet, W15)
	// omits the field and must not clear a previously-reported value;
	// scrape failures clear it above.
	if healthResp.SpawnEnv != nil {
		reason := ""
		if healthResp.SpawnEnv.Degraded {
			reason = healthResp.SpawnEnv.Reason
		}
		if reason == "" && healthResp.SpawnEnv.FilesDegraded {
			reason = healthResp.SpawnEnv.FilesReason
		}
		// US-72.4 (design 0058 §4.5/§4.6): the relay-only liveness degrade
		// rides the SAME SecretsDelivery surface the US-72.3 staging
		// classification reads (classifyRelayStale / relayRouterRejection).
		// Precedence: a relay reason WINS over a co-present spawn-env
		// reason — token_expired/credential_stale/reject codes are
		// class-critical (they select the escalation/rejection branch) and
		// must not be masked, while every co-present spawn_env_* code maps
		// to the same delivery class a relay_unreachable would (no
		// classification is lost either way).
		if healthResp.Relay != nil && healthResp.Relay.DegradedReason != "" {
			reason = healthResp.Relay.DegradedReason
		}
		ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{
			SpawnedRev:     healthResp.SpawnEnv.SpawnedRev,
			DegradedReason: reason,
			FilesRev:       healthResp.SpawnEnv.FilesRev,
			RelayRevision:  relayRevisionOf(healthResp.Relay),
		}
	} else if healthResp.Relay != nil {
		// Relay liveness without spawn-env evidence (a pre-US-70.1 runtime
		// cannot happen post-US-72.4, but the surfaces are independent):
		// still carry the relay slice — it is evidence from a live pod.
		ws.Status.SecretsDelivery = &v1.SecretsDeliveryStatus{
			DegradedReason: healthResp.Relay.DegradedReason,
			RelayRevision:  relayRevisionOf(healthResp.Relay),
		}
	}
	// US-72.6 (design 0058 §8): the one-time legacy-key scrub report →
	// the LegacyKeysScrubbed condition (+ the exactly-once event).
	r.mirrorLegacyScrub(ctx, ws, healthResp.LegacyScrub)
	// #1342 item 4 (L11): mirror the deferred-credential-apply state so
	// operators see WHY a credential change has not applied — it rides a
	// maintenance window behind busy sessions. Absent field (nothing
	// pending, or a pre-#1342 runtime) clears the condition.
	if healthResp.PendingApply != nil {
		r.setCondition(ws, v1.WorkspaceConditionCredentialsApplyPending, "True",
			v1.ReasonCredentialsApplyDeferred,
			fmt.Sprintf("credential change deferred behind %d busy session(s) for %ds (applies on idle or stalled-session interrupt)",
				healthResp.PendingApply.BusySessions, healthResp.PendingApply.WaitingSeconds))
	} else {
		removeCondition(ws, v1.WorkspaceConditionCredentialsApplyPending)
	}
	r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "True",
		v1.ReasonAgentHealthy, appendAgentWarnings(
			fmt.Sprintf("agentd alive, uptime=%ds", healthResp.UptimeSeconds), healthResp.Warnings))
}

// mirrorLegacyScrub (US-72.6, design 0058 §8): healthz's (/v1/healthz —
// the surface checkAgentHealth polls) one-time scrub report → the
// LegacyKeysScrubbed condition (idempotent by setCondition's
// status+reason dedupe) + the LegacyKeysScrubbed event EXACT ONCE per
// report-change (the report is static after the boot scrub, so the
// event fires on first observation only — a persisted last-observed
// annotation makes the idempotency survive controller restarts).
func (r *WorkspaceReconciler) mirrorLegacyScrub(ctx context.Context, ws *v1.Workspace, rep *agentd.LegacyScrubHealth) {
	if rep == nil {
		return
	}
	summary := fmt.Sprintf("auth=%d config=%d", rep.AuthKeysRemoved, rep.ConfigKeysRemoved)
	noticeable := rep.AuthKeysRemoved > 0 || rep.ConfigKeysRemoved > 0 || rep.Error != ""
	if rep.Error != "" {
		r.setCondition(ws, v1.WorkspaceConditionLegacyKeysScrubbed, "False", "ScrubError", rep.Error)
	} else if noticeable {
		r.setCondition(ws, v1.WorkspaceConditionLegacyKeysScrubbed, "True", "KeysRemoved", "legacy keys removed: "+summary)
	} else {
		r.setCondition(ws, v1.WorkspaceConditionLegacyKeysScrubbed, "True", "Clean", "clean")
	}
	if !noticeable || r.Recorder == nil {
		return
	}
	const annKey = "llmsafespaces.dev/legacy-scrub-reported"
	stamp := summary
	if rep.Error != "" {
		stamp = "error"
	}
	if ws.Annotations[annKey] == stamp {
		return // already reported this exact outcome (in-memory fast path)
	}
	// PERSIST the stamp via a FULL object Update on a FRESH fetch — the
	// health-check's r.Status().Update writes only the status subresource
	// and DROPS metadata, so an in-memory-only stamp would re-fire the
	// event on every reconcile (~15s cadence, forever); and updating the
	// caller's live object directly would clobber its accumulated status
	// mutations (the fake client resets status on plain Update under a
	// registered status subresource — the same reason
	// clearForceRecycleAnnotation updates a fresh object and RV-syncs).
	var fresh v1.Workspace
	if err := r.Get(ctx, types.NamespacedName{Name: ws.Name, Namespace: ws.Namespace}, &fresh); err != nil {
		return // a later pass retries (the in-memory stamp stays unset)
	}
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[annKey] = stamp
	if err := r.Update(ctx, &fresh); err != nil {
		return // a later pass retries honestly; never silently dropped
	}
	ws.ResourceVersion = fresh.ResourceVersion // keep the caller's later Status().Update conflict-free
	if ws.Annotations == nil {                 // in-memory fast path for this pass
		ws.Annotations = map[string]string{}
	}
	ws.Annotations[annKey] = stamp
	if rep.Error != "" {
		r.Recorder.Eventf(ws, "Warning", "LegacyKeysScrubbed",
			"one-time legacy-key scrub ERRORED: %s", rep.Error)
	} else {
		r.Recorder.Eventf(ws, "Normal", "LegacyKeysScrubbed",
			"one-time legacy-key scrub complete (%s)", summary)
	}
}

// relayRevisionOf extracts the applied relay revision from a relay
// liveness slice (nil-safe).
func relayRevisionOf(relay *agentd.RelayHealth) string {
	if relay == nil {
		return ""
	}
	return relay.AppliedRevision
}

// appendAgentWarnings suffixes agentd's boot-time degradation notices to a
// condition message so users see them via the API's agentHealth.message and
// the structured warnings field (e.g. an unresolvable default model —
// incident 2026-08-16). The API's parsers are pinned against this suffix:
// versionRe excludes ";" (PR #909 review round — `\S+` captured "1.18.10;")
// and warningsRe extracts the suffix as structured data. Warning copy MUST
// NOT contain semicolons (agentd-side renderer contract).
func appendAgentWarnings(msg string, warnings []string) string {
	if len(warnings) == 0 {
		return msg
	}
	return msg + "; warnings: " + strings.Join(warnings, "; ")
}

// restartAgentPod performs the controller-initiated pod restart shared by
// the unreachable and unhealthy health-check paths: deletes the pod,
// decrements WorkspacesRunning, transitions to Creating, and bumps the
// restart counters.
func (r *WorkspaceReconciler) restartAgentPod(ctx context.Context, ws *v1.Workspace) {
	r.deletePodByName(ctx, podName(ws.Name, string(ws.UID)), ws.Namespace)
	metrics.WorkspacesRunning.WithLabelValues(ws.Spec.Runtime, string(ws.Spec.SecurityLevel)).Dec()
	ws.Status.Phase = v1.WorkspacePhaseCreating
	ws.Status.PodIP = ""
	ws.Status.Endpoint = ""
	ws.Status.RestartCount++
	ws.Status.ControllerRestartCount++
	metrics.WorkspaceControllerRestartsTotal.Inc()
	ws.Status.ConsecutiveHealthFailures = 0
}

// maybeEnrichAgentStatus calls enrichAgentStatus at most once per
// deepStatusInterval (60s). Tracks last-call time in-memory per workspace.
func (r *WorkspaceReconciler) maybeEnrichAgentStatus(ctx context.Context, ws *v1.Workspace) {
	if ws.Status.StartTime == nil || ws.Status.PodIP == "" {
		return
	}
	if time.Since(ws.Status.StartTime.Time) < healthCheckGracePeriod {
		return
	}

	r.lastDeepStatusMu.Lock()
	if r.lastDeepStatus == nil {
		r.lastDeepStatus = make(map[string]time.Time)
	}
	last, exists := r.lastDeepStatus[ws.Name]
	if exists && time.Since(last) < deepStatusInterval {
		r.lastDeepStatusMu.Unlock()
		return
	}
	r.lastDeepStatus[ws.Name] = time.Now()
	elapsed := deepStatusInterval
	if exists {
		elapsed = time.Since(last)
		if elapsed > 2*deepStatusInterval {
			elapsed = deepStatusInterval
		}
	}
	r.lastDeepStatusMu.Unlock()

	r.enrichAgentStatus(ctx, ws, elapsed)
}

// enrichAgentStatus polls /v1/statusz for session/disk/provider metadata.
// It runs on a slower cadence (deepStatusInterval) and its failures are
// informational only — they never trigger pod restarts.
// statuszWithBearers GETs the admin-mux endpoint trying each Bearer
// credential in order; a 401 advances to the next candidate (#887 D5.1
// mixed fleet: distinct admin token first, workspace password fallback
// for legacy pods). Transport errors surface immediately; an empty
// candidate list or all-401 outcome is an error.
func statuszWithBearers(ctx context.Context, client *http.Client, url string, bearers []string) (*http.Response, error) {
	if len(bearers) == 0 {
		// No candidates (missing Secret / dev cluster with un-gated
		// agentd) — one unauthenticated attempt, matching the pre-#887
		// behavior for Secret-less environments.
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

// agentStatuszBearers assembles the Bearer try-order for the admin-mux
// /v1/statusz scrape (#887 D5.1): the DISTINCT admin token (file-delivery
// pods) first, then the workspace password (legacy pods whose agentd still
// authenticates with it). Read best-effort — a missing Secret yields an
// empty candidate list, which statuszWithBearers degrades to a single
// unauthenticated attempt (matching the pre-#887 behavior for Secret-less
// environments; the #761 drain relies on the resulting failure to fail
// open on the password-secret-missing path).
func (r *WorkspaceReconciler) agentStatuszBearers(ctx context.Context, ws *v1.Workspace) []string {
	bearers := []string{}
	pwSec := &corev1.Secret{}
	if pwErr := r.Get(ctx, types.NamespacedName{Name: passwordSecretName(ws.Name), Namespace: ws.Namespace}, pwSec); pwErr == nil {
		if v, ok := pwSec.Data["admin-token"]; ok && len(v) > 0 {
			bearers = append(bearers, string(v))
		}
		if v, ok := pwSec.Data["password"]; ok {
			bearers = append(bearers, string(v))
		}
	}
	return bearers
}

func (r *WorkspaceReconciler) enrichAgentStatus(ctx context.Context, ws *v1.Workspace, elapsed time.Duration) {
	if ws.Status.PodIP == "" {
		return
	}

	status, err := r.fetchAgentStatusz(ctx, deepStatusHTTPClient, ws)
	if err != nil {
		// Deep-status failure is informational only. Log at debug level.
		log.FromContext(ctx).V(1).Info("deep-status poll failed (informational only)", "error", err.Error())
		return
	}

	if !status.Healthy {
		// Agent reports unhealthy via deep-status. Don't populate metadata
		// from an unhealthy agent — the data may be stale or corrupt.
		return
	}

	if !status.Ready || len(status.Connected) == 0 {
		r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "False",
			v1.ReasonAgentDegraded, appendAgentWarnings(
				fmt.Sprintf("no providers connected (configured=%d, connected=%v)",
					status.ProvidersConfigured, status.Connected),
				status.Warnings))
		r.setCondition(ws, v1.WorkspaceConditionProviderReady, "False",
			v1.ReasonProvidersNotConnected, fmt.Sprintf("no providers connected (configured=%d, connected=%v)",
				status.ProvidersConfigured, status.Connected))
		// Degraded: don't populate session/disk metadata — providers aren't
		// connected so session data is meaningless.
		return
	}

	// Populate agent-reported metadata on CRD status.
	ws.Status.ActiveSessions = int32(status.SessionsActive) //nolint:gosec // G115: int->int32 bounded by pod resource limits
	if len(status.Sessions) > 0 {
		sessions := make([]v1.AgentSessionStatus, len(status.Sessions))
		for i, s := range status.Sessions {
			sessions[i] = v1.AgentSessionStatus{ID: s.ID, Title: s.Title, Status: s.Status, ContextUsed: s.ContextUsed}
		}
		ws.Status.Sessions = sessions
	} else {
		ws.Status.Sessions = nil
	}
	userID := ws.Labels["user-id"]
	elapsedSecs := elapsed.Seconds()

	if status.Disk != nil {
		ws.Status.DiskUsedBytes = status.Disk.UsedBytes
		ws.Status.DiskTotalBytes = status.Disk.TotalBytes
		byteSecs := float64(status.Disk.UsedBytes) * elapsedSecs
		metrics.WorkspaceDiskUsedBytesSecondsTotal.WithLabelValues(ws.Name, userID).Add(byteSecs)
		metrics.UserDiskBytesSecondsTotal.WithLabelValues(userID).Add(byteSecs)
		metrics.WorkspaceDiskUsedBytes.WithLabelValues(ws.Name, userID).Set(float64(status.Disk.UsedBytes))
		// US-24.17: PVC DiskPressure detection. Threshold matches design
		// doc US-24.17-degraded-detection.md: ratio > 0.95 sets condition;
		// below 95% auto-clears. Never restarts the pod — degraded is a
		// signal, not a recoverable failure.
		if status.Disk.TotalBytes > 0 {
			ratio := float64(status.Disk.UsedBytes) / float64(status.Disk.TotalBytes)
			if ratio > 0.95 {
				r.setCondition(ws, v1.WorkspaceConditionDiskPressure, "True",
					v1.ReasonDiskPressure,
					fmt.Sprintf("disk %.0f%% full (%d/%d bytes)",
						ratio*100, status.Disk.UsedBytes, status.Disk.TotalBytes))
			} else {
				removeCondition(ws, v1.WorkspaceConditionDiskPressure)
			}
		}
	}
	if status.Memory != nil {
		ws.Status.MemoryUsedBytes = status.Memory.UsedBytes
		ws.Status.MemoryTotalBytes = status.Memory.TotalBytes
		byteSecs := float64(status.Memory.UsedBytes) * elapsedSecs
		metrics.WorkspaceMemoryUsedBytesSecondsTotal.WithLabelValues(ws.Name, userID).Add(byteSecs)
		metrics.UserMemoryBytesSecondsTotal.WithLabelValues(userID).Add(byteSecs)
		metrics.WorkspaceMemoryUsedBytes.WithLabelValues(ws.Name, userID).Set(float64(status.Memory.UsedBytes))
	}
	// US-44.5: surface memory pressure condition from agentd's cgroup
	// monitoring. agentd checks every 60s; the condition auto-clears
	// when pressure drops below the 85% threshold. Never restarts the
	// pod — this is a degraded signal, not a recoverable failure.
	if status.MemoryPressure {
		usedPct := 0.0
		if status.Memory != nil && status.Memory.TotalBytes > 0 {
			usedPct = float64(status.Memory.UsedBytes) / float64(status.Memory.TotalBytes) * 100
		}
		r.setCondition(ws, v1.WorkspaceConditionMemoryPressure, "True",
			v1.ReasonMemoryPressure,
			fmt.Sprintf("memory usage high (%.0f%%). Consider reducing concurrent sessions or increasing workspace memory limit.", usedPct))
	} else {
		removeCondition(ws, v1.WorkspaceConditionMemoryPressure)
	}
	if status.CPU != nil && status.CPU.UsageMicros > 0 {
		if ws.Status.CpuUsageMicros > 0 && status.CPU.UsageMicros >= ws.Status.CpuUsageMicros {
			deltaMs := float64(status.CPU.UsageMicros-ws.Status.CpuUsageMicros) / 1000.0
			metrics.WorkspaceCPUMillisecondsTotal.WithLabelValues(ws.Name, userID).Add(deltaMs)
			metrics.UserCPUMillisecondsTotal.WithLabelValues(userID).Add(deltaMs)
		}
		ws.Status.CpuUsageMicros = status.CPU.UsageMicros
		ws.Status.CpuLimitMicrosPerSec = status.CPU.LimitMicrosPerSec
	}
	if status.Context != nil {
		ws.Status.ContextUsed = status.Context.UsedTokens
		ws.Status.ContextTotal = status.Context.TotalTokens
	}

	r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "True",
		v1.ReasonAgentHealthy, appendAgentWarnings(
			fmt.Sprintf("connected=%v configured=%d sessions=%d version=%s",
				status.Connected, status.ProvidersConfigured, status.SessionsActive, status.AgentVersion),
			status.Warnings))
	// S18.11: Surface provider connectivity as a dedicated condition so
	// operators can `kubectl wait --for=condition=ProviderReady` without
	// regex-parsing the AgentHealthy message.
	r.setCondition(ws, v1.WorkspaceConditionProviderReady, "True",
		v1.ReasonProvidersReady, fmt.Sprintf("connected=%v", status.Connected))
}

// ptrQuantity is a small helper that parses a Kubernetes quantity
// string into a *resource.Quantity for use in EmptyDirVolumeSource
// SizeLimit and similar pointer-only fields. Panics on parse error
// (caller bugs); callers pass only literal constants from this
// package.
func ptrQuantity(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func removeCondition(ws *v1.Workspace, condType v1.WorkspaceConditionType) {
	filtered := ws.Status.Conditions[:0]
	for _, c := range ws.Status.Conditions {
		if c.Type != condType {
			filtered = append(filtered, c)
		}
	}
	ws.Status.Conditions = filtered
}
