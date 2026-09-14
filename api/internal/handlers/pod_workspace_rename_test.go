// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/types"
)

type fakeWorkspaceRenamer struct {
	called bool
	userID string
	wsID   string
	name   string
	err    error
}

func (f *fakeWorkspaceRenamer) RenameWorkspace(_ context.Context, userID, workspaceID, name string) error {
	f.called = true
	f.userID = userID
	f.wsID = workspaceID
	f.name = name
	return f.err
}

const testRenameNamespace = "llmsafespace"

func newTestRenameRouter(t *testing.T, reviewer *fakeTokenReviewer, lookup *fakeBootstrapLookup, renamer *fakeWorkspaceRenamer) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewPodWorkspaceRenameHandler(reviewer, lookup, renamer, testRenameNamespace)
	r.POST("/internal/v1/workspace-rename", h.Rename)
	return r
}

func doRename(t *testing.T, router *gin.Engine, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/workspace-rename", bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// A valid token minted by the workspace-<id> SA in the expected namespace,
// with a workspace that exists, renames via the service under the owner
// resolved from the lookup (the pod has no user identity — ownership is
// pod identity).
func TestPodWorkspaceRename_HappyPath(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, lookup, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"My Workspace"}`)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.True(t, renamer.called)
	assert.Equal(t, "user-7", renamer.userID)
	assert.Equal(t, "ws-1", renamer.wsID)
	assert.Equal(t, "My Workspace", renamer.name)
}

func TestPodWorkspaceRename_MissingToken(t *testing.T) {
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, &fakeTokenReviewer{}, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_TokenNotAuthenticated(t *testing.T) {
	reviewer := &fakeTokenReviewer{err: errTokenNotAuthenticated}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_TokenReviewFails(t *testing.T) {
	reviewer := &fakeTokenReviewer{err: assert.AnError}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_ExpiredToken(t *testing.T) {
	// An expired-but-signed token: build a three-segment JWT whose exp is
	// far in the past (the F4b defense-in-depth check — TokenReview does
	// not enforce exp on its own).
	expired := expiredTestToken(t)
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, expired, `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_MissingWorkspaceID(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{"name":"x"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_EmptyName(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	for _, body := range []string{
		`{"workspaceID":"ws-1"}`,
		`{"workspaceID":"ws-1","name":""}`,
		`{"workspaceID":"ws-1","name":"   "}`,
	} {
		w := doRename(t, r, "tok", body)
		assert.Equal(t, http.StatusBadRequest, w.Code, "body %s", body)
		assert.False(t, renamer.called)
	}
}

func TestPodWorkspaceRename_NameTooLong(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"`+strings.Repeat("a", 256)+`"}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_NameIsTrimmed(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, lookup, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"  padded  "}`)
	require.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, "padded", renamer.name)
}

func TestPodWorkspaceRename_SAMismatch(t *testing.T) {
	// The token belongs to workspace-ws-OTHER but the body asks to rename
	// ws-1 — must be refused before any service call.
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-OTHER"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_NamespaceMismatch(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:other-ns:workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_NonSAPrincipal(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:admin"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_WorkspaceNotFound(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{ws: nil}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_LookupFails(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{err: assert.AnError}, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.False(t, renamer.called)
}

func TestPodWorkspaceRename_RenameFails(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	lookup := &fakeBootstrapLookup{ws: &types.WorkspaceMetadata{ID: "ws-1", UserID: "user-7"}}
	renamer := &fakeWorkspaceRenamer{err: assert.AnError}
	r := newTestRenameRouter(t, reviewer, lookup, renamer)

	w := doRename(t, r, "tok", `{"workspaceID":"ws-1","name":"x"}`)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestPodWorkspaceRename_MalformedBody(t *testing.T) {
	reviewer := &fakeTokenReviewer{username: "system:serviceaccount:" + testRenameNamespace + ":workspace-ws-1"}
	renamer := &fakeWorkspaceRenamer{}
	r := newTestRenameRouter(t, reviewer, &fakeBootstrapLookup{}, renamer)

	w := doRename(t, r, "tok", `{not json`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.False(t, renamer.called)
}

// expiredTestToken builds a minimal JWT-shaped token whose exp claim is
// in the past — enough for unverifiedJWTExp's parse (it never verifies
// the signature; the fake reviewer stands in for TokenReview).
func expiredTestToken(t *testing.T) string {
	t.Helper()
	enc := func(s string) string {
		return base64.RawURLEncoding.EncodeToString([]byte(s))
	}
	return enc(`{"alg":"RS256","typ":"JWT"}`) + "." + enc(`{"exp":1000}`) + ".sig"
}
