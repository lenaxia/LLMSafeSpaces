// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package handlers

// session_registry_integration_test.go — #1817 piece 3's create-via-API
// half, pinned through the REAL handler + the REAL registration seam over
// Postgres. agentd's replica warm (cmd/workspace-agentd) consumes exactly
// this contract: POST registers one session and returns the phrase, GET
// lists harness→phrase pairs for the pod's workspace. The identity seam
// (TokenReview/lookup) is the same fake the handler's unit tests use.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/database"
	"github.com/lenaxia/llmsafespaces/api/internal/services/sessionindex"
	"github.com/lenaxia/llmsafespaces/api/internal/testharness"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func TestIntegrationSessionRegistry_RegisterThenListRoundTrip(t *testing.T) {
	h := testharness.New(t)
	ctx := h.NewContext()

	db := &database.Service{DB: h.SQLDB()}
	session := sessionindex.New(db, nil)

	workspace := "ws-" + h.ID()
	harnessID := "ses_" + h.ID()
	owner := "user-" + h.ID()

	reviewer := &fakeTokenReviewer{
		username: "system:serviceaccount:" + testRenameNamespace + ":workspace-" + workspace,
	}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: workspace, UserID: owner}}
	handler := NewPodSessionRegistryHandler(reviewer, lookup, db, db, testRenameNamespace)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/v1/session-register", handler.Register)
	r.GET("/internal/v1/session-registry", handler.List)

	registerBody, err := json.Marshal(map[string]string{"workspaceID": workspace, "sessionID": harnessID})
	require.NoError(t, err)
	registerReq := httptest.NewRequest(http.MethodPost, "/internal/v1/session-register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerReq.Header.Set("Authorization", "Bearer sa-token")
	registerResp := httptest.NewRecorder()
	r.ServeHTTP(registerResp, registerReq)
	require.Equal(t, http.StatusOK, registerResp.Code, registerResp.Body.String())

	var registered struct {
		ShortID string `json:"shortID"`
	}
	require.NoError(t, json.Unmarshal(registerResp.Body.Bytes(), &registered))
	require.NotEmpty(t, registered.ShortID)
	phrase := registered.ShortID

	listReq := httptest.NewRequest(http.MethodGet,
		"/internal/v1/session-registry?workspaceID="+workspace, nil)
	listReq.Header.Set("Authorization", "Bearer sa-token")
	listResp := httptest.NewRecorder()
	r.ServeHTTP(listResp, listReq)
	require.Equal(t, http.StatusOK, listResp.Code, listResp.Body.String())

	var listed struct {
		Sessions []struct {
			SessionID string `json:"sessionID"`
			ShortID   string `json:"shortID"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(listResp.Body.Bytes(), &listed))
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, harnessID, listed.Sessions[0].SessionID)
	require.Equal(t, phrase, listed.Sessions[0].ShortID)

	rows, err := session.ListByWorkspace(ctx, workspace)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, phrase, rows[0].ID)
	require.Equal(t, harnessID, rows[0].InternalID)
	require.Equal(t, phrase, rows[0].ShortID)
}

// TestIntegrationSessionRegistry_PostSpawnWriteStormKeepsCreationPhrase
// is #1850's wiring-tier pin (review r1's blocking finding 2): a
// session registered through the REAL handler (the create_session
// platform half — the phrase minted and returned at creation), then
// the post-spawn write storm driven through the REAL session-index
// SERVICE (message SSEs via RecordMessage+drain, title, context
// usage, last-seen — the exact paths the defect traveled), keeps its
// CREATION phrase on every surface: the registry pull the agentd
// replica consumes, the sidebar list, and phrase resolution. The
// DB-layer pins assert the store semantics; THIS asserts the
// service→store composition the re-phrase actually broke.
func TestIntegrationSessionRegistry_PostSpawnWriteStormKeepsCreationPhrase(t *testing.T) {
	h := testharness.New(t)
	ctx := h.NewContext()

	db := &database.Service{DB: h.SQLDB()}
	session := sessionindex.New(db, nil)
	require.NoError(t, session.Start())

	workspace := "ws-" + h.ID()
	harnessID := "ses_" + h.ID()
	owner := "user-" + h.ID()

	reviewer := &fakeTokenReviewer{
		username: "system:serviceaccount:" + testRenameNamespace + ":workspace-" + workspace,
	}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: workspace, UserID: owner}}
	handler := NewPodSessionRegistryHandler(reviewer, lookup, db, db, testRenameNamespace)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/v1/session-register", handler.Register)
	r.GET("/internal/v1/session-registry", handler.List)

	registerBody, err := json.Marshal(map[string]string{"workspaceID": workspace, "sessionID": harnessID})
	require.NoError(t, err)
	registerReq := httptest.NewRequest(http.MethodPost, "/internal/v1/session-register", bytes.NewReader(registerBody))
	registerReq.Header.Set("Content-Type", "application/json")
	registerReq.Header.Set("Authorization", "Bearer sa-token")
	registerResp := httptest.NewRecorder()
	r.ServeHTTP(registerResp, registerReq)
	require.Equal(t, http.StatusOK, registerResp.Code, registerResp.Body.String())

	var registered struct {
		ShortID string `json:"shortID"`
	}
	require.NoError(t, json.Unmarshal(registerResp.Body.Bytes(), &registered))
	creationPhrase := registered.ShortID
	require.NotEmpty(t, creationPhrase, "creation mint must return a phrase")

	// The post-spawn write storm: the message SSE queue (drained by
	// the service's own background worker), the title fetch, the
	// context-usage write, and the last-seen flip — every row-creating
	// path the spawn window races.
	session.RecordMessage(workspace, harnessID, "worker", time.Now().UTC())
	session.RecordMessage(workspace, harnessID, "worker", time.Now().UTC())
	require.NoError(t, session.UpsertTitle(ctx, workspace, harnessID, "Lane worker"))
	require.NoError(t, session.UpsertContextUsed(ctx, workspace, harnessID, 4242))
	require.NoError(t, session.UpdateLastSeen(ctx, workspace, harnessID))
	// The queue drains async; stop() flushes deterministically.
	require.NoError(t, session.Stop())

	// Surface 1: the registry pull the agentd replica consumes — the
	// creation phrase still maps to the harness id.
	listReq := httptest.NewRequest(http.MethodGet,
		"/internal/v1/session-registry?workspaceID="+workspace, nil)
	listReq.Header.Set("Authorization", "Bearer sa-token")
	listResp := httptest.NewRecorder()
	r.ServeHTTP(listResp, listReq)
	require.Equal(t, http.StatusOK, listResp.Code, listResp.Body.String())

	var listed struct {
		Sessions []struct {
			SessionID string `json:"sessionID"`
			ShortID   string `json:"shortID"`
		} `json:"sessions"`
	}
	require.NoError(t, json.Unmarshal(listResp.Body.Bytes(), &listed))
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, harnessID, listed.Sessions[0].SessionID)
	require.Equal(t, creationPhrase, listed.Sessions[0].ShortID,
		"the registry pull must carry the CREATION phrase after the write storm")

	// Surface 2: the sidebar list row.
	rows, err := session.ListByWorkspace(ctx, workspace)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, creationPhrase, rows[0].ID)
	require.Equal(t, creationPhrase, rows[0].ShortID)
	require.Equal(t, harnessID, rows[0].InternalID)
	require.Equal(t, 2, rows[0].MessageCount, "the storm's message writes landed on the SAME row")

	// Surface 3: phrase resolution — addressing the creation phrase
	// still reaches the session (the send_message contract).
	got, err := db.FindSessionByShortID(ctx, workspace, creationPhrase)
	require.NoError(t, err)
	require.Equal(t, harnessID, got)
}
