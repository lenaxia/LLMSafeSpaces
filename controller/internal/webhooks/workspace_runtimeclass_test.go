// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Epic 51 S51.1 design: gVisor is opt-in for the cluster (chart value
// gvisor.enabled), but opt-out for individual workspaces must be
// admin-gated, not tenant-selectable. The CRD comment on
// spec.runtimeClass (workspace_types.go:158-161) explicitly says
// "webhook validation to prevent tenants from setting this field via
// direct kubectl is deferred to S51.2."
//
// Admin-gating scheme: spec.runtimeClass is rejected at admission unless
// the Workspace object carries the annotation
//   llmsafespaces.dev/allow-runtime-class-override: "true"
// Operators with cluster-admin / namespace-admin RBAC are the ones who
// can apply that annotation; tenant RBAC scopes remain minimal.
//
// Closes the S51.2 deferral noted in workspace_types.go:158-161.

func ptrString(s string) *string { return &s }

func TestWorkspaceWebhook_RuntimeClassRejectsByDefault(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.RuntimeClass = ptrString("runc")

	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed,
		"spec.runtimeClass without the admin annotation must be rejected (reason=%q)",
		resp.Result.Reason)
}

func TestWorkspaceWebhook_RuntimeClassAllowedWithAdminAnnotation(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.RuntimeClass = ptrString("runc")
	ws.Annotations = map[string]string{
		allowRuntimeClassOverrideAnnotation: "true",
	}

	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.True(t, resp.Allowed,
		"spec.runtimeClass with the admin annotation must be allowed (reason=%q)",
		resp.Result.Reason)
}

func TestWorkspaceWebhook_RuntimeClassNilAlwaysAllowed(t *testing.T) {
	// nil runtimeClass = use the controller default (typically gVisor in
	// prod). Always allowed — that's the secure default.
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	// RuntimeClass nil

	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.True(t, resp.Allowed,
		"nil spec.runtimeClass (use controller default) must always be allowed (reason=%q)",
		resp.Result.Reason)
}

func TestWorkspaceWebhook_RuntimeClassAnnotationWrongValue(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/"},
		MaxStorageGi:           1024,
	}
	ws := minimalValidWorkspace()
	ws.Spec.RuntimeClass = ptrString("runc")
	ws.Annotations = map[string]string{
		allowRuntimeClassOverrideAnnotation: "false",
	}

	resp := v.Handle(context.Background(), newWorkspaceCreateRequest(t, ws))
	assert.False(t, resp.Allowed,
		"annotation value must be exactly \"true\"; any other value is treated as absent")
}

func TestWorkspaceWebhook_RuntimeClassUpdateRejectedWithoutAnnotation(t *testing.T) {
	// An UPDATE that introduces spec.runtimeClass on a previously-clean
	// workspace must also be rejected without the admin annotation.
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/"},
		MaxStorageGi:           1024,
	}
	oldWS := minimalValidWorkspace()
	newWS := minimalValidWorkspace()
	newWS.Spec.RuntimeClass = ptrString("runc")

	resp := v.Handle(context.Background(), newWorkspaceUpdateRequest(t, oldWS, newWS))
	assert.False(t, resp.Allowed,
		"UPDATE introducing spec.runtimeClass without admin annotation must be rejected")
}

func TestWorkspaceWebhook_RuntimeClassUpdateAllowedWithAnnotation(t *testing.T) {
	v := &WorkspaceValidator{
		Decoder:                admission.NewDecoder(newScheme(t)),
		AllowedImageRegistries: []string{"ghcr.io/lenaxia/"},
		MaxStorageGi:           1024,
	}
	oldWS := minimalValidWorkspace()
	newWS := minimalValidWorkspace()
	newWS.Spec.RuntimeClass = ptrString("runc")
	newWS.Annotations = map[string]string{
		allowRuntimeClassOverrideAnnotation: "true",
	}

	resp := v.Handle(context.Background(), newWorkspaceUpdateRequest(t, oldWS, newWS))
	assert.True(t, resp.Allowed,
		"UPDATE with admin annotation must be allowed (reason=%q)", resp.Result.Reason)
}
