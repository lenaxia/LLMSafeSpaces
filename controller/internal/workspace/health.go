package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
		// Pod unreachable — clear UserCredsPresent so a stale "true"
		// from a previous pod doesn't suppress the API's auto-push
		// after recreation (worklog 0591). The field reflects the
		// CURRENT pod's state; unreachable means we don't know it.
		ws.Status.UserCredsPresent = nil
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
		ws.Status.UserCredsPresent = nil
		r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "Unknown",
			v1.ReasonHealthCheckFailed, "failed to decode healthz response")
		return
	}

	if !healthResp.Healthy {
		ws.Status.ConsecutiveHealthFailures++
		// Agent reports unhealthy: don't trust its cache-file signal.
		ws.Status.UserCredsPresent = nil
		r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "False",
			v1.ReasonAgentUnhealthy, "agent process not responding")
		if ws.Status.ConsecutiveHealthFailures >= healthCheckFailureThreshold {
			logger.Info("Agent unhealthy beyond threshold; restarting pod",
				"failures", ws.Status.ConsecutiveHealthFailures, "pod", podName(ws.Name, string(ws.UID)))
			r.restartAgentPod(ctx, ws)
		}
		return
	}

	// Liveness passed — reset failure counter and mirror the
	// user-creds signal onto CRD status so the API's workspace
	// watcher can consume it (worklog 0591).
	ws.Status.ConsecutiveHealthFailures = 0
	ucp := healthResp.UserCredsPresent
	ws.Status.UserCredsPresent = &ucp
	r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "True",
		v1.ReasonAgentHealthy, fmt.Sprintf("agentd alive, uptime=%ds", healthResp.UptimeSeconds))
}

// controllerRestartSafeModeThreshold is the ControllerRestartCount above
// which the workspace enters SafeMode without a stability window between
// restarts (US-24.7 AC 3 / US-24.13 entry trigger 2 — the A13 fix for
// persistent unreachability). ControllerRestartCount is reset to 0 on
// stability (maybeResetConsecutiveFailures) and on restartGeneration bump,
// so exceeding this threshold implies relentless health-check cycling.
const controllerRestartSafeModeThreshold = 5

// restartAgentPod performs the controller-initiated pod restart shared by
// the unreachable and unhealthy health-check paths: deletes the pod,
// decrements WorkspacesRunning, transitions to Creating, bumps the restart
// counters, and evaluates the persistent-unreachability SafeMode trigger.
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
	r.maybeEnterSafeModeFromRestarts(ctx, ws)
}

// maybeEnterSafeModeFromRestarts trips SafeMode when ControllerRestartCount
// exceeds the persistent-unreachability threshold. Idempotent: no-op when
// already in SafeMode. Emitted metrics use the "controller_restart" trigger
// label so operators can distinguish this entry from recovery-exhaustion
// (ConsecutiveFailures) entries labeled by failure class.
func (r *WorkspaceReconciler) maybeEnterSafeModeFromRestarts(ctx context.Context, ws *v1.Workspace) {
	if ws.Status.SafeMode || ws.Status.ControllerRestartCount <= controllerRestartSafeModeThreshold {
		return
	}
	ws.Status.SafeMode = true
	r.setCondition(ws, v1.WorkspaceConditionType("SafeMode"), "True", "PersistentUnreachability",
		fmt.Sprintf("Entering safe mode after %d controller-initiated restarts", ws.Status.ControllerRestartCount))
	log.FromContext(ctx).Info("Entering safe mode (controller restart threshold)",
		"controllerRestartCount", ws.Status.ControllerRestartCount)
	metrics.WorkspaceSafeModeActive.Inc()
	metrics.WorkspaceSafeModeEntriesTotal.WithLabelValues("controller_restart").Inc()
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
func (r *WorkspaceReconciler) enrichAgentStatus(ctx context.Context, ws *v1.Workspace, elapsed time.Duration) {
	if ws.Status.PodIP == "" {
		return
	}

	endpoint := fmt.Sprintf("http://%s:%d/v1/statusz", ws.Status.PodIP, agentdAdminPort)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return
	}

	// F1.4.2 (Epic 17): /v1/statusz now requires a Bearer token sourced
	// from the per-workspace password Secret. Read it best-effort —
	// missing Secret means failed auth on the request, which is logged
	// at V(1) like any other deep-status failure (informational only).
	pwSec := &corev1.Secret{}
	if pwErr := r.Get(ctx, types.NamespacedName{Name: passwordSecretName(ws.Name), Namespace: ws.Namespace}, pwSec); pwErr == nil {
		if v, ok := pwSec.Data["password"]; ok {
			req.Header.Set("Authorization", "Bearer "+string(v))
		}
	}

	resp, err := deepStatusHTTPClient.Do(req)
	if err != nil {
		// Deep-status failure is informational only. Log at debug level.
		log.FromContext(ctx).V(1).Info("deep-status poll failed (informational only)", "error", err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var status agentd.StatuszResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return
	}

	if !status.Healthy {
		// Agent reports unhealthy via deep-status. Don't populate metadata
		// from an unhealthy agent — the data may be stale or corrupt.
		return
	}

	if !status.Ready || len(status.Connected) == 0 {
		r.setCondition(ws, v1.WorkspaceConditionAgentHealthy, "False",
			v1.ReasonAgentDegraded, fmt.Sprintf("no providers connected (configured=%d, connected=%v)",
				status.ProvidersConfigured, status.Connected))
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
				r.removeCondition(ws, v1.WorkspaceConditionDiskPressure)
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
		r.removeCondition(ws, v1.WorkspaceConditionMemoryPressure)
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
		v1.ReasonAgentHealthy, fmt.Sprintf("connected=%v sessions=%d version=%s",
			status.Connected, status.SessionsActive, status.AgentVersion))
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

func (r *WorkspaceReconciler) removeCondition(ws *v1.Workspace, condType v1.WorkspaceConditionType) {
	filtered := ws.Status.Conditions[:0]
	for _, c := range ws.Status.Conditions {
		if c.Type != condType {
			filtered = append(filtered, c)
		}
	}
	ws.Status.Conditions = filtered
}
