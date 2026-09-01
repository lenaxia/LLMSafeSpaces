// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package workspace

// Security regression tests for Epic 17 sandbox pod hardening.
//
// G17 regression: ensure sandbox pods explicitly set
// AutomountServiceAccountToken=false. K8s defaults this to true, which
// would mount a default-namespace ServiceAccount token at
// /var/run/secrets/kubernetes.io/serviceaccount/token — readable by any
// process inside the pod. A compromised agent in the sandbox should never
// have a path to the K8s API.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func newWorkspaceForSecurity(t *testing.T) *v1.Workspace {
	t.Helper()
	return &v1.Workspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ws-sec-regression",
			Namespace: "default",
		},
		Spec: v1.WorkspaceSpec{
			// Use an explicit image reference so resolveRuntimeImage doesn't
			// require a RuntimeEnvironment CRD in the fake client.
			Runtime: "ghcr.io/lenaxia/llmsafespaces/runtimes/base:test",
		},
		Status: v1.WorkspaceStatus{
			PVCName: "pvc-sec-regression",
		},
	}
}

// TestG17_SandboxPodDoesNotAutomountSAToken is the headline regression for G17.
// Pre-fix the field was unset → kubelet defaults to true → SA token mounted.
// Post-fix the field is explicitly false → no token mount.
func TestG17_SandboxPodDoesNotAutomountSAToken(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, pod, "buildPod must not return nil pod")

	require.NotNil(t, pod.Spec.AutomountServiceAccountToken,
		"AutomountServiceAccountToken must be explicitly set, not relying on default (which is true)")
	require.False(t, *pod.Spec.AutomountServiceAccountToken,
		"AutomountServiceAccountToken must be false on sandbox pods (G17)")

	// Epic 35: the pod runs under the per-workspace SA so the projected token
	// is for the correct identity. The SA has no RBAC (no secrets read, no API
	// access) — automount=false ensures the main container never gets a token.
	require.Equal(t, "workspace-ws-sec-regression", pod.Spec.ServiceAccountName,
		"ServiceAccountName must be the per-workspace bootstrap SA (Epic 35)")
}

// TestSandboxPod_SecurityContextHardening locks in the existing security
// context guarantees so a future refactor can't silently weaken them.
func TestSandboxPod_SecurityContextHardening(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, pod)
	require.NotEmpty(t, pod.Spec.Containers, "pod must have at least one container")

	main := pod.Spec.Containers[0]
	require.NotNil(t, main.SecurityContext, "main container must have SecurityContext")

	require.NotNil(t, main.SecurityContext.ReadOnlyRootFilesystem)
	require.True(t, *main.SecurityContext.ReadOnlyRootFilesystem,
		"main container must have ReadOnlyRootFilesystem=true")

	require.NotNil(t, main.SecurityContext.RunAsNonRoot)
	require.True(t, *main.SecurityContext.RunAsNonRoot,
		"main container must have RunAsNonRoot=true")

	require.NotNil(t, main.SecurityContext.AllowPrivilegeEscalation)
	require.False(t, *main.SecurityContext.AllowPrivilegeEscalation,
		"main container must have AllowPrivilegeEscalation=false")

	require.NotNil(t, main.SecurityContext.Capabilities)
	require.Contains(t, main.SecurityContext.Capabilities.Drop, corev1.Capability("ALL"),
		"main container must drop ALL capabilities")
}

// TestSandboxPod_VolumeFootprint locks in the volume mount inventory.
// Any new mount widens the attack surface and must be added to the threat
// model and to this list deliberately.
func TestSandboxPod_VolumeFootprint(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	// Set InitScript so buildPod creates the workspace-setup init container,
	// enabling the SubPath regression assertion below to actually execute.
	ws.Spec.InitScript = "true"
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)

	expectedVolumes := map[string]bool{
		"workspace":       false,
		"sandbox-cfg":     false,
		"sandbox-runtime": false,
		"pw-secret":       false,
		"bootstrap-token": false,
	}
	for _, v := range pod.Spec.Volumes {
		if _, ok := expectedVolumes[v.Name]; ok {
			expectedVolumes[v.Name] = true
		}
	}
	for name, found := range expectedVolumes {
		require.True(t, found, "expected sandbox volume %q to be present", name)
	}

	// The "tmp" emptyDir must no longer exist — /tmp is now a PVC subPath.
	for _, v := range pod.Spec.Volumes {
		require.NotEqual(t, "tmp", v.Name, "tmp emptyDir volume must not exist; /tmp is now a PVC subPath")
	}

	// Epic 35: no workspace-secrets-* volume must exist — secretless injection
	// replaced it with the bootstrap-token projected volume.
	for _, v := range pod.Spec.Volumes {
		require.NotEqual(t, "user-secrets", v.Name,
			"user-secrets Secret volume must not exist (Epic 35: replaced by bootstrap-token)")
	}

	// bootstrap-token must be a projected ServiceAccountToken volume with the
	// correct audience and a bounded TTL.
	var bootstrapTokenVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "bootstrap-token" {
			bootstrapTokenVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, bootstrapTokenVol, "bootstrap-token volume must exist")
	require.NotNil(t, bootstrapTokenVol.Projected, "bootstrap-token must be a projected volume")
	require.Len(t, bootstrapTokenVol.Projected.Sources, 1, "bootstrap-token must have exactly one source")
	satProj := bootstrapTokenVol.Projected.Sources[0].ServiceAccountToken
	require.NotNil(t, satProj, "bootstrap-token source must be ServiceAccountToken")
	require.Equal(t, "llmsafespace-api", satProj.Audience,
		"bootstrap-token audience must be llmsafespace-api")
	require.NotNil(t, satProj.ExpirationSeconds)
	require.Equal(t, int64(600), *satProj.ExpirationSeconds,
		"bootstrap-token TTL must be 600s (10 minutes, K8s minimum)")

	// US-70.3: the projected token now ALSO mounts on the main container
	// in single-container mode — agentd runs there (--supervise) and its
	// resync endpoint must re-pull secrets for the pod's lifetime with a
	// kubelet-rotated token. The mount stays READ-ONLY (agentd only ever
	// reads it) and AutomountServiceAccountToken stays false (this is the
	// explicit audience-scoped projected volume, never the default token).
	require.NotEmpty(t, pod.Spec.Containers)
	main := pod.Spec.Containers[0]
	var mainTokenMount *corev1.VolumeMount
	for i := range main.VolumeMounts {
		if main.VolumeMounts[i].Name == "bootstrap-token" {
			mainTokenMount = &main.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, mainTokenMount,
		"single-container mode: bootstrap-token must mount on the main container (agentd --supervise re-pulls)")
	require.Equal(t, "/var/run/bootstrap", mainTokenMount.MountPath)
	require.True(t, mainTokenMount.ReadOnly,
		"the projected SA token is read-only on the main container")

	// US-35.7: sandbox-runtime must be tmpfs (Memory medium) and RW on main container.
	var sandboxRuntimeVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "sandbox-runtime" {
			sandboxRuntimeVol = &pod.Spec.Volumes[i]
			break
		}
	}
	require.NotNil(t, sandboxRuntimeVol, "sandbox-runtime volume must exist")
	require.NotNil(t, sandboxRuntimeVol.EmptyDir, "sandbox-runtime must be emptyDir")
	require.Equal(t, corev1.StorageMediumMemory, sandboxRuntimeVol.EmptyDir.Medium,
		"sandbox-runtime must be Memory medium (tmpfs)")
	require.NotNil(t, sandboxRuntimeVol.EmptyDir.SizeLimit)
	require.Equal(t, "96Mi", sandboxRuntimeVol.EmptyDir.SizeLimit.String(),
		"sandbox-runtime SizeLimit must be 96Mi")

	var sandboxRuntimeMount *corev1.VolumeMount
	for _, m := range main.VolumeMounts {
		if m.Name == "sandbox-runtime" {
			sandboxRuntimeMount = &m
			break
		}
	}
	require.NotNil(t, sandboxRuntimeMount, "sandbox-runtime must be mounted on main container")
	require.False(t, sandboxRuntimeMount.ReadOnly,
		"sandbox-runtime must be RW on main container (agentd writes here)")

	// sandbox-cfg must still be RO on main container (input-only volume).
	var sandboxCfgMount *corev1.VolumeMount
	for _, m := range main.VolumeMounts {
		if m.Name == "sandbox-cfg" {
			sandboxCfgMount = &m
			break
		}
	}
	require.NotNil(t, sandboxCfgMount, "sandbox-cfg must be mounted on main container")
	require.True(t, sandboxCfgMount.ReadOnly,
		"sandbox-cfg must remain RO on main container (input-only)")

	// US-35.7 (design 0053 S3 shape): the PVC symlink farm moved into the
	// agentd init-fs subcommand (cmd/workspace-agentd/init_fs.go — its
	// tests pin the farm itself). The controller-side invariant is the
	// mount topology that makes init-fs possible: PVC ROOT RW (subPath
	// + symlink creation), sandbox-cfg + sandbox-runtime RW, pw-secret RO.
	var platformInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "platform-init" {
			platformInit = &pod.Spec.InitContainers[i]
			break
		}
	}
	require.NotNil(t, platformInit, "platform-init init container must exist")
	initMountNames := make(map[string]bool)
	for _, m := range platformInit.VolumeMounts {
		initMountNames[m.Name+"@"+m.MountPath] = !m.ReadOnly
	}
	require.True(t, initMountNames["workspace@/pvc"],
		"platform-init must mount the PVC root RW (init-fs creates subPaths + symlinks)")
	require.True(t, initMountNames["sandbox-runtime@/sandbox-runtime"],
		"platform-init must mount /sandbox-runtime RW")
	require.True(t, initMountNames["pw-secret@/mnt/secrets/password"] == false,
		"pw-secret must be RO on platform-init (input-only)")

	// The PVC symlink farm is init-fs internal now, but its ordering
	// precondition survives: platform-init (subPath roots + symlinks)
	// must be the FIRST init container so later inits and the main
	// container see the completed filesystem.
	require.NotEmpty(t, pod.Spec.InitContainers)
	assert.Equal(t, "platform-init", pod.Spec.InitContainers[0].Name,
		"platform-init must run first (PVC prep precedes every consumer)")

	expectedMounts := map[string]bool{
		"workspace":   false,
		"sandbox-cfg": false,
	}
	for _, m := range main.VolumeMounts {
		if _, ok := expectedMounts[m.Name]; ok {
			expectedMounts[m.Name] = true
		}
	}
	for name, found := range expectedMounts {
		require.True(t, found, "expected main container mount %q to be present", name)
	}

	// The workspace PVC is mounted via explicit subPaths: /workspace
	// (workspace), /home/sandbox (home), /tmp (tmp), and — since Epic 69
	// US-69.2 — /platform (platform: the sessionstate durable cursor;
	// single-container mode only, the documented uid-1000 weakening).
	var workspaceMountPaths []string
	homeMountSubPath := ""
	workspaceMountSubPath := ""
	tmpMountSubPath := ""
	for _, m := range main.VolumeMounts {
		if m.Name == "workspace" {
			workspaceMountPaths = append(workspaceMountPaths, m.MountPath)
			if m.MountPath == "/home/sandbox" {
				homeMountSubPath = m.SubPath
			}
			if m.MountPath == "/workspace" {
				workspaceMountSubPath = m.SubPath
			}
			if m.MountPath == "/tmp" {
				tmpMountSubPath = m.SubPath
			}
		}
		require.NotEqual(t, "sandbox-home", m.Name, "sandbox-home emptyDir mount must not exist")
	}
	require.ElementsMatch(t, []string{"/workspace", "/home/sandbox", "/tmp", "/platform"}, workspaceMountPaths,
		"workspace PVC must be mounted at /workspace, /home/sandbox, /tmp, and /platform (Epic 69 platform/ subPath)")
	require.Equal(t, "workspace", workspaceMountSubPath,
		"/workspace mount must use SubPath: \"workspace\"")
	require.Equal(t, "home", homeMountSubPath,
		"/home/sandbox mount must use SubPath: \"home\"")
	require.Equal(t, "tmp", tmpMountSubPath,
		"/tmp mount must use SubPath: \"tmp\"")
	platformMountSubPath := ""
	for _, m := range main.VolumeMounts {
		if m.MountPath == "/platform" {
			platformMountSubPath = m.SubPath
		}
	}
	require.Equal(t, "platform", platformMountSubPath,
		"/platform mount must use SubPath: \"platform\" (Epic 69 US-69.2)")

	for _, v := range pod.Spec.Volumes {
		require.NotEqual(t, "sandbox-home", v.Name, "sandbox-home emptyDir volume must not exist")
	}

	// platform-init (agentd init-fs) must always be present (first in the
	// list) and must mount the workspace PVC at /pvc with no subPath so it
	// can create the workspace/, home/, and tmp/ subdirectories on a fresh
	// PVC (design 0053 S3 — absorbed from the deleted workspace-dirs init).
	require.NotEmpty(t, pod.Spec.InitContainers)
	require.Equal(t, "platform-init", pod.Spec.InitContainers[0].Name,
		"platform-init must be the first init container")
	var wsDirsInit = pod.Spec.InitContainers[0]
	var pvcRootMount *corev1.VolumeMount
	for i := range wsDirsInit.VolumeMounts {
		if wsDirsInit.VolumeMounts[i].Name == "workspace" && wsDirsInit.VolumeMounts[i].MountPath == "/pvc" {
			pvcRootMount = &wsDirsInit.VolumeMounts[i]
			break
		}
	}
	require.NotNil(t, pvcRootMount, "workspace-dirs must mount workspace PVC at /pvc")
	require.Empty(t, pvcRootMount.SubPath, "workspace-dirs /pvc mount must have no SubPath (PVC root)")

	// workspace-setup init container must use SubPath: "workspace" to match the main
	// container — a mismatch would silently write packages to the wrong PVC location.
	var wsSetupInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "workspace-setup" {
			wsSetupInit = &pod.Spec.InitContainers[i]
			break
		}
	}
	require.NotNil(t, wsSetupInit, "workspace-setup init container must exist when InitScript is set")
	for _, m := range wsSetupInit.VolumeMounts {
		if m.Name == "workspace" && m.MountPath == "/workspace" {
			require.Equal(t, "workspace", m.SubPath,
				"workspace-setup init container /workspace mount must use SubPath: \"workspace\"")
		}
		if m.Name == "workspace" && m.MountPath == "/tmp" {
			require.Equal(t, "tmp", m.SubPath,
				"workspace-setup init container /tmp mount must use SubPath: \"tmp\"")
		}
	}
}

// =============================================================================
// G4 — F1.2.3 + F1.2.5: Resources applied + Packages shell-injection guard
// =============================================================================
//
// F1.2.3 (High): pre-fix, the workspace pod was created without ANY
// resource requests or limits, so workspace.spec.resources was silently
// ignored. A user could declare resources but the controller would not
// apply them, leading to (a) operator-supplied limits not being honored
// and (b) workspace pods running without quota and DoSing the node.
//
// F1.2.5 (High): pre-fix, `buildWorkspaceSetupScript` interpolated each
// `Spec.Packages[].Requirements[]` directly into a shell command:
//     args += " " + req
//     script += "pip install --target=/workspace/packages" + args + "\n"
// A user with `Requirements: ["pkg; rm -rf /workspace"]` got code
// execution as the workspace user inside the init container. Defense
// in depth: the same payload is also blocked at admission time by the
// webhook — but the controller-side hardening guards against admission
// being disabled.

func TestG4_F123_PodAppliesSpecResources(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	ws.Spec.Resources = &v1.ResourceRequirements{
		CPU:    "750m",
		Memory: "1Gi",
	}
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotEmpty(t, pod.Spec.Containers)
	main := pod.Spec.Containers[0]

	require.NotEmpty(t, main.Resources.Limits,
		"main container must carry resource limits derived from spec.resources (F1.2.3)")
	require.NotEmpty(t, main.Resources.Requests,
		"main container must carry resource requests derived from spec.resources (F1.2.3)")

	cpuLimit := main.Resources.Limits[corev1.ResourceCPU]
	require.Equal(t, "3", cpuLimit.String(),
		"CPU limit must be 4× spec.resources.cpu (burstable QoS)")
	memLimit := main.Resources.Limits[corev1.ResourceMemory]
	require.Equal(t, "4Gi", memLimit.String(),
		"memory limit must be 4× spec.resources.memory (burstable QoS)")

	// Ephemeral-storage is intentionally NOT set on the pod (kubelet's
	// log rotation already bounds the only ephemeral consumer).
	_, hasEphReq := main.Resources.Requests[corev1.ResourceEphemeralStorage]
	require.False(t, hasEphReq, "ephemeral-storage must not appear in Requests")
	_, hasEphLim := main.Resources.Limits[corev1.ResourceEphemeralStorage]
	require.False(t, hasEphLim, "ephemeral-storage must not appear in Limits")
}

func TestG4_F123_PodAppliesDefaultsWhenSpecResourcesNil(t *testing.T) {
	// Workspaces created via the API server with kubebuilder defaults
	// will have Resources populated. Workspaces created via
	// `kubectl apply` of a minimal YAML (no resources block) get nil.
	// The controller must apply a sane default rather than emit a pod
	// with zero limits (which kubelet allows but is unbounded).
	ws := newWorkspaceForSecurity(t)
	ws.Spec.Resources = nil
	r := reconcilerFor(t)

	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	main := pod.Spec.Containers[0]
	require.NotEmpty(t, main.Resources.Limits,
		"nil spec.resources must fall back to chart defaults, not empty Limits")
}

func TestG4_F125_PackageRequirementsAreNeitherShellEscapedNorPositionallyInjected(t *testing.T) {
	// Defense-in-depth check at the controller layer. If admission is
	// bypassed (e.g. failurePolicy=Ignore + webhook outage), an
	// adversarial Requirements value must not produce an init script
	// where the requirement is interpreted as shell tokens.
	//
	// Verification: the adversarial bytes must appear ONLY inside a
	// single-quoted shell argument. We assert by walking the script
	// byte-by-byte tracking quote state, and by exec-ing `sh -n` for
	// a syntax check (no execution).
	ws := newWorkspaceForSecurity(t)
	ws.Spec.Packages = []v1.WorkspacePackageSet{
		{
			Runtime: "python:3.11",
			Requirements: []string{
				"requests==2.31.0",
				// Adversarial; would break out of pip install if not quoted.
				"requests; rm -rf /workspace",
				// Single-quote injection attempt.
				`evil'; rm -rf /; echo '`,
			},
		},
	}
	script := buildWorkspaceSetupScript(ws)

	// Walk the script byte-by-byte tracking quote state. POSIX rules:
	//   - inside a single-quoted region every byte is literal except
	//     for the closing single quote.
	//   - outside any quote, a backslash escapes the next byte (so
	//     `\'` produces a literal apostrophe). This is exactly what
	//     the standard `'\''` escape pattern relies on.
	//
	// We treat `\'` outside the quote as still-outside-quote (the
	// backslash-escaped quote is a literal byte, not a quote opener).
	inQuote := false
	for i := 0; i < len(script); i++ {
		c := script[i]
		if !inQuote && c == '\\' && i+1 < len(script) {
			// Skip the escaped byte; quote state unchanged.
			i++
			continue
		}
		if c == '\'' {
			inQuote = !inQuote
			continue
		}
		if inQuote {
			continue
		}
		const dangerous = "; rm "
		if i+len(dangerous) <= len(script) && script[i:i+len(dangerous)] == dangerous {
			t.Fatalf("F1.2.5 broken: adversarial '; rm ' appears OUTSIDE single quotes at offset %d:\n%s",
				i, script)
		}
	}

	// Run the script through `sh -n` to confirm it parses (proves
	// the quoting did not produce a syntax error that would cause
	// the init container to fail at runtime — which would also be a
	// regression for legitimate package installs).
	cmd := exec.Command("sh", "-n")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err,
		"the rendered init script must be valid POSIX shell syntax: %s", out)
}

// =============================================================================
// G4 part 2 — F1.2.4: Spec.NetworkAccess generates per-workspace NetworkPolicy
// =============================================================================
//
// Pre-fix: Spec.NetworkAccess was completely ignored by the controller.
// A user could declare `networkAccess.egress: [{domain: api.openai.com}]`
// expecting outbound traffic to be limited to that allow-list, but the
// controller never created a NetworkPolicy reflecting the field.
//
// Fix: when Spec.NetworkAccess is non-nil and has at least one Egress
// entry, the controller creates a NetworkPolicy named
// `workspace-egress-<ws>-<uid>` selecting just that workspace's pod
// (via WorkspaceID label). Egress rules are generated from the
// declared FQDN list with DNS-resolved /32 ipBlock entries plus
// DNS port 53 to kube-dns.
//
// Trade-off note: standard k8s NetworkPolicy doesn't support FQDN
// matching. We resolve at reconcile time and refresh on each pass
// (controllers reconcile periodically, so the IP set self-refreshes).
// Operators who need stricter FQDN guarantees should layer a Cilium
// FQDN policy on top — out of scope for this fix.

// stubResolver is a HostResolver implementation for hermetic tests:
// no real DNS calls. Returns the predefined response (or an error) for
// each host. Unknown hosts return NXDOMAIN-equivalent.
type stubResolver struct {
	hosts map[string][]string
	err   error
}

func (s stubResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	if v, ok := s.hosts[host]; ok {
		return v, nil
	}
	return nil, fmt.Errorf("no such host: %s", host)
}

func TestG4_F124_GeneratesPerWorkspaceEgressPolicyWhenDeclared(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	ws.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{
		Egress: []v1.WorkspaceEgressRule{
			{Domain: "api.openai.com"},
			{Domain: "api.anthropic.com"},
		},
	}
	r := reconcilerFor(t)
	r.HostResolver = stubResolver{hosts: map[string][]string{
		"api.openai.com":    {"104.18.0.1"},
		"api.anthropic.com": {"172.66.0.5"},
	}}

	np, err := r.buildWorkspaceEgressNetworkPolicy(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, np, "non-empty Egress must produce a NetworkPolicy")

	// Pod selector must scope to just this workspace.
	require.NotNil(t, np.Spec.PodSelector.MatchLabels)
	require.Equal(t, ws.Name, np.Spec.PodSelector.MatchLabels[LabelWorkspace],
		"per-workspace NetPol must select via LabelWorkspace")

	// PolicyTypes must include Egress.
	require.Contains(t, np.Spec.PolicyTypes, networkingv1.PolicyTypeEgress)

	// HTTPS rule with the public IPs as /32 ipBlocks.
	foundHTTPS := false
	for _, rule := range np.Spec.Egress {
		hasHTTPS := false
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntValue() == 443 {
				hasHTTPS = true
			}
		}
		if !hasHTTPS {
			continue
		}
		foundHTTPS = true
		// Confirm the resolved IPs landed.
		gotCIDRs := map[string]bool{}
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				gotCIDRs[peer.IPBlock.CIDR] = true
			}
		}
		require.True(t, gotCIDRs["104.18.0.1/32"],
			"HTTPS rule must include /32 for api.openai.com's public IP")
		require.True(t, gotCIDRs["172.66.0.5/32"],
			"HTTPS rule must include /32 for api.anthropic.com's public IP")
	}
	require.True(t, foundHTTPS,
		"per-workspace NetPol must allow HTTPS to declared FQDNs")
}

func TestG4_F124_DropsResolvedPrivateIPs(t *testing.T) {
	// Validator-found bypass class: a domain that resolves into RFC1918
	// or 169.254/16 must NOT produce an ipBlock allow even though it
	// passed the cluster-internal-suffix check at admission. (This is
	// defense-in-depth — the webhook now blocks the cluster-internal
	// suffixes, but if a public domain RESOLVES to a private IP, we
	// still drop it.)
	ws := newWorkspaceForSecurity(t)
	ws.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{
		Egress: []v1.WorkspaceEgressRule{
			{Domain: "rebound.example.com"},
		},
	}
	r := reconcilerFor(t)
	r.HostResolver = stubResolver{hosts: map[string][]string{
		"rebound.example.com": {"169.254.169.254", "10.0.0.5"},
	}}

	np, err := r.buildWorkspaceEgressNetworkPolicy(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, np)

	// No ipBlock allow whatsoever for this set — both IPs are private.
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				t.Fatalf("F1.2.4 broken: private/internal IP %q leaked into NetPol allow",
					peer.IPBlock.CIDR)
			}
		}
	}
}

func TestG4_F124_NilNetworkAccessProducesNoExtraPolicy(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	ws.Spec.NetworkAccess = nil
	r := reconcilerFor(t)

	np, err := r.buildWorkspaceEgressNetworkPolicy(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, np,
		"nil Spec.NetworkAccess must produce no per-workspace NetPol — chart-wide policy applies")
}

func TestG4_F124_EmptyEgressProducesNoExtraPolicy(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	ws.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{Egress: nil}
	r := reconcilerFor(t)

	np, err := r.buildWorkspaceEgressNetworkPolicy(context.Background(), ws)
	require.NoError(t, err)
	require.Nil(t, np,
		"empty Egress must produce no per-workspace NetPol")
}

// =============================================================================
// G22 (F1.4.2-adjacent) — EnableServiceLinks: false
// =============================================================================

func TestG22_PodHasEnableServiceLinksFalse(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerFor(t)
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, pod.Spec.EnableServiceLinks,
		"EnableServiceLinks must be explicitly set, not left to the default true (G22)")
	require.False(t, *pod.Spec.EnableServiceLinks,
		"EnableServiceLinks must be false to prevent service-discovery env-var leak")
}

// =============================================================================
// G24 — seccompProfile: RuntimeDefault
// =============================================================================

func TestG24_PodHasRuntimeDefaultSeccompProfile(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerFor(t)
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.SeccompProfile,
		"PodSecurityContext.SeccompProfile must be set (G24)")
	require.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type,
		"SeccompProfile.Type must be RuntimeDefault")
}

// =============================================================================
// G44 — Pod-level RunAsNonRoot
// =============================================================================

// TestG44_PodSecurityContextHasRunAsNonRoot is the G44 regression: the
// pod-level SecurityContext must set RunAsNonRoot=true. Pre-fix only
// container-level SecurityContext set it; a future sidecar added
// without its own SecurityContext would inherit the pod default (nil)
// and could run as root. The kubelet enforces RunAsNonRoot by refusing
// to start any container that resolves to UID 0, so pod-level setting
// makes the guarantee structural rather than per-container.
func TestG44_PodSecurityContextHasRunAsNonRoot(t *testing.T) {
	ws := newWorkspaceForSecurity(t)
	r := reconcilerFor(t)
	pod, err := r.buildPod(context.Background(), ws)
	require.NoError(t, err)
	require.NotNil(t, pod.Spec.SecurityContext,
		"PodSecurityContext must be set (G24/G44)")
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot,
		"G44: PodSecurityContext.RunAsNonRoot must be set (was nil pre-fix)")
	require.True(t, *pod.Spec.SecurityContext.RunAsNonRoot,
		"G44: PodSecurityContext.RunAsNonRoot must be true")
}
