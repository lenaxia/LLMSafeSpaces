// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
	"github.com/lenaxia/llmsafespaces/pkg/fsaudit"
)

// --- #1808 fs-audit facade tests (mocks borrowed from the
// disk-recover suite: mockWsSvc, mockPodIPResolver, diskRecoverPwGetter) ---

func newFsAuditRouter(handler *FsAuditHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/workspaces/:id/fs-audit", handler.Audit)
	return router
}

func activeFsAuditHandler(wsSvc WorkspaceServicer, pod PodIPResolver, pw string, port int, client *http.Client) *FsAuditHandler {
	h := NewFsAuditHandler(wsSvc, pod, client, nil)
	h.SetPasswordGetter(&diskRecoverPwGetter{pw: pw})
	h.agentdPort = port
	return h
}

// PIN (owner authz): no session → 401; a workspace the caller does not
// own (GetWorkspace error, the userID-scoped lookup's answer for a
// foreign workspace) → 404, never a dispatch.
func TestFsAudit_AuthAndOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bare := gin.New() // no userID middleware — the anonymous caller
	handler := NewFsAuditHandler(nil, nil, nil, nil)
	bare.POST("/workspaces/:id/fs-audit", handler.Audit)
	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit", nil)
	rec := httptest.NewRecorder()
	bare.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	notFound := NewFsAuditHandler(
		&mockWsSvc{err: errNotFoundForTest()}, &mockPodIPResolver{ip: "10.0.0.1"}, nil, nil)
	notFound.SetPasswordGetter(&diskRecoverPwGetter{pw: "pw"})
	notFound.agentdPort = 1 // would fail loudly if dispatched
	router2 := newFsAuditRouter(notFound)
	rec2 := httptest.NewRecorder()
	router2.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit", nil))
	assert.Equal(t, http.StatusNotFound, rec2.Code)
}

// PIN (phase gate): non-Active workspace → 409 before any dispatch.
func TestFsAudit_PhaseGate(t *testing.T) {
	h := activeFsAuditHandler(&mockWsSvc{ws: typesWorkspace("Suspended")}, &mockPodIPResolver{ip: "10.0.0.1"}, "pw", 1, nil)
	router := newFsAuditRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "Suspended")
}

// PIN (dispatch contract): the facade POSTs the typed request to
// agentd's /v1/fs-audit with the workspace password as Basic auth and
// relays the typed report; the `root` query param crosses as the JSON
// body's root.
func TestFsAudit_DispatchAndTypedRelay(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	agentdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var buf [4096]byte
		n, _ := r.Body.Read(buf[:])
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fsaudit.Report{
			Root: "repo", FilesScanned: 12, NulFilledCount: 2,
			NulFilled: []fsaudit.FileReport{{Path: "a.yaml", Size: 4096, Verdict: fsaudit.VerdictNulFilled}},
		})
	}))
	defer agentdSrv.Close()

	h := activeFsAuditHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: "127.0.0.1"}, "sekrit", agentdSrv.Listener.Addr().(*net.TCPAddr).Port, agentdSrv.Client())
	router := newFsAuditRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit?root=repo", nil))

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "/v1/fs-audit", gotPath)
	assert.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte(agentd.AuthUsername+":sekrit")), gotAuth)
	assert.Contains(t, gotBody, `"root":"repo"`)
	var out fsaudit.Report
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 2, out.NulFilledCount)
	require.Len(t, out.NulFilled, 1)
	assert.Equal(t, "a.yaml", out.NulFilled[0].Path)
	assert.Equal(t, fsaudit.VerdictNulFilled, out.NulFilled[0].Verdict)
}

// PIN (nil-list hardening): a 200 body with null lists still leaves
// the facade emitting [] to the owner (the diskrecovery r1 F2/F4
// lesson).
func TestFsAudit_NullListHardening(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"root":".","nulFilled":null,"suspect":null,"errors":null}`))
	}))
	defer srv.Close()
	h := activeFsAuditHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: "127.0.0.1"}, "pw", srv.Listener.Addr().(*net.TCPAddr).Port, srv.Client())
	router := newFsAuditRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"nulFilled":[]`)
	assert.Contains(t, rec.Body.String(), `"suspect":[]`)
	assert.Contains(t, rec.Body.String(), `"errors":[]`)
}

// PIN (agentd status mapping + plain-text error lesson): 409/503/504
// cross as their HTTP classes; agentd's non-200 paths emit PLAIN TEXT
// via http.Error — the facade maps the status class without decoding
// the body (disk-recover r1 F1).
func TestFsAudit_AgentdStatusMapping(t *testing.T) {
	for _, c := range []struct {
		agentdCode int
		agentdBody string
		want       int
	}{
		{http.StatusConflict, "fs audit already in progress\n", http.StatusConflict},
		{http.StatusServiceUnavailable, "fs audit not wired\n", http.StatusServiceUnavailable},
		{http.StatusGatewayTimeout, "fs audit timed out\n", http.StatusGatewayTimeout},
		{http.StatusInternalServerError, "fs audit failed\n", http.StatusInternalServerError},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, c.agentdBody, c.agentdCode)
		}))
		h := activeFsAuditHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: "127.0.0.1"}, "pw", srv.Listener.Addr().(*net.TCPAddr).Port, srv.Client())
		router := newFsAuditRouter(h)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit", nil))
		assert.Equal(t, c.want, rec.Code, "agentd %d: body=%s", c.agentdCode, rec.Body.String())
		assert.False(t, strings.Contains(rec.Body.String(), "decode"), "plain-text bodies must never surface as decode failures")
		srv.Close()
	}
}

// PIN (pod unreachable): no pod → 409 with a human message.
func TestFsAudit_PodUnreachable(t *testing.T) {
	h := activeFsAuditHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: ""}, "pw", 0, nil)
	router := newFsAuditRouter(h)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "not reachable")
}

// PIN (bad body): a body that is not the typed request shape is a 400
// before any dispatch.
func TestFsAudit_BadBodyIs400(t *testing.T) {
	h := activeFsAuditHandler(&mockWsSvc{ws: typesWorkspace("Active")}, &mockPodIPResolver{ip: "127.0.0.1"}, "pw", 1, nil)
	router := newFsAuditRouter(h)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/workspaces/ws-1/fs-audit", strings.NewReader(`{"root": 42}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
