// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

// Shared types for MCP server management (Epic 53). Mirrors the Go types
// in pkg/types/mcp.go. Secrets (env/headers) are NEVER in the response —
// hasSecret reports whether the server carries an encrypted payload.

export type McpTransport = "http" | "sse" | "stdio";

export interface McpServerResponse {
  id: string;
  name: string;
  transport: McpTransport;
  url?: string;
  command?: string;
  args?: string[];
  timeoutMs?: number;
  hasSecret: boolean;
  enabled: boolean;
  createdAt: string;
  updatedAt: string;
}

export interface CreateMcpServerRequest {
  name: string;
  transport: McpTransport;
  url?: string;
  command?: string;
  args?: string[];
  timeoutMs?: number;
  enabled?: boolean;
  env?: Record<string, string>;
  headers?: Record<string, string>;
  autoApply?: { targetType: string; targetId?: string };
}

export interface UpdateMcpServerRequest {
  name?: string;
  url?: string;
  command?: string;
  args?: string[];
  timeoutMs?: number;
  enabled?: boolean;
  env?: Record<string, string>;
  headers?: Record<string, string>;
}

export interface McpAutoApplyRule {
  targetType: string;
  targetId?: string;
}
