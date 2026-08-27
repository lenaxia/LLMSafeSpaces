// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lenaxia/llmsafespaces/pkg/session/attachments"
)

// TestIntegration_FullWorkflow tests the complete MCP flow:
// workspace_create → workspace_activate → session_create → session_message → session_history → workspace_stop
func TestIntegration_FullWorkflow(t *testing.T) {
	mockClient := &MockAPIClient{}
	srv := NewServer(mockClient, 30*time.Second)

	// Set up mock expectations for the full workflow
	mockClient.On("CreateWorkspace", mock.Anything, CreateWorkspaceReq{Runtime: "python:3.10", Name: "test"}).
		Return(&WorkspaceResp{ID: "ws-1", Name: "test", Runtime: "python:3.10", Phase: "Active"}, nil)
	mockClient.On("ActivateWorkspace", mock.Anything, "ws-1").
		Return(&ActivateResp{Resumed: "ws-1"}, nil)
	mockClient.On("CreateSession", mock.Anything, "ws-1").
		Return(&SessionResp{ID: "sess-1"}, nil)
	mockClient.On("SendMessage", mock.Anything, "ws-1", "sess-1", "write hello world in python", 30*time.Second).
		Return("```python\nprint('hello world')\n```", nil)
	mockClient.On("GetHistory", mock.Anything, "ws-1", "sess-1").
		Return([]Message{
			{
				ID: "msg_1", Type: "user",
				Parts: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{{Type: "text", Text: "write hello world in python"}},
			},
			{
				ID: "msg_2", Type: "assistant",
				Parts: []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}{{Type: "text", Text: "```python\nprint('hello world')\n```"}},
			},
		}, nil)
	mockClient.On("SuspendWorkspace", mock.Anything, "ws-1").Return(nil)

	// Create in-process MCP client
	mcpClient, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	defer mcpClient.Close()

	ctx := context.Background()
	require.NoError(t, mcpClient.Start(ctx))

	// Initialize
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1.0"}
	_, err = mcpClient.Initialize(ctx, initReq)
	require.NoError(t, err)

	// 1. List tools - verify all are registered (12 + 1 run_resolve + 11 Epic 64
	// + 1 workspace_file_upload = 25).
	toolsResp, err := mcpClient.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	assert.Len(t, toolsResp.Tools, 25)

	toolNames := make(map[string]bool)
	toolSchemas := make(map[string]mcp.Tool)
	for _, tool := range toolsResp.Tools {
		toolNames[tool.Name] = true
		toolSchemas[tool.Name] = tool
	}

	// #880/#905 schema pin: workspace_create must REQUIRE name — the API
	// validation (Epic 46) rejects empty names with a guaranteed 422, so
	// an optional-name schema advises clients into an error. Catches a
	// silent revert to optional.
	requiredNames := append([]string(nil), toolSchemas["workspace_create"].InputSchema.Required...)
	require.Contains(t, requiredNames, "name",
		"workspace_create schema must require name (API validation 422s on empty)")
	assert.True(t, toolNames["workspace_create"])
	assert.True(t, toolNames["workspace_activate"])
	assert.True(t, toolNames["workspace_stop"])
	assert.True(t, toolNames["workspace_refresh_compute"])
	assert.True(t, toolNames["session_create"])
	assert.True(t, toolNames["session_message"])
	assert.True(t, toolNames["session_history"])
	assert.True(t, toolNames["run_resolve"])
	assert.True(t, toolNames["workspace_file_upload"])

	// 2. workspace_create
	result, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Request: mcp.Request{},
		Params: mcp.CallToolParams{
			Name:      "workspace_create",
			Arguments: map[string]any{"runtime": "python:3.10", "name": "test"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "ws-1")

	// 3. workspace_activate
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "workspace_activate",
			Arguments: map[string]any{"workspace_id": "ws-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	// 4. session_create
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "session_create",
			Arguments: map[string]any{"workspace_id": "ws-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "sess-1")

	// 5. session_message
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "session_message",
			Arguments: map[string]any{"workspace_id": "ws-1", "session_id": "sess-1", "message": "write hello world in python"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "print('hello world')")

	// 6. session_history
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "session_history",
			Arguments: map[string]any{"workspace_id": "ws-1", "session_id": "sess-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "hello world")

	// 7. workspace_stop
	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "workspace_stop",
			Arguments: map[string]any{"workspace_id": "ws-1"},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	mockClient.AssertExpectations(t)
}

// TestIntegration_UploadThenMessageWithFiles exercises the Epic 68 MCP flow
// (integration scenario I10): workspace_file_upload → session_message(files)
// through a real in-process MCP client, with the manifest composed by the
// shared attachments.Compose (single block dispatched — D7/D15).
func TestIntegration_UploadThenMessageWithFiles(t *testing.T) {
	mockClient := &MockAPIClient{}
	srv := NewServer(mockClient, 30*time.Second)

	content := []byte("quarterly numbers inside")
	mockClient.On("UploadFile", mock.Anything, "ws-1", "notes.txt", content).
		Return(&UploadResp{Path: uploadPathA, Name: "notes.txt", Size: int64(len(content))}, nil)

	wantComposed, cerr := attachments.Compose("read the attached notes and quote the first line", []string{uploadPathA})
	require.NoError(t, cerr)
	mockClient.On("SendMessage", mock.Anything, "ws-1", "sess-1", wantComposed, 30*time.Second).
		Return("first line: quarterly numbers inside", nil)

	mcpClient, err := client.NewInProcessClient(srv)
	require.NoError(t, err)
	defer mcpClient.Close()

	ctx := context.Background()
	require.NoError(t, mcpClient.Start(ctx))

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "test", Version: "1.0"}
	_, err = mcpClient.Initialize(ctx, initReq)
	require.NoError(t, err)

	result, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "workspace_file_upload",
			Arguments: map[string]any{
				"workspace_id": "ws-1",
				"filename":     "notes.txt",
				"content_b64":  base64.StdEncoding.EncodeToString(content),
			},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, uploadPathA)

	result, err = mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "session_message",
			Arguments: map[string]any{
				"workspace_id": "ws-1",
				"session_id":   "sess-1",
				"message":      "read the attached notes and quote the first line",
				"files":        []string{uploadPathA},
			},
		},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "quarterly numbers inside")

	mockClient.AssertExpectations(t)
}

// TestIntegration_ExternalStdioClient_UploadAndMessageWithFiles is e2e
// row E8 (Epic 68): a REAL external MCP client over the stdio transport —
// the compiled cmd/mcp binary as a subprocess — against a fake REST API.
// The wire assertions live in the fake API: multipart upload framing with
// auth, and the v1 attachment manifest composed into the dispatched
// prompt. This is the shape a third-party MCP host (Claude Desktop,
// cursor, etc.) exercises.
func TestIntegration_ExternalStdioClient_UploadAndMessageWithFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("external stdio client test requires building cmd/mcp")
	}

	const uploadPath = "/workspace/uploads/11111111-2222-3333-4444-555555555555-notes.txt"
	const apiKey = "lsp-e2e-stdio-key"

	var mu sync.Mutex
	var gotAuth []string
	var gotUploadFilename, gotUploadContent string
	var gotPromptText string

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/uploads"):
			f, header, err := r.FormFile("file")
			require.NoError(t, err)
			defer f.Close()
			body, err := io.ReadAll(f)
			require.NoError(t, err)
			mu.Lock()
			gotUploadFilename, gotUploadContent = header.Filename, string(body)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"path":"` + uploadPath + `","name":"notes.txt","size":22}`))

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/prompt"):
			var body struct {
				Parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"parts"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			mu.Lock()
			for _, p := range body.Parts {
				if p.Type == "text" {
					gotPromptText = p.Text
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/session-events"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"session.event\",\"data\":{\"type\":\"message.part.delta\",\"sessionId\":\"sess-1\",\"delta\":\"first line: quarterly numbers\"}}\n\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"session.status\",\"session_id\":\"sess-1\",\"status\":\"idle\"}\n\n"))

		default:
			t.Errorf("unexpected API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)

	bin := filepath.Join(t.TempDir(), "mcp-e2e")
	build := exec.Command("go", "build", "-o", bin, "github.com/lenaxia/llmsafespaces/cmd/mcp")
	build.Dir = "../../"
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build cmd/mcp: %s", out)

	mcpClient, err := client.NewStdioMCPClient(bin, []string{
		"LLMSAFESPACES_URL=" + api.URL,
		"LLMSAFESPACES_API_KEY=" + apiKey,
	})
	require.NoError(t, err)
	defer mcpClient.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, mcpClient.Start(ctx))

	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "e2e-external-stdio", Version: "1.0"}
	_, err = mcpClient.Initialize(ctx, initReq)
	require.NoError(t, err)

	content := []byte("quarterly numbers inside")
	upResult, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "workspace_file_upload",
			Arguments: map[string]any{
				"workspace_id": "ws-1",
				"filename":     "notes.txt",
				"content_b64":  base64.StdEncoding.EncodeToString(content),
			},
		},
	})
	require.NoError(t, err)
	assert.False(t, upResult.IsError)
	assert.Contains(t, upResult.Content[0].(mcp.TextContent).Text, uploadPath)

	msgResult, err := mcpClient.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "session_message",
			Arguments: map[string]any{
				"workspace_id": "ws-1",
				"session_id":   "sess-1",
				"message":      "read the attached notes and quote the first line",
				"files":        []string{uploadPath},
			},
		},
	})
	require.NoError(t, err)
	assert.False(t, msgResult.IsError)
	assert.Contains(t, msgResult.Content[0].(mcp.TextContent).Text, "quarterly numbers")

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "notes.txt", gotUploadFilename, "upload filename")
	assert.Equal(t, string(content), gotUploadContent, "upload bytes")
	wantPrompt := "read the attached notes and quote the first line\n\n" +
		`[llmsafespaces:attachment path="` + uploadPath + `" name="notes.txt"]` + "\n"
	assert.Equal(t, wantPrompt, gotPromptText, "dispatched prompt must carry exactly one v1 manifest block")
	for _, auth := range gotAuth {
		assert.Equal(t, "Bearer "+apiKey, auth, "every hop authenticates with the API key")
	}
}
