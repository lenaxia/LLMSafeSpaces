package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const mcpTestPassword = "test-password"

// mcpAuthedRequest builds a /v1/mcp request carrying the agentd Basic-auth
// credential required at handler entry.
func mcpAuthedRequest(body []byte) *http.Request {
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	r.Header.Set("Authorization", "Basic "+basicAuth(mcpTestPassword))
	return r
}

func TestMCPHandler_RequiresAuth(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 99, Method: "tools/list"}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	mcpHandler(mcpTestPassword)(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "unauthenticated JSON-RPC must be rejected")
	assert.Equal(t, `Basic realm="agentd"`, w.Header().Get("WWW-Authenticate"))
}

func TestMCPHandler_WrongPassword(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 99, Method: "initialize"}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	r.Header.Set("Authorization", "Basic "+basicAuth("wrong"))
	mcpHandler(mcpTestPassword)(w, r)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMCPHandler_Initialize(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	assert.Equal(t, 200, w.Code)
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.Equal(t, "llmsafespaces-workspace", result["serverInfo"].(map[string]any)["name"])
}

func TestMCPHandler_ToolsList(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 2, Method: "tools/list"}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	assert.Equal(t, 200, w.Code)
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	assert.GreaterOrEqual(t, len(tools), 2)
}

func TestMCPHandler_UnknownMethod(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 3, Method: "unknown/method"}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	assert.Equal(t, 200, w.Code)
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotNil(t, resp.Error)
	assert.Equal(t, -32601, resp.Error.Code)
}

func TestMCPHandler_ToolsCall_UnknownTool(t *testing.T) {
	params, _ := json.Marshal(map[string]any{
		"name":      "nonexistent_tool",
		"arguments": map[string]any{},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 4, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	assert.Equal(t, 200, w.Code)
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.True(t, result["isError"].(bool))
}

func TestMCPHandler_ToolsCall_SessionRead_MissingSessionID(t *testing.T) {
	params, _ := json.Marshal(map[string]any{
		"name":      "session_read",
		"arguments": map[string]any{},
	})
	req := mcpRequest{JSONRPC: "2.0", ID: 5, Method: "tools/call", Params: params}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	assert.Equal(t, 200, w.Code)
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.True(t, result["isError"].(bool))
}

func TestInjectAgentdMCPServer_EmptyConfig(t *testing.T) {
	cfg := map[string]json.RawMessage{}
	injectAgentdMCPServer(mcpTestPassword)(cfg)

	mcpRaw, ok := cfg["mcp"]
	require.True(t, ok, "mcp section should be present")

	var mcpMap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mcpRaw, &mcpMap))

	entryRaw, ok := mcpMap["llmsafespaces"]
	require.True(t, ok, "llmsafespaces entry should be present")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(entryRaw, &entry))
	assert.Equal(t, true, entry["enabled"])
	assert.Equal(t, "remote", entry["type"])
	assert.Contains(t, entry["url"].(string), "/v1/mcp")
	assert.Contains(t, entry["url"].(string), ":4097", "MCP server must be injected on the user mux port (4097), not admin (4098)")
	headers := entry["headers"].(map[string]any)
	assert.Equal(t, "Basic "+basicAuth(mcpTestPassword), headers["Authorization"],
		"entry must carry the Basic credential — /v1/mcp rejects unauthenticated JSON-RPC (#847)")
}

func TestInjectAgentdMCPServer_ExistingMCP(t *testing.T) {
	existing := map[string]json.RawMessage{
		"github": json.RawMessage(`{"type":"local","command":["npx"]}`),
	}
	existingJSON, _ := json.Marshal(existing)
	cfg := map[string]json.RawMessage{
		"mcp": existingJSON,
	}

	injectAgentdMCPServer(mcpTestPassword)(cfg)

	var mcpMap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfg["mcp"], &mcpMap))
	_, hasGithub := mcpMap["github"]
	assert.True(t, hasGithub, "existing github entry should be preserved")
	_, hasBuiltin := mcpMap["llmsafespaces"]
	assert.True(t, hasBuiltin, "llmsafespaces entry should be added")
}

func TestCallMCPTool_UnknownTool(t *testing.T) {
	_, err := callMCPTool(context.Background(), "password", "unknown", map[string]any{})
	assert.Error(t, err)
}

func TestCallMCPTool_SessionRead_MissingID(t *testing.T) {
	_, err := callMCPTool(context.Background(), "password", "session_read", map[string]any{})
	assert.Error(t, err)
}

func TestCallMCPTool_DevPreviewURL_HappyPath(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "ws-abc-123")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")

	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
		"port": float64(5173),
	})
	require.NoError(t, err)
	assert.Contains(t, result, "https://platform.example.com/api/v1/workspaces/ws-abc-123/dev-preview/5173/")
	assert.Regexp(t, `^DEV_PREVIEW 5173 path$`, strings.SplitN(result, "\n", 2)[0])
	assert.Contains(t, result, "Requires dev preview enabled")
	assert.Contains(t, result, "Workspace Settings")
}

func TestCallMCPTool_DevPreviewURL_WithPath(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "ws-abc-123")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")

	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
		"port": float64(3000),
		"path": "/dashboard",
	})
	require.NoError(t, err)
	assert.Contains(t, result, "/dev-preview/3000/dashboard")
	assert.Contains(t, result, "Requires dev preview enabled")
}

func TestCallMCPTool_DevPreviewURL_PathWithoutSlash(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "ws-abc-123")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")

	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
		"port": float64(5173),
		"path": "about",
	})
	require.NoError(t, err)
	assert.Contains(t, result, "/dev-preview/5173/about")
}

func TestCallMCPTool_DevPreviewURL_PortOmitted(t *testing.T) {
	// Port is optional now (UX round 2): omitted → default 5173, no error.
	t.Setenv("WORKSPACE_ID", "ws-abc-123")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, result, "/dev-preview/5173/")
}

func TestCallMCPTool_DevPreviewURL_PortOutOfRange(t *testing.T) {
	for _, port := range []float64{0, -1, 65536} {
		_, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
			"port": port,
		})
		assert.Error(t, err, "port %v should be rejected", port)
	}
}

func TestMCPHandler_ToolsList_IncludesDevPreviewURL(t2 *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 2, Method: "tools/list"}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := mcpAuthedRequest(body)
	mcpHandler(mcpTestPassword)(w, r)

	var resp mcpResponse
	require.NoError(t2, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)

	var found bool
	for _, tool := range tools {
		t := tool.(map[string]any)
		if t["name"] == "dev_preview_url" {
			found = true
			props := t["inputSchema"].(map[string]any)["properties"].(map[string]any)
			_, hasPort := props["port"]
			assert.True(t2, hasPort, "dev_preview_url should have a port parameter")
		}
	}
	assert.True(t2, found, "dev_preview_url tool should be in the tools list")
}

func newBodyReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}

// --- #749 regression tests ---

// TestMCPSessionList_RejectsNon200 verifies that mcpSessionList returns
// an error when opencode returns a non-200 status (#749).
func TestMCPSessionList_RejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(srv.URL)
	defer agentAddrAtomic.Store(old)

	_, err := mcpSessionList(context.Background(), "test-pw")
	assert.Error(t, err, "non-200 status must return error")
}

// TestMCPSessionRead_RejectsNon200 verifies that mcpSessionRead returns
// an error when opencode returns a non-200 status (#749).
func TestMCPSessionRead_RejectsNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(srv.URL)
	defer agentAddrAtomic.Store(old)

	_, err := mcpSessionRead(context.Background(), "test-pw", "ses_1", 50)
	assert.Error(t, err, "non-200 status must return error")
}

// TestMCPSessionList_HappyPath verifies a 200 response returns the body.
func TestMCPSessionList_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"id":"ses_1"}]`))
	}))
	defer srv.Close()

	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(srv.URL)
	defer agentAddrAtomic.Store(old)

	body, err := mcpSessionList(context.Background(), "test-pw")
	assert.NoError(t, err)
	assert.Contains(t, body, "ses_1")
}

// TestMCPSessionRead_HappyPath verifies a 200 response returns the body.
func TestMCPSessionRead_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`[{"id":"msg_1"}]`))
	}))
	defer srv.Close()

	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store(srv.URL)
	defer agentAddrAtomic.Store(old)

	body, err := mcpSessionRead(context.Background(), "test-pw", "ses_1", 50)
	assert.NoError(t, err)
	assert.Contains(t, body, "msg_1")
}

// TestMCPSessionRead_MalformedSessionID_NoPanic pins the URL-build error
// branch (Go 1.26 toolchain bump PR): a sessionID containing a control
// character makes the URL unparseable; previously req was nil and
// SetBasicAuth panicked before Do. Must return a build error, not panic.
func TestMCPSessionRead_MalformedSessionID_NoPanic(t *testing.T) {
	var got error
	assert.NotPanics(t, func() {
		_, got = mcpSessionRead(context.Background(), "test-pw", "ses_\x7f", 50)
	})
	assert.Error(t, got, "malformed sessionID must return a build error")
	assert.Contains(t, got.Error(), "failed to build session read request")
}

// TestMCPSessionList_MalformedAgentAddr_NoPanic pins the list-path build
// error branch: an agent addr that yields an unparseable URL must return
// a build error, not panic at SetBasicAuth.
func TestMCPSessionList_MalformedAgentAddr_NoPanic(t *testing.T) {
	old := agentAddrAtomic.Load().(string)
	agentAddrAtomic.Store("http://127.0.0.1:1:2") // double port — unparseable
	defer agentAddrAtomic.Store(old)

	var got error
	assert.NotPanics(t, func() {
		_, got = mcpSessionList(context.Background(), "test-pw")
	})
	assert.Error(t, got, "malformed agent addr must return a build error")
	assert.Contains(t, got.Error(), "failed to build session list request")
}

// TestInjectAgentdMCPServer_EmptyPassword_Disabled pins the empty-password
// branch: no credential is stamped and the entry is DISABLED — an enabled
// entry without headers would 401 on every JSON-RPC call against the
// gated /v1/mcp, so opencode would pointlessly retry an unusable server.
func TestInjectAgentdMCPServer_EmptyPassword_Disabled(t *testing.T) {
	cfg := map[string]json.RawMessage{}
	injectAgentdMCPServer("")(cfg)

	var mcpMap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfg["mcp"], &mcpMap))
	var entry map[string]any
	require.NoError(t, json.Unmarshal(mcpMap["llmsafespaces"], &entry))
	assert.Equal(t, false, entry["enabled"], "empty password must disable the entry, not stamp an unusable enabled one")
	assert.NotContains(t, entry, "headers", "no credential exists to stamp")
}

// TestInjectAgentdMCPServer_CredentialAcceptedByGate is the coupling test
// between the two sides of the credential: the header the hook stamps on
// the opencode entry must be exactly what mcpHandler's gate accepts. Each
// side is tested against the shared basicAuth helper in isolation; without
// this round-trip, a divergent change to one side (header format, username)
// would pass its own test while breaking the live agent.
func TestInjectAgentdMCPServer_CredentialAcceptedByGate(t *testing.T) {
	const pw = "coupling-test-pw"
	cfg := map[string]json.RawMessage{}
	injectAgentdMCPServer(pw)(cfg)

	var mcpMap map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(cfg["mcp"], &mcpMap))
	var entry struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.Unmarshal(mcpMap["llmsafespaces"], &entry))
	require.Contains(t, entry.Headers, "Authorization")

	// Drive the stamped credential through the actual handler gate with a
	// minimal JSON-RPC request — 200, not 401.
	body, _ := json.Marshal(mcpRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	req := httptest.NewRequest(http.MethodPost, "/v1/mcp", newBodyReader(body))
	req.Header.Set("Authorization", entry.Headers["Authorization"])
	w := httptest.NewRecorder()
	mcpHandler(pw)(w, req)
	require.Equal(t, http.StatusOK, w.Code,
		"the credential stamped on the opencode entry must pass the /v1/mcp gate")
}

func TestCallMCPTool_DevPreviewURL_RefusesPrivilegedAndReservedPorts(t *testing.T) {
	// Tool-layer port policy (THREAT-MODEL T3): refused BEFORE any URL is
	// minted; generic message — no service names leaked at this boundary.
	for _, port := range []float64{80, 443, 1023, 4096, 4097, 4098} {
		_, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
			"port": port,
		})
		require.Error(t, err, "port %v must be refused by the tool", port)
		if strings.Contains(err.Error(), "agentd") || strings.Contains(err.Error(), "opencode") || strings.Contains(err.Error(), "mux") {
			t.Errorf("port %v refusal leaks topology: %v", port, err)
		}
	}
	// Boundary sanity: first usable port passes validation.
	_, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
		"port": float64(1024),
	})
	require.NoError(t, err)
}

func TestCallMCPTool_DevPreviewURL_OriginMode(t *testing.T) {
	// Epic 66 Phase 1: with PREVIEW_ORIGIN_BASE_DOMAIN set, the tool returns
	// the bootstrap URL; ownership is verified by the API middleware chain
	// and the browser is redirected to the per-workspace origin.
	t.Setenv("WORKSPACE_ID", "0d2a9a1b-c3d4-4e5f-8a9b-0c1d2e3f4a5b")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	t.Setenv("PREVIEW_ORIGIN_BASE_DOMAIN", "safespaces.dev")

	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
		"port": float64(5173),
	})
	require.NoError(t, err)
	assert.Contains(t, result, "https://platform.example.com/api/v1/workspaces/0d2a9a1b-c3d4-4e5f-8a9b-0c1d2e3f4a5b/dev-preview-bootstrap/5173")
	assert.Contains(t, result, "0d2a9a1b-c3d4-4e5f-8a9b-0c1d2e3f4a5b")
	assert.NotContains(t, result, "/dev-preview/5173/", "origin mode must not return the path-based URL")
}

func TestCallMCPTool_DevPreviewURL_PathModeUnchangedWithoutEnv(t *testing.T) {
	t.Setenv("WORKSPACE_ID", "ws-abc-123")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	t.Setenv("PREVIEW_ORIGIN_BASE_DOMAIN", "") // unset → path mode

	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
		"port": float64(5173),
	})
	require.NoError(t, err)
	assert.Contains(t, result, "/dev-preview/5173/")
	assert.NotContains(t, result, "dev-preview-bootstrap")
}

func TestCallMCPTool_DevPreviewURL_DefaultPort(t *testing.T) {
	// Port omitted → 5173 (Vite default, matches the landing form).
	t.Setenv("WORKSPACE_ID", "ws-abc-123")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")

	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, result, "DEV_PREVIEW 5173 ")
	assert.Contains(t, result, "/dev-preview/5173/")
	assert.Contains(t, result, "[Open dev preview :5173]")
}

func TestCallMCPTool_DevPreviewURL_ButtonMarkerShape(t *testing.T) {
	// The chat UI keys on the marker line + markdown link; pin the shape.
	t.Setenv("WORKSPACE_ID", "0d2a9a1b-c3d4-4e5f-8a9b-0c1d2e3f4a5b")
	t.Setenv("LLMSAFESPACE_API_URL", "https://platform.example.com")
	t.Setenv("PREVIEW_ORIGIN_BASE_DOMAIN", "safespaces.dev")

	result, err := callMCPTool(context.Background(), "password", "dev_preview_url", map[string]any{
		"port": float64(3000),
	})
	require.NoError(t, err)
	lines := strings.SplitN(result, "\n", 3)
	assert.Regexp(t, `^DEV_PREVIEW 3000 safespaces\.dev$`, lines[0])
	assert.Regexp(t, `^\[Open dev preview :3000\]\(https://platform\.example\.com/api/v1/workspaces/0d2a9a1b-c3d4-4e5f-8a9b-0c1d2e3f4a5b/dev-preview-bootstrap/3000\)$`, lines[1])
	assert.NotEmpty(t, lines[2], "explanation must be present")
}
