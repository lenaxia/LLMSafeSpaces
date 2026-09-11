package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lenaxia/llmsafespaces/pkg/agentd"
)

// resyncBaseURLAtomic holds the base URL of THIS pod's resync endpoint
// (the secrets_resync MCP tool's loopback target). Tests mutate it; the
// default is the agentd user mux on localhost.
var resyncBaseURLAtomic atomic.Value

func init() {
	resyncBaseURLAtomic.Store(fmt.Sprintf("http://127.0.0.1:%d", agentd.AgentdPort))
}

func resyncBaseURL() string {
	return resyncBaseURLAtomic.Load().(string)
}

// mcpResyncHTTPTimeout bounds the tool's loopback wait: the notify
// path's 5s client budget covers a healthy in-process pull, so double
// that is the margin (a hung endpoint must not wedge the tool call).
const mcpResyncHTTPTimeout = 10 * time.Second

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
		// #847: the proxy exposes session_list/session_read — the
		// workspace's full conversation history. Without this gate any
		// in-pod process could read it unauthenticated. The injected
		// opencode MCP entry carries the same Basic credential
		// (injectAgentdMCPServer); opencode applies remote-entry
		// headers to every JSON-RPC request including initialize
		// (verified against opencode v1.18.10 mcp/index.ts — headers
		// flow into the transport requestInit).
		if !checkBasicAuth(r, password) {
			rejectUnauthorized(w)
			return
		}
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
						Description: "List past agent sessions (conversations) from this workspace. This workspace's history is often the fastest source of context: what was already built, tried, decided, or broken. Use when: starting a task in a workspace you did not create from scratch; the user references earlier work (\"continue\", \"like last time\", \"that bug from before\"); you are about to rebuild something that may already exist; you are resuming after a suspend/resume or a fresh chat in the same workspace; the user asks what was done previously. Not for: the current conversation (you already have it in context). Pair with session_read to pull the details of a specific session.",
						InputSchema: map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
					{
						Name:        "session_read",
						Description: "Read the message history of a past session in this workspace (IDs from session_list). Use when you need the specifics of prior work: file paths touched, approaches tried and abandoned, decisions and their reasons, commands that worked or failed. Prefer a limit first and read more only if needed — summarize for yourself rather than dumping full histories into your reply. Not for: current-conversation content or live program state (these are past transcripts, not a running log).",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"session_id": map[string]any{"type": "string", "description": "The session ID to read (from session_list)"},
								"limit":      map[string]any{"type": "integer", "description": "Max messages to return (default 20)"},
							},
							"required": []string{"session_id"},
						},
					},
					{
						Name:        "dev_preview_url",
						Description: "Returns the preview URL for a web app running in this workspace, which you can offer to the user as an open-preview link (the chat UI renders it as a button; otherwise relay it as a markdown link). Offer it when: the user is working on frontend/UI changes (components, pages, styles) — offer to spin up the dev preview unprompted: start the dev server and share the link so they can follow the work as it lands, rather than waiting to be asked; you have started or verified a web server on a localhost port; you finish building a UI the user will want to inspect; the user asks to see or try the app. Do not use when: nothing is listening on that port and you have not offered to start it — the link does not start the app, it only points at it; you have already shared the URL for that port (it is deterministic — one link suffices); the port is below 1024 or in 4096-4098 (refused). Requirements: the user must have Dev Preview enabled (Workspace Settings → Dev Preview) — if the preview does not load, point them there. No API call is made.",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"port": map[string]any{"type": "integer", "description": "The localhost port the dev server is listening on (e.g. 5173 for Vite, 3000 for Next/Express). Defaults to 5173. Must be >= 1024; ports 4096-4098 are refused."},
								"path": map[string]any{"type": "string", "description": "Optional path on the dev server (defaults to /). Carried through on path-based preview URLs; on per-workspace-origin deployments the preview opens at the app root"},
							},
							"required": []string{},
						},
					},
					{
						// US-70.3 PR-4 (design 0052 §4.7): the agent's
						// on-demand re-materialization escape hatch. No
						// inputs — never accepts credential material
						// (0050 finding-3); the applied state can only
						// come from the platform's authenticated pull.
						Name:        "secrets_resync",
						Description: "Re-pull this workspace's secrets from the platform on demand (the same resync the platform triggers after credential changes). Use when credentials, API keys, or env vars seem missing or stale in this workspace — e.g. a key was just rotated, bound, or revoked, or an API call fails auth in a way the current config should fix. Takes NO inputs and never accepts credential material of any kind: the applied state can only come from the platform's authenticated pull. Returns the applied revision and whether delivery converged (applied/not_modified both mean converged). If the platform is unreachable it reports converged=false with expectedRev = the revision this pod still stands at (pending) — retry later, the platform's reconcile loop also converges this automatically. Rapid repeated calls are rate-limited and refused with retryAfterMs; resyncing may restart the agent session when env-class secrets change.",
						InputSchema: map[string]any{
							"type":       "object",
							"properties": map[string]any{},
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
	case "secrets_resync":
		// args are DELIBERATELY unread (0050 finding-3): the tool
		// accepts no input — the applied state can only come from the
		// platform's authenticated pull. The hand-rolled JSON-RPC
		// layer has no schema validator, so undeclared arguments reach
		// the dispatcher and are ignored here by construction.
		return mcpSecretsResync(ctx, password)
	case "dev_preview_url":
		// Port is optional: default 5173 (the Vite default, the common
		// case; the landing page's form defaults to the same).
		port := 5173
		if raw, present := args["port"]; present && raw != nil {
			p, ok := toInt(raw)
			if !ok || p < 1 || p > 65535 {
				return "", fmt.Errorf("port must be between 1 and 65535")
			}
			port = p
		}
		// Tool-layer port policy (redesign-2026-08-19 THREAT-MODEL T3):
		// refuse BEFORE minting any URL. The proxy layer refuses again —
		// either layer alone is a single point of failure. Generic message:
		// no topology disclosure at this boundary either.
		if port < 1024 {
			return "", fmt.Errorf("port %d is not available for dev preview (privileged ports are refused)", port)
		}
		if _, denied := devPreviewDeniedPorts[port]; denied {
			return "", fmt.Errorf("port %d is not available for dev preview", port)
		}
		path := "/"
		if p, ok := args["path"].(string); ok && p != "" {
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
			path = p
		}
		return mcpDevPreviewURL(port, path)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func mcpSessionList(ctx context.Context, password string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("%s/session", getAgentAddr()), nil)
	if err != nil {
		return "", fmt.Errorf("failed to build session list request: %w", err)
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("opencode returned status %d for session list", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return string(body), nil
}

func mcpSessionRead(ctx context.Context, password, sessionID string, limit int) (string, error) {
	url := fmt.Sprintf("%s/session/%s/message?limit=%d", getAgentAddr(), sessionID, limit)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		// A malformed sessionID (spaces, control chars) makes the URL
		// unparseable; req is nil and SetBasicAuth would panic.
		return "", fmt.Errorf("failed to build session read request: %w", err)
	}
	req.SetBasicAuth(agentd.AuthUsername, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to read session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("opencode returned status %d for session read", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return string(body), nil
}

func mcpDevPreviewURL(port int, path string) (string, error) {
	workspaceID := os.Getenv("WORKSPACE_ID")
	apiURL, err := mcpPublicAPIOrigin()
	if err != nil {
		return "", err
	}

	// First line is a machine-readable marker the chat UI keys on to
	// render an open-preview button; everything after is for humans.
	// The sentinel is namespaced (LSP_), versioned (V1), and structured
	// (key=value) so it cannot collide with organic tool output that
	// merely mentions "dev preview". If the button does not render
	// (older UI), the markdown link on line 2 still carries the URL.
	if base := os.Getenv("PREVIEW_ORIGIN_BASE_DOMAIN"); base != "" {
		// Epic 68: Use port-in-subdomain format (<port>-<uuid>-preview.<baseDomain>)
		// instead of legacy format (<uuid>-preview.<baseDomain>/<port>/).
		// This fixes root-absolute URL breakage (the primary Epic 68 motivation).
		url := fmt.Sprintf("%s/api/v1/workspaces/%s/dev-preview-bootstrap/%d", apiURL, workspaceID, port)
		return fmt.Sprintf(
			"LSP_DEV_PREVIEW_V1 port=%d origin=%s\n[Open dev preview :%d](%s)\nOpens the per-workspace preview origin (workspace %s, port %d) in a new tab. Requires dev preview enabled (Workspace Settings → Dev Preview) and an owner login; a one-time bootstrap grants a 7-day preview session. The app itself must be listening on localhost:%d in the workspace.",
			port, base, port, url, workspaceID, port, port), nil
	}

	url := fmt.Sprintf("%s/api/v1/workspaces/%s/dev-preview/%d%s", apiURL, workspaceID, port, path)
	return fmt.Sprintf(
		"LSP_DEV_PREVIEW_V1 port=%d mode=path\n[Open dev preview :%d](%s)\nOpens the dev preview tunnel (workspace %s, port %d). Requires dev preview enabled (Workspace Settings → Dev Preview → Enable); otherwise the URL returns 503. The app must be listening on localhost:%d in the workspace.",
		port, port, url, workspaceID, port, port), nil
}

// mcpPublicAPIOrigin resolves the PUBLICLY REACHABLE API origin for
// user-facing dev-preview links (#1332). Resolution order:
//
//  1. LLMSAFESPACE_API_PUBLIC_URL — the dedicated public origin, wired to
//     every container that runs agentd tooling. Required in sidecar mode,
//     where LLMSAFESPACE_API_URL is deliberately the in-cluster svc
//     coordinate (the sidecar's boot phase bootstraps against it). When
//     explicitly set it MUST be public: a cluster-internal value here is
//     a misconfigured knob and fails loud rather than silently falling
//     through (the operator named the wrong origin; guessing on their
//     behalf hides it).
//  2. LLMSAFESPACE_API_URL — used when it is externally reachable; a
//     cluster-internal value (the legitimate sidecar topology) is
//     SKIPPED, not an error — the derivation below still applies.
//  3. https://api.<PREVIEW_ORIGIN_BASE_DOMAIN> — public by construction;
//     the fallback for pods that carry only the base domain.
//
// The tool's contract is a URL a browser can reach. When NO candidate
// yields a public origin the call errors naming the fix — relaying an
// .svc URL hands the user a dead link they must hand-rewrite (the
// 2026-09-10 incident's exact failure chain: the hand-rewrite dropped
// the trailing slash and relative assets resolved off the port segment).
func mcpPublicAPIOrigin() (string, error) {
	if pub := normalizeOrigin(os.Getenv("LLMSAFESPACE_API_PUBLIC_URL")); pub != "" {
		if err := assertPublicAPIOrigin(pub); err != nil {
			return "", err
		}
		return pub, nil
	}
	if api := normalizeOrigin(os.Getenv("LLMSAFESPACE_API_URL")); api != "" {
		if err := assertPublicAPIOrigin(api); err == nil {
			return api, nil
		}
		// Internal or unparseable — fall through to the derivation.
	}
	if base := os.Getenv("PREVIEW_ORIGIN_BASE_DOMAIN"); base != "" {
		return "https://api." + base, nil
	}
	return "", fmt.Errorf("dev preview URL unavailable: no publicly reachable API origin configured — %s", apiPublicOriginHint)
}

// apiPublicOriginHint is the single remediation string for every
// public-origin failure (one constant so the flag and Helm value names
// cannot drift apart across literals — review finding on the first
// iteration, where three copies had already diverged from the chart).
const apiPublicOriginHint = "set LLMSAFESPACE_API_PUBLIC_URL to the public API origin (controller flag --api-public-url / Helm value controller.apiPublicURL)"

func normalizeOrigin(raw string) string {
	return strings.TrimSuffix(strings.TrimSpace(raw), "/")
}

// assertPublicAPIOrigin rejects cluster-internal origins: Kubernetes
// service suffixes (.svc, .svc.cluster.local, .cluster.local) and the
// dotless same-namespace service form (http://api-name:8080 — a single
// DNS label is never a publicly resolvable name), localhost names
// INCLUDING RFC 6761 subdomains (foo.localhost — browsers resolve the
// whole special-use domain to loopback, so it is as internal as
// localhost itself), and loopback/RFC1918/link-local/unspecified IPs
// (v4 and v6). Hosts are compared with ONE trailing DNS dot stripped
// (the FQDN-terminus form): svc.cluster.local. is the same internal
// name, while a public example.com. stays allowed.
func assertPublicAPIOrigin(origin string) error {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Scheme == "" {
		return fmt.Errorf("dev preview URL unavailable: configured API origin %q is not a usable absolute URL — %s", origin, apiPublicOriginHint)
	}
	host := strings.TrimSuffix(u.Hostname(), ".")
	// DNS names are case-insensitive (RFC 4343) — K8s DNS constructs
	// lowercase, but the env vars this reads are operator-written:
	// LLMSAFESPACES-API.LLMSAFESPACES.SVC is the svc coordinate and must
	// refuse exactly like its lowercase form.
	host = strings.ToLower(host)
	ip := net.ParseIP(host)
	internal := host == "localhost" ||
		strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".svc") ||
		strings.HasSuffix(host, ".svc.cluster.local") ||
		strings.HasSuffix(host, ".cluster.local") ||
		(ip == nil && !strings.Contains(host, "."))
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()) {
		internal = true
	}
	if internal {
		return fmt.Errorf("dev preview URL unavailable: the API origin %q is cluster-internal and unreachable from the user's browser — %s", origin, apiPublicOriginHint)
	}
	return nil
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
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

// injectAgentdMCPServer returns the pre-marshal hook that stamps the
// platform's llmsafespaces MCP entry into agent-config.json. The entry
// carries the Basic credential because /v1/mcp enforces auth (#847); an
// empty password yields a DISABLED entry — an enabled-but-credential-less
// entry would just 401 on every JSON-RPC call, so disabling keeps
// opencode from retrying a provably unusable server. In production the
// password is never empty here: the credential-setup init script installs
// it before invoking materialize (this hook's only pre-boot caller), and
// even a failed read self-heals — ensureBootAgentConfig unconditionally
// re-stamps a credentialed entry before opencode starts.
func injectAgentdMCPServer(password string) func(map[string]json.RawMessage) {
	return func(cfg map[string]json.RawMessage) {
		mcpEntry := map[string]any{
			"type": "remote",
			"url":  fmt.Sprintf("http://127.0.0.1:%d/v1/mcp", agentd.AgentdPort),
		}
		if password != "" {
			mcpEntry["enabled"] = true
			mcpEntry["headers"] = map[string]string{
				"Authorization": "Basic " + basicAuth(password),
			}
		} else {
			mcpEntry["enabled"] = false
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
}

// mcpSecretsResync implements the secrets_resync MCP tool (US-70.3
// PR-4, design 0052 §4.7): trigger THIS workspace's on-demand
// re-materialization and report the outcome. The tool executes inside
// the pod it serves (the platform MCP entry injectAgentdMCPServer
// stamps points here), so the workspace is resolved by pod identity —
// the loopback dispatch below carries no workspace argument anywhere.
//
// Mechanism: an empty, workspace-password-authenticated POST to this
// pod's own /v1/resync-secrets — the SAME endpoint the platform's
// notify targets (server.go mounts it as "the notify-pull target and
// the secrets_resync MCP tool call[s] this"). That endpoint runs the
// v2 conditional bootstrap pull in-process (fresh SA token; the API
// recomputes the manifest and mints drift server-side) and applies with
// the pod's I15 rate limit — which the tool therefore SHARES with the
// notify path and the reconcile loop. No request body is ever sent
// (0050 finding-3).
//
// Outcome shapes:
//   - applied / not_modified → {"status", "appliedRev", "converged":true}
//   - 429                    → {"error":"rate_limited","retryAfterMs":N}
//   - failed / unreachable   → {"status":"failed","reason", converged:false,
//     "expectedRev":<the pod's anchored rev>, "pending":true} — never a
//     tool error: the reconcile loop converges the workspace (I3), and
//     expectedRev reports the revision this pod still stands at.
func mcpSecretsResync(ctx context.Context, password string) (string, error) {
	pendingReport := func(reason string) (string, error) {
		out, _ := json.Marshal(map[string]any{
			"status":      "failed",
			"reason":      reason,
			"converged":   false,
			"expectedRev": servedEnvRevAnchor(secretsEnvPathFromEnv()),
			"pending":     true,
		})
		return string(out), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		resyncBaseURL()+"/v1/resync-secrets", http.NoBody)
	if err != nil {
		return pendingReport("resync_request_unbuildable")
	}
	req.SetBasicAuth(agentd.AuthUsername, password)

	// Bounded wait: the resync budget plus margin, so a hung endpoint
	// cannot wedge the agent's tool call forever.
	client := &http.Client{Timeout: mcpResyncHTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return pendingReport("resync_unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch resp.StatusCode {
	case http.StatusOK:
		var result struct {
			Status     string `json:"status"`
			AppliedRev string `json:"appliedRev"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return pendingReport("resync_response_unparseable")
		}
		converged := result.Status == "applied" || result.Status == "not_modified"
		out, _ := json.Marshal(map[string]any{
			"status":     result.Status,
			"appliedRev": result.AppliedRev,
			"converged":  converged,
		})
		return string(out), nil

	case http.StatusTooManyRequests:
		var result struct {
			RetryAfterMs float64 `json:"retryAfterMs"`
		}
		_ = json.Unmarshal(body, &result)
		if result.RetryAfterMs <= 0 {
			// The endpoint did not say; the shared floor (same process,
			// same env) is the honest upper bound.
			result.RetryAfterMs = float64(resyncMinIntervalFromEnv().Milliseconds())
		}
		out, _ := json.Marshal(map[string]any{
			"error":        "rate_limited",
			"retryAfterMs": result.RetryAfterMs,
		})
		return string(out), nil

	default:
		var result struct {
			Reason string `json:"reason"`
			Error  string `json:"error"`
		}
		_ = json.Unmarshal(body, &result)
		reason := result.Reason
		if reason == "" {
			reason = result.Error
		}
		if reason == "" {
			reason = fmt.Sprintf("http_%d", resp.StatusCode)
		}
		return pendingReport(reason)
	}
}
