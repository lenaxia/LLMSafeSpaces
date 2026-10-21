// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// #1756 post-union finding 2 pins: the registry pull feeds agentd's
// replica of the phrase→harness-id mapping. The payload's `sessionID`
// is the CANONICAL harness id (InternalID) — the post-#1756 wire `ID`
// is the phrase and would poison the replica phrase→phrase — and rows
// without a minted phrase are skipped (the replica's install rejects
// empty short ids).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type fakePodRegistry struct {
	registered []string // "ws/harness/type"
}

func (f *fakePodRegistry) EnsureSessionRegistered(_ context.Context, workspaceID, sessionID, harnessType string) (string, error) {
	f.registered = append(f.registered, workspaceID+"/"+sessionID+"/"+harnessType)
	return "minted-for-" + sessionID, nil
}

type fakeRegistryLister struct {
	items []types.SessionListItem
}

func (f *fakeRegistryLister) ListSessionIndex(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return f.items, nil
}

func newPodSessionRegistryRouter(t *testing.T, lister *fakeRegistryLister) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	h := NewPodSessionRegistryHandler(reviewer, lookup, &fakePodRegistry{}, lister, testRenameNamespace)
	r.GET("/internal/v1/session-registry", h.List)
	return r
}

func TestPodSessionRegistry_ListCarriesInternalIDAndSkipsUnminted(t *testing.T) {
	lister := &fakeRegistryLister{items: []types.SessionListItem{
		{ID: "amber-falcon-glides", InternalID: "ses-1", ShortID: "amber-falcon-glides"},
		{ID: "ses-2", InternalID: "ses-2"}, // never minted — must be skipped
		{ID: "calm-otter-2-naps", InternalID: "ses-3", ShortID: "calm-otter-2-naps"},
	}}
	r := newPodSessionRegistryRouter(t, lister)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/session-registry?workspaceID=ws-1", nil)
	req.Header.Set("Authorization", "Bearer tok")
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"sessions":[
		{"sessionID":"ses-1","shortID":"amber-falcon-glides"},
		{"sessionID":"ses-3","shortID":"calm-otter-2-naps"}
	]}`, w.Body.String(),
		"sessionID must be the harness id (InternalID); unminted rows are skipped")
}
