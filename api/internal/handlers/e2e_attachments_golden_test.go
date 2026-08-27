// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
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
)

// TestE2E_PromptFilesGoldenBytes is e2e row E7 (Epic 68): the prompt
// dispatched to the workspace agent must byte-match the authoritative
// golden fixtures in pkg/session/attachments/testdata/ — the wire-level
// lock on manifest format v1 across releases. The stub agent (httptest
// backend, same harness as the adapter e2e suite) captures every
// dispatched body; each fixture pair drives a real POST /prompt through
// the router and asserts exact bytes.
func TestE2E_PromptFilesGoldenBytes(t *testing.T) {
	fixtures := []string{
		"compose_one_file",
		"compose_three_files",
		"compose_strip_existing_block",
		"compose_trailing_newlines_3",
	}

	const fixtureDir = "../../../pkg/session/attachments/testdata"

	var mu sync.Mutex
	var dispatched []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message") {
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			dispatched = append(dispatched, firstPartText(t, body))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"info":{"role":"assistant","id":"msg_1"},"parts":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(backend.Close)
	env := newE2EEnv(t, backend)

	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			inRaw, err := os.ReadFile(filepath.Join(fixtureDir, name+".in.json"))
			require.NoError(t, err, "fixture %s", name)
			wantRaw, err := os.ReadFile(filepath.Join(fixtureDir, name+".want.json"))
			require.NoError(t, err, "fixture %s", name)

			var in struct {
				Text  string   `json:"text"`
				Files []string `json:"files"`
			}
			require.NoError(t, json.Unmarshal(inRaw, &in), "fixture %s: input must be a compose-case pair", name)
			var want string
			require.NoError(t, json.Unmarshal(wantRaw, &want))

			before := len(dispatched)
			body := fmt.Sprintf(`{"parts":[{"type":"text","text":%s}],"files":%s}`,
				mustJSONString(t, in.Text), mustJSONArray(t, in.Files))
			w := env.do(http.MethodPost, "/api/v1/workspaces/ws-1/sessions/ses_1/prompt", strings.NewReader(body))
			require.Equal(t, http.StatusOK, w.Code, "prompt accepted: %s", w.Body.String())

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, dispatched, before+1, "exactly one dispatch per prompt")
			assert.Equal(t, want, dispatched[before], "dispatched prompt must byte-match golden fixture %s", name)
		})
	}
}

func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	require.NoError(t, err)
	return string(raw)
}

func mustJSONArray(t *testing.T, files []string) string {
	t.Helper()
	if files == nil {
		files = []string{}
	}
	raw, err := json.Marshal(files)
	require.NoError(t, err)
	return string(raw)
}
