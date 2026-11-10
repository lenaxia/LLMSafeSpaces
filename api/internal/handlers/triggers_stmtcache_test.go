// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

// Pins for the SECOND 2026-10-30 live-fire (trigger 5da42f34,
// "v04411-release-watch", created 21:31:58Z — AFTER the #1995 fix
// shipped in v0.44.10): over the 21:40-22:40 v0.44.11 release+rollout
// window, per-id operations 500ed — fires/update → "failed to fetch
// trigger", delete → "failed to delete trigger" — while LIST read the
// same row fine, and every per-id verb succeeded first-try again once
// the rollout settled (verified live at 22:43-22:47).
//
// Root cause (empirically pinned on real PG — see
// store_integration_test.go TestTriggerStatementCacheRotation_Class26IsOneShot):
// when a rollout rotates server-side prepared statements behind pgx's
// statement cache (pooler recycle / failover-side DEALLOCATE), the next
// execution of a cached per-id statement fails ONCE with SQLSTATE 26000
// `prepared statement "stmtcache_…" does not exist`; the immediate
// re-run re-prepares and succeeds. #1995's isTransientStoreErr covered
// TCP-class faults (08/53/57/58 + net.Error) but let the deterministic-
// LOOKING class 26 through un-retried — a per-(connection, statement)
// one-shot blip became the hard 500s. LIST survived on connections
// whose statement had already been revalidated; per-id ops on the one
// row being operated on kept landing on unburned ones. The row's
// creation time (post-fix) was a red herring: the failure is
// condition-dependent (rollout window), not row-shape-dependent — the
// seeds below recreate the exact live row shape anyway, so a future
// deterministic row-shape regression cannot hide behind that fact.

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

// errStmtCacheGone is the exact error the rollout window produced (code
// and message shape captured from the real-PG repro: DEALLOCATE ALL
// behind pgx's statement cache, then re-executing the cached per-id
// statement).
var errStmtCacheGone = &pgconn.PgError{
	Code:    "26000",
	Message: `prepared statement "stmtcache_4de252cdc7eab90f680b2fed84d2e9b6b0eaa3a224b382ef" does not exist`,
}

// v04411WatchScript is the live row's inline python gate script,
// verbatim (marker file + gh run poll) — including it keeps the seed at
// the exact failing row shape.
const v04411WatchScript = `import json, subprocess, os

MARKER = '/tmp/opencode/v04411-deploy-fired.marker'
RUN_ID = '41131334216'

def handler(input):
    try:
        out = subprocess.run(['gh', 'run', 'view', RUN_ID, '--json', 'status,conclusion'], capture_output=True, text=True, timeout=30)
        if out.returncode != 0:
            return {'proceed': False, 'vars': {}}
        run = json.loads(out.stdout)
        if run.get('status', '') != 'completed':
            return {'proceed': False, 'vars': {}}
        if os.path.exists(MARKER):
            return {'proceed': False, 'vars': {}}
        os.makedirs(os.path.dirname(MARKER), exist_ok=True)
        conclusion = run.get('conclusion') or 'unknown'
        with open(MARKER, 'w') as f:
            f.write(conclusion)
        return {'proceed': True, 'vars': {'conclusion': conclusion, 'run_id': RUN_ID}}
    except Exception:
        return {'proceed': False, 'vars': {}}
`

// seedV04411Watch inserts the exact live-row shape of trigger 5da42f34:
// enabled cron */5 routine whose sourceConfig carries NO tz key (the
// one visible field difference from the healthy sibling rows — benign,
// schedule.go defaults to UTC), the inline marker-file gate script, and
// autoDisableAfter 12. Returns the seeded id.
func seedV04411Watch(t *testing.T, store *flakyTriggerStore) string {
	t.Helper()
	row := &wf.TriggerRow{
		ID: "trg-v04411-watch", OwnerType: "user", OwnerID: "test-user",
		Name: "v04411-release-watch", Enabled: true, SourceType: "cron",
		SourceConfig:   json.RawMessage(`{"expr":"*/5 * * * *"}`),
		WorkspaceID:    wsPtrTransient("ws-1"),
		Prompt:         "The v0.44.11 release run (GitHub Actions id {{run_id}}) completed with conclusion: {{conclusion}}.",
		ScriptLanguage: "python", ScriptHandler: v04411WatchScript,
		MemoryMode: "none", MemoryMaxRuns: 1, CaptureMode: "errors_only",
		PreserveSession: "never", AutoDisableAfter: 12, InputFrom: "envelope",
	}
	store.triggers[row.ID] = row
	return row.ID
}

// --- the incident verbs, one-shot class-26 fault on the per-id arm ------------

func TestTriggerUpdate_StmtCacheRotation_FetchFault_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedV04411Watch(t, store)
	// The pre-fetch (the arm behind the live "failed to fetch trigger"
	// 500s) hits a connection whose cached GetTrigger statement was
	// rotated away server-side; the retry re-prepares and reads the row.
	store.getErrs = []error{errStmtCacheGone}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.getCalls)
	assert.Equal(t, 1, store.updateCalls)

	var updated map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &updated))
	assert.Equal(t, false, updated["enabled"])
}

func TestTriggerDelete_StmtCacheRotation_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedV04411Watch(t, store)
	store.deleteErrs = []error{errStmtCacheGone}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+id, nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.deleteCalls)
	assert.Equal(t, 0, len(store.triggers))
	var deleted map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &deleted))
	assert.Equal(t, true, deleted["deleted"])
}

func TestTriggerListFires_StmtCacheRotation_FetchFault_SingleRetrySucceeds(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedV04411Watch(t, store)
	store.getErrs = []error{errStmtCacheGone}
	r := setupTransientTriggerRouter(t, store, &triggerFaultLogger{})

	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+id+"/fires", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 2, store.getCalls)
}

func TestTriggerUpdate_StmtCacheRotation_Persists_500WithLoggedCause(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedV04411Watch(t, store)
	store.getErrs = []error{errStmtCacheGone, errStmtCacheGone}
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	w := doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Equal(t, 2, store.getCalls) // one retry, then the honest 500

	// Sustained staleness (more than one burned connection in a row) is
	// still a real fault — but a DIAGNOSED one: the 500 arm logs the
	// class-26 cause with the identifying fields.
	require.Equal(t, 1, logger.errorCount())
	entry := logger.errs[0]
	assert.Contains(t, entry, "triggers: fault behind a 5xx")
	assert.Contains(t, entry, "26000")
	assert.Contains(t, entry, "trg-v04411-watch")
}

// --- the incident pin: full per-id round-trip on the live row shape -----------

func TestTriggerV04411RoundTrip_StmtCacheBlips(t *testing.T) {
	store := newFlakyTriggerStore()
	id := seedV04411Watch(t, store)
	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		fired := now.Add(-time.Duration(i+1) * 5 * time.Minute)
		completed := fired.Add(240 * time.Millisecond)
		store.fires = append(store.fires, &wf.TriggerFireRow{
			ID: string(rune('a' + i)), TriggerID: id, SourceType: "cron",
			ActionType: "routine", ActionResult: json.RawMessage(`{"gate":{"proceed":false}}`),
			Status: "skipped", FiredAt: fired, CompletedAt: &completed,
		})
	}
	logger := &triggerFaultLogger{}
	r := setupTransientTriggerRouter(t, store, logger)

	// Fires audit readable (the incident's GET /fires, one-shot class-26
	// blip on the owner-scoped pre-fetch, retried away).
	store.getErrs = []error{errStmtCacheGone}
	w := doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+id+"/fires", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var firesResp struct {
		Fires []map[string]any `json:"fires"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &firesResp))
	require.Len(t, firesResp.Fires, 2)

	// Disable — the verb that 500ed live twice: 200 through the blip.
	store.getErrs = []error{errStmtCacheGone}
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{"enabled": false})
	require.Equal(t, http.StatusOK, w.Code)

	// Delete — the verb that 500ed live twice: 200 through the blip.
	store.deleteErrs = []error{errStmtCacheGone}
	w = doTriggerRequest(t, r, "DELETE", "/api/v1/me/triggers/"+id, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var deleted map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &deleted))
	assert.Equal(t, true, deleted["deleted"])

	// Post-delete: gone.
	w = doTriggerRequest(t, r, "GET", "/api/v1/me/triggers/"+id, nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}
