// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mcpserver contains minimal MCP server implementations for testing.
// Each transport (HTTP, SSE, stdio) implements the MCP JSON-RPC protocol
// just enough to: respond to initialize, list one tool, and call that tool.
// They exist so integration/e2e tests can verify the full pipeline without
// depending on external MCP servers or the full mark3labs/mcp-go SDK.
package mcpserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// jsonRPCRequest is the MCP JSON-RPC request envelope.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// jsonRPCResponse is the MCP JSON-RPC response envelope.
type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonRPCErr     `json:"error,omitempty"`
}

type jsonRPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// HandleMCPRequest processes a single MCP JSON-RPC request and returns the
// response. This is the shared handler for all three transport test servers.
func HandleMCPRequest(body []byte) jsonRPCResponse {
	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonRPCResponse{JSONRPC: "2.0", Error: &jsonRPCErr{Code: -32700, Message: "parse error"}}
	}

	resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}

	switch req.Method {
	case "initialize":
		resp.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "llmsafespaces-test-mcp",
				"version": "1.0.0",
			},
		}

	case "tools/list":
		resp.Result = map[string]any{
			"tools": []map[string]any{
				{
					"name":        "ping",
					"description": "Returns a pong message",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					},
				},
			},
		}

	case "tools/call":
		resp.Result = map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "pong"},
			},
		}

	case "ping":
		resp.Result = map[string]any{}

	default:
		resp.Error = &jsonRPCErr{Code: -32601, Message: fmt.Sprintf("method %q not found", req.Method)}
	}

	return resp
}

// --- HTTP transport ---

// HTTPTestServer is a minimal HTTP MCP server. It accepts POST requests with
// JSON-RPC bodies and responds with the shared handler.
type HTTPTestServer struct {
	srv *http.Server
	URL string
}

// NewHTTPTestServer starts an HTTP MCP server on a random port.
func NewHTTPTestServer() *HTTPTestServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := HandleMCPRequest(body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := &http.Server{
		Handler:            mux,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       30 * time.Second,
	}
	listener, err := netListen()
	if err != nil {
		panic(fmt.Sprintf("failed to listen: %v", err))
	}
	go func() { _ = srv.Serve(listener) }()
	return &HTTPTestServer{srv: srv, URL: "http://" + listener.Addr().String() + "/mcp"}
}

// Close shuts down the HTTP server.
func (s *HTTPTestServer) Close() { _ = s.srv.Close() }

// --- SSE transport ---

// SSETestServer is a minimal SSE MCP server. It implements the streamable
// HTTP transport: the client POSTs JSON-RPC requests, the server responds
// with text/event-stream frames.
type SSETestServer struct {
	srv *http.Server
	URL string
}

// NewSSETestServer starts an SSE MCP server on a random port.
func NewSSETestServer() *SSETestServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := HandleMCPRequest(body)
		respJSON, _ := json.Marshal(resp)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", respJSON)
	})

	srv := &http.Server{
		Handler:            mux,
		ReadHeaderTimeout:  5 * time.Second,
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       30 * time.Second,
	}
	listener, err := netListen()
	if err != nil {
		panic(fmt.Sprintf("failed to listen: %v", err))
	}
	go func() { _ = srv.Serve(listener) }()
	return &SSETestServer{srv: srv, URL: "http://" + listener.Addr().String() + "/sse"}
}

// Close shuts down the SSE server.
func (s *SSETestServer) Close() { _ = s.srv.Close() }

// --- stdio transport ---

// RunStdio reads JSON-RPC requests from stdin, one per line, and writes
// responses to stdout. Designed to be invoked as a subprocess (matching
// opencode's local MCP server execution model).
func RunStdio() {
	scanner := newLineScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		resp := HandleMCPRequest(line)
		respJSON, _ := json.Marshal(resp)
		_, _ = fmt.Fprintln(os.Stdout, string(respJSON))
	}
}
