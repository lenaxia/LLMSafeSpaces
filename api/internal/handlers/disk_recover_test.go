// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apierrors "github.com/lenaxia/llmsafespaces/api/internal/errors"
	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/diskrecovery"
	"github.com/lenaxia/llmsafespaces/pkg/types"
)

func errNotFoundForTest() error {
	return apierrors.NewNotFoundError("workspace", "ws-1", nil)
}

func typesWorkspace(phase string) *types.Workspace {
	return &types.Workspace{Phase: phase}
}

// --- #1601 disk-recover facade tests (mocks borrowed from the reload
// suite: mockWsSvc, mockPodIPResolver) ---

type diskRecoverPwGetter struct {
	pw  string
	err error
}

func (m *diskRecoverPwGetter) WorkspacePassword(_ context.Context, _ string) (string, error) {
	return m.pw, m.err
}

func newDiskRecoverRouter(handler *DiskRecoverHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/disk-recover", handler.Recover)
	return router
}

func activeHandler(wsSvc WorkspaceServicer, pod PodIPResolver, pw string, port int, client *http.Client) *DiskRecoverHandler {
	h := NewDiskRecoverHandler(wsSvc, pod, client, nil)
	h.SetPasswordGetter(&diskRecoverPwGetter{pw: pw})
	h.agentdPort = port
	return h
}

// PIN (owner authz): no session → 401; a workspace the caller does not
// own (GetWorkspace error, the userID-scoped lookup's answer for a
// foreign workspace) → 404, never a dispatch.
func TestDiskRecover_AuthAndOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bare := gin.New() // no userID middleware — the anonymous caller
	handler := NewDiskRecoverHandler(nil, nil, nil, nil)
	bare.POST("/workspaces/:id/disk-recover", handler.Recover)
	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/disk-recover", nil)
	rec := httptest.NewRecorder()
	bare.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// The owner-authz seam IS the userID-scoped GetWorkspace: an error
	// (not-found = someone else's workspace) must surface, not dispatch.
	notFound := NewDiskRecoverHandler(
		&mockWsSvc{err: errNotFoundForTest()}, &mockPodIPResolver{ip: "10.0.0.1"}, nil, nil)
	notFound.SetPasswordGetter(&diskRecoverPwGetter{pw: "pw"})
	notFound.agentdPort = 1 // would fail loudly if dispatched
	router2 := newDiskRecoverRouter(notFound)
	rec2 := httptest.NewRecorder()
	router2.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/disk-recover", nil))
	assert.Equal(t, http.StatusNotFound, rec2.Code)
}

// PIN (phase gate): non-Active workspace → 409 before any dispatch.
func TestDiskRecover_PhaseGate(t *testing.T) {
	h := activeHandler(&mockWsSvc{ws: typesWorkspace("Suspended")}, &mockPodIPResolver{ip: "10.0.0.1"}, "pw", 1, nil)
	router := newDiskRecoverRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/disk-recover", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "Suspended")
}

// PIN (dispatch contract): the facade posts {dryRun} to agentd's
// /v1/disk-recover with the workspace password as Basic auth, and
// relays the typed report — dryRun=true crosses as the JSON body, not
// a query param.
func TestDiskRecover_DispatchAndTypedRelay(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	agentdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var buf [4096]byte
		n, _ := r.Body.Read(buf[:])
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(diskrecovery.Report{
			DryRun: true, BytesFreed: 0, RuntimeBase: "opencode",
			Classes: []diskrecovery.ClassReport{{Class: "npm-cache", Bytes: 4096, Status: diskrecovery.ClassWouldFree}},
		})
	}))
	defer agentdSrv.Close()

	h := activeHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: "127.0.0.1"}, "sekrit", agentdSrv.Listener.Addr().(*net.TCPAddr).Port, agentdSrv.Client())
	router := newDiskRecoverRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/disk-recover?dryRun=true", nil))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "/v1/disk-recover", gotPath)
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":sekrit")), gotAuth)
	assert.Contains(t, gotBody, `"dryRun":true`)
	var out diskrecovery.Report
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.EqualValues(t, 0, out.BytesFreed)
	require.Len(t, out.Classes, 1)
	assert.Equal(t, "npm-cache", out.Classes[0].Class)
	assert.Equal(t, diskrecovery.ClassWouldFree, out.Classes[0].Status)
}

// PIN (agentd status mapping): 409 busy and 503 no-usage cross as
// their HTTP classes (the button renders "already in progress"
// distinctly from "usage unavailable"); 500 generic.
func TestDiskRecover_AgentdStatusMapping(t *testing.T) {
	cases := []struct {
		agentdCode int
		want       int
	}{
		{http.StatusConflict, http.StatusConflict},
		{http.StatusServiceUnavailable, http.StatusServiceUnavailable},
		{http.StatusGatewayTimeout, http.StatusGatewayTimeout},
		{http.StatusInternalServerError, http.StatusInternalServerError},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(c.agentdCode)
			_, _ = w.Write([]byte(`{}`))
		}))
		h := activeHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: "127.0.0.1"}, "pw", srv.Listener.Addr().(*net.TCPAddr).Port, srv.Client())
		router := newDiskRecoverRouter(h)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/disk-recover", nil))
		assert.Equal(t, c.want, rec.Code, "agentd %d", c.agentdCode)
		srv.Close()
	}
}

// PIN (pod unreachable): no pod → 409 with a human message.
func TestDiskRecover_PodUnreachable(t *testing.T) {
	h := activeHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: ""}, "pw", 0, nil)
	router := newDiskRecoverRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/disk-recover", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "not reachable")
}

// PIN (real agentd error shapes, review r1 F1): agentd's non-200
// paths emit PLAIN TEXT via http.Error — the facade must map the
// status class without decoding the body. (Pre-fix, the decode-first
// order turned every real 409/503/504 into 500 decode_failed; the
// existing mapping test used JSON bodies and masked it.)
func TestDiskRecover_AgentdPlainTextErrorsMapByStatus(t *testing.T) {
	cases := []struct {
		agentdCode int
		agentdBody string
		want       int
	}{
		{http.StatusConflict, "disk recovery already in progress\n", http.StatusConflict},
		{http.StatusServiceUnavailable, "disk usage unavailable\n", http.StatusServiceUnavailable},
		{http.StatusGatewayTimeout, "disk recovery timed out\n", http.StatusGatewayTimeout},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, c.agentdBody, c.agentdCode)
		}))
		h := activeHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: "127.0.0.1"}, "pw", srv.Listener.Addr().(*net.TCPAddr).Port, srv.Client())
		router := newDiskRecoverRouter(h)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/disk-recover", nil))
		assert.Equal(t, c.want, rec.Code, "agentd %d plain-text: got %d body=%s", c.agentdCode, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "decode", "plain-text bodies must never surface as decode failures")
		srv.Close()
	}
}
