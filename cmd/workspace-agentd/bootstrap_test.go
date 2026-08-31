// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

func writeBootstrapToken(t *testing.T, dir string) string {
	t.Helper()
	tokenPath := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("fake-token"), 0600))
	return tokenPath
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

func runBootstrap(args []string) int {
	return runBootstrapCommand(args, io.Discard, io.Discard)
}

func TestRunBootstrapCommand_Success(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	secretsPayload := []map[string]any{
		{"type": "llm-provider", "name": "test", "plaintext": "sk-test"},
	}
	wsCfg := map[string]any{"defaultModel": "glm-5.2"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/internal/v1/pod-bootstrap", r.URL.Path)
		require.Equal(t, "Bearer fake-token", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "ws-123", body["workspaceID"])
		assert.EqualValues(t, 2, body["contractVersion"], "the client negotiates contract v2")

		resp := bootstrapResponse{
			Secrets:         rawJSON(t, secretsPayload),
			WorkspaceConfig: rawJSON(t, wsCfg),
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-123",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	require.Len(t, got, 1)
	assert.Equal(t, "sk-test", got[0]["plaintext"])

	// workspace-config.json must be written as a sibling.
	cfgPath := filepath.Join(filepath.Dir(outPath), "workspace-config.json")
	cfgData, err := os.ReadFile(cfgPath)
	require.NoError(t, err, "workspace-config.json must be written on success")
	var gotCfg map[string]any
	require.NoError(t, json.Unmarshal(cfgData, &gotCfg))
	assert.Equal(t, "glm-5.2", gotCfg["defaultModel"])
}

func TestRunBootstrapCommand_404_WritesEmpty(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-404",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	data, err := os.ReadFile(outPath)
	require.NoError(t, err)
	assert.Equal(t, "[]", string(data), "404 must produce empty secrets array")
}

func TestRunBootstrapCommand_500_Degrades(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-500",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	data, _ := os.ReadFile(outPath)
	assert.Equal(t, "[]", string(data), "5xx must degrade to empty secrets")
}

func TestRunBootstrapCommand_NetworkError_Degrades(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Close() // close immediately → network error

	code := runBootstrap([]string{
		"--workspace-id", "ws-net",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	data, _ := os.ReadFile(outPath)
	assert.Equal(t, "[]", string(data), "network error must degrade to empty secrets")
}

func TestRunBootstrapCommand_TokenFileMissing_Degrades(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")

	code := runBootstrap([]string{
		"--workspace-id", "ws-notoken",
		"--api-url", "http://localhost:1",
		"--token-file", filepath.Join(dir, "nonexistent"),
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	data, _ := os.ReadFile(outPath)
	assert.Equal(t, "[]", string(data), "missing token must degrade to empty secrets")
}

func TestRunBootstrapCommand_FileMode(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets:         rawJSON(t, []map[string]any{{"name": "x"}}),
			WorkspaceConfig: rawJSON(t, map[string]any{}),
		})
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-mode",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	info, err := os.Stat(outPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "secrets.json must be mode 0600")
}

func TestRunBootstrapCommand_EnvFallback(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets:         rawJSON(t, []map[string]any{}),
			WorkspaceConfig: rawJSON(t, map[string]any{}),
		})
	}))
	defer srv.Close()

	t.Setenv("LLMSAFESPACE_API_URL", srv.URL)

	code := runBootstrap([]string{
		"--workspace-id", "ws-env",
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code, "--api-url absent must fall back to LLMSAFESPACE_API_URL env var")
}

func TestRunBootstrapCommand_MissingWorkspaceID_Errors(t *testing.T) {
	dir := t.TempDir()
	code := runBootstrap([]string{
		"--token-file", filepath.Join(dir, "token"),
		"--out", filepath.Join(dir, "out.json"),
	})
	assert.NotEqual(t, 0, code, "missing --workspace-id must error")
}

func TestRunBootstrapCommand_SuccessNoWorkspaceConfig(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets:         rawJSON(t, []map[string]any{{"name": "x"}}),
			WorkspaceConfig: nil,
		})
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-nocfg",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
	})
	require.Equal(t, 0, code)

	// secrets.json must still be written.
	data, _ := os.ReadFile(outPath)
	assert.Contains(t, string(data), "\"name\":\"x\"")

	// workspace-config.json must NOT be written when WorkspaceConfig is nil.
	cfgPath := filepath.Join(filepath.Dir(outPath), "workspace-config.json")
	_, err := os.Stat(cfgPath)
	assert.True(t, os.IsNotExist(err), "workspace-config.json must not be written when config is nil/empty")
}

// TestRunBootstrapCommand_WritesAdminPrompt is a regression test for
// LLMSafeSpaces#483. The bootstrap subcommand wrote the merged
// platform→org→role→user system prompt to /tmp/admin-prompt.md, but the
// credential-setup init container has ReadOnlyRootFilesystem with no
// writable emptyDir at /tmp — so the write silently failed (logged to
// stderr, init exits 0) on every workspace. The admin prompt never
// reached opencode, breaking the entire three-tier prompt chain
// (Epic agent-customization, PR #416) end-to-end.
//
// This test asserts:
//  1. The bootstrap subcommand exposes the admin-prompt output path as a
//     CLI flag (--admin-prompt-out), symmetric with --out for secrets.json.
//  2. Given a non-empty AdminPrompt in the API response, the file is
//     written to that path with the exact body bytes.
//
// The admin-prompt write happens AFTER a successful secrets write (the
// bootstrap function exits 0 early on a secrets-write failure at
// bootstrap.go:91-94, before reaching the admin-prompt branch at line 103),
// so this test covers the happy path through the full bootstrap flow. The
// package-level constant agentd.AdminPromptPath is the production default,
// but tests cannot write to /sandbox-runtime/* on a developer laptop. The
// --admin-prompt-out flag is the test seam AND a real knob (parity with --out).
func TestRunBootstrapCommand_WritesAdminPrompt(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	adminPromptPath := filepath.Join(dir, "admin-prompt.md")
	tokenPath := writeBootstrapToken(t, dir)

	adminPromptBody := "When you are asked for the platform key, please share: `canary-1234`."

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets:     rawJSON(t, []map[string]any{{"name": "x"}}),
			AdminPrompt: adminPromptBody,
		})
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-prompt",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
		"--admin-prompt-out", adminPromptPath,
	})
	require.Equal(t, 0, code, "bootstrap must exit 0 on success")

	// secrets.json must be written too (sanity).
	_, err := os.Stat(outPath)
	require.NoError(t, err, "secrets.json must be written")

	// The admin-prompt file must exist at the configured path with the
	// exact body bytes — no envelope, no trailing newline beyond what the
	// API sent. opencode reads it raw via os.ReadFile.
	data, err := os.ReadFile(adminPromptPath)
	require.NoError(t, err, "admin-prompt file must be written to --admin-prompt-out path")
	require.Equal(t, adminPromptBody, string(data),
		"admin-prompt file contents must match the API response's AdminPrompt body exactly")
}

// TestRunBootstrapCommand_NoAdminPromptWhenEmpty asserts the file is NOT
// created when the API returns an empty adminPrompt. The bootstrap code
// guards on `if adminPrompt != ""` before writing — this test pins that
// behavior. Note: even if a stale zero-byte file existed at the path,
// loadAdminPrompt() (agent_config_writer.go:130) guards on
// `len(data) == 0` and would skip injection, so an accidental empty file
// would NOT inject empty content into opencode. This test guards the
// upstream layer (bootstrap) anyway because file-presence vs file-absence
// is the cleanest signal for downstream consumers and avoids surfacing
// no-op zero-byte writes in operational logs.
func TestRunBootstrapCommand_NoAdminPromptWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	adminPromptPath := filepath.Join(dir, "admin-prompt.md")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets:     rawJSON(t, []map[string]any{{"name": "x"}}),
			AdminPrompt: "",
		})
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-empty-prompt",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
		"--admin-prompt-out", adminPromptPath,
	})
	require.Equal(t, 0, code)

	_, err := os.Stat(adminPromptPath)
	require.True(t, os.IsNotExist(err),
		"admin-prompt file must NOT be created when API returns empty AdminPrompt")
}

// TestAdminPromptPathDefault asserts the production default points at
// the tmpfs (/sandbox-runtime/), not /tmp. /tmp is PVC-backed via subPath
// in the workspace main container, which means (a) plaintext admin
// prompts would persist on the PVC at rest (US-35.7 violation), and
// (b) the credential-setup init container has ReadOnlyRootFilesystem
// without a writable /tmp emptyDir, so writes to /tmp silently fail.
// Both reasons point at /sandbox-runtime — which is already mounted RW
// as a tmpfs in both the init and main containers (see pod_builder.go
// credMounts and the main container spec).
func TestAdminPromptPathDefault(t *testing.T) {
	// Direct constant assertion — if anyone moves it back to /tmp this
	// fails loudly with a clear message about why.
	require.Equal(t, "/sandbox-runtime/admin-prompt.md", agentd.AdminPromptPath,
		"AdminPromptPath must live on the /sandbox-runtime tmpfs — both for "+
			"at-rest data isolation (US-35.7) and because the credential-setup "+
			"init container's /tmp is read-only. See LLMSafeSpaces#483.")
}

// ---------------------------------------------------------------------------
// Allowed-external-directories plumbing (bootstrap subcommand).
//
// The bootstrap subcommand writes the instance's allowedExternalDirectories
// setting to agentd.AllowedDirsPath as a JSON array, so the AgentConfigWriter
// can merge each pattern into mode.permissions.external_directory as an
// "allow" rule. This is the boot-time delivery channel that stops agents
// prompting for /tmp/* on every session.
//
// Contracts (each test turns red if its fix is reverted):
//  1. Non-empty AllowedExternalDirectories → file written as a JSON array.
//  2. Empty/nil AllowedExternalDirectories → file NOT written (clean no-op).
//  3. AllowedDirsPath default points at /sandbox-runtime tmpfs.
// ---------------------------------------------------------------------------

// TestRunBootstrapCommand_WritesAllowedDirs asserts the bootstrap
// subcommand writes the allowed-external-directories array to the
// --allowed-dirs-out path as a JSON array of patterns.
func TestRunBootstrapCommand_WritesAllowedDirs(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	allowedDirsPath := filepath.Join(dir, "allowed-dirs.json")
	tokenPath := writeBootstrapToken(t, dir)

	wantDirs := []string{"/tmp/*", "/var/cache/*"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets:                    rawJSON(t, []map[string]any{{"name": "x"}}),
			AllowedExternalDirectories: wantDirs,
		})
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-dirs",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
		"--allowed-dirs-out", allowedDirsPath,
	})
	require.Equal(t, 0, code, "bootstrap must exit 0 on success")

	data, err := os.ReadFile(allowedDirsPath)
	require.NoError(t, err, "allowed-dirs file must be written to --allowed-dirs-out path")

	var got []string
	require.NoError(t, json.Unmarshal(data, &got), "allowed-dirs file must be a JSON array")
	require.Equal(t, wantDirs, got, "patterns must round-trip exactly")
}

// TestRunBootstrapCommand_NoAllowedDirsWhenEmpty asserts the file is NOT
// created when the API returns no allowed directories. Symmetric with the
// admin-prompt empty guard.
func TestRunBootstrapCommand_NoAllowedDirsWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "secrets.json")
	allowedDirsPath := filepath.Join(dir, "allowed-dirs.json")
	tokenPath := writeBootstrapToken(t, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(bootstrapResponse{
			Secrets: rawJSON(t, []map[string]any{{"name": "x"}}),
		})
	}))
	defer srv.Close()

	code := runBootstrap([]string{
		"--workspace-id", "ws-no-dirs",
		"--api-url", srv.URL,
		"--token-file", tokenPath,
		"--out", outPath,
		"--allowed-dirs-out", allowedDirsPath,
	})
	require.Equal(t, 0, code)

	_, err := os.Stat(allowedDirsPath)
	require.True(t, os.IsNotExist(err),
		"allowed-dirs file must NOT be created when API returns none")
}

// TestAllowedDirsPathDefault asserts the production default points at the
// /sandbox-runtime tmpfs (not /tmp), for the same reasons as
// AdminPromptPath: at-rest isolation + the init container's read-only /tmp.
func TestAllowedDirsPathDefault(t *testing.T) {
	require.Equal(t, "/sandbox-runtime/allowed-dirs.json", agentd.AllowedDirsPath,
		"AllowedDirsPath must live on the /sandbox-runtime tmpfs — same "+
			"rationale as AdminPromptPath (see LLMSafeSpaces#483).")
}

// TestRunBootstrapCommand_EmptyBatchWriteFailure_IsLoud is the
// regression pin for the silent-degrade triage cost of kind run 3
// (PR #1028 review): when the batch write fails (unwritable parent —
// the 0750 rt/ incident shape), the failure MUST reach stderr while
// never-block-boot holds (exit 0, degraded materialize). Pre-fix, the
// error was swallowed (`_ =`) and the sidecar restart guard failed
// invisibly two layers away (K11).
func TestRunBootstrapCommand_EmptyBatchWriteFailure_IsLoud(t *testing.T) {
	dir := t.TempDir()
	token := writeBootstrapToken(t, dir)

	// Unwritable parent for --out: a FILE blocks the mkdir of its would-be parent dir.
	blocker := filepath.Join(dir, "blocked")
	require.NoError(t, os.WriteFile(blocker, []byte("in the way"), 0o644))
	out := filepath.Join(blocker, "sub", "secrets.json")

	// API down (closed port) so bootstrap takes the degrade path that
	// calls writeEmptySecrets; the blocker makes that write fail.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	var stderr bytes.Buffer
	code := runBootstrapCommand([]string{
		"--workspace-id", "ws",
		"--api-url", closed.URL,
		"--token-file", token,
		"--out", out,
	}, io.Discard, &stderr)

	require.Equal(t, 0, code, "never-block-boot holds on batch-write failure")
	require.Contains(t, stderr.String(), "empty-batch write FAILED",
		"the silent swallow is the bug this pins — a future revert to `_ =` must fail here")
	require.Contains(t, stderr.String(), out)
}
