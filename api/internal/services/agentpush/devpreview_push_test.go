// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package agentpush_test

// devpreview_push_test.go — #1617: the workspace dev-preview toggle must
// reach the RUNNING pod. PushDevPreviewState posts the absolute state to
// agentd's /v1/dev-preview-state so feature_status / dev_preview_url read
// live values instead of the boot-time env snapshot.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
)

func TestPushDevPreviewState_PostsStateToPod(t *testing.T) {
	server, spy := newNotifyServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok","enabled":true}`))
	})
	svc := newNotifyService(t, server.URL)

	err := svc.PushDevPreviewState(context.Background(), "user-1", "ws-1", true)
	require.NoError(t, err)

	path, method, auth, bodyLen := spy.snapshot()
	assert.Equal(t, "/v1/dev-preview-state", path)
	assert.Equal(t, http.MethodPost, method)
	assert.NotEmpty(t, auth, "the dispatch must carry the workspace Basic credential")
	assert.Greater(t, bodyLen, 0, "the absolute state rides the JSON body")
}

func TestPushDevPreviewState_NoRunningPodSurfacesSentinel(t *testing.T) {
	svc := newNotifyService(t, "http://127.0.0.1:1",
		agentpush.WithPodIPResolver(&fakeResolver{ip: ""}))
	err := svc.PushDevPreviewState(context.Background(), "user-1", "ws-1", true)
	require.ErrorIs(t, err, agentpush.ErrNoRunningPod)
}

func TestPushDevPreviewState_PodErrorSurfaces(t *testing.T) {
	server, _ := newNotifyServer(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	svc := newNotifyService(t, server.URL)
	err := svc.PushDevPreviewState(context.Background(), "user-1", "ws-1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestPushDevPreviewState_BothStatesRideTheBody(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		var gotPath, gotBody string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			gotPath = r.URL.Path
			gotBody = string(body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}))
		svc := newNotifyService(t, server.URL)

		err := svc.PushDevPreviewState(context.Background(), "user-1", "ws-1", enabled)
		require.NoError(t, err)
		assert.Equal(t, "/v1/dev-preview-state", gotPath)
		want := `"enabled":true`
		if !enabled {
			want = `"enabled":false`
		}
		assert.Contains(t, gotBody, want, "the pushed value must be the absolute state")
		server.Close()
	}
}
