// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Pins for the 2026-10-30 live-fire (trigger 7df2f7d4,
// "release-v0449-watch"): delete → 500 "failed to delete trigger" and
// update {enabled:false} → 500 "failed to fetch trigger" five times
// over 8 minutes while list read the same row happily. The physical
// fault was a transient connection-class store error during the
// v0.44.9 rollout window; the fix under test is (a) one bounded retry
// on connection-class faults, (b) the underlying error logged before
// every generic 500 arm, (c) client-canceled contexts classified 499
// instead of 500, and (d) idempotent-complete deletes when a retry
// after an uncertain transient attempt finds the row gone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/interfaces"
	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// errConnDown is the canonical connection-class PG fault
// (08006 connection_failure) the rollout window produced.
var errConnDown = &pgconn.PgError{Code: "08006", Message: "connection to server lost"}

// errUniqueViolation is a deterministic SQL fault — retrying it must
// NOT happen.
var errUniqueViolation = &pgconn.PgError{Code: "23505", Message: "duplicate key"}

// triggerFaultLogger records Error/Warn/Info calls (kv stringified) for
// assertions.
type triggerFaultLogger struct {
	mu    sync.Mutex
	errs  []string
	warns []string
	infos []string
}

func (l *triggerFaultLogger) Debug(msg string, kv ...interface{}) {
}
func (l *triggerFaultLogger) Info(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, msg+" "+kvString(kv))
}
func (l *triggerFaultLogger) Warn(msg string, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, msg+" "+kvString(kv))
}
func (l *triggerFaultLogger) Error(msg string, err error, kv ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.errs = append(l.errs, msg+" | "+err.Error()+" | "+kvString(kv))
}
func (l *triggerFaultLogger) Fatal(msg string, err error, kv ...interface{}) {}
func (l *triggerFaultLogger) With(kv ...interface{}) interfaces.LoggerInterface {
	return l
}
func (l *triggerFaultLogger) Sync() error { return nil }

func (l *triggerFaultLogger) errorCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.errs)
}

func (l *triggerFaultLogger) infoCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.infos)
}

func kvString(kv []interface{}) string {
	parts := make([]string, 0, len(kv))
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, fmt.Sprintf("%v=%v", kv[i], kv[i+1]))
	}
	return strings.Join(parts, " ")
}

// flakyTriggerStore wraps mockTriggerStore with per-verb scripted
// error sequences and call counters.
type flakyTriggerStore struct {
	*mockTriggerStore
	getErrs    []error
	updateErrs []error
	deleteErrs []error
	// getWorkflowErrs scripts the 0059 mapping-validation fetch
	// (reachable from the UPDATE verb via every mapping-touching patch).
	getWorkflowErrs []error
	// createErrs scripts CreateTrigger (NOT retried — the insert is not
	// idempotent; only the arm's logging is under test).
	createErrs []error

	getCalls         int
	updateCalls      int
	deleteCalls      int
	getWorkflowCalls int
	createCalls      int
}

func newFlakyTriggerStore() *flakyTriggerStore {
	return &flakyTriggerStore{mockTriggerStore: newMockTriggerStore()}
}

func (f *flakyTriggerStore) GetTrigger(ctx context.Context, ownerType, ownerID, triggerID string) (*wf.TriggerRow, error) {
	f.getCalls++
	if f.getCalls <= len(f.getErrs) && f.getErrs[f.getCalls-1] != nil {
		return nil, f.getErrs[f.getCalls-1]
	}
	return f.mockTriggerStore.GetTrigger(ctx, ownerType, ownerID, triggerID)
}

func (f *flakyTriggerStore) GetWorkflow(ctx context.Context, ownerType, ownerID, workflowID string) (*wf.WorkflowRow, error) {
	f.getWorkflowCalls++
	if f.getWorkflowCalls <= len(f.getWorkflowErrs) && f.getWorkflowErrs[f.getWorkflowCalls-1] != nil {
		return nil, f.getWorkflowErrs[f.getWorkflowCalls-1]
	}
	return f.mockTriggerStore.GetWorkflow(ctx, ownerType, ownerID, workflowID)
}

func (f *flakyTriggerStore) CreateTrigger(ctx context.Context, row *wf.TriggerRow) error {
	f.createCalls++
	if f.createCalls <= len(f.createErrs) && f.createErrs[f.createCalls-1] != nil {
		return f.createErrs[f.createCalls-1]
	}
	return f.mockTriggerStore.CreateTrigger(ctx, row)
}

func (f *flakyTriggerStore) UpdateTrigger(ctx context.Context, ownerType, ownerID, triggerID string, upd *wf.TriggerUpdate) (*wf.TriggerRow, error) {
	f.updateCalls++
	if f.updateCalls <= len(f.updateErrs) && f.updateErrs[f.updateCalls-1] != nil {
		return nil, f.updateErrs[f.updateCalls-1]
	}
	return f.mockTriggerStore.UpdateTrigger(ctx, ownerType, ownerID, triggerID, upd)
}

func (f *flakyTriggerStore) DeleteTrigger(ctx context.Context, ownerType, ownerID, triggerID string) error {
	f.deleteCalls++
	if f.deleteCalls <= len(f.deleteErrs) && f.deleteErrs[f.deleteCalls-1] != nil {
		return f.deleteErrs[f.deleteCalls-1]
	}
	return f.mockTriggerStore.DeleteTrigger(ctx, ownerType, ownerID, triggerID)
}

// setupTransientTriggerRouter mirrors setupTriggerRouter but installs
// the capture logger (the diagnosability path under test).
func setupTransientTriggerRouter(t *testing.T, store triggerStore, logger *triggerFaultLogger) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewUserTriggersHandler(store, &mockQuotaChecker{values: map[string]int{}}, &mockEncryptor{})
	h.SetLogger(logger)
	group := r.Group("/api/v1/me/triggers")
	group.Use(func(c *gin.Context) { c.Set("userID", "test-user"); c.Next() })
	group.GET("", h.UserList)
	group.POST("", h.UserCreate)
	group.GET("/:id", h.UserGet)
	group.PUT("/:id", h.UserUpdate)
	group.DELETE("/:id", h.UserDelete)
	group.POST("/:id/rotate-secret", h.UserRotateWebhookSecret)
	group.GET("/:id/fires", h.UserListFires)
	return r
}

// seedCronTrigger inserts one enabled cron routine trigger and returns
// its id — the shape of the live repro row (inline gate script
// included).
func seedCronTrigger(t *testing.T, store *flakyTriggerStore) string {
	t.Helper()
	row := &wf.TriggerRow{
		ID: "trg-transient-1", OwnerType: "user", OwnerID: "test-user",
		Name: "seed", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{"expr":"*/3 * * * *","tz":"UTC"}`),
		WorkspaceID:  wsPtrTransient("ws-1"), Prompt: "routine",
		ScriptLanguage: "python", ScriptHandler: "def handler(input):\n    return {'proceed': False, 'vars': {}}",
		MemoryMode: "none", CaptureMode: "errors_only", PreserveSession: "never",
	}
	store.triggers[row.ID] = row
	return row.ID
}

func wsPtrTransient(s string) *string { return &s }

// seedWorkflowTrigger inserts one enabled cron trigger targeting a
// workflow in mapped mode — every input/inputFrom patch on it walks the
// 0059 mapping validation and its GetWorkflow fetch (UPDATE-reachable).
func seedWorkflowTrigger(t *testing.T, store *flakyTriggerStore) string {
	t.Helper()
	wfID := "wf-transient-1"
	store.workflows[wfID] = &wf.WorkflowRow{ID: wfID, OwnerType: "user", OwnerID: "test-user"}
	row := &wf.TriggerRow{
		ID: "trg-transient-wf", OwnerType: "user", OwnerID: "test-user",
		Name: "seed-wf", Enabled: true, SourceType: "cron",
		SourceConfig: json.RawMessage(`{"expr":"*/3 * * * *","tz":"UTC"}`),
		WorkflowID:   wsPtrTransient(wfID), InputFrom: "mapped",
		MemoryMode: "none", CaptureMode: "errors_only", PreserveSession: "never",
	}
	store.triggers[row.ID] = row
	return row.ID
}

// --- isTransientStoreErr classification -------------------------------------

func TestIsTransientStoreErr(t *testing.T) {
	netErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"pg connection exception 08006", errConnDown, true},
		{"pg admin shutdown 57P01", &pgconn.PgError{Code: "57P01"}, true},
		{"pg crash shutdown 57P02", &pgconn.PgError{Code: "57P02"}, true},
		{"pg too many connections 53300", &pgconn.PgError{Code: "53300"}, true},
		{"pg system error 58000", &pgconn.PgError{Code: "58000"}, true},
		// The 2026-10-30 22:00-22:40 v0.44.11 live-fire class: the cached
		// per-id statement rotated away server-side (pooler recycle /
		// failover DEALLOCATE) — one-shot per (connection, statement),
		// healed by the immediate re-run; retried like every other
		// connection-lifecycle fault (pins in triggers_stmtcache_test.go
		// and store_integration_test.go).
		{"pg statement-cache miss 26000", errStmtCacheGone, true},
		{"wrapped pg statement-cache miss", errors.Join(errors.New("query: "), errStmtCacheGone), true},
		{"wrapped pg connection fault", errors.Join(errors.New("query: "), errConnDown), true},
		{"net op error", netErr, true},
		{"pg unique violation 23505", errUniqueViolation, false},
		{"pg syntax 42601", &pgconn.PgError{Code: "42601"}, false},
		// Structural guard: a PgError with a sub-2-byte code must
		// classify false, not panic (no server sends one; the type does
		// not forbid it).
		{"pg short code", &pgconn.PgError{Code: "X"}, false},
		{"context canceled", context.Canceled, false},
		{"plain error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isTransientStoreErr(tc.err))
		})
	}
}

// --- update: the "failed to fetch trigger" incident arm ----------------------

func TestTriggerUpdate_TransientFetchFault_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	// First GetTrigger dies with the rollout-class connection fault;
	// the retry reads the row — the incident's PUT {enabled:false}
	// now succeeds instead of 500ing.
	store.getErrs = []error{errConnDown}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.getCalls)
	assert.Equal(t, 1, store.updateCalls)

	var updated map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, false, updated["enabled"])
}

func TestTriggerUpdate_TransientFetchFault_Persists_500WithLoggedCause(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.getErrs = []error{errConnDown, errConnDown}
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 2, store.getCalls) // one retry, then the honest 500

	// The diagnosability pin: the 500 arm logged the underlying store
	// error with the identifying fields (pre-fix: silence).
	require.Equal(t, 1, logger.errorCount())
	entry := logger.errs[0]
	assert.Contains(t, entry, "triggers: fault behind a 5xx")
	assert.Contains(t, entry, "08006")
	assert.Contains(t, entry, "trg-transient-1")
}

func TestTriggerUpdate_DeterministicFetchFault_NotRetried(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.getErrs = []error{errUniqueViolation}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 1, store.getCalls) // deterministic fault: no retry
}

func TestTriggerUpdate_TransientStoreUpdateFault_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.updateErrs = []error{errConnDown}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.updateCalls)
}

// --- delete: the "failed to delete trigger" incident arm ---------------------

func TestTriggerDelete_TransientFault_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.deleteErrs = []error{errConnDown}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+id, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.deleteCalls)
	assert.Equal(t, 0, len(store.triggers))
}

func TestTriggerDelete_TransientFault_RetryNotFoundMeansGone(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	// First attempt dies uncertain (the statement may have committed on
	// the dead connection); the retry finds the row gone — the caller's
	// goal state holds, so 200, not the "undeletable" 404-after-500 trap.
	store.deleteErrs = []error{errConnDown}
	store.triggers = map[string]*wf.TriggerRow{} // row vanishes before the retry
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	w := doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+id, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.deleteCalls)
	assert.NotEmpty(t, logger.warns) // the idempotent-complete rationale is logged
}

func TestTriggerDelete_PersistentFault_500WithLoggedCause(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.deleteErrs = []error{errConnDown, errConnDown}
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	w := doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+id, nil)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Equal(t, 1, logger.errorCount())
	assert.Contains(t, logger.errs[0], "08006")
}

func TestTriggerDelete_PlainMiss_Stays404(t *testing.T) {
	store := newFlakyTriggerStore()
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/never-existed", nil)
	require.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, 1, store.deleteCalls)
}

// --- client-canceled classification ------------------------------------------

func TestTriggerUpdate_ClientCanceled_Classified499(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.getErrs = []error{context.Canceled}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/triggers/"+id, strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	cancel() // the client is gone before the store call returns
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, 499, w.Code)
	// A client-driven cancel is not retried and not a server fault.
	assert.Equal(t, 1, store.getCalls)
}

func TestTriggerUpdate_ClientCanceled_LogsClientEventNotFault(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.getErrs = []error{context.Canceled}
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/me/triggers/"+id, strings.NewReader(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	cancel() // the client is gone before the store call returns
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, 499, w.Code)
	// Review r1 pin: the 499 path logs an Info-level CLIENT event —
	// never an Error-level "fault behind a 5xx" (the mislabel).
	assert.Equal(t, 1, logger.infoCount())
	assert.Contains(t, logger.infos[0], "triggers: client canceled mid-request")
	assert.Equal(t, 0, logger.errorCount())
}

// --- the wired arms the review called out: fetch workflow (UPDATE-
// reachable via mapping patches) and create trigger --------------------------

func TestTriggerUpdate_MappingPatch_TransientWorkflowFetch_RetriedTo200(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedWorkflowTrigger(t, store)
	// The mapping-validation GetWorkflow dies once with the
	// connection-class fault; the retry reaches the schema and the
	// mapping patch lands — the UPDATE verb no longer 500s here.
	store.getWorkflowErrs = []error{errConnDown}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"input": map[string]any{"k": "v"}})
	require.Equal(t, http.StatusOK, w.Code)
	// mapping fetch (fault) + retry + the #1519 merged-view existence
	// check that follows validation — the retried fetch is calls 1–2.
	assert.Equal(t, 3, store.getWorkflowCalls)
}

func TestTriggerUpdate_MappingPatch_PersistentWorkflowFetch_500WithLoggedCause(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedWorkflowTrigger(t, store)
	store.getWorkflowErrs = []error{errConnDown, errConnDown}
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"input": map[string]any{"k": "v"}})
	require.Equal(t, http.StatusInternalServerError, w.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "failed to fetch workflow", body["error"])

	// The previously-silent arm now logs the underlying error with the
	// workflow and owner fields.
	require.Equal(t, 1, logger.errorCount())
	assert.Contains(t, logger.errs[0], "fetch_workflow")
	assert.Contains(t, logger.errs[0], "wf-transient-1")
	assert.Contains(t, logger.errs[0], "08006")
}

func TestTriggerCreate_PersistentFault_500WithLoggedCause(t *testing.T) {
	store := newFlakyTriggerStore()
	// CreateTrigger is deliberately NOT retried (the insert is not
	// idempotent — a retry after an uncertain commit would surface the
	// duplicate-key 23505, not the original fault); the arm's job is to
	// LOG the cause it used to swallow.
	store.createErrs = []error{errConnDown}
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "watch",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "*/3 * * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "routine",
	})
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 1, store.createCalls) // no retry on the insert
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "failed to create trigger", body["error"])

	require.Equal(t, 1, logger.errorCount())
	assert.Contains(t, logger.errs[0], "triggers: fault behind a 5xx")
	assert.Contains(t, logger.errs[0], "08006")
}

// --- get + listFires share the retry ------------------------------------------

func TestTriggerGet_TransientFault_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.getErrs = []error{errConnDown}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+id, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.getCalls)
}

func TestTriggerListFires_TransientFault_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	store.getErrs = []error{errConnDown}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+id+"/fires", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.getCalls)
}

// --- the incident pin: full round-trip over a trigger WITH fire history ------

func TestTriggerRoundTrip_WithFireHistory(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedCronTrigger(t, store)
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		fired := now.Add(-time.Duration(i+1) * 3 * time.Minute)
		completed := fired.Add(500 * time.Millisecond)
		store.fires = append(store.fires, &wf.TriggerFireRow{
			ID: string(rune('a' + i)), TriggerID: id, SourceType: "cron",
			ActionType: "routine", ActionResult: json.RawMessage(`{"gate":{"proceed":false}}`),
			Status: "skipped", FiredAt: fired, CompletedAt: &completed,
		})
	}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	// The history is visible through the API surface (the "list reads
	// the row fine" half of the incident).
	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+id+"/fires", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var firesResp struct {
		Fires []map[string]any `json:"fires"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &firesResp))
	require.Len(t, firesResp.Fires, 3)

	// Disable (the verb that 500ed live): 200.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, w.Code)

	// Delete (the other verb that 500ed live): 200 — fire history does
	// not block it at the handler layer (the store-level FK cascade is
	// pinned in store_integration_test.go).
	w = doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+id, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var deleted map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &deleted))
	assert.Equal(t, true, deleted["deleted"])

	// Post-delete: gone.
	w = doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+id, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
	w = doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+id, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}
