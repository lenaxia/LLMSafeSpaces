// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	agentd "github.com/lenaxia/llmsafespaces/pkg/agentd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// proxy_actions.go — US-69.9 (design 0055 M1 op 5): the API edge of the
// typed actions union. The route forwards the union payload verbatim to
// the pod's Act op (agentd is the sole writer of session mutations); the
// path's sessionId is authoritative — a body sessionId never overrides
// it. Behind AGENTD_STATE_AUTHORITY (D4: single regime — when the flag is
// off the surface does not exist, the legacy routes keep serving).

const abiActPath = "/llmsafespaces.abi.v1.HarnessABIService/Act"

// SessionAction serves POST /workspaces/:id/sessions/:sessionId/actions.
func (h *ProxyHandler) SessionAction(c *gin.Context) {
	workspaceID := c.Param("id")
	sessionID := c.Param("sessionId")

	if !h.agentdTerminus {
		c.JSON(http.StatusNotImplemented, gin.H{"error": gin.H{
			"code":       "not_supported",
			"capability": "abi.actions",
			"detail":     "typed actions require AGENTD_STATE_AUTHORITY (design 0055 M4/D4: single delivery regime)",
		}})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_argument", "detail": "unreadable body"}})
		return
	}
	var payload map[string]json.RawMessage
	if len(body) > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"code": "invalid_argument", "detail": "the action union must be a JSON object"}})
			return
		}
	} else {
		payload = map[string]json.RawMessage{}
	}
	// The path is authoritative; drop any body sessionId before injecting.
	payload["sessionId"] = json.RawMessage(strconv.Quote(sessionID))

	base, pw, err := h.agentdEndpoint(c.Request.Context(), workspaceID)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "unresolved", "detail": err.Error()}})
		return
	}

	var out json.RawMessage
	if err := abiAct(c.Request.Context(), base, pw, payload, &out); err != nil {
		status, code := mapConnectError(err)
		c.JSON(status, gin.H{"error": gin.H{"code": code, "detail": err.Error()}})
		return
	}
	c.Data(http.StatusOK, "application/json", out)
}

// abiAct POSTs the union to the pod's Act op (Connect JSON envelope — the
// terminus transport discipline: zero generated-code coupling in the API
// binary path).
func abiAct(ctx context.Context, base, pw string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+abiActPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth("opencode", pw)
	req.Header.Set("Content-Type", "application/json")
	resp, err := agentdHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("act: status %d: %s", resp.StatusCode, string(data))
	}
	var env struct {
		Message json.RawMessage `json:"message"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("act: decode envelope: %w", err)
	}
	if env.Error != nil {
		return &connectCodeError{code: env.Error.Code, msg: env.Error.Message}
	}
	if len(env.Message) == 0 {
		return fmt.Errorf("act: empty message envelope")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Message, out)
}

// connectCodeError carries the connect error code string off the wire.
type connectCodeError struct {
	code string
	msg  string
}

func (e *connectCodeError) Error() string { return e.code + ": " + e.msg }

// mapConnectError maps a connect code (or transport failure) to the API's
// HTTP shape.
func mapConnectError(err error) (int, string) {
	if cce, ok := err.(*connectCodeError); ok {
		switch cce.code {
		case "invalid_argument", "out_of_range":
			return http.StatusBadRequest, "invalid_argument"
		case "not_found":
			return http.StatusNotFound, "not_found"
		case "resource_exhausted":
			return http.StatusTooManyRequests, "resource_exhausted"
		case "unimplemented":
			return http.StatusNotImplemented, "not_supported"
		case "unauthenticated":
			return http.StatusUnauthorized, "unauthenticated"
		case "deadline_exceeded":
			return http.StatusGatewayTimeout, "deadline_exceeded"
		}
		return http.StatusBadGateway, cce.code
	}
	return http.StatusBadGateway, "unavailable"
}

// agentdEndpoint resolves the pod's ABI base URL + password (resume-safe
// re-resolution semantics, A7 — the terminus's resolve, shared).
func (h *ProxyHandler) agentdEndpoint(ctx context.Context, workspaceID string) (string, string, error) {
	pw, err := h.getPassword(ctx, workspaceID)
	if err != nil {
		return "", "", err
	}
	v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
	if v1Err != nil {
		return "", "", v1Err
	}
	wsObj, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	if wsObj.Status.PodIP == "" {
		return "", "", fmt.Errorf("agentd: no pod IP for %s (phase %s)", workspaceID, wsObj.Status.Phase)
	}
	port := agentd.AgentdPort
	if h.agentdPortOverride > 0 {
		port = h.agentdPortOverride
	}
	return fmt.Sprintf("http://%s:%d", wsObj.Status.PodIP, port), pw, nil
}
