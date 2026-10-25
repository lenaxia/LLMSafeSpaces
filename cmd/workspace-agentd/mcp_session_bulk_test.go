// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// --- session_archive_bulk / session_delete_bulk (#1836 session siblings) ---
//
// The four contract clauses of #1836, pinned at the tool seam the lived
// pain was on (one agent round-trip instead of ~48):
//   1. per-item result array — partial failures per item
//   2. idempotent re-submit (archive: repeat write → done; delete:
//      absence ≡ miss → the same bare not_found, never an error)
//   3. per-item validation — one malformed id sinks only its own item
//   4. batch cap — whole-call refusal, nothing processed

func TestMCPSessionArchiveBulk_MixedBatch(t *testing.T) {
	var mu sync.Mutex
	archived := map[string]bool{}
	var writes int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			writes++
			var body struct {
				SessionID string `json:"sessionID"`
				Archived  bool   `json:"archived"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("bad body: %v", err)
				return
			}
			if body.SessionID == "ses-ghost" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found: session ses-ghost"}`))
				return
			}
			archived[body.SessionID] = body.Archived
			w.WriteHeader(http.StatusNoContent)
		case "/internal/v1/session-archived":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"archived":["ses-1","ses-2"]}`))
		default:
			t.Errorf("unexpected platform path %s", r.URL.Path)
		}
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, []any{"ses-1", "ses-ghost", "ses-2"}, true)
	require.NoError(t, err, "a mixed batch never fails the whole call")
	var res struct {
		Results []struct {
			ID                 string `json:"id"`
			Status             string `json:"status"`
			ArchivedVisibility string `json:"archived_visibility"`
			Error              *struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Results, 3)

	assert.Equal(t, "ses-1", res.Results[0].ID, "the per-item id echoes the caller's ref verbatim")
	assert.Equal(t, "done", res.Results[0].Status)
	assert.Equal(t, "verified", res.Results[0].ArchivedVisibility, "#1879 semantics per item, one shared confirmation read")

	assert.Equal(t, "ses-ghost", res.Results[1].ID)
	assert.Equal(t, "failed", res.Results[1].Status)
	require.NotNil(t, res.Results[1].Error)
	assert.Equal(t, "not_found", res.Results[1].Error.Code)

	assert.Equal(t, "ses-2", res.Results[2].ID)
	assert.Equal(t, "done", res.Results[2].Status)
	assert.Equal(t, "verified", res.Results[2].ArchivedVisibility)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 3, writes, "per-item delegation: one platform write per item, the miss included")
	assert.True(t, archived["ses-1"] && archived["ses-2"])
	assert.NotContains(t, archived, "ses-ghost")
}

func TestMCPSessionArchiveBulk_UnarchiveDirection(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			w.WriteHeader(http.StatusNoContent)
		case "/internal/v1/session-archived":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"archived":[]}`))
		}
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, []any{"ses-1"}, false)
	require.NoError(t, err)
	assert.Contains(t, out, `"status":"done"`, "unarchive rides the same batch form (archived:false)")
	assert.Contains(t, out, `"archived_visibility":"verified"`, "an empty archived set confirms the unarchive")
}

func TestMCPSessionArchiveBulk_MalformedItemIsolation(t *testing.T) {
	var mu sync.Mutex
	var writes int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			writes++
			w.WriteHeader(http.StatusNoContent)
		case "/internal/v1/session-archived":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"archived":[]}`))
		}
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, []any{"ses-1", "   "}, true)
	require.NoError(t, err)
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Results, 2)
	assert.Equal(t, "done", res.Results[0].Status, "the well-formed item still processes")
	assert.Equal(t, "failed", res.Results[1].Status)
	require.NotNil(t, res.Results[1].Error)
	assert.Equal(t, "invalid_input", res.Results[1].Error.Code, "a blank id is that item's own failure")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, writes, "only the well-formed item reached the platform")
}

func TestMCPSessionArchiveBulk_IdempotentResubmit(t *testing.T) {
	var mu sync.Mutex
	var writes int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			writes++
			// The platform seam is idempotent for repeat writes (the
			// root row always matches): 204 again on re-submit.
			w.WriteHeader(http.StatusNoContent)
		case "/internal/v1/session-archived":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"archived":["ses-1","ses-2"]}`))
		}
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	for call := 1; call <= 2; call++ {
		out, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, []any{"ses-1", "ses-2"}, true)
		require.NoError(t, err, "re-submit %d never errors", call)
		var res struct {
			Results []struct {
				Status string `json:"status"`
			} `json:"results"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &res))
		require.Len(t, res.Results, 2)
		for _, r := range res.Results {
			assert.Equal(t, "done", r.Status, "re-submit %d: already-done renders done, not conflict", call)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 4, writes, "each submit writes per item — the platform's repeat-write idempotency carries contract 2")
}

func TestMCPSessionArchiveBulk_UnverifiedWhenConfirmationDisagrees(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			w.WriteHeader(http.StatusNoContent)
		case "/internal/v1/session-archived":
			// The read disagrees (stale set) — the write still holds.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"archived":[]}`))
		}
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, []any{"ses-1"}, true)
	require.NoError(t, err)
	assert.Contains(t, out, `"archived_visibility":"unverified"`,
		"#1879: a disagreeing confirmation read never fails the item and never fabricates freshness")
	assert.Contains(t, out, `"status":"done"`)
}

func TestMCPSessionArchiveBulk_BatchLevelRefusals(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a batch-level refusal must spend zero wire calls")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	_, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, nil, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one")

	tooMany := make([]any, board.BulkMaxItems+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("ses-%d", i)
	}
	_, err = mcpSessionArchiveBulk(context.Background(), mcpTestPassword, tooMany, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("capped at %d", board.BulkMaxItems), "a clear overflow error names the cap")
}

// The REQUIRED DIRECTION discipline is batch-level (one shared boolean
// across the batch — per-item direction would be N single calls): an
// omitted or non-bool `archived` refuses the whole call, exactly the
// single tool's review-r1 rule.
func TestMCPSessionArchiveBulk_MissingDirection_Refuses(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the platform must not be called without an explicit boolean")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	_, err := callMCPTool(context.Background(), mcpTestPassword, "session_archive_bulk", map[string]any{
		"sessionIds": []any{"ses-1"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "archived (boolean) is required")

	_, err = callMCPTool(context.Background(), mcpTestPassword, "session_archive_bulk", map[string]any{
		"sessionIds": []any{"ses-1"},
		"archived":   "true",
	})
	require.Error(t, err, "a string \"true\" must not be accepted")
}

func TestMCPSessionDeleteBulk_MixedBatch(t *testing.T) {
	var mu sync.Mutex
	deleted := map[string]bool{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/internal/v1/session-delete", r.URL.Path)
		var body struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad body: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if body.SessionID == "ses-ghost" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found: session ses-ghost"}`))
			return
		}
		deleted[body.SessionID] = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpDeleteSessionBulk(context.Background(), mcpTestPassword, []any{"ses-1", "ses-ghost"}, "ses-caller")
	require.NoError(t, err)
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Results, 2)
	assert.Equal(t, "done", res.Results[0].Status)
	assert.Equal(t, "ses-ghost", res.Results[1].ID)
	assert.Equal(t, "failed", res.Results[1].Status)
	require.NotNil(t, res.Results[1].Error)
	assert.Equal(t, "not_found", res.Results[1].Error.Code)

	mu.Lock()
	defer mu.Unlock()
	assert.True(t, deleted["ses-1"])
	assert.NotContains(t, deleted, "ses-ghost")
}

// Re-submit of a batch whose items a prior submit already deleted:
// absence ≡ miss — the SAME bare not_found item, never an error,
// never a conflict (the destructive-variant ruling recorded on #1836).
// The 404 here is the REAL wire shape post-r1: the internal delete
// route now classifies the agent-side not-found verdict (both
// authority regimes — pinned platform-side by
// TestPodSessionDelete_AbsenceAdapteRegime_404 / _ActRegime_404)
// instead of flattening every failure to 502.
func TestMCPSessionDeleteBulk_IdempotentResubmit_AbsenceRendersNotFound(t *testing.T) {
	var mu sync.Mutex
	var calls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found: session ses-1"}`))
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	// Re-submit of a batch whose items a prior submit already deleted:
	// absence ≡ miss — the SAME bare not_found item, never an error,
	// never a conflict (the destructive-variant ruling recorded on
	// #1836).
	out, err := mcpDeleteSessionBulk(context.Background(), mcpTestPassword, []any{"ses-1", "ses-2"}, "ses-caller")
	require.NoError(t, err, "the re-submit never fails the call")
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Results, 2)
	for _, r := range res.Results {
		assert.Equal(t, "failed", r.Status)
		require.NotNil(t, r.Error)
		assert.Equal(t, "not_found", r.Error.Code)
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, calls, "each item is still delegated — no fabricated outcome, the platform's 404 is the verdict")
}

func TestMCPSessionDeleteBulk_SelfGuardItemIsolation(t *testing.T) {
	var mu sync.Mutex
	var asks []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad body: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		asks = append(asks, body.SessionID)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpDeleteSessionBulk(context.Background(), mcpTestPassword, []any{"ses-me", "ses-other"}, "ses-me")
	require.NoError(t, err, "the self-target is that ITEM's refusal, not the batch's")
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Code   string `json:"code"`
				Detail string `json:"detail"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Results, 2)
	assert.Equal(t, "failed", res.Results[0].Status)
	require.NotNil(t, res.Results[0].Error)
	assert.Equal(t, "self_delete_refused", res.Results[0].Error.Code)
	assert.Contains(t, res.Results[0].Error.Detail, "your own current session")
	assert.Equal(t, "done", res.Results[1].Status, "the rest of the batch still processes")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"ses-other"}, asks, "the platform is never asked to delete the caller's own session")
}

func TestMCPSessionDeleteBulk_NoInjectionFallsBackToSingleBusy(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the platform must not be asked to delete the caller's own session")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	f := newFakeAgent()
	s1 := f.newSession("one")
	f.busySet[s1] = true
	withAgentServer(t, f.handler(t))

	_, err := mcpDeleteSessionBulk(context.Background(), mcpTestPassword, []any{s1}, "")
	require.NoError(t, err, "the refusal is per-item")
}

func TestMCPSessionDeleteBulk_MalformedItemAndCap(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a batch-level refusal must spend zero wire calls")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	_, err := mcpDeleteSessionBulk(context.Background(), mcpTestPassword, []any{}, "ses-caller")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at least one")

	tooMany := make([]any, board.BulkMaxItems+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("ses-%d", i)
	}
	_, err = mcpDeleteSessionBulk(context.Background(), mcpTestPassword, tooMany, "ses-caller")
	require.Error(t, err)
	assert.Contains(t, err.Error(), fmt.Sprintf("capped at %d", board.BulkMaxItems))
}

// Phrase-form refs resolve per item (the #1831/#1930 dual-accept —
// "phrase-id and raw-id both accepted per item where the single-shot
// tools do"), and the per-item id echoes the caller's ref VERBATIM
// while the platform receives the canonical id.
func TestMCPSessionArchiveBulk_PhraseRefResolvesPerItem(t *testing.T) {
	f := newFakeAgent()
	canonical := f.newSession("bulk target")
	withAgentServer(t, f.handler(t))

	var mu sync.Mutex
	var gotID string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			var body struct {
				SessionID string `json:"sessionID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("bad body: %v", err)
				return
			}
			mu.Lock()
			gotID = body.SessionID
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case "/internal/v1/session-archived":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"archived":[]}`))
		}
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	// Prime the short-id replica: the harness id must map to a phrase
	// for resolution to exercise the short-id arm.
	phrase := "copper-lime-weaver"
	store := withShortIDStore(t)
	store.put(phrase, canonical)

	out, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, []any{phrase}, true)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, canonical, gotID, "the platform receives the CANONICAL harness id, not the phrase")
	assert.Contains(t, out, `"id":"`+phrase+`"`, "the per-item id echoes the caller's phrase verbatim")
	assert.Contains(t, out, `"status":"done"`)
}

func TestMCPSessionBulk_NonStringEntryIsItemFailure(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a non-string entry must never reach the platform")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_delete_bulk", map[string]any{
		"sessionIds":           []any{42},
		"lsp_injected_session": "ses-caller",
	})
	require.NoError(t, err, "a malformed entry is per-item, not call-level")
	assert.Contains(t, out, `"status":"failed"`, "the non-string entry renders its own failed item")
	assert.Contains(t, out, "invalid_input")
}

func TestMCPSessionBulk_DeprecatedRefNoticed(t *testing.T) {
	// The #1816 §2 envelope carrier must scan ARRAY args too: a
	// deprecated-form ref inside sessionIds gets the one-release notice.
	note := sessionArgsDeprecationNote(map[string]any{
		"sessionIds": []any{"ses_fullhashid123456789"},
	})
	assert.NotEmpty(t, note, "a deprecated-form ref inside the bulk array carries the notice")
	assert.Empty(t, sessionArgsDeprecationNote(map[string]any{
		"sessionIds": []any{"copper-lime-weaver"},
	}), "clean refs carry no notice")
}

func TestMCPRegistry_SessionBulkToolsListed(t *testing.T) {
	req, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	w := httptest.NewRecorder()
	mcpHandler(mcpTestPassword)(w, mcpAuthedRequest(req))
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	tools := resp.Result.(map[string]any)["tools"].([]any)

	descs := map[string]bool{}
	schemaDescs := map[string]string{} // "tool/property" -> property description
	for _, tool := range tools {
		tt := tool.(map[string]any)
		descs[tt["name"].(string)] = true
		if schema, ok := tt["inputSchema"].(map[string]any); ok {
			if props, ok := schema["properties"].(map[string]any); ok {
				for prop, raw := range props {
					if pm, ok := raw.(map[string]any); ok {
						if pd, ok := pm["description"].(string); ok {
							schemaDescs[tt["name"].(string)+"/"+prop] = pd
						}
					}
				}
			}
		}
	}
	for _, name := range []string{"session_archive_bulk", "session_delete_bulk"} {
		require.True(t, descs[name], "%s in tools/list", name)
	}
	sd, ok := schemaDescs["session_archive_bulk/archived"]
	require.True(t, ok, "session_archive_bulk/archived schema description present")
	assert.Contains(t, sd, "REQUIRED DIRECTION", "the no-default direction discipline is schema-visible")
	sdIDs, ok := schemaDescs["session_archive_bulk/sessionIds"]
	require.True(t, ok)
	assert.Contains(t, sdIDs, "50", "the cap is schema-visible")
}

// The plugin must stamp the delete bulk too (the self-delete guard's
// injection half) — the TestOriginPlugin_InjectionTargetsMatchDispatcher
// discipline extended to the bulk tool.
func TestOriginPlugin_InjectionTargetsIncludeDeleteBulk(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "runtimes", "opencode", "plugins", "llmsafespaces-origin.js"))
	require.NoError(t, err)
	assert.Contains(t, string(src), `"llmsafespaces_session_delete_bulk"`,
		"the plugin must stamp the delete bulk — without injection the self-guard degrades to the busy fallback")
}

func TestCallMCPTool_SessionDeleteBulk_DispatcherReadsInjection(t *testing.T) {
	var mu sync.Mutex
	var asks []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("bad body: %v", err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		asks = append(asks, body.SessionID)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_delete_bulk", map[string]any{
		"sessionIds":           []any{"ses-me", "ses-other"},
		"lsp_injected_session": "ses-me",
	})
	require.NoError(t, err, "the self-item refuses; the batch returns its per-item array")
	assert.Contains(t, out, "self_delete_refused", "the dispatcher must feed the injection into the self-guard")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"ses-other"}, asks, "only the non-self item reaches the platform")
}

// --- review round 1 pins ----------------------------------------------------

// F1's other half: a REAL platform failure (502 — the agent-side
// delete failed for transport/harness reasons, the class the internal
// route honestly keeps) renders internal_error, NEVER a fabricated
// not_found — a destructive batch must not report "row gone" out of an
// infrastructure blip (the retry-never-converges inversion r1 found in
// the pre-classification wire).
func TestMCPSessionDeleteBulk_RealFailure_IsInternalErrorNotNotFound(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"failed to delete session"}`))
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := mcpDeleteSessionBulk(context.Background(), mcpTestPassword, []any{"ses-1"}, "ses-caller")
	require.NoError(t, err, "the failure is per-item, not call-level")
	var res struct {
		Results []struct {
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Results, 1)
	assert.Equal(t, "failed", res.Results[0].Status)
	require.NotNil(t, res.Results[0].Error)
	assert.Equal(t, "internal_error", res.Results[0].Error.Code,
		"the transient class keeps its honest code — retry converges once the failure clears")
}

// F2: malformed entries echo DISTINGUISHABLE ids — [42, blank, 3.5]
// renders three failed rows the caller can tell apart (the pre-fix
// ""-echo made them indistinguishable).
func TestMCPSessionBulk_MalformedEntriesEchoDistinguishableIDs(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a malformed entry must never reach the platform")
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	out, err := callMCPTool(context.Background(), mcpTestPassword, "session_delete_bulk", map[string]any{
		"sessionIds":           []any{42, "   ", 3.5},
		"lsp_injected_session": "ses-caller",
	})
	require.NoError(t, err)
	var res struct {
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  *struct {
				Code string `json:"code"`
			} `json:"error"`
		} `json:"results"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &res))
	require.Len(t, res.Results, 3)
	for i, want := range []string{"42", "   ", "3.5"} {
		assert.Equal(t, want, res.Results[i].ID, "entry %d echoes distinguishably", i)
		assert.Equal(t, "failed", res.Results[i].Status)
		assert.Equal(t, "invalid_input", res.Results[i].Error.Code)
	}
}

// The r1 cost note: the near-miss clause source fetches the session
// list ONCE per bulk call, not once per missing item — a many-miss
// batch costs one list read total (the singles pay one per call).
func TestMCPSessionArchiveBulk_SuggesterOneFetchPerCall(t *testing.T) {
	f := newFakeAgent()
	f.newSession("one")
	f.newSession("two")
	listHits := 0
	srv := withAgentServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/session") {
			listHits++
		}
		f.handler(t).ServeHTTP(w, r)
	}))
	_ = srv

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/v1/session-archive":
			// The platform's classified absence shape (post-r1): every
			// item misses.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found: session"}`))
		case "/internal/v1/session-archived":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"archived":[]}`))
		}
	}))
	defer api.Close()
	setupPodSessionEnv(t, api)

	refs := make([]any, 5)
	for i := range refs {
		// Legacy ses_-form refs resolve as zero-fetch passthroughs, so
		// the ONLY list reads left are the clause source's — the thing
		// this pin isolates.
		refs[i] = fmt.Sprintf("ses_miss_%d", i)
	}
	out, err := mcpSessionArchiveBulk(context.Background(), mcpTestPassword, refs, true)
	require.NoError(t, err)
	assert.Contains(t, out, `"not_found"`)

	assert.Equal(t, 1, listHits,
		"five misses share ONE clause-source list fetch — never five (the r1 cost note)")
}
