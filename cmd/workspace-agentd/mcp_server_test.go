package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPHandler_Initialize(t *testing.T) {
	req := mcpRequest{JSONRPC: "2.0", ID: 1, Method: "initialize"}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	mcpHandler("test-password")(w, r)

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
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	mcpHandler("test-password")(w, r)

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
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	mcpHandler("test-password")(w, r)

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
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	mcpHandler("test-password")(w, r)

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
	r := httptest.NewRequest("POST", "/v1/mcp", newBodyReader(body))
	mcpHandler("test-password")(w, r)

	assert.Equal(t, 200, w.Code)
	var resp mcpResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	result := resp.Result.(map[string]any)
	assert.True(t, result["isError"].(bool))
}

func TestInjectAgentdMCPServer_EmptyConfig(t *testing.T) {
	cfg := map[string]json.RawMessage{}
	injectAgentdMCPServer(cfg)

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
}

func TestInjectAgentdMCPServer_ExistingMCP(t *testing.T) {
	existing := map[string]json.RawMessage{
		"github": json.RawMessage(`{"type":"local","command":["npx"]}`),
	}
	existingJSON, _ := json.Marshal(existing)
	cfg := map[string]json.RawMessage{
		"mcp": existingJSON,
	}

	injectAgentdMCPServer(cfg)

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

func newBodyReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}
