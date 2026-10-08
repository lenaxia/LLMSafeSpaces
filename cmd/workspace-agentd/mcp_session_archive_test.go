// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- session_archive / delete_session (#1627) ------------------------------

func setupPodSessionEnv(t *testing.T, apiSrv *httptest.Server) {
	t.Helper()
	t.Setenv("WORKSPACE_ID", "ws-1")
	t.Setenv("LLMSAFESPACE_API_URL", apiSrv.URL)
	token := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))
	t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", token)
	// Isolate the archived-set cache between tests (the cache is
	// package-level; a prior test's fetch must not bleed in).
	archivedSetCacheMu.Lock()
	archivedSetCacheVal, archivedSetCacheAt = nil, time.Time{}
	archivedSetCacheMu.Unlock()
}

func TestMCPSessionArchive_HappyPath(t *testing.T) {
	var gotPath, gotBody, gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpSessionArchive(context.Background(), "ses-1", true)
	require.NoError(t, err)
	assert.Equal(t, "/internal/v1/session-archive", gotPath)
	assert.Equal(t, "Bearer sa-token", gotAuth)
	assert.Contains(t, gotBody, `"workspaceID":"ws-1"`)
	assert.Contains(t, gotBody, `"sessionID":"ses-1"`)
	assert.Contains(t, gotBody, `"archived":true`)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "archived", res["status"])
}

func TestMCPSessionArchive_Unarchive(t *testing.T) {
	var gotBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpSessionArchive(context.Background(), "ses-1", false)
	require.NoError(t, err)
	assert.Contains(t, gotBody, `"archived":false`)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "unarchived", res["status"])
}

func TestMCPSessionArchive_RequiresSessionID(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("API must not be called without a session id")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	_, err := mcpSessionArchive(context.Background(), "  ", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session_id")
}

func TestMCPSessionArchive_UnindexedSurfaces404(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found: session ses-ghost"}`))
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	_, err := mcpSessionArchive(context.Background(), "ses-ghost", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "404", "the tool must surface the not-indexed verdict, not mask it")
}

func TestMCPDeleteSession_HappyPath(t *testing.T) {
	var gotPath, gotBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpDeleteSession(context.Background(), mcpTestPassword, "ses-1", "ses-other")
	require.NoError(t, err)
	assert.Equal(t, "/internal/v1/session-delete", gotPath)
	assert.Contains(t, gotBody, `"sessionID":"ses-1"`)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Equal(t, "deleted", res["status"])
}

func TestMCPDeleteSession_RefusesOwnCurrentSession(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the platform must not be asked to delete the caller's own running session")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	_, err := mcpDeleteSession(context.Background(), mcpTestPassword, "ses-me", "ses-me")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "your own current session")
}

// --- session_metadata default change (#1627 ruling 5) ----------------------

func setupMetadataEnv(t *testing.T, platform *httptest.Server) {
	t.Helper()
	t.Setenv("WORKSPACE_ID", "ws-meta")
	if platform != nil {
		t.Setenv("LLMSAFESPACE_API_URL", platform.URL)
		token := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))
		t.Setenv("LLMSAFESPACE_BOOTSTRAP_TOKEN_FILE", token)
	}
	archivedSetCacheMu.Lock()
	archivedSetCacheVal, archivedSetCacheAt = nil, time.Time{}
	archivedSetCacheMu.Unlock()
}

// BREAKING default: omitted session_id now means the CURRENT session
// (the plugin-injected lsp_injected_session), not every session.
func TestMCPSessionMetadata_DefaultIsCurrentSessionOnly(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	f.newSession("two")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", s1, false)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	sessions := res["sessions"].([]any)
	require.Len(t, sessions, 1, "default scope is the CALLING session only — pass all_sessions:true for the old behavior")
	assert.Equal(t, s1, sessions[0].(map[string]any)["session_id"])
}

func TestMCPSessionMetadata_AllSessionsOptIn(t *testing.T) {
	f := newFakeAgent()
	f.newSession("one")
	f.newSession("two")
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", true)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	assert.Len(t, res["sessions"].([]any), 2, "all_sessions:true preserves the old every-session behavior")
}

// Degraded pod (no plugin injection): the busy-session fallback — the
// calling session is the busy one while this tool executes.
func TestMCPSessionMetadata_NoInjectionFallsBackToBusySession(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	f.newSession("two")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", false)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	sessions := res["sessions"].([]any)
	require.Len(t, sessions, 1)
	assert.Equal(t, s1, sessions[0].(map[string]any)["session_id"])
}

func TestMCPSessionMetadata_NoInjectionNoBusy_IsExplicitError(t *testing.T) {
	f := newFakeAgent()
	f.newSession("one")
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)

	_, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session_id")
}

func TestMCPSessionMetadata_ExplicitSessionIDWins(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	f.newSession("two")
	withAgentServer(t, f.handler(t))
	setupMetadataEnv(t, nil)

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, s1, "ses-injected", false)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	sessions := res["sessions"].([]any)
	require.Len(t, sessions, 1)
	assert.Equal(t, s1, sessions[0].(map[string]any)["session_id"], "an explicit session_id overrides the injected current-session default")
}

func TestMCPSessionMetadata_ArchivedAnnotation(t *testing.T) {
	f := newFakeAgent()
	s1 := f.newSession("one")
	s2 := f.newSession("two")
	withAgentServer(t, f.handler(t))
	var mu sync.Mutex
	platformHits := 0
	platform := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		platformHits++
		mu.Unlock()
		assert.Equal(t, "/internal/v1/session-archived", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"archived":["` + s1 + `"]}`))
	}))
	defer platform.Close()
	setupMetadataEnv(t, platform)

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", true)
	require.NoError(t, err)
	var res map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	byID := map[string]map[string]any{}
	for _, s := range res["sessions"].([]any) {
		m := s.(map[string]any)
		byID[m["session_id"].(string)] = m
	}
	assert.Equal(t, true, byID[s1]["archived"], "archived sessions carry the marker (#1627 §4: MCP listings carry archive status)")
	assert.NotContains(t, byID[s2], "archived", "not-archived sessions OMIT the field (ABSENT = not archived)")

	// Cache pin: a second call inside the TTL must not re-fetch.
	out2, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", true)
	require.NoError(t, err)
	assert.NotEmpty(t, out2)
	mu.Lock()
	hits := platformHits
	mu.Unlock()
	assert.Equal(t, 1, hits, "the archived set is cached (~15s TTL) — one fetch per window")
}

func TestMCPSessionMetadata_PlatformUnreachableDegradesSilently(t *testing.T) {
	f := newFakeAgent()
	f.newSession("one")
	withAgentServer(t, f.handler(t))
	// Unreachable platform origin AND no token file: the annotation
	// degrades to absent — metadata stays available (advisory field).
	t.Setenv("WORKSPACE_ID", "ws-meta")
	t.Setenv("LLMSAFESPACE_API_URL", "http://127.0.0.1:1")
	archivedSetCacheMu.Lock()
	archivedSetCacheVal, archivedSetCacheAt = nil, time.Time{}
	archivedSetCacheMu.Unlock()

	out, err := mcpSessionMetadata(context.Background(), mcpTestPassword, "", "", true)
	require.NoError(t, err, "a platform fetch failure must never fail the metadata tool")
	assert.NotContains(t, out, "127.0.0.1", "fetch failure must not leak the origin either")
}

// Review r1 finding 1: an omitted or non-bool `archived` must REFUSE —
// the tool's contract says "no default"; a silent default would
// archive on a malformed call.
func TestMCPSessionArchive_MissingOrNonBoolArchived_Refuses(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the platform must not be called without an explicit boolean")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	_, err := callMCPTool(context.Background(), mcpTestPassword, "session_archive", map[string]any{"session_id": "ses-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived (boolean) is required")

	_, err = callMCPTool(context.Background(), mcpTestPassword, "session_archive", map[string]any{"session_id": "ses-1", "archived": "true"})
	require.Error(t, err, "a string \"true\" must not be accepted")
}

// Review r1 finding 4: on a degraded pod (no injection) the self-delete
// guard falls back to the single busy session — the caller.
func TestMCPDeleteSession_NoInjection_SingleBusyTarget_Refused(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the platform must not be asked to delete the caller's own session")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	f := newFakeAgent()
	s1 := f.newSession("one")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	_, err := mcpDeleteSession(context.Background(), mcpTestPassword, s1, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "your own current session")
}

func TestMCPDeleteSession_NoInjection_OtherTarget_Proceeds(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	f := newFakeAgent()
	s1 := f.newSession("one")
	f.newSession("two")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	out, err := mcpDeleteSession(context.Background(), mcpTestPassword, "ses-two", "")
	require.NoError(t, err)
	assert.Contains(t, out, `"status":"deleted"`)
}

// Review r1 missing-test 3: the plugin's injection-target Set is
// cross-checked against the tools the dispatcher actually reads
// lsp_injected_session for — a typo on either side fails here (the
// silent guard-disabling class).
func TestOriginPlugin_InjectionTargetsMatchDispatcher(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "runtimes", "opencode", "plugins", "llmsafespaces-origin.js"))
	require.NoError(t, err)
	for _, tool := range []string{"llmsafespaces_send_message", "llmsafespaces_session_metadata", "llmsafespaces_delete_session"} {
		assert.Contains(t, string(src), `"`+tool+`"`,
			"the plugin must stamp %s — a typo here silently disables injection (and with it the metadata default + self-delete guard)", tool)
	}
}
