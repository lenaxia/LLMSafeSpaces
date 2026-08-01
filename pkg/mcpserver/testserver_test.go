// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcpserver

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleMCPRequest_Initialize(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	resp := HandleMCPRequest([]byte(body))

	assert.Equal(t, "2.0", resp.JSONRPC)
	result, ok := resp.Result.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "llmsafespaces-test-mcp", result["serverInfo"].(map[string]any)["name"])
}

func TestHandleMCPRequest_ToolsList(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	resp := HandleMCPRequest([]byte(body))

	result := resp.Result.(map[string]any)
	tools := result["tools"].([]map[string]any)
	require.Len(t, tools, 1)
	assert.Equal(t, "ping", tools[0]["name"])
}

func TestHandleMCPRequest_ToolsCall(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ping"}}`
	resp := HandleMCPRequest([]byte(body))

	result := resp.Result.(map[string]any)
	content := result["content"].([]map[string]any)
	require.Len(t, content, 1)
	assert.Equal(t, "pong", content[0]["text"])
}

func TestHandleMCPRequest_UnknownMethod(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":4,"method":"nonexistent"}`
	resp := HandleMCPRequest([]byte(body))

	require.NotNil(t, resp.Error)
	assert.Equal(t, -32601, resp.Error.Code)
}

func TestHTTPTestServer_FullHandshake(t *testing.T) {
	srv := NewHTTPTestServer()
	defer srv.Close()

	// initialize
	initResp := postJSON(t, srv.URL, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	assert.Equal(t, "llmsafespaces-test-mcp", initResp["result"].(map[string]any)["serverInfo"].(map[string]any)["name"])

	// tools/list
	listResp := postJSON(t, srv.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := listResp["result"].(map[string]any)["tools"].([]any)
	assert.Len(t, tools, 1)

	// tools/call
	callResp := postJSON(t, srv.URL, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"ping"}}`)
	content := callResp["result"].(map[string]any)["content"].([]any)
	assert.Contains(t, content[0].(map[string]any)["text"], "pong")
}

func TestSSETestServer_Responds(t *testing.T) {
	srv := NewSSETestServer()
	defer srv.Close()

	resp, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "event: message")
	assert.Contains(t, bodyStr, "ping")

	// Verify the JSON payload is parseable.
	var parsed map[string]any
	lines := strings.Split(bodyStr, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			require.NoError(t, json.Unmarshal([]byte(line[6:]), &parsed))
			break
		}
	}
	assert.NotNil(t, parsed["result"])
}

func postJSON(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var result map[string]any
	require.NoError(t, json.Unmarshal(respBody, &result), "body: %s", respBody)
	return result
}
