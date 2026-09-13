// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/services/wsstate"
)

func (h *ProxyHandler) shouldAutoApprovePermissions(ctx context.Context, workspaceID string) bool {
	if cfg, ok := h.state().GetWorkspaceConfig(ctx, workspaceID); ok {
		return cfg.AutoApprovePermissions
	}

	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return false
	}
	workspace, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return false
	}

	h.state().SetWorkspaceConfig(ctx, workspaceID, wsstate.Config{
		MaxActiveSessions:      int(workspace.Spec.MaxActiveSessions),
		AutoApprovePermissions: workspace.Spec.AutoApprovePermissions,
	})

	return workspace.Spec.AutoApprovePermissions
}

func (h *ProxyHandler) autoApprovePermission(workspaceID, requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// adapter.Resolve handles the permission reply translation
	// internally — no raw HTTP, no hardcoded body.
	if err := h.adapter.Resolve(ctx, "", workspaceID, requestID, "always"); err != nil {
		h.logger.Warn("Auto-approve permission failed via adapter", "error", err,
			"workspaceID", workspaceID, "requestID", requestID)
		return
	}
	h.logger.Info("Auto-approved permission",
		"workspaceID", workspaceID, "requestID", requestID)
}
