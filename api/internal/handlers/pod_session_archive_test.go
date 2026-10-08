// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

// mockSessionIndexForPod is the minimal lister for the archived-set
// endpoint (one method — the caller-shaped interface keeps it small).
type mockSessionIndexForPod struct {
	items []types.SessionListItem
	err   error
}

func (m *mockSessionIndexForPod) ListByWorkspace(_ context.Context, _ string) ([]types.SessionListItem, error) {
	return m.items, m.err
}

// --- fakes for the pod session-archive surface (#1627 PR2) ---

type fakePodSessionProxy struct {
	deleted   []string
	published []string // "ws/sid/archived" entries
	deleteErr error
}

func (f *fakePodSessionProxy) HardDeleteSession(_ context.Context, workspaceID, sessionID string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, workspaceID+"/"+sessionID)
	return nil
}

func (f *fakePodSessionProxy) PublishSessionArchived(workspaceID, sessionID string, archived bool) {
	f.published = append(f.published, fmt.Sprintf("%s/%s/%v", workspaceID, sessionID, archived))
}

type fakePodArchiver struct {
	called   int
	userID   string
	archived bool
	err      error
}

func (f *fakePodArchiver) SetSessionArchived(_ context.Context, userID, _, _ string, archived bool) error {
	f.called++
	f.userID = userID
	f.archived = archived
	return f.err
}

func newPodSessionArchiveRouter(t *testing.T, reviewer *fakeTokenReviewer, lookup *fakeBootstrapLookup, archiver *fakePodArchiver, proxy *fakePodSessionProxy, lister *mockSessionIndexForPod) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodSessionArchiveHandler(reviewer, lookup, archiver, proxy, lister, testRenameNamespace)
	r.POST("/internal/v1/session-archive", h.Archive)
	r.POST("/internal/v1/session-delete", h.Delete)
	r.GET("/internal/v1/session-archived", h.ArchivedSet)
	return r
}

func doPodSessionCall(t *testing.T, r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPodSessionArchive_HappyPath(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	archiver := &fakePodArchiver{}
	proxy := &fakePodSessionProxy{}
	lister := &mockSessionIndexForPod{}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, archiver, proxy, lister)

	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-archive", "tok", `{"workspaceID":"ws-1","sessionID":"ses-1","archived":true}`)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Equal(t, 1, archiver.called)
	assert.Equal(t, "user-7", archiver.userID, "ownership is pod identity — the resolved owner, never a caller-supplied userID")
	assert.True(t, archiver.archived)
	require.Len(t, proxy.published, 1)
	assert.Equal(t, "ws-1/ses-1/true", proxy.published[0], "archive transitions announce on the SSE streams")
	assert.Empty(t, proxy.deleted)
}

func TestPodSessionArchive_UnarchivePublishesFalse(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	archiver := &fakePodArchiver{}
	proxy := &fakePodSessionProxy{}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, archiver, proxy, &mockSessionIndexForPod{})

	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-archive", "tok", `{"workspaceID":"ws-1","sessionID":"ses-1","archived":false}`)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.False(t, archiver.archived)
	require.Len(t, proxy.published, 1)
	assert.Equal(t, "ws-1/ses-1/false", proxy.published[0])
}

func TestPodSessionArchive_UnindexedSession_Returns404(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	archiver := &fakePodArchiver{err: apierrors.NewNotFoundError("session", "ses-ghost", nil)}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, archiver, &fakePodSessionProxy{}, &mockSessionIndexForPod{})

	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-archive", "tok", `{"workspaceID":"ws-1","sessionID":"ses-ghost","archived":true}`)
	assert.Equal(t, http.StatusNotFound, w.Code, "the tool must learn the session is not indexed: %s", w.Body.String())
}

func TestPodSessionArchive_IdentityMismatch_Forbidden(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-other"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	archiver := &fakePodArchiver{}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, archiver, &fakePodSessionProxy{}, &mockSessionIndexForPod{})

	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-archive", "tok", `{"workspaceID":"ws-1","sessionID":"ses-1","archived":true}`)
	assert.Equal(t, http.StatusForbidden, w.Code, "a pod can only archive its own workspace's sessions")
	assert.Zero(t, archiver.called)
}

func TestPodSessionArchive_MissingFields_400(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, &fakePodArchiver{}, &fakePodSessionProxy{}, &mockSessionIndexForPod{})

	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-archive", "tok", `{"workspaceID":"ws-1"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	w = doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-archive", "tok", `{"workspaceID":"ws-1","sessionID":"ses-1"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code, "archived (boolean) is required — the tool must not guess")
}

func TestPodSessionDelete_HardDeleteFlow(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	proxy := &fakePodSessionProxy{}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, &fakePodArchiver{}, proxy, &mockSessionIndexForPod{})

	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-delete", "tok", `{"workspaceID":"ws-1","sessionID":"ses-1"}`)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Len(t, proxy.deleted, 1)
	assert.Equal(t, "ws-1/ses-1", proxy.deleted[0])
}

func TestPodSessionDelete_AdapterFailure_502(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	proxy := &fakePodSessionProxy{deleteErr: fmt.Errorf("pod unreachable")}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, &fakePodArchiver{}, proxy, &mockSessionIndexForPod{})

	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-delete", "tok", `{"workspaceID":"ws-1","sessionID":"ses-1"}`)
	assert.Equal(t, http.StatusBadGateway, w.Code)
}

func TestPodSessionArchivedSet_ReturnsArchivedIDs(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	lister := &mockSessionIndexForPod{
		items: []types.SessionListItem{
			{ID: "ses-1", Archived: true},
			{ID: "ses-2"},
			{ID: "ses-3", Archived: true},
		},
	}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, &fakePodArchiver{}, &fakePodSessionProxy{}, lister)

	w := doPodSessionCall(t, r, http.MethodGet, "/internal/v1/session-archived?workspaceID=ws-1", "tok", "")
	require.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"archived":["ses-1","ses-3"]}`, w.Body.String())
}

func TestPodSessionArchivedSet_RequiresWorkspaceID(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	r := newPodSessionArchiveRouter(t, reviewer, &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}, &fakePodArchiver{}, &fakePodSessionProxy{}, &mockSessionIndexForPod{})

	// Authed (the SA exists) but no workspaceID — the identity cannot
	// be bound to a workspace, so the call refuses.
	w := doPodSessionCall(t, r, http.MethodGet, "/internal/v1/session-archived", "tok", "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Review r1 finding 2: model-supplied session IDs are validated with
// the same charset/length/traversal guard the REST surfaces enforce.
func TestPodSessionArchive_TraversalSessionID_400(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	r := newPodSessionArchiveRouter(t, reviewer, lookup, &fakePodArchiver{}, &fakePodSessionProxy{}, &mockSessionIndexForPod{})

	for _, bad := range []string{"../ws-other/ses", "ses with spaces", strings.Repeat("a", 129)} {
		w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-archive", "tok",
			fmt.Sprintf(`{"workspaceID":"ws-1","sessionID":%q,"archived":true}`, bad))
		assert.Equal(t, http.StatusBadRequest, w.Code, "sessionID %q must be rejected: %s", bad, w.Body.String())
	}
	w := doPodSessionCall(t, r, http.MethodPost, "/internal/v1/session-delete", "tok",
		`{"workspaceID":"ws-1","sessionID":"../../etc"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
