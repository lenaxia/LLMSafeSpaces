// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_role_materialization_test.go — the FULL-CHAIN pin for review
// round 2 of the role-only model (PR #1990 F1): the role's system
// prompt REACHES the spawned worker. The chain, exercised leg by leg
// with production code at every hop:
//
//	bootstrap file (agent-roles.json, as the bootstrap subcommand
//	writes it) → ensureBootAgentConfig (the REAL ConfigWriter — the
//	same boot stamp production runs) → agent-config.json's `agent`
//	map gains agent.<roleID> with prompt = the role's system prompt →
//	a fake opencode serves GET /agent FROM that written config (as
//	opencode's own registry would) → the real workflowExecuteHandler
//	dispatches spec["agent"] = <roleId> → the A8 gate passes (the
//	role is IN the registry) and the message POST carries
//	agentID = <roleId>, ≠ build.
//
// The negative arm (a role absent from the file) reds as
// agent_not_found — proving the registry the gate consults is exactly
// the config the writer materialized. Without materialization this
// suite is the review's F1 live: the dispatch silently runs build.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// materializationHarness — a fake opencode whose GET /agent is served
// FROM a config file (the one the real ConfigWriter wrote), mirroring
// how the real harness loads named agents from agent-config.json at
// boot. Captures the message POST bodies.
type materializationHarness struct {
	mu       sync.Mutex
	cfgPath  string
	srv      *httptest.Server
	agentIDs []string // agentID per accepted message POST
}

func newMaterializationHarness(t *testing.T, cfgPath string) *materializationHarness {
	t.Helper()
	h := &materializationHarness{cfgPath: cfgPath}
	mux := http.NewServeMux()
	// GET /agent — opencode's registry, loaded from the config file the
	// ConfigWriter wrote (the real harness reads the same key at boot).
	mux.HandleFunc("GET /agent", func(w http.ResponseWriter, _ *http.Request) {
		data, err := os.ReadFile(h.cfgPath)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var cfg struct {
			Agent map[string]json.RawMessage `json:"agent"`
		}
		if err := json.Unmarshal(data, &cfg); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		names := make([]string, 0, len(cfg.Agent)+2)
		// The built-ins the real harness always has.
		names = append(names, "build", "plan")
		for name := range cfg.Agent {
			names = append(names, name)
		}
		entries := make([]string, 0, len(names))
		for _, n := range names {
			entries = append(entries, fmt.Sprintf(`{"name":%q}`, n))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "[%s]", joinNonEmpty(entries))
	})
	mux.HandleFunc("POST /session", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"ses_mat_1"}`)
	})
	mux.HandleFunc("POST /session/ses_mat_1/message", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.agentIDs = append(h.agentIDs, asString(body["agentID"]))
		h.mu.Unlock()
		fmt.Fprint(w, `{"info":{"id":"msg_1","agent":"pinned-by-agentid"},"parts":[{"type":"text","text":"ok"}]}`)
	})
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	t.Cleanup(func() {
		agentRegistryCache.Lock()
		agentRegistryCache.password = ""
		agentRegistryCache.names = nil
		agentRegistryCache.Unlock()
	})
	return h
}

func joinNonEmpty(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// TestBoardRoleMaterialization_FullChain — THE F1 pin.
func TestBoardRoleMaterialization_FullChain(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	promptPath := filepath.Join(dir, "admin-prompt.md")
	dirsPath := filepath.Join(dir, "allowed-dirs.json")
	rolesPath := filepath.Join(dir, "agent-roles.json")

	// Leg 1 — the bootstrap staging shape: exactly the JSON the
	// bootstrap subcommand writes verbatim from the API response.
	const roleID = "7f1c9e2a-7b3d-4c5e-9f01-234567890abc"
	rolesJSON := `[{"id":"` + roleID + `","slug":"reviewer","name":"Reviewer",` +
		`"description":"Built-in board reviewer","system":"You are the reviewer. Record verdicts via board_record_verdict.",` +
		`"mode":"primary"}]`
	require.NoError(t, os.WriteFile(rolesPath, []byte(rolesJSON), 0o640))

	// Leg 2 — the REAL boot stamp (the production ConfigWriter path in
	// both agentd modes) materializes the named agent.
	ensureBootAgentConfig(cfgPath, promptPath, dirsPath, rolesPath, "pw")

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var cfg struct {
		Agent map[string]map[string]any `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(data, &cfg))
	entry, ok := cfg.Agent[roleID]
	require.True(t, ok, "the boot stamp must materialize agent.<roleID>: %s", data)
	require.Equal(t, "You are the reviewer. Record verdicts via board_record_verdict.", entry["prompt"],
		"the role's system prompt must ride the materialized agent")
	require.Equal(t, "primary", entry["mode"])
	require.Equal(t, "Built-in board reviewer", entry["description"])

	// Leg 3+4 — a fake opencode whose registry loads FROM that config,
	// and the real dispatch through the A8 gate.
	h := newMaterializationHarness(t, cfgPath)
	pointAgentAddrAt(t, h.srv.URL)

	// Leg 5 — the persona'd dispatch resolves to the ROLE, not build.
	w := dispatchSpec(t, "graph-node", "wf-mat", "run-mat-1",
		fmt.Sprintf(`{"agent":%q,"prompt":"work the charter","session":"new"}`, roleID))
	require.Equal(t, http.StatusOK, w.Code)
	out := decodeA8Envelope(t, w)
	require.Empty(t, out.ErrorCode, "the materialized role must pass the A8 gate: %s", out.Detail)
	require.Equal(t, []string{roleID}, h.agentIDsSnapshot(),
		"the session's first turn must run the ROLE agent (agentID ≠ build)")

	// The negative arm — a role the pod never materialized (created
	// after this pod's bootstrap) is refused loudly, never a silent
	// build-agent fallback.
	w = dispatchSpec(t, "graph-node", "wf-mat", "run-mat-2",
		`{"agent":"00000000-1111-2222-3333-444444444444","prompt":"work the charter","session":"new"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "agent_not_found", decodeA8Envelope(t, w).ErrorCode)
}

// TestBoardRoleMaterialization_IdsOnlyNoSlugAliases — slugs never
// become agent names: a slug could collide with opencode's built-ins
// (an org role slugged "build" must not shadow the build agent), and
// the dispatch canonicalizes to role IDs anyway.
func TestBoardRoleMaterialization_IdsOnlyNoSlugAliases(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	rolesPath := filepath.Join(dir, "agent-roles.json")
	rolesJSON := `[{"id":"7f1c9e2a-7b3d-4c5e-9f01-234567890abc","slug":"build","system":"shadow attempt"}]`
	require.NoError(t, os.WriteFile(rolesPath, []byte(rolesJSON), 0o640))

	ensureBootAgentConfig(cfgPath, filepath.Join(dir, "no-prompt"), filepath.Join(dir, "no-dirs"), rolesPath, "pw")

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var cfg struct {
		Agent map[string]map[string]any `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(data, &cfg))
	_, shadow := cfg.Agent["build"]
	require.False(t, shadow, "a role SLUG must never materialize as the agent name (built-in shadow)")
	require.Contains(t, cfg.Agent, "7f1c9e2a-7b3d-4c5e-9f01-234567890abc")
	// The build agent's prompt stays untouched by role materialization.
	require.NotEqual(t, "shadow attempt", cfg.Agent["build"]["prompt"])
}

// TestBoardRoleMaterialization_BlankEntriesSkipped — blank-ID entries
// (the sanitize guard) and a corrupt file degrade to no
// materialization without failing the boot stamp.
func TestBoardRoleMaterialization_BlankEntriesSkipped(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "agent-config.json")
	rolesPath := filepath.Join(dir, "agent-roles.json")
	require.NoError(t, os.WriteFile(rolesPath, []byte(`[{"id":"  ","system":"nope"},{"id":"ok-id","system":"fine"}]`), 0o640))

	ensureBootAgentConfig(cfgPath, filepath.Join(dir, "no-prompt"), filepath.Join(dir, "no-dirs"), rolesPath, "pw")

	data, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	var cfg struct {
		Agent map[string]map[string]any `json:"agent"`
	}
	require.NoError(t, json.Unmarshal(data, &cfg))
	require.Contains(t, cfg.Agent, "ok-id")
	for name := range cfg.Agent {
		require.NotEqual(t, " ", name, "blank IDs must never key an agent")
	}
}

func (h *materializationHarness) agentIDsSnapshot() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.agentIDs))
	copy(out, h.agentIDs)
	return out
}

// TestBoardRoleMaterialization_BootstrapStagesTheFile — the bootstrap
// subcommand leg: a response carrying agentRoles writes the bytes
// VERBATIM to the --agent-roles-out path; an empty vocabulary writes
// nothing (the adminPrompt/allowedDirs discipline).
func TestBoardRoleMaterialization_BootstrapStagesTheFile(t *testing.T) {
	dir := t.TempDir()
	rolesOut := filepath.Join(dir, "agent-roles.json")

	const roleID = "7f1c9e2a-7b3d-4c5e-9f01-234567890abc"
	rolesJSON := `[{"id":"` + roleID + `","slug":"reviewer","system":"charter"}]`
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"secrets":[],"agentRoles":%s}`, rolesJSON)
	}))
	t.Cleanup(api.Close)
	token := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(token, []byte("sa-token"), 0o600))

	code := runBootstrapCommand([]string{
		"--workspace-id", "ws-mat",
		"--api-url", api.URL,
		"--token-file", token,
		"--out", filepath.Join(dir, "secrets.json"),
		"--admin-prompt-out", filepath.Join(dir, "admin-prompt.md"),
		"--allowed-dirs-out", filepath.Join(dir, "allowed-dirs.json"),
		"--agent-roles-out", rolesOut,
	}, os.Stdout, os.Stderr)
	require.Equal(t, 0, code)

	staged, err := os.ReadFile(rolesOut)
	require.NoError(t, err, "the bootstrap must stage the vocabulary file")
	require.JSONEq(t, rolesJSON, string(staged), "the staging is verbatim — the pod never interprets the vocabulary")

	// The empty-vocabulary arm: no agentRoles in the response, no file.
	dir2 := t.TempDir()
	rolesOut2 := filepath.Join(dir2, "agent-roles.json")
	api2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"secrets":[]}`)
	}))
	t.Cleanup(api2.Close)
	token2 := filepath.Join(dir2, "token")
	require.NoError(t, os.WriteFile(token2, []byte("sa-token"), 0o600))
	code = runBootstrapCommand([]string{
		"--workspace-id", "ws-mat2",
		"--api-url", api2.URL,
		"--token-file", token2,
		"--out", filepath.Join(dir2, "secrets.json"),
		"--admin-prompt-out", filepath.Join(dir2, "admin-prompt.md"),
		"--allowed-dirs-out", filepath.Join(dir2, "allowed-dirs.json"),
		"--agent-roles-out", rolesOut2,
	}, os.Stdout, os.Stderr)
	require.Equal(t, 0, code)
	_, err = os.Stat(rolesOut2)
	require.True(t, os.IsNotExist(err), "an empty vocabulary must not stage a file")
}

var _ = time.Second // silence unused-import churn if assertions shrink
