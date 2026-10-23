// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/fsaudit"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// FsAuditHandler handles POST /api/v1/workspaces/:id/fs-audit
// (#1808): the owner-facing facade over agentd's /v1/fs-audit. Authz
// is workspace ownership (the userID-scoped GetWorkspace lookup — a
// non-owner's workspace ID is a 404, identical to every other
// workspace action), the phase gate mirrors agent reload and
// disk-recover. The report is relayed typed (decode → re-encode),
// never a raw passthrough. The audit writes nothing on the workspace
// volume — this facade's own request path writes nothing either (the
// #1601 response-only ruling, inherited).
//
// Body: optional JSON {root, maxFileBytes, maxTotalBytes}; with no
// body, `root` may be given as a query param (both empty = the whole
// workspace).
type FsAuditHandler struct {
	workspaceSvc WorkspaceServicer
	podResolver  PodIPResolver
	httpClient   *http.Client
	logger       pkginterfaces.LoggerInterface
	getPassword  interfaces.WorkspacePasswordProvider
	// agentdPort overrides the dispatch port. Zero → agentd.AgentdPort.
	// Tests point this at an httptest server.
	agentdPort int
}

// NewFsAuditHandler constructs the handler.
func NewFsAuditHandler(
	wsSvc WorkspaceServicer,
	podResolver PodIPResolver,
	httpClient *http.Client,
	logger pkginterfaces.LoggerInterface,
) *FsAuditHandler {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: fsAuditClientTimeout}
	}
	return &FsAuditHandler{
		workspaceSvc: wsSvc,
		podResolver:  podResolver,
		httpClient:   httpClient,
		logger:       logger,
	}
}

// fsAuditClientTimeout must cover agentd's full bounded sweep
// (fsAuditTimeout = 60s in agentd) plus slack.
const fsAuditClientTimeout = 90 * time.Second

// SetPasswordGetter injects the workspace-password provider (app.go
// wiring; the handler never imports the resolver directly).
func (h *FsAuditHandler) SetPasswordGetter(p interfaces.WorkspacePasswordProvider) {
	h.getPassword = p
}

// agentdDispatchPort returns the override or the production default.
func (h *FsAuditHandler) agentdDispatchPort() int {
	if h.agentdPort != 0 {
		return h.agentdPort
	}
	return agentd.AgentdPort
}

// Audit handles POST /api/v1/workspaces/:id/fs-audit.
func (h *FsAuditHandler) Audit(c *gin.Context) {
	userID, _ := extractAuth(c)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	workspaceID := c.Param("id")

	ws, err := h.workspaceSvc.GetWorkspace(c.Request.Context(), userID, workspaceID)
	if err != nil {
		respondWithAPIError(c, err)
		return
	}
	if ws.Phase != "Active" {
		c.JSON(http.StatusConflict, gin.H{
			"error": fmt.Sprintf("cannot audit filesystem: workspace is in phase %q (must be Active)", ws.Phase),
		})
		return
	}

	podIP, err := h.podResolver.GetWorkspacePodIP(c.Request.Context(), userID, workspaceID)
	if err != nil || podIP == "" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "cannot audit filesystem: workspace pod is not reachable",
		})
		return
	}

	if h.getPassword == nil {
		respondWithAPIError(c, apierrors.NewInternalError("password_getter_not_wired",
			fmt.Errorf("fs audit: password getter not configured")))
		return
	}
	password, err := h.getPassword.WorkspacePassword(c.Request.Context(), workspaceID)
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("get_workspace_password_failed", err))
		return
	}

	body := fsaudit.Request{Root: c.Query("root")}
	// Optional JSON body overrides/refines the query form.
	if c.Request != nil && c.Request.Body != nil && c.Request.ContentLength > 0 {
		dec := json.NewDecoder(io.LimitReader(c.Request.Body, 4096))
		if err := dec.Decode(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "body must be one JSON document {root, maxFileBytes, maxTotalBytes}"})
			return
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("encode_request_failed", err))
		return
	}
	agentdURL := fmt.Sprintf("http://%s:%d/v1/fs-audit", podIP, h.agentdDispatchPort())
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, agentdURL, bytes.NewReader(payload))
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("fs_audit_url_invalid", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+password)))

	resp, err := h.httpClient.Do(req)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("fs audit: agentd unreachable", err)
		}
		respondWithAPIError(c, apierrors.NewInternalError("agent_unreachable", err))
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	// Relay typed on the 200 path (decode → re-encode — a raw byte
	// passthrough would forward whatever a compromised agentd chose to
	// send); map the non-200 classes without touching their plain-text
	// bodies (the disk-recover r1 F1 lesson: agentd errors are
	// http.Error text, not JSON).
	switch resp.StatusCode {
	case http.StatusOK:
		var report fsaudit.Report
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&report); err != nil {
			respondWithAPIError(c, apierrors.NewInternalError("fs_audit_decode_failed", err))
			return
		}
		if report.NulFilled == nil {
			// Wire-shape hardening: the engine initializes the lists on
			// every path; a future regression to nil would crash
			// null-naive consumers.
			report.NulFilled = []fsaudit.FileReport{}
		}
		if report.Suspect == nil {
			report.Suspect = []fsaudit.FileReport{}
		}
		if report.Errors == nil {
			report.Errors = []fsaudit.FileReport{}
		}
		c.JSON(http.StatusOK, report)
	case http.StatusConflict:
		c.JSON(http.StatusConflict, gin.H{"error": "fs audit already in progress"})
	case http.StatusServiceUnavailable:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "fs audit not wired on the workspace pod"})
	case http.StatusGatewayTimeout:
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "fs audit timed out on the workspace pod"})
	default:
		respondWithAPIError(c, apierrors.NewInternalError("fs_audit_failed",
			fmt.Errorf("agentd returned %d", resp.StatusCode)))
	}
}
