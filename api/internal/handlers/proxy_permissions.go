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

// autoApprovePermission is the headless auto-approve (workspace spec
// AutoApprovePermissions). S1 (#1302): the API makes zero mutating
// harness calls — in the authority regime it rides the same Act path as
// PermissionReply (AnswerInputAction reply="always"); under flag-off it
// uses the typed adapter method (ReplyPermission), never the legacy
// Resolve probe.
func (h *ProxyHandler) autoApprovePermission(workspaceID, requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if h.agentdTerminus {
		sessionID, resolvable, _ := h.inputRequestSession(ctx, workspaceID, requestID)
		if !resolvable {
			// Non-authoritative skip: the pending set is unreadable or
			// the ask is gone (agentd's resolve-by-absence covers the
			// ask side; auto-approved permissions carry no inbox record
			// — the bridge returns before recordInboxAsk).
			h.logger.Warn("Auto-approve skipped: input pending set unknown or ask gone",
				"workspaceID", workspaceID, "requestID", requestID)
			return
		}
		if err := h.actAnswerInputCtx(ctx, workspaceID, sessionID, requestID, map[string]any{"reply": "always"}); err != nil {
			h.logger.Warn("Auto-approve permission failed via Act", "error", err,
				"workspaceID", workspaceID, "requestID", requestID)
			return
		}
		h.logger.Info("Auto-approved permission",
			"workspaceID", workspaceID, "requestID", requestID)
		return
	}
	if err := h.adapter.ReplyPermission(ctx, "", workspaceID, requestID, "always", ""); err != nil {
		h.logger.Warn("Auto-approve permission failed via adapter", "error", err,
			"workspaceID", workspaceID, "requestID", requestID)
		return
	}
	h.logger.Info("Auto-approved permission",
		"workspaceID", workspaceID, "requestID", requestID)
}
