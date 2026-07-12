package workspace

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/lenaxia/llmsafespaces/controller/internal/freemodels"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (r *WorkspaceReconciler) buildPod(ctx context.Context, workspace *v1.Workspace) (*corev1.Pod, error) {
	uid := string(workspace.UID)
	name := podName(workspace.Name, uid)

	runtimeImage, runtimeEnvName, err := resolveRuntimeImage(ctx, r.Client, workspace.Spec.Runtime)
	if err != nil {
		return nil, fmt.Errorf("resolving runtime image: %w", err)
	}

	// F1.4.2 (Epic 17): Read the per-workspace admin token from the
	// password Secret. Used as the `Authorization: Bearer <token>`
	// header for the readiness probe so kubelet can hit the
	// authenticated /v1/readyz endpoint. ensurePasswordSecret() runs
	// in handlePending before buildPod is reached, so the Secret
	// is guaranteed to exist; if Get fails we fall back to omitting
	// the header (probe will fail closed and the pod won't be ready
	// — observable + safe).
	adminToken := ""
	pwSec := &corev1.Secret{}
	if pwErr := r.Get(ctx, types.NamespacedName{Name: passwordSecretName(workspace.Name), Namespace: workspace.Namespace}, pwSec); pwErr == nil {
		if v, ok := pwSec.Data["password"]; ok {
			adminToken = string(v)
		}
	}

	labels := map[string]string{
		LabelApp:       AppName,
		LabelComponent: ComponentWorkspace,
		LabelWorkspace: workspace.Name,
		LabelRuntime:   sanitizeLabelValue(workspace.Spec.Runtime),
		LabelTenant:    sanitizeLabelValue(tenantID(workspace.Spec.Owner)),
	}

	annotations := map[string]string{
		"llmsafespaces.dev/created-by": "controller",
	}
	if runtimeEnvName != "" {
		annotations["llmsafespaces.dev/runtime-env"] = runtimeEnvName
	}

	trueVal := true
	falseVal := false

	mainContainer := corev1.Container{
		Name:    "workspace",
		Image:   runtimeImage,
		Command: []string{"/usr/local/bin/entrypoint-opencode.sh"},
		Ports: []corev1.ContainerPort{
			{ContainerPort: agentd.AgentPort, Name: "opencode", Protocol: corev1.ProtocolTCP},
			{ContainerPort: agentd.AgentdPort, Name: "agentd", Protocol: corev1.ProtocolTCP},
			{ContainerPort: agentd.AgentdAdminPort, Name: "agentd-admin", Protocol: corev1.ProtocolTCP},
		},
		Env: []corev1.EnvVar{
			{Name: "WORKSPACE_ID", Value: workspace.Name},
			{Name: "WORKSPACE_DIR", Value: agentd.WorkspacePath},
			{Name: "AGENTD_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: passwordSecretName(workspace.Name),
					},
					Key: "password",
				},
			}},
			// Enable the opencode v2 event system so session.next.step.ended
			// is emitted to the /event SSE stream. Without this flag the API
			// proxy never receives token-usage events and session_index.context_used
			// stays NULL for every session, causing the Sidebar to show "0/Unknown".
			// Proven by live cluster experiment: setting this flag on a running pod
			// caused context_used to be written within one second of the next LLM
			// step completing. See worklog 0263.
			{Name: "OPENCODE_EXPERIMENTAL_EVENT_SYSTEM", Value: "true"},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/v1/readyz",
					Port: intstr.FromInt(agentd.AgentdAdminPort),
					HTTPHeaders: func() []corev1.HTTPHeader {
						if adminToken == "" {
							return nil
						}
						return []corev1.HTTPHeader{
							{Name: "Authorization", Value: "Bearer " + adminToken},
						}
					}(),
				},
			},
			// Tight cadence (2026-06-23 perf audit, item #3). The startup
			// probe below handles boot-time tightening; this readiness
			// probe runs at steady-state and after startup completes.
			// Period=2s means a Ready transition happens at most 2s after
			// the agent's first /v1/readyz=200. Total ready budget is
			// FailureThreshold * Period = 60s, generous against transient
			// network issues.
			InitialDelaySeconds: 2, PeriodSeconds: 2, TimeoutSeconds: 2, FailureThreshold: 30,
		},
		// StartupProbe (2026-06-23 perf audit, item #4). When set,
		// kubelet pauses readiness/liveness probes until startup
		// succeeds (one HTTP 200 response from /v1/readyz). This lets
		// us probe at 1s during boot without paying the cost on every
		// steady-state liveness check. FailureThreshold=120 gives a
		// 2-minute boot budget, comfortably covering the relay-injector
		// restart cycle (~30s today, ~5s once item #1a lands) plus all
		// other init work.
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/v1/readyz",
					Port: intstr.FromInt(agentd.AgentdAdminPort),
					HTTPHeaders: func() []corev1.HTTPHeader {
						if adminToken == "" {
							return nil
						}
						return []corev1.HTTPHeader{
							{Name: "Authorization", Value: "Bearer " + adminToken},
						}
					}(),
				},
			},
			InitialDelaySeconds: 1, PeriodSeconds: 1, TimeoutSeconds: 2, FailureThreshold: 120,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/v1/healthz",
					Port: intstr.FromInt(agentd.AgentdAdminPort),
				},
			},
			InitialDelaySeconds: 15, PeriodSeconds: 30, TimeoutSeconds: 5, FailureThreshold: 6,
		},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			// The workspace PVC contains three named subtrees via explicit subPaths:
			//   workspace/ — user workspace data, opencode.db
			//   home/      — symlinks to credential paths (plaintext lives in tmpfs)
			//   tmp/       — init scripts, package caches; NOT credentials (US-35.7)
			{Name: "workspace", MountPath: "/workspace", SubPath: "workspace"},
			{Name: "sandbox-cfg", MountPath: "/sandbox-cfg", ReadOnly: true},
			// US-35.7: sandbox-runtime is RW tmpfs for credential output files
			// (agent-config.json, secrets-env) and symlink targets for $HOME-relative
			// credential paths. Wiped on pod death — no plaintext on PVC at rest.
			{Name: "sandbox-runtime", MountPath: "/sandbox-runtime"},
			{Name: "workspace", MountPath: "/tmp", SubPath: "tmp"},
			{Name: "workspace", MountPath: "/home/sandbox", SubPath: "home"},
		},
		Resources: resourceRequirementsFor(workspace),
	}

	volumes := []corev1.Volume{
		{Name: "workspace", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspace.Status.PVCName},
		}},
		// G15 (Epic 17): sandbox-cfg is tmpfs-backed (Memory medium) to
		// prevent plaintext secrets / session keys from touching node disk.
		// US-35.7: bumped from 4Mi to 32Mi for headroom on bootstrap secrets.json
		// with many providers.
		{Name: "sandbox-cfg", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: ptrQuantity("32Mi"),
		}}},
		// US-35.7: sandbox-runtime is RW tmpfs for credential OUTPUT files.
		// agent-config.json, secrets-env, and symlink targets for SSH/git/secrets/auth.json
		// live here. Wiped on pod death regardless of how it dies (SIGTERM, SIGKILL,
		// eviction) — no plaintext credential bytes persist on the PVC at rest.
		{Name: "sandbox-runtime", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium:    corev1.StorageMediumMemory,
			SizeLimit: ptrQuantity("96Mi"),
		}}},
	}

	var initContainers []corev1.Container

	// workspace-dirs init: unconditionally ensures all three PVC subPath
	// directories exist at the PVC root before any other init or the main
	// container mounts them. Without this, kubelet fails the pod with
	// "subPath not found" on a fresh PVC. Runs as the same non-root UID as
	// the main container; writes only to the PVC root.
	initContainers = append(initContainers, buildWorkspaceDirsInit(runtimeImage))

	// Epic 42 / 26: inject relay baseURL so agentd can configure the opencode
	// provider to route free-tier inference through the self-hosted relay fleet
	// for IP distribution. Empty InferenceRelayURL (the chart default) leaves
	// the env var unset; agentd then no-ops the relay injector and opencode
	// calls https://opencode.ai/zen/v1 directly using its built-in `public` key.
	relayBaseURL := r.InferenceRelayURL
	if relayBaseURL != "" {
		mainContainer.Env = append(mainContainer.Env,
			corev1.EnvVar{Name: "INFERENCE_RELAY_BASEURL", Value: relayBaseURL},
		)
	}

	// Workspace setup init (packages + initScript).
	if len(workspace.Spec.Packages) > 0 || workspace.Spec.InitScript != "" {
		initContainers = append(initContainers, buildWorkspaceSetupInit(workspace, runtimeImage))
	}

	// Credential setup init.
	credInit, pwVolume, bootstrapTokenVol, err := r.buildCredentialSetupInit(workspace, runtimeImage, relayBaseURL)
	if err != nil {
		return nil, err
	}
	initContainers = append(initContainers, credInit)
	volumes = append(volumes, pwVolume)
	volumes = append(volumes, bootstrapTokenVol)

	// Free-models ConfigMap volume (2026-06-23 cold-start optimization,
	// item #1a). Mounted optionally so a pod started before the
	// controller's first refresh — or on a cluster with the refresher
	// disabled — still boots cleanly. The credential-setup init script
	// copies the file if present; agentd's materialize subcommand reads
	// it to pre-render the relay-provider block in agent-config.json
	// before opencode starts, eliminating the in-pod opencode-restart
	// cycle that the legacy relay injector imposed.
	if relayBaseURL != "" {
		volumes = append(volumes, buildFreeModelsVolume())
	}

	// Epic 51 S51.1: Runtime class resolution. Per-workspace opt-out
	// (spec.runtimeClass) takes precedence; otherwise use the controller's
	// DefaultRuntimeClass (typically "gvisor" in production multi-tenant).
	// Empty string = runc (K8s default).
	runtimeClassName := r.DefaultRuntimeClass
	if workspace.Spec.RuntimeClass != nil {
		runtimeClassName = *workspace.Spec.RuntimeClass
	}

	// terminationGracePeriodSeconds (2026-06-23 perf audit, item #5).
	// The kubelet default of 30s wasted ~25s on every pod termination.
	//
	// agentd has TWO different shutdown budgets layered:
	//   1. The HTTP server's overall shutdown context is 25s
	//      (cmd/workspace-agentd/main.go runShutdown). It includes
	//      graceful HTTP server drain, background goroutine wait
	//      (5s), and the opencode child SIGTERM→SIGKILL fallback.
	//   2. The opencode child SIGTERM grace is 5s (managed_process.go
	//      stop()). After 5s without exit, agentd SIGKILLs opencode.
	//
	// Setting kubelet's terminationGracePeriodSeconds=5 means kubelet
	// will SIGKILL the entire pod 5s after sending SIGTERM,
	// short-circuiting agentd's outer 25s budget. That sounds
	// aggressive, but it's safe in this codebase because:
	//   - The 25s budget is a worst-case for a stuck HTTP server or
	//     hung goroutine; live cluster measurement (see worklog) shows
	//     pod-gone in ~2.2s after pod-delete in normal operation.
	//   - opencode's 5s SIGTERM window matches kubelet's 5s here;
	//     agentd will have just enough time to send SIGTERM and
	//     observe a clean exit before kubelet SIGKILLs the whole pod.
	//   - Even when agentd is killed mid-shutdown by kubelet, the only
	//     state on disk is the workspace PVC, which is not modified
	//     by shutdown. There is no graceful-state to lose.
	//
	// If the in-process measurement ever shows clean shutdowns
	// approaching 5s, raise this to 10s rather than back to 30s.
	terminationGrace := int64(5)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   workspace.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			InitContainers:                initContainers,
			Containers:                    []corev1.Container{mainContainer},
			Volumes:                       volumes,
			NodeSelector:                  buildNodeSelector(workspace),
			TerminationGracePeriodSeconds: &terminationGrace,
			// G17 (Epic 17): Sandbox pods MUST NOT automount the default
			// ServiceAccount token. The agent has no business calling the
			// K8s API; mounting the token only widens the blast radius for
			// a compromised sandbox. Without this, kubelet writes a JWT to
			// /var/run/secrets/kubernetes.io/serviceaccount/token that any
			// process inside the pod can read. See
			// `controller/internal/workspace/security_test.go` for the
			// regression that locks this in.
			//
			// Epic 35: the pod runs under the per-workspace SA
			// (workspace-<name>) so the projected SA token volume in the
			// init container is for the correct identity. AutomountServiceAccountToken
			// stays false — the projected token is an explicit volume mount
			// (init container only), not the default automount (which would
			// also appear in the main container).
			ServiceAccountName:           bootstrapSAName(workspace.Name),
			AutomountServiceAccountToken: &falseVal,
			// G22 (Epic 17 worklog 0088 RT-3.3): EnableServiceLinks
			// defaults to true in K8s, which materializes 30+
			// `<SVC>_SERVICE_HOST/PORT` env vars in the workspace
			// pod's PID-1 environ. This leaks namespace topology to
			// any process inside the sandbox (and to anyone who can
			// read /proc/PID/environ). Disable explicitly.
			EnableServiceLinks: &falseVal,
			SecurityContext:    buildPodSecurityContext(workspace),
		},
	}
	if runtimeClassName != "" {
		pod.Spec.RuntimeClassName = &runtimeClassName
	}
	return pod, nil
}

// resourceRequirementsFor maps the Workspace's spec.resources to a
// corev1.ResourceRequirements block. Closes F1.2.3 (Epic 17): pre-fix
// the controller never applied the operator-supplied resource limits,
// so workspace pods ran without quota and could DoS the node.
//
// Behavior:
//   - If spec.resources is nil, fall back to a sane default (matches
//     the kubebuilder defaults documented on `WorkspaceSpec`):
//     500m CPU, 512Mi memory. This guarantees every workspace carries
//     at least basic limits even when the operator submits a minimal
//     YAML.
//   - ephemeral-storage is intentionally NOT set on the pod. The
//     workspace's writable surfaces (PVC subPaths for /workspace,
//     /home, /tmp; Memory-backed emptyDir for /sandbox-cfg) do not
//     count toward node ephemeral storage. The only consumer is
//     kubelet's container log files, which kubelet already rotates
//     (~50 MiB per pod). A per-pod ephemeral limit added no
//     protection beyond what kubelet's own log rotation provides.
//   - Quantity parsing failures fall back to the default rather than
//     panicking. The CRD pattern + (future) webhook caps protect
//     against bad input; if both are bypassed (e.g. CRD validation
//     disabled cluster-wide), we degrade gracefully.
func resourceRequirementsFor(workspace *v1.Workspace) corev1.ResourceRequirements {
	const (
		defaultCPU    = "500m"
		defaultMemory = "512Mi"
		burstFactor   = 4
	)
	cpu := defaultCPU
	memory := defaultMemory
	cpuLimit := ""
	memoryLimit := ""
	if r := workspace.Spec.Resources; r != nil {
		if r.CPU != "" {
			cpu = r.CPU
		}
		if r.Memory != "" {
			memory = r.Memory
		}
		cpuLimit = r.CPULimit
		memoryLimit = r.MemoryLimit
	}
	parseOrDefault := func(s, fallback string) resource.Quantity {
		if q, err := resource.ParseQuantity(s); err == nil {
			return q
		}
		return resource.MustParse(fallback)
	}

	cpuReq := parseOrDefault(cpu, defaultCPU)
	memReq := parseOrDefault(memory, defaultMemory)

	// CPU limit: explicit > 4× request
	var cpuLim resource.Quantity
	if cpuLimit != "" {
		if q, err := resource.ParseQuantity(cpuLimit); err == nil {
			cpuLim = q
		} else {
			cpuLim = multiplyQuantity(cpuReq, burstFactor)
		}
	} else {
		cpuLim = multiplyQuantity(cpuReq, burstFactor)
	}

	// Memory limit: explicit > 4× request
	var memLim resource.Quantity
	if memoryLimit != "" {
		if q, err := resource.ParseQuantity(memoryLimit); err == nil {
			memLim = q
		} else {
			memLim = multiplyQuantity(memReq, burstFactor)
		}
	} else {
		memLim = multiplyQuantity(memReq, burstFactor)
	}

	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    cpuReq,
			corev1.ResourceMemory: memReq,
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    cpuLim,
			corev1.ResourceMemory: memLim,
		},
	}
}

func multiplyQuantity(q resource.Quantity, factor int64) resource.Quantity {
	if q.Format == resource.DecimalSI {
		return *resource.NewMilliQuantity(q.MilliValue()*factor, resource.DecimalSI)
	}
	return *resource.NewQuantity(q.Value()*factor, q.Format)
}

func buildPodSecurityContext(workspace *v1.Workspace) *corev1.PodSecurityContext {
	runAsUser := int64(1000)
	runAsGroup := int64(1000)
	runAsNonRoot := true
	if psc := workspace.Spec.PodSecurityContext; psc != nil {
		if psc.RunAsUser != 0 {
			runAsUser = psc.RunAsUser
		}
		if psc.RunAsGroup != 0 {
			runAsGroup = psc.RunAsGroup
		}
	}
	return &corev1.PodSecurityContext{
		RunAsUser:  &runAsUser,
		RunAsGroup: &runAsGroup,
		FSGroup:    &runAsGroup,
		// G44: pod-level RunAsNonRoot is defense-in-depth. Every
		// container today sets RunAsNonRoot: &trueVal explicitly (line
		// 151 for main, lines 595/621/671 for init containers), but a
		// future sidecar added without its own SecurityContext would
		// inherit the pod default. Setting it at the pod level ensures
		// that default is non-root, matching the container-level
		// guarantee. The kubelet enforces RunAsNonRoot by refusing to
		// start any container that resolves to UID 0, so this is a
		// hard fail at admission rather than a runtime surprise.
		RunAsNonRoot: &runAsNonRoot,
		// G24 (Epic 17 worklog 0088 RT-3.7): RuntimeDefault seccomp
		// profile blocks dangerous syscalls (unshare/clone/keyctl/
		// ptrace/etc.) at the kernel level. Defense-in-depth — cap-
		// drop ALL + NoNewPrivs:1 already EPERM these, but
		// RuntimeDefault hardens the boundary further at zero cost.
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func buildNodeSelector(workspace *v1.Workspace) map[string]string {
	arch := workspace.Spec.Architecture
	if arch == "" {
		arch = "amd64"
	}
	return map[string]string{
		"kubernetes.io/arch": arch,
	}
}

func (r *WorkspaceReconciler) buildCredentialSetupInit(workspace *v1.Workspace, runtimeImage string, relayBaseURL string) (corev1.Container, corev1.Volume, corev1.Volume, error) {
	credScript := `
set -e

# US-35.7: create symlink farm so credential files resolve to tmpfs, not PVC.
# The PVC paths ($HOME/.ssh, $HOME/.secrets, $HOME/.git-credentials,
# $WORKSPACE/.local/opencode/auth.json) become symlinks pointing into
# /sandbox-runtime/rt/*. On pod death, tmpfs is wiped — the PVC retains
# only dangling symlink inodes, no plaintext bytes.
mkdir -p /sandbox-runtime/rt/ssh /sandbox-runtime/rt/secrets /sandbox-runtime/rt
chmod 700 /sandbox-runtime/rt/ssh /sandbox-runtime/rt/secrets

# rm -rf is required: ln -s into an existing directory creates the symlink
# inside it. These are credential paths that reset() wipes on every reload
# — no user data is lost.
rm -rf /home/sandbox/.ssh /home/sandbox/.secrets /home/sandbox/.git-credentials
ln -s /sandbox-runtime/rt/ssh             /home/sandbox/.ssh
ln -s /sandbox-runtime/rt/secrets         /home/sandbox/.secrets
ln -s /sandbox-runtime/rt/git-credentials /home/sandbox/.git-credentials

mkdir -p /workspace/.local/opencode
rm -f /workspace/.local/opencode/auth.json
ln -s /sandbox-runtime/rt/auth.json /workspace/.local/opencode/auth.json

# 2026-06-23 cold-start optimization (item #1a): copy the cluster-wide
# free-models catalog into /sandbox-cfg so the materialize subcommand
# can render the relay agent-config.json block before opencode boots.
# Mounted optional: an absent file is normal (relay disabled or
# controller hasn't fetched yet). The script swallows the error and
# materialize falls back to the legacy in-pod relay injector path.
if [ -f /mnt/freemodels/models.json ]; then
  cp /mnt/freemodels/models.json /sandbox-cfg/free-models.json
fi

workspace-agentd bootstrap --workspace-id "$WORKSPACE_ID" --api-url "$LLMSAFESPACE_API_URL"
workspace-agentd materialize
# G21: install (not cp) so the password file is created with mode 0600
# regardless of the source Secret's defaultMode. cp preserves the
# source mode (0644 by default for K8s Secret projections), leaving
# the password world-readable in the pod filesystem. install -m 0600
# sets the mode atomically with the copy, so the file is never briefly
# world-readable even on slow filesystems.
install -m 0600 /mnt/secrets/password/password /sandbox-cfg/password
`
	pwVolume := corev1.Volume{
		Name: "pw-secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: passwordSecretName(workspace.Name)},
		},
	}

	credMounts := []corev1.VolumeMount{
		{Name: "sandbox-cfg", MountPath: "/sandbox-cfg"},
		{Name: "sandbox-runtime", MountPath: "/sandbox-runtime"},
		{Name: "pw-secret", MountPath: "/mnt/secrets/password", ReadOnly: true},
		{Name: "bootstrap-token", MountPath: "/var/run/bootstrap", ReadOnly: true},
		// US-35.7: RW PVC mounts needed for symlink creation on the PVC paths.
		// Without these, ReadOnlyRootFilesystem causes ln -s to silently fail.
		{Name: "workspace", MountPath: "/home/sandbox", SubPath: "home"},
		{Name: "workspace", MountPath: "/workspace", SubPath: "workspace"},
	}

	// 2026-06-23 cold-start optimization (item #1a). The free-models
	// ConfigMap is added to pod.Spec.Volumes by buildPod when a relay
	// URL is configured; we always mount it here when the volume is
	// present (init-side) so the cp in the script can read it. Optional
	// mount semantics live on the Volume itself (Optional: true), so an
	// absent CM is harmless — the cp simply finds no file.
	if relayBaseURL != "" {
		credMounts = append(credMounts, corev1.VolumeMount{
			Name: "free-models", MountPath: "/mnt/freemodels", ReadOnly: true,
		})
	}

	// Epic 35 US-35.4: projected SA token volume. The kubelet creates a token
	// for the pod's ServiceAccount (workspace-<name>) with the specified
	// audience and expiry. Mounted only on the init container — the main
	// container never sees this token (AutomountServiceAccountToken: false
	// suppresses the default mount; this is an explicit projected volume).
	tokenTTL := int64(600)
	bootstrapTokenVolume := corev1.Volume{
		Name: "bootstrap-token",
		VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
						Path:              "token",
						ExpirationSeconds: &tokenTTL,
						Audience:          bootstrapAudience,
					},
				}},
			},
		},
	}

	trueVal := true
	falseVal := false
	credInit := corev1.Container{
		Name:    "credential-setup",
		Image:   runtimeImage,
		Command: []string{"/bin/sh", "-c", credScript},
		Env: func() []corev1.EnvVar {
			env := []corev1.EnvVar{
				{Name: "WORKSPACE_ID", Value: workspace.Name},
				{Name: "LLMSAFESPACE_API_URL", Value: r.APIServiceURL},
				// 2026-06-24 PR #401 review fix: XDG_DATA_HOME must
				// match the value entrypoint-opencode.sh sets in the
				// MAIN container so agentd's materialize subcommand
				// (running in the INIT container) reads auth.json from
				// the same location opencode will read it from in the
				// main container — i.e. the symlink the init script
				// creates at /workspace/.local/opencode/auth.json
				// pointing into /sandbox-runtime/rt/auth.json (US-35.7).
				//
				// Without this, preBootAuthJSONPath falls back to
				// $HOME/.local/opencode/auth.json
				// (=/home/sandbox/.local/opencode/auth.json), which
				// for a fresh pod doesn't exist (correct by accident:
				// shouldSkipRelay returns false → relay proceeds), but
				// for a resumed pod with a stale pre-US-35.7 auth.json
				// at PVC:home/.local/opencode/auth.json containing a
				// personal key, the bypass check would silently miss
				// the key and the cold-start optimization would then
				// be lost (the legacy in-pod injector would pick up
				// the slack and skip injection itself, but the user
				// loses the ~6-8s savings).
				{Name: "XDG_DATA_HOME", Value: "/workspace/.local"},
			}
			// 2026-06-23 cold-start optimization (item #1a): propagate
			// the relay URL into the init container so the materialize
			// subcommand can pre-render the relay agent-config block
			// before opencode boots. Without this, materialize has no
			// way to know whether to inject relay (it currently runs
			// in the main container as a goroutine after opencode is
			// already up).
			if relayBaseURL != "" {
				env = append(env, corev1.EnvVar{Name: "INFERENCE_RELAY_BASEURL", Value: relayBaseURL})
			}
			return env
		}(),
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: credMounts,
	}
	return credInit, pwVolume, bootstrapTokenVolume, nil
}

// buildWorkspaceDirsInit returns an always-running init container that creates
// the three PVC subPath directories (workspace/, home/, tmp/) at the PVC root
// before any other init or the main container attempts to mount them.
// Without this, kubelet fails the pod with "subPath not found" on a fresh PVC.
func buildWorkspaceDirsInit(runtimeImage string) corev1.Container {
	trueVal := true
	falseVal := false
	return corev1.Container{
		Name:    "workspace-dirs",
		Image:   runtimeImage,
		Command: []string{"/bin/sh", "-c", "mkdir -p /pvc/workspace /pvc/home /pvc/tmp"},
		VolumeMounts: []corev1.VolumeMount{
			// Mount PVC root (no subPath) so we can create the subdirectories.
			{Name: "workspace", MountPath: "/pvc"},
		},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// buildFreeModelsVolume returns the volume spec for the cluster-wide
// free-models ConfigMap (2026-06-23 cold-start optimization, item #1a).
// The credential-setup init container mounts it at /mnt/freemodels and
// copies models.json into /sandbox-cfg/ so agentd's materialize
// subcommand can read it before opencode boots.
//
// Optional: true is critical — pods can be created before the
// controller's free-models refresher runs its first fetch (e.g.
// immediately after a fresh install), and the refresher can be
// disabled entirely via --enable-free-models-refresher=false. In
// either case, kubelet skips the mount silently and the credential-setup
// init script's `if [ -f ... ]` guard finds no file. agentd's
// materialize subcommand then falls back to the legacy in-pod
// relay-injector path (Phase D will short-circuit when this file IS
// present).
func buildFreeModelsVolume() corev1.Volume {
	optionalTrue := true
	return corev1.Volume{
		Name: "free-models",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: freemodels.ConfigMapName,
				},
				Optional: &optionalTrue,
			},
		},
	}
}

func buildWorkspaceSetupInit(workspace *v1.Workspace, runtimeImage string) corev1.Container {
	trueVal := true
	falseVal := false
	return corev1.Container{
		Name:    "workspace-setup",
		Image:   runtimeImage,
		Command: []string{"/bin/sh", "-c", buildWorkspaceSetupScript(workspace)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", SubPath: "workspace"},
			{Name: "workspace", MountPath: "/tmp", SubPath: "tmp"},
		},
		SecurityContext: &corev1.SecurityContext{
			ReadOnlyRootFilesystem:   &trueVal,
			RunAsNonRoot:             &trueVal,
			AllowPrivilegeEscalation: &falseVal,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// shellQuoteSingle wraps an argument in POSIX single quotes, escaping
// any embedded single-quote bytes via the standard `'\”` pattern.
// The result is safe to pass to /bin/sh as a single positional
// argument: nothing inside the quotes is interpreted by the shell.
//
// Closes F1.2.5 (Epic 17): pre-fix the controller did
//
//	args += " " + req
//	script += "pip install --target=... " + args
//
// which let an adversarial requirement string contain shell
// metacharacters (`;`, `|`, `\“, `$()`) and break out of the pip
// invocation. Post-fix every requirement is wrapped in single quotes,
// so the only thing pip / npm / go install ever sees is the literal
// requirement bytes — which they will reject as a parse error if
// adversarial. Defense in depth: the admission webhook also rejects
// these payloads at CREATE/UPDATE.
func shellQuoteSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func buildWorkspaceSetupScript(ws *v1.Workspace) string {
	script := "#!/bin/sh\nset -e\nmkdir -p /workspace/packages\n"
	for _, pkgSet := range ws.Spec.Packages {
		if len(pkgSet.Requirements) == 0 {
			continue
		}
		args := ""
		for _, req := range pkgSet.Requirements {
			args += " " + shellQuoteSingle(req)
		}
		rt := pkgSet.Runtime
		// `--` after the package-manager flags terminates argv parsing,
		// so even if a requirement somehow starts with `-` (admission
		// is normally blocking that), the package manager will treat
		// it as a positional argument and reject it as an unknown
		// package name rather than parsing it as a flag (RCE class —
		// see worklog 0098 / F1.2.5 validator pass 2).
		switch {
		case len(rt) >= 6 && rt[:6] == "nodejs":
			script += "cd /workspace/packages && npm install --" + args + "\n"
		case len(rt) >= 2 && rt[:2] == "go":
			for _, req := range pkgSet.Requirements {
				// `go install` does not support `--`; we rely on the
				// admission webhook + shellQuoteSingle. The webhook
				// rejects leading `-` and URL-shaped strings.
				script += "cd /workspace/packages && go install " + shellQuoteSingle(req) + "\n"
			}
		default:
			script += "pip install --target=/workspace/packages --" + args + "\n"
		}
	}
	if ws.Spec.InitScript != "" {
		// InitScript is ALREADY a multi-line shell payload deliberately
		// authored by the workspace owner. We do NOT shell-quote it (it
		// is meant to BE a script). The here-document delimiter
		// `INITSCRIPT` is literal-quoted so embedded $variables and
		// $(commands) are preserved verbatim. F1.2.5 explicitly does
		// NOT cover InitScript — that is by design a code-execution
		// surface.
		script += "cat > /tmp/init-script.sh << 'INITSCRIPT'\n"
		script += ws.Spec.InitScript + "\n"
		script += "INITSCRIPT\n"
		script += "chmod +x /tmp/init-script.sh\n"
		script += "/tmp/init-script.sh\n"
	}
	return script
}

// --- Setup ---
