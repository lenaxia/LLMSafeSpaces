package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func mcpHandler(password string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var req mcpRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeMCPError(w, nil, -32700, "Parse error")
			return
		}

		switch req.Method {
		case "initialize":
			writeMCPResult(w, req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
				"serverInfo": map[string]any{
					"name":    "llmsafespaces-workspace",
					"version": "1.0",
				},
			})

		case "tools/list":
			writeMCPResult(w, req.ID, map[string]any{
				"tools": []mcpTool{
					{
						Name:        "session_list",
						Description: "List opencode sessions in this workspace",
						InputSchema: map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
					{
						Name:        "session_read",
						Description: "Read message history from an opencode session",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"session_id": map[string]any{"type": "string", "description": "The session ID to read"},
								"limit":      map[string]any{"type": "integer", "description": "Max messages to return (default 20)"},
							},
							"required": []string{"session_id"},
						},
					},
				},
			})

		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeMCPError(w, req.ID, -32602, "Invalid params")
				return
			}
			result, err := callMCPTool(r.Context(), password, params.Name, params.Arguments)
			if err != nil {
				writeMCPResult(w, req.ID, map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": fmt.Sprintf("Error: %v", err)},
					},
					"isError": true,
				})
				return
			}
			writeMCPResult(w, req.ID, map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": result},
				},
			})

		default:
			writeMCPError(w, req.ID, -32601, "Method not found: "+req.Method)
		}
	}
}

func callMCPTool(ctx context.Context, password, name string, args map[string]any) (string, error) {
	switch name {
	case "session_list":
		return mcpSessionList(ctx, password)
	case "session_read":
		sessionID, _ := args["session_id"].(string)
		if sessionID == "" {
			return "", fmt.Errorf("session_id is required")
		}
		limit := 20
		if l, ok := args["limit"].(float64); ok && l > 0 {
			limit = int(l)
		}
		return mcpSessionRead(ctx, password, sessionID, limit)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func mcpSessionList(ctx context.Context, password string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("http://127.0.0.1:%d/session", agentd.AgentPort), nil)
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

func mcpSessionRead(ctx context.Context, password, sessionID string, limit int) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d/session/%s/message?limit=%d", agentd.AgentPort, sessionID, limit)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to read session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return string(body), nil
}

func writeMCPResult(w http.ResponseWriter, id any, result any) {
	_ = json.NewEncoder(w).Encode(mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	})
}

func writeMCPError(w http.ResponseWriter, id any, code int, msg string) {
	_ = json.NewEncoder(w).Encode(mcpResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &mcpError{Code: code, Message: msg},
	})
}

func injectAgentdMCPServer(cfg map[string]json.RawMessage) {
	mcpEntry := map[string]any{
		"enabled": true,
		"type":    "remote",
		"url":     fmt.Sprintf("http://127.0.0.1:%d/v1/mcp", agentd.AgentdAdminPort),
	}
	entryJSON, _ := json.Marshal(mcpEntry)

	if existing, ok := cfg["mcp"]; ok {
		var mcpMap map[string]json.RawMessage
		if json.Unmarshal(existing, &mcpMap) == nil {
			mcpMap["llmsafespaces"] = entryJSON
			merged, _ := json.Marshal(mcpMap)
			cfg["mcp"] = merged
			return
		}
	}
	mcpMap := map[string]json.RawMessage{"llmsafespaces": entryJSON}
	merged, _ := json.Marshal(mcpMap)
	cfg["mcp"] = merged
}
