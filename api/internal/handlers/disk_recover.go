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
	"github.com/lenaxia/llmsafespaces/pkg/diskrecovery"
	pkginterfaces "github.com/lenaxia/llmsafespaces/pkg/interfaces"
)

// DiskRecoverHandler handles POST /api/v1/workspaces/:id/disk-recover
// (#1601): the owner-facing facade over agentd's /v1/disk-recover.
// Authz is workspace ownership (the userID-scoped GetWorkspace lookup —
// a non-owner's workspace ID is a 404, identical to every other
// workspace action), the phase gate mirrors agent reload. The report is
// relayed typed (decode → re-encode), never a raw passthrough.
//
// The full-disk physics live in agentd (owner ruling on #1601): this
// facade is a thin authenticated proxy — its own request path writes
// nothing to the workspace volume.
type DiskRecoverHandler struct {
	workspaceSvc WorkspaceServicer
	podResolver  PodIPResolver
	httpClient   *http.Client
	logger       pkginterfaces.LoggerInterface
	getPassword  interfaces.WorkspacePasswordProvider
	// agentdPort overrides the dispatch port. Zero → agentd.AgentdPort.
	// Tests point this at an httptest server.
	agentdPort int
}

// NewDiskRecoverHandler constructs the handler.
func NewDiskRecoverHandler(
	wsSvc WorkspaceServicer,
	podResolver PodIPResolver,
	httpClient *http.Client,
	logger pkginterfaces.LoggerInterface,
) *DiskRecoverHandler {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: diskRecoverClientTimeout}
	}
	return &DiskRecoverHandler{
		workspaceSvc: wsSvc,
		podResolver:  podResolver,
		httpClient:   httpClient,
		logger:       logger,
	}
}

// diskRecoverClientTimeout must cover agentd's full bounded sweep
// (diskRecoverTimeout = 90s in agentd) plus slack — a huge modcache
// walk is legitimately slow on a 99%-full volume.
const diskRecoverClientTimeout = 120 * time.Second

// SetPasswordGetter injects the workspace-password provider (app.go
// wiring; the handler never imports the resolver directly).
func (h *DiskRecoverHandler) SetPasswordGetter(p interfaces.WorkspacePasswordProvider) {
	h.getPassword = p
}

// agentdDispatchPort returns the override or the production default.
func (h *DiskRecoverHandler) agentdDispatchPort() int {
	if h.agentdPort != 0 {
		return h.agentdPort
	}
	return agentd.AgentdPort
}

// Recover handles POST /api/v1/workspaces/:id/disk-recover?dryRun=true.
//
// dryRun=true (the frontend button's FIRST render) reports what a
// sweep would free without mutating anything; the follow-up call
// without the flag executes the free-ENOUGH sweep.
func (h *DiskRecoverHandler) Recover(c *gin.Context) {
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
			"error": fmt.Sprintf("cannot recover disk: workspace is in phase %q (must be Active)", ws.Phase),
		})
		return
	}

	podIP, err := h.podResolver.GetWorkspacePodIP(c.Request.Context(), userID, workspaceID)
	if err != nil || podIP == "" {
		c.JSON(http.StatusConflict, gin.H{
			"error": "cannot recover disk: workspace pod is not reachable",
		})
		return
	}

	if h.getPassword == nil {
		respondWithAPIError(c, apierrors.NewInternalError("password_getter_not_wired",
			fmt.Errorf("disk recover: password getter not configured")))
		return
	}
	password, err := h.getPassword.WorkspacePassword(c.Request.Context(), workspaceID)
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("get_workspace_password_failed", err))
		return
	}

	body := diskrecovery.Request{DryRun: c.Query("dryRun") == "true"}
	payload, err := json.Marshal(body)
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("encode_request_failed", err))
		return
	}
	agentdURL := fmt.Sprintf("http://%s:%d/v1/disk-recover", podIP, h.agentdDispatchPort())
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, agentdURL, bytes.NewReader(payload))
	if err != nil {
		respondWithAPIError(c, apierrors.NewInternalError("disk_recover_url_invalid", err))
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":"+password)))

	resp, err := h.httpClient.Do(req)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("disk recover: agentd unreachable", err)
		}
		respondWithAPIError(c, apierrors.NewInternalError("agent_unreachable", err))
		return
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck

	// Relay typed on the 200 path (decode → re-encode — a raw byte
	// passthrough would forward whatever a compromised agentd chose to
	// send into the owner's browser); map the non-200 classes without
	// touching their plain-text bodies (F1: they are http.Error text,
	// not JSON).
	switch resp.StatusCode {
	case http.StatusOK:
		var report diskrecovery.Report
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&report); err != nil {
			respondWithAPIError(c, apierrors.NewInternalError("disk_recover_decode_failed", err))
			return
		}
		if report.Classes == nil {
			// Wire-shape hardening: the engine initializes Classes on
			// every path; a future regression to nil would crash
			// null-naive consumers.
			report.Classes = []diskrecovery.ClassReport{}
		}
		c.JSON(http.StatusOK, report)
	case http.StatusConflict:
		c.JSON(http.StatusConflict, gin.H{"error": "disk recovery already in progress"})
	case http.StatusServiceUnavailable:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "disk usage unavailable on the workspace pod"})
	case http.StatusGatewayTimeout:
		c.JSON(http.StatusGatewayTimeout, gin.H{"error": "disk recovery timed out on the workspace pod"})
	default:
		respondWithAPIError(c, apierrors.NewInternalError("disk_recover_failed",
			fmt.Errorf("agentd returned %d", resp.StatusCode)))
	}
}
