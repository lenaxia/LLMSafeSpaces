// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	wf "github.com/lenaxia/llmsafespaces/pkg/workflows"
)

func wfRowOwned(id string) *wf.WorkflowRow {
	return &wf.WorkflowRow{ID: id, OwnerType: "user", OwnerID: "test-user"}
}

// #1812: the trigger's inline gate script — create/update validation.
// Every rejection must be self-teaching: name the field, the expected
// shape, and the rule.

func TestTriggerCreate_ScriptRoutineTarget(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "gated-nightly",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "Report on {{topic}}",
		"script": map[string]any{
			"language": "python",
			"handler":  "def handler(input):\n    return {\"proceed\": True, \"vars\": {\"topic\": input[\"source\"][\"id\"]}}\n",
		},
	})
	require.Equal(t, 201, w.Code)

	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	script, ok := resp["script"].(map[string]any)
	require.True(t, ok, "response must carry the script object, got %v", resp["script"])
	assert.Equal(t, "python", script["language"])
	assert.Contains(t, script["handler"], "def handler")

	row := store.triggers[resp["id"].(string)]
	assert.Equal(t, "python", row.ScriptLanguage)
	assert.Contains(t, row.ScriptHandler, "def handler")
}

func TestTriggerCreate_ScriptWithWorkflowTargetRejected(t *testing.T) {
	store := newMockTriggerStore()
	store.workflows["wf_123"] = wfRowOwned("wf_123")
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "gated-dag",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workflowId":   "wf_123",
		"script":       map[string]any{"language": "python", "handler": "def handler(input):\n    return {\"proceed\": True}\n"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "script")
	assert.Contains(t, w.Body.String(), "routine")
}

func TestTriggerCreate_ScriptBadLanguage(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "gated-bad-lang",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "p",
		"script":       map[string]any{"language": "bash", "handler": "echo hi"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "language")
	assert.Contains(t, w.Body.String(), "python, node")
}

func TestTriggerCreate_ScriptPartialRejected(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "gated-half",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "p",
		"script":       map[string]any{"language": "python"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "handler")

	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "gated-other-half",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "p",
		"script":       map[string]any{"handler": "def handler(input): pass"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "language")
}

// r2 review Finding 2: a present script object that binds NEITHER
// recognized field (empty {}, or only misspelled keys) must 400 — not
// silently store no script (create) or erase the stored one (update).
func TestTriggerCreate_ScriptEmptyAndTypoObjectsRejected(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "gated-empty", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "p",
		"script": map[string]any{},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "language")
	assert.Contains(t, w.Body.String(), "handler")

	w = doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name": "gated-typo", "sourceType": "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1", "prompt": "p",
		"script": map[string]any{"languagee": "python", "handlr": "def handler(input): pass"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "misspell")
}

func TestTriggerUpdate_ScriptLifecycle(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "gate-lifecycle",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "p",
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	id := created["id"].(string)

	// Absent key keeps the no-script state.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{
		"description": "no script touch",
	})
	require.Equal(t, 200, w.Code)
	if store.triggers[id].ScriptLanguage != "" || store.triggers[id].ScriptHandler != "" {
		t.Fatalf("absent script key must keep the empty (no-script) state")
	}

	// Object replaces.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{
		"script": map[string]any{"language": "node", "handler": "exports.handler = (input) => ({proceed: true, vars: {}});"},
	})
	require.Equal(t, 200, w.Code)
	var resp map[string]any
	json.Unmarshal(w.Body.Bytes(), &resp)
	script, ok := resp["script"].(map[string]any)
	require.True(t, ok, "update response must carry the script")
	assert.Equal(t, "node", script["language"])
	assert.Equal(t, "node", store.triggers[id].ScriptLanguage)
	// JSON null clears.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{
		"script": nil,
	})
	require.Equal(t, 200, w.Code)
	if store.triggers[id].ScriptLanguage != "" || store.triggers[id].ScriptHandler != "" {
		t.Fatalf("JSON null must clear the script back to the no-script state")
	}
	var cleared map[string]any
	json.Unmarshal(w.Body.Bytes(), &cleared)
	if cleared["script"] != nil {
		t.Fatalf("cleared script must not appear in the response, got %v", cleared["script"])
	}
}

// r2 review Finding 2 (update arm): {} and typo-keyed script objects
// must 400 instead of erasing the stored script with a 200.
func TestTriggerUpdate_ScriptEmptyAndTypoObjectsRejected(t *testing.T) {
	store := newMockTriggerStore()
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "gate-erase-guard",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "p",
		"script":       map[string]any{"language": "python", "handler": "def handler(input):\n    return {\"proceed\": True}\n"},
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	id := created["id"].(string)

	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{
		"script": map[string]any{},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "misspell")
	assert.Equal(t, "python", store.triggers[id].ScriptLanguage, "the empty object must not erase the stored script")

	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{
		"script": map[string]any{"languagee": "python", "handlr": "x"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "misspell")
	assert.Equal(t, "python", store.triggers[id].ScriptLanguage, "the typo object must not erase the stored script")
	assert.Contains(t, store.triggers[id].ScriptHandler, "def handler")
}

func TestTriggerUpdate_ScriptOnWorkflowTargetRejected(t *testing.T) {
	store := newMockTriggerStore()
	store.workflows["wf_123"] = wfRowOwned("wf_123")
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "dag-target",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workflowId":   "wf_123",
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	id := created["id"].(string)

	// Patching a script onto a workflow-targeted trigger: rejected on the
	// post-patch merged view.
	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{
		"script": map[string]any{"language": "python", "handler": "def handler(input):\n    return {\"proceed\": True}\n"},
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "script")
}

func TestTriggerUpdate_WorkflowTargetPatchOnScriptTriggerRejected(t *testing.T) {
	store := newMockTriggerStore()
	store.workflows["wf_123"] = wfRowOwned("wf_123")
	quota := &mockQuotaChecker{values: map[string]int{}}
	encrypt := &mockEncryptor{}
	r := setupTriggerRouter(t, store, quota, encrypt)

	w := doTriggerRequest(t, r, "POST", "/api/v1/me/triggers", map[string]any{
		"name":         "scripted-routine",
		"sourceType":   "cron",
		"sourceConfig": map[string]any{"expr": "0 2 * * *", "tz": "UTC"},
		"workspaceId":  "ws-1",
		"prompt":       "p",
		"script":       map[string]any{"language": "python", "handler": "def handler(input):\n    return {\"proceed\": True}\n"},
	})
	require.Equal(t, 201, w.Code)
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	id := created["id"].(string)

	w = doTriggerRequest(t, r, "PUT", "/api/v1/me/triggers/"+id, map[string]any{
		"workflowId": "wf_123",
	})
	require.Equal(t, 400, w.Code)
	assert.Contains(t, w.Body.String(), "script")
}
