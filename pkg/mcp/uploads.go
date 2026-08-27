// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// Epic 68 US-68.4 — workspace_file_upload tool + HTTPClient.UploadFile.
//
// The MCP server is a client of the REST API (cmd/mcp wires an HTTPClient),
// so the upload seam is the same POST /api/v1/workspaces/:id/uploads route
// the web/SDK clients use: the phase/disk/cap gates of US-68.2 apply
// unchanged. Caps here are MCP-specific on top (D4): base64 transport through
// stdio is hostile to large payloads, so the tool accepts at most 5 MiB
// decoded, rejects encoded input over 7 MiB before decoding (DoS guard,
// U1.5.13 — 4/3 · 5 MiB ≈ 6.7 MiB, so 7 MiB encoded can never decode under
// the cap), and its description points larger files at REST/SDK. Base64 is
// whitespace-normalized before a strict decode (D18): CLI-wrapped input
// decodes; strictly malformed input after normalization is a tool error,
// never a panic.

const (
	mcpUploadMaxDecodedBytes = 5 << 20
	mcpUploadMaxEncodedBytes = 7 << 20
)

const mcpUploadOverCapMessage = "content exceeds the MCP upload cap of 5 MiB decoded; " +
	"use the REST/SDK upload endpoint (POST /api/v1/workspaces/<id>/uploads) for files up to 25 MiB"

// base64Whitespace matches the characters stripped before decoding (D18):
// newlines, carriage returns, spaces, and tabs from CLI-wrapped clients.
var base64Whitespace = strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "")

// UploadResp is the upload result: the workspace-absolute path of the stored
// file, its sanitized name, and its size in bytes.
type UploadResp struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// UploadError is the typed classification of an upload rejected by the API:
// the HTTP status, a public-safe message, and — for phase-gated rejections
// (409) — the workspace phase, so the tool error can name it instead of
// passing raw HTTP through (U1.5.11).
type UploadError struct {
	Status  int
	Message string
	Phase   string
}

func (e *UploadError) Error() string {
	if e.Phase != "" {
		return fmt.Sprintf("upload rejected: %s (phase: %s)", e.Message, e.Phase)
	}
	return fmt.Sprintf("upload rejected: %s", e.Message)
}

var workspaceFileUploadTool = mcp.NewTool("workspace_file_upload",
	mcp.WithDescription("Upload a file into a workspace so the agent can read it with its own tools. "+
		"The file lands on the workspace's persistent volume under /workspace/uploads/ and the tool returns "+
		"{path, name, size}; pass path to session_message's files parameter (or reference it in a raw prompt) "+
		"so the agent sees the attachment. Content is standard base64 (embedded whitespace/newlines are tolerated). "+
		"Capped at 5 MiB decoded per file; use the REST/SDK upload endpoint "+
		"(POST /api/v1/workspaces/<id>/uploads, up to 25 MiB) for larger files. "+
		"The workspace must be Active (phase-gated)."),
	mcp.WithString("workspace_id", mcp.Required(), mcp.Description("Workspace ID")),
	mcp.WithString("filename", mcp.Required(), mcp.Description("Destination filename; sanitized (path components, control chars, and RTL overrides stripped) before upload")),
	mcp.WithString("content_b64", mcp.Required(), mcp.Description("File content as standard base64 with padding; embedded \\n, \\r, spaces, and tabs are stripped before decoding")),
)

func (h *handlers) workspaceFileUpload(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	workspaceID := strArg(args, "workspace_id")
	filename := strArg(args, "filename")
	contentB64 := strArg(args, "content_b64")

	if workspaceID == "" {
		return mcp.NewToolResultError("workspace_id is required"), nil
	}
	if filename == "" {
		return mcp.NewToolResultError("filename is required"), nil
	}
	if contentB64 == "" {
		return mcp.NewToolResultError("content_b64 is required"), nil
	}
	if len(contentB64) > mcpUploadMaxEncodedBytes {
		return mcp.NewToolResultError(mcpUploadOverCapMessage), nil
	}

	normalized := base64Whitespace.Replace(contentB64)
	if normalized == "" {
		return mcp.NewToolResultError("content_b64 is empty"), nil
	}
	decoded, err := base64.StdEncoding.DecodeString(normalized)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("content_b64 is not valid base64: %v", err)), nil
	}
	if len(decoded) > mcpUploadMaxDecodedBytes {
		return mcp.NewToolResultError(mcpUploadOverCapMessage), nil
	}

	sanitized, ok := agentd.SanitizeFilename(filename)
	if !ok {
		return mcp.NewToolResultError("filename is empty or invalid after sanitization"), nil
	}

	resp, err := h.client.UploadFile(ctx, workspaceID, sanitized, decoded)
	if err != nil {
		return mcp.NewToolResultError(uploadToolErrorMessage(err)), nil
	}

	out, _ := json.Marshal(resp)
	return mcp.NewToolResultText(string(out)), nil
}

// uploadToolErrorMessage maps typed upload rejections to phase- or
// cause-naming messages (U1.5.11: the phase is surfaced, never a raw HTTP
// passthrough).
func uploadToolErrorMessage(err error) string {
	var uerr *UploadError
	if errors.As(err, &uerr) {
		switch {
		case uerr.Phase != "":
			return fmt.Sprintf("cannot upload: workspace not active (phase: %s)", uerr.Phase)
		case uerr.Status == http.StatusRequestEntityTooLarge:
			return mcpUploadOverCapMessage
		case uerr.Status == http.StatusInsufficientStorage:
			return "cannot upload: workspace disk is full; free space or contact the administrator"
		default:
			return fmt.Sprintf("failed to upload file: %s", uerr.Message)
		}
	}
	return fmt.Sprintf("failed to upload file: %v", err)
}

// UploadFile uploads content to the workspace via the REST upload route
// (US-68.2). The filename must already be sanitized (the handler applies
// agentd.SanitizeFilename; the API re-sanitizes — D9 defense-in-depth).
func (c *HTTPClient) UploadFile(ctx context.Context, workspaceID, filename string, content []byte) (*UploadResp, error) {
	if err := validateID(workspaceID, "workspace_id"); err != nil {
		return nil, err
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, requestTimeout)
		defer cancel()
	}

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", filename)
	if err != nil {
		return nil, fmt.Errorf("build multipart body: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return nil, fmt.Errorf("build multipart body: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("build multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/api/v1/workspaces/"+workspaceID+"/uploads", &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	limited := io.LimitReader(resp.Body, maxResponseBody)

	if resp.StatusCode != http.StatusCreated {
		var body struct {
			Error string `json:"error"`
			Phase string `json:"phase"`
		}
		raw, _ := io.ReadAll(limited)
		_ = json.Unmarshal(raw, &body)
		msg := body.Error
		if msg == "" {
			msg = fmt.Sprintf("upload failed (HTTP %d)", resp.StatusCode)
		}
		return nil, &UploadError{Status: resp.StatusCode, Message: msg, Phase: body.Phase}
	}

	var uploaded UploadResp
	if err := json.NewDecoder(limited).Decode(&uploaded); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &uploaded, nil
}
