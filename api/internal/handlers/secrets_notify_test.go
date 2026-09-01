// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/api/internal/services/agentpush"
	"github.com/lenaxia/llmsafespaces/pkg/secrets"
)

// notifyPathStore is a full SecretStore + CredentialStore + RevisionStore
// for the notify-flip and revoke-fan-out tests. Only the methods the
// paths exercise carry real state; the rest are quiet no-ops.
type notifyPathStore struct {
	mu          sync.Mutex
	credentials []secrets.CredentialBinding
	bindings    map[string][]string
	secrets     map[string]*secrets.UserSecret
	revisions   map[string]notifyRevRow
	audit       []*secrets.AuditEntry
}

type notifyRevRow struct {
	seq  int64
	hash string
}

func newNotifyPathStore() *notifyPathStore {
	return &notifyPathStore{
		bindings:  make(map[string][]string),
		secrets:   make(map[string]*secrets.UserSecret),
		revisions: make(map[string]notifyRevRow),
	}
}

func (s *notifyPathStore) GetWorkspaceCredentials(_ context.Context, _ string) ([]secrets.CredentialBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]secrets.CredentialBinding, len(s.credentials))
	copy(cp, s.credentials)
	return cp, nil
}
func (s *notifyPathStore) UpsertFreeTierCredential(context.Context, []byte) error { return nil }
func (s *notifyPathStore) SeedWorkspaceCredentials(context.Context, string, string, *string) error {
	return nil
}
func (s *notifyPathStore) BindCredentialToAllUserWorkspaces(context.Context, string, string) error {
	return nil
}
func (s *notifyPathStore) HasUserProviderCredential(context.Context, string, string) (bool, error) {
	return false, nil
}
func (s *notifyPathStore) GetWorkspaceMCPServers(context.Context, string) ([]secrets.MCPServerBindingRow, error) {
	return nil, nil
}

func (s *notifyPathStore) GetBindings(_ context.Context, ws string) ([]*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*secrets.UserSecret, 0, len(s.bindings[ws]))
	for _, sid := range s.bindings[ws] {
		if sec := s.secrets[sid]; sec != nil {
			cp := *sec
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *notifyPathStore) GetBindingsForSecret(_ context.Context, secretID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for wsID, sids := range s.bindings {
		for _, sid := range sids {
			if sid == secretID {
				out = append(out, wsID)
				break
			}
		}
	}
	return out, nil
}

func (s *notifyPathStore) LogAudit(_ context.Context, entry *secrets.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, entry)
	return nil
}

func (s *notifyPathStore) CurrentRevision(_ context.Context, ws string) (int64, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.revisions[ws]
	if !ok {
		return 0, "", false, nil
	}
	return row.seq, row.hash, true, nil
}

func (s *notifyPathStore) EnsureRevision(_ context.Context, ws, hash string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.revisions[ws]; ok && row.hash == hash {
		return row.seq, nil
	}
	next := s.revisions[ws].seq + 1
	s.revisions[ws] = notifyRevRow{seq: next, hash: hash}
	return next, nil
}

func (s *notifyPathStore) CreateSecret(_ context.Context, sec *secrets.UserSecret) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec.ID == "" {
		sec.ID = "sec-" + sec.Name
	}
	cp := *sec
	s.secrets[sec.ID] = &cp
	return nil
}

func (s *notifyPathStore) GetSecret(_ context.Context, _, id string) (*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sec := s.secrets[id]; sec != nil {
		cp := *sec
		return &cp, nil
	}
	return nil, nil
}

func (s *notifyPathStore) GetSecretByName(_ context.Context, _, name string) (*secrets.UserSecret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sec := range s.secrets {
		if sec.Name == name {
			cp := *sec
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *notifyPathStore) ListSecrets(context.Context, string) ([]*secrets.UserSecret, error) {
	return nil, nil
}
func (s *notifyPathStore) ListGlobalDefaultSecrets(context.Context, string) ([]*secrets.UserSecret, error) {
	return nil, nil
}
func (s *notifyPathStore) UpdateSecret(_ context.Context, sec *secrets.UserSecret) error { return nil }
func (s *notifyPathStore) ReEncryptUserSecrets(context.Context, string, int,
	func([]byte) ([]byte, error), func(context.Context) error) error {
	return nil
}

func (s *notifyPathStore) DeleteSecret(_ context.Context, _, secretID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.secrets[secretID]; !ok {
		return &missingSecretErr{id: secretID}
	}
	delete(s.secrets, secretID)
	for wsID, sids := range s.bindings {
		filtered := make([]string, 0, len(sids))
		for _, sid := range sids {
			if sid != secretID {
				filtered = append(filtered, sid)
			}
		}
		s.bindings[wsID] = filtered
	}
	return nil
}

type missingSecretErr struct{ id string }

func (e *missingSecretErr) Error() string { return "not found: " + e.id }
func (e *missingSecretErr) Unwrap() error { return secrets.ErrSecretNotFound }

func (s *notifyPathStore) SetBindings(_ context.Context, ws string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[ws] = ids
	return nil
}

func (s *notifyPathStore) AddBindings(_ context.Context, ws string, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindings[ws] = append(s.bindings[ws], ids...)
	return nil
}

func (s *notifyPathStore) QueryAudit(_ context.Context, _ string, _ secrets.AuditQuery) ([]*secrets.AuditEntry, error) {
	return nil, nil
}

// agentdResyncMock is an in-pod agentd stand-in on the real :4097 port.
type agentdResyncMock struct {
	mu      sync.Mutex
	calls   []resyncCall
	handler func(w http.ResponseWriter)
}

type resyncCall struct {
	path    string
	auth    string
	bodyLen int
}

func startAgentdResyncMock(t *testing.T, handler func(w http.ResponseWriter)) *agentdResyncMock {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:4097")
	if err != nil {
		t.Skip("port 4097 not available for test agentd mock")
	}
	mock := &agentdResyncMock{handler: handler}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mock.mu.Lock()
		mock.calls = append(mock.calls, resyncCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), bodyLen: len(body)})
		mock.mu.Unlock()
		handler(w)
	}))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return mock
}

func (m *agentdResyncMock) dispatched() []resyncCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]resyncCall, len(m.calls))
	copy(cp, m.calls)
	return cp
}

func newNotifyTestHandler(t *testing.T, store *notifyPathStore) *SecretsHandler {
	t.Helper()
	keySvc := secrets.NewKeyService(newTestKeyStore(), newTestDEKCache())
	svc := secrets.NewSecretService(keySvc, store)
	handler := NewSecretsHandler(svc)
	handler.SetPodIPResolver(&staticPodIPResolver{addr: "127.0.0.1"})
	handler.SetPasswordProvider(staticPasswordProvider{})
	handler.SetLogger(&recordingLogger{})
	return handler
}

func seedNotifySecret(t *testing.T, store *notifyPathStore, name string) string {
	t.Helper()
	sec := &secrets.UserSecret{
		ID: "sec-" + name, UserID: "user-1", Name: name, Type: secrets.SecretTypeEnvSecret,
		Ciphertext: []byte("opaque"), KeyVersion: 1, Version: 1,
		Metadata: json.RawMessage(`{"var_name":"` + strings.ToUpper(name) + `"}`),
	}
	require.NoError(t, store.CreateSecret(context.Background(), sec))
	return sec.ID
}

// TestHandler_BindNotifiesInsteadOfBodyPush is the US-70.3 flip pin: a
// bind produces an EMPTY notify to /v1/resync-secrets, never a batch
// body — the pod re-pulls through the conditional bootstrap path.
func TestHandler_BindNotifiesInsteadOfBodyPush(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := startAgentdResyncMock(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"2:a:b","restarted":false}`))
	})

	store := newNotifyPathStore()
	secID := seedNotifySecret(t, store, "db_url")
	store.bindings["ws-1"] = []string{secID}

	handler := newNotifyTestHandler(t, store)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Set("sessionID", "apikey:fake-hash")
		c.Next()
	})
	router.PUT("/api/v1/workspaces/:id/bindings", handler.SetBindings)

	bindBody, _ := json.Marshal(map[string][]string{"secretIds": {secID}})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-1/bindings", strings.NewReader(string(bindBody)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)

	calls := mock.dispatched()
	require.Len(t, calls, 1, "the bind must dispatch exactly one notify")
	assert.Equal(t, "/v1/resync-secrets", calls[0].path)
	assert.Equal(t, 0, calls[0].bodyLen, "the notify must carry NO batch body")
	assert.True(t, strings.HasPrefix(calls[0].auth, "Basic "),
		"the notify must authenticate with the workspace password")
}

// TestHandler_ReloadSecretsReturnsNotifyResult pins the
// POST /workspaces/:id/reload-secrets response shape after the flip:
// the pod's resync outcome, not a server-built reload count.
func TestHandler_ReloadSecretsReturnsNotifyResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := startAgentdResyncMock(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"9:x:y","restarted":true}`))
	})
	_ = mock

	store := newNotifyPathStore()
	handler := newNotifyTestHandler(t, store)

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/api/v1/workspaces/:id/reload-secrets", handler.ReloadSecrets)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/reload-secrets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var body struct {
		Status     string `json:"status"`
		AppliedRev string `json:"appliedRev"`
		Restarted  bool   `json:"restarted"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "applied", body.Status)
	assert.Equal(t, "9:x:y", body.AppliedRev)
	assert.True(t, body.Restarted)
}

func TestHandler_ReloadSecretsNoPodIs409(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := newNotifyPathStore()
	handler := newNotifyTestHandler(t, store)
	handler.SetPodIPResolver(&staticPodIPResolver{addr: ""})

	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.POST("/api/v1/workspaces/:id/reload-secrets", handler.ReloadSecrets)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/reload-secrets", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusConflict, w.Code)
}

// TestHandler_DeleteSecretIsForceRevoke pins I12: a plain DELETE by the
// owner revokes everywhere — bindings gone, stored revision bumped for
// exactly the affected workspaces, one notify per affected live pod —
// and a notify failure never fails the delete.
func TestHandler_DeleteSecretIsForceRevoke(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var failNotifies bool
	mock := startAgentdResyncMock(t, func(w http.ResponseWriter) {
		if failNotifies {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"status":"failed","reason":"pull_failed"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"4:a:b","restarted":true}`))
	})

	store := newNotifyPathStore()
	doomedID := seedNotifySecret(t, store, "doomed")
	keeperID := seedNotifySecret(t, store, "keeper")
	store.bindings["ws-1"] = []string{doomedID}
	store.bindings["ws-2"] = []string{doomedID, keeperID}
	store.bindings["ws-3"] = []string{keeperID}

	for _, ws := range []string{"ws-1", "ws-2", "ws-3"} {
		_, err := store.EnsureRevision(context.Background(), ws, "pre-revoke-hash")
		require.NoError(t, err)
	}

	handler := newNotifyTestHandler(t, store)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.DELETE("/api/v1/secrets/:id", handler.DeleteSecret)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/"+doomedID, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code, "notify failures must never fail the revoke")

	assert.Empty(t, mustListBindings(t, store, "ws-1"), "ws-1 must no longer bind the revoked secret")
	assert.Equal(t, []string{keeperID}, mustListBindings(t, store, "ws-2"))
	assert.Equal(t, []string{keeperID}, mustListBindings(t, store, "ws-3"))

	for _, ws := range []string{"ws-1", "ws-2"} {
		seq, hash, ok, err := store.CurrentRevision(context.Background(), ws)
		require.NoError(t, err)
		require.True(t, ok, ws)
		assert.Equal(t, int64(2), seq, "affected workspace %s must have its stored revision bumped", ws)
		assert.NotEqual(t, "pre-revoke-hash", hash)
	}
	seq, _, ok, err := store.CurrentRevision(context.Background(), "ws-3")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(1), seq, "unaffected workspace must not mint a new seq")

	// ws-2 is notified once (not once per binding), ws-1 once; ws-3 never.
	paths := mock.dispatched()
	notified := map[string]bool{}
	for _, c := range paths {
		assert.Equal(t, "/v1/resync-secrets", c.path)
		notified[c.path] = true
	}
	require.Len(t, paths, 2, "exactly the affected workspaces get notified")

	// Second delete of the same secret: 404, no further notifies.
	failNotifies = true
	req2 := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/"+doomedID, nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusNotFound, w2.Code)
	assert.Len(t, mock.dispatched(), 2)
}

// TestHandler_DeleteSecretNotifyFailureStillSucceeds pins that a fully
// unreachable fleet degrades to eventual reconcile convergence: the
// delete itself (rows + revision refresh) must succeed.
func TestHandler_DeleteSecretNotifyFailureStillSucceeds(t *testing.T) {
	gin.SetMode(gin.TestMode)
	startAgentdResyncMock(t, func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"status":"failed","reason":"pull_failed"}`))
	})

	store := newNotifyPathStore()
	doomedID := seedNotifySecret(t, store, "doomed")
	store.bindings["ws-remote"] = []string{doomedID}
	_, err := store.EnsureRevision(context.Background(), "ws-remote", "h")
	require.NoError(t, err)

	handler := newNotifyTestHandler(t, store)
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userID", "user-1"); c.Next() })
	router.DELETE("/api/v1/secrets/:id", handler.DeleteSecret)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/secrets/"+doomedID, nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Empty(t, mustListBindings(t, store, "ws-remote"))
	seq, _, ok, err := store.CurrentRevision(context.Background(), "ws-remote")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, int64(2), seq, "the revision refresh runs even when every notify fails")
}

func mustListBindings(t *testing.T, store *notifyPathStore, ws string) []string {
	t.Helper()
	bound, err := store.GetBindings(context.Background(), ws)
	require.NoError(t, err)
	ids := make([]string, 0, len(bound))
	for _, sec := range bound {
		ids = append(ids, sec.ID)
	}
	return ids
}

// TestHandler_WorkspaceEnvMutationsNotify pins the env path of the
// flip: PUT /workspaces/:id/env and DELETE /workspaces/:id/env/:name
// both notify the pod (env vars are bound secrets — AC-3 live delivery).
func TestHandler_WorkspaceEnvMutationsNotify(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mock := startAgentdResyncMock(t, func(w http.ResponseWriter) {
		_, _ = w.Write([]byte(`{"status":"applied","appliedRev":"2:a:b","restarted":false}`))
	})

	store := newNotifyPathStore()
	notifier := agentpush.New(
		agentpush.WithPodIPResolver(&staticPodIPResolver{addr: "127.0.0.1"}),
		agentpush.WithPasswordProvider(staticPasswordProvider{}),
	)
	envHandler := NewWorkspaceEnvHandler(mustEnvService(t, store))
	envHandler.SetLogger(&recordingLogger{})
	envHandler.SetNotifier(func(ctx context.Context, userID, workspaceID string) error {
		_, err := notifier.Notify(ctx, userID, workspaceID)
		return err
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("userID", "user-1")
		c.Set("sessionID", "session-1")
		c.Next()
	})
	router.PUT("/api/v1/workspaces/:id/env", envHandler.SetWorkspaceEnv)
	router.DELETE("/api/v1/workspaces/:id/env/:name", envHandler.DeleteWorkspaceEnv)

	putBody := `{"vars":{"API_TOKEN":"tok"}}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/workspaces/ws-9/env", strings.NewReader(putBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusNoContent, w.Code)
	require.Len(t, mock.dispatched(), 1, "PUT env must notify")

	// Simulate the pod-side pull that the PUT's notify triggered: the
	// bootstrap build mints the workspace's first revision row.
	revSeq, err := store.EnsureRevision(context.Background(), "ws-9", "post-put-hash")
	require.NoError(t, err)
	before := revSeq

	req2 := httptest.NewRequest(http.MethodDelete, "/api/v1/workspaces/ws-9/env/API_TOKEN", nil)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusNoContent, w2.Code)
	require.Len(t, mock.dispatched(), 2, "DELETE env must notify")

	revSeq, _, ok, err := store.CurrentRevision(context.Background(), "ws-9")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Greater(t, revSeq, before,
		"env delete must force-refresh the stored revision (revoke semantics)")
}

func mustEnvService(t *testing.T, store *notifyPathStore) *secrets.SecretService {
	t.Helper()
	keySvc := secrets.NewKeyService(newTestKeyStore(), newTestDEKCache())
	return secrets.NewSecretService(keySvc, store)
}
