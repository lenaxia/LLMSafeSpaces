// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lenaxia/llmsafespaces/api/internal/interfaces"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	v1 "github.com/lenaxia/llmsafespaces/pkg/apis/llmsafespaces/v1"
)

func (h *ProxyHandler) SetSessionIndex(si interfaces.SessionIndexService) {
	h.sessionIndex = si
}

func (h *ProxyHandler) fetchAndPersistTitle(workspaceID, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	v1Client, err := h.k8sClient.LlmsafespacesV1()
	if err != nil {
		return
	}
	workspace, err := v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	if err != nil || workspace.Status.PodIP == "" {
		return
	}
	password, err := h.getPassword(ctx, workspaceID)
	if err != nil {
		return
	}

	url := fmt.Sprintf("http://%s:%d/session/%s", workspace.Status.PodIP, opencodePort, sessionID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(agentd.AuthUsername, password)

	resp, err := h.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	defer func() { _ = resp.Body.Close() }()

	var session struct {
		Title    string `json:"title"`
		ParentID string `json:"parentID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return
	}

	if session.Title != "" {
		if err := h.sessionIndex.UpsertTitle(ctx, workspaceID, sessionID, session.Title); err != nil {
			h.logger.Error("Failed to persist session title", err, "workspaceID", workspaceID, "sessionID", sessionID)
		}
	}
	if session.ParentID != "" {
		if err := h.sessionIndex.UpsertParent(ctx, workspaceID, sessionID, session.ParentID); err != nil {
			h.logger.Error("Failed to persist session parent", err, "workspaceID", workspaceID, "sessionID", sessionID)
		}
	}
}

func (h *ProxyHandler) BackfillSessionParents(ctx context.Context, workspaceID string) {
	if h.sessionIndex == nil || h.dialect == nil {
		return
	}
	if h.state().GetParentBackfilled(ctx, workspaceID) {
		return
	}
	h.state().SetParentBackfilled(ctx, workspaceID)

	// Fire-and-forget: the backfill benefits future requests, not this one, so
	// it must survive client disconnect. runParentBackfill uses a detached
	// context.Background() bounded by a 15s timeout (not the request ctx).
	go h.runParentBackfill(workspaceID) //nolint:gosec,contextcheck // G118: intentional fire-and-forget detach
}

// runParentBackfill deliberately uses a detached context.Background() (bounded
// by a 15s timeout): the backfill must survive client disconnect (the request
// ctx is canceled once the response is flushed), which would abort the
// parent_session_id persistence. It takes no ctx so contextcheck does not
// expect propagation from the (request-scoped) caller.
func (h *ProxyHandler) runParentBackfill(workspaceID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	v1Client, v1Err := h.k8sClient.LlmsafespacesV1()
	workspace, err := func() (*v1.Workspace, error) {
		if v1Err != nil {
			return nil, v1Err
		}
		return v1Client.Workspaces(h.namespace).Get(ctx, workspaceID, metav1.GetOptions{})
	}()
	if err != nil || workspace.Status.Phase != phaseActive || workspace.Status.PodIP == "" {
		h.state().DeleteParentBackfilled(ctx, workspaceID)
		return
	}

	password, err := h.getPassword(ctx, workspaceID)
	if err != nil {
		h.state().DeleteParentBackfilled(ctx, workspaceID)
		return
	}

	url := fmt.Sprintf("http://%s:%d%s", workspace.Status.PodIP, opencodePort, h.dialect.SessionListPath())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.SetBasicAuth(agentd.AuthUsername, password)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		h.logger.Debug("Backfill: session list fetch failed", "workspaceID", workspaceID, "error", err)
		h.state().DeleteParentBackfilled(ctx, workspaceID)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		h.state().DeleteParentBackfilled(ctx, workspaceID)
		return
	}

	var sessions []struct {
		ID       string `json:"id"`
		ParentID string `json:"parentID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return
	}

	written := 0
	for _, s := range sessions {
		if s.ID == "" || s.ParentID == "" {
			continue
		}
		if err := h.sessionIndex.UpsertParent(ctx, workspaceID, s.ID, s.ParentID); err != nil {
			h.logger.Debug("Backfill: upsert parent failed", "workspaceID", workspaceID, "sessionID", s.ID, "error", err)
			continue
		}
		written++
	}
	if written > 0 {
		h.logger.Info("Backfilled session parents", "workspaceID", workspaceID, "count", written)
	}
}
