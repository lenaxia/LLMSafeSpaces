// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// opencode error field allowlist — safe to surface to workspace owners.
var opencodeErrorAllowlist = []string{
	"_tag", "message", "kind", "field", "resource",
	"service", "status", "operation", "ref",
	"providerID", "modelID", "suggestions", "sessionID",
}

// EnrichChatErrorBody adds agentNeedsRefresh hint to error responses
// when the workspace has staged credentials.
func EnrichChatErrorBody(
	body []byte,
	needsRefresh bool,
	since time.Time,
	workspaceID string,
) []byte {
	out := map[string]any{}
	if len(body) > 0 {
		var orig map[string]any
		if json.Unmarshal(body, &orig) == nil {
			for _, k := range opencodeErrorAllowlist {
				if v, ok := orig[k]; ok {
					out[k] = v
				}
			}
		} else {
			text := string(body)
			if len(text) > 1024 {
				text = text[:1024] + "..."
			}
			out["message"] = text
		}
	}
	if needsRefresh {
		out["agentNeedsRefresh"] = true
		out["credentialsPendingSince"] = since.Format(time.RFC3339)
		out["hint"] = fmt.Sprintf(
			"You added or modified llm-provider credentials at %s but have not reloaded "+
				"the agent yet. If this error is related to a provider or model you just changed, "+
				"call POST /api/v1/workspaces/%s/agent/reload to apply the new credentials.",
			since.Format(time.RFC3339), workspaceID,
		)
	}
	result, _ := json.Marshal(out)
	return result
}

// AgentStateChecker is the interface for checking workspace agent state.
type AgentStateChecker interface {
	GetLastCredentialChangedAt(ctx context.Context, workspaceID string) (time.Time, error)
}

// writeTextOnlyWedgeBody is the #1307 targeted send-failure surface: the
// adapter classified the provider 400 as image-content-in-history on a
// text-only model — a permanently wedged session while that model stays
// selected. The raw provider body is deliberately NOT echoed; the user
// gets the cause and both escapes (switch model works immediately — the
// wedge is only fatal while pinned to the text-only model).
func writeTextOnlyWedgeBody(c *gin.Context) {
	c.JSON(http.StatusUnprocessableEntity, gin.H{
		"code":  "text_only_model_image_history",
		"error": "This session's history contains an image, and the current model only accepts text. Switch to a vision-capable model in the model picker to continue this session, or start a new session.",
	})
}
