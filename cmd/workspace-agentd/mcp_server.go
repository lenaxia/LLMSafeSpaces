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

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
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
					{
						Name:        "rename_session",
						Description: "Rename a session in this workspace — your own current conversation or a past one (IDs from session_list). Use when a session's purpose has crystallized and the auto-generated title no longer describes it: after a pivot mid-task, when the user asks to rename, or to keep history navigable (titles are what you and the user scan in the session list). Pick short, specific, human-scannable titles (\"Fix: PVC subPath mounts\", not \"Task\"). Not for: renaming the workspace itself (rename_workspace), or creating sessions (create_session).",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"session_id": map[string]any{"type": "string", "description": "The session to rename (from session_list, or your own session)"},
								"title":      map[string]any{"type": "string", "description": "The new title (max 200 chars)"},
							},
							"required": []string{"session_id", "title"},
						},
					},
					{
						Name:        "rename_workspace",
						Description: "Rename THIS workspace — the container/label the user sees in their workspace list (not the session, not a file). Use when the user asks to rename the workspace, or when the work has outgrown the original name (a \"quick fix\" that became a refactor). Only affects this workspace's display name; nothing about the filesystem, credentials, or sessions changes. The pod's platform identity authenticates the call — no input identifies the workspace, and a pod can only ever rename itself.",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{"type": "string", "description": "The new workspace display name (max 255 chars)"},
							},
							"required": []string{"name"},
						},
					},
					{
						Name:        "call_with_model",
						Description: "Make ONE single-shot LLM call with a different model than the session's, returning its text response — the exchange lands in THIS conversation as the tool call + result (like every tool), in line with your turn. A clean carrier session runs the call and is deleted afterward, leaving history clean; the carrier is required, not a quirk: a session running a turn (yours, by definition, while a tool executes) BLOCKS incoming messages, so the call must run on an idle session. Use when a capability would serve one sub-task better than switching the whole session's model: a VISION model to interpret images (pass their workspace paths in images — bytes ride the call as attachments), a long-context model to digest a huge file, a fast/cheap model to draft or classify, a second opinion. The model does not inherit your conversation — only your prompt (and images) crosses over — though it does receive the workspace agent's standard setup, so include every bit of context the call needs explicitly. Prefer switching the session model (or asking the user to) when the capability is needed for the ongoing conversation rather than one call.",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"prompt": map[string]any{"type": "string", "description": "The complete prompt for the one-shot call — the target model has no tools and no conversation context"},
								"model":  map[string]any{"type": "string", "description": "Target model as provider/model (e.g. \"anthropic/claude-sonnet-4-5\"). Must be a provider configured in this workspace"},
								"images": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional workspace file paths of images to show the model (png/jpg/jpeg/gif/webp, 5 MiB each, 8 MiB combined). Vision-incapable models are refused up front"},
							},
							"required": []string{"prompt", "model"},
						},
					},
					{
						Name:        "create_session",
						Description: "Create a NEW top-level agent session in this workspace — a peer of yours — and hand it a starting prompt, fire-and-forget: returns the session_id immediately while the first turn runs in the background. The result does NOT come back to you — create_session never returns the session's output to this thread. Use it ONLY when: (1) the task is independent and you will NOT depend on its result (true fire-and-forget — launch it and move on), or (2) the task will require HUMAN input or is intended for human consumption — the session appears in the workspace's session list, where a person can open it, answer its questions, and steer it. NOT for fully autonomous work whose outcome you need (implementing a user story, an investigation, any question you need answered): use the task tool instead — it blocks and returns the result to this thread. The new session is a full agent sharing this workspace's files and tools — coordinate via files, not assumptions, and make the prompt self-contained (it does not inherit this conversation's context). Include the returned session_id in the child's prompt when you can: a session that knows its own ID can attribute its send_message traffic without looking it up. Track it later with session_list / session_read / session_metadata. Delivery is not retried: if the agent restarts mid-turn the prompt is lost — re-send by reading the session and continuing it.",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"prompt": map[string]any{"type": "string", "description": "The first prompt for the new session — self-contained: same workspace files, none of this conversation's context. Its output will NOT return to you."},
								"title":  map[string]any{"type": "string", "description": "Optional title (max 200 chars); omitted = auto-generated"},
							},
							"required": []string{"prompt"},
						},
					},
					{
						Name:        "send_message",
						Description: "Send a text message to another session in this workspace (IDs from session_list / session_metadata), fire-and-forget: the message is delivered and the target session's reply — if any — stays in THAT session; nothing returns to you. Read the target later with session_read if you need its response. Every delivered message carries your origin as a return address (visible to the recipient as \"message from session …\") so they know who to reply to — reply via send_message to that origin. The origin is metadata, not authentication: claiming another session's ID grants nothing (all sessions share one credential) — it exists so the receiving thread knows where to direct its response. The platform stamps your session ID automatically; pass from_session_id (your own session, findable via session_metadata — your title identifies you) ONLY as a fallback when the result says the platform could not inject it. Use to steer or follow up on sessions you created (create_session), to hand work to an idle session, or to answer a question another session's agent asked you in its transcript. Busy targets queue the message server-side and deliver it the moment their current turn ends (status says delivering_after_current_turn). Sending to your OWN current session schedules the message as your next turn after this one completes — a self follow-up, not mid-turn injection — and the result carries a loud warning field when target==your own session (self-addressed sends are a common misfire; intentional self-notes may carry on). If the session_id value you emit accidentally pastes a duplicated-argument fragment (a valid TARGET id followed by a ,\"lsp_injected_session\":\"…\" JSON tail — a known emission slip), the server RECOVERS the leading id and delivers there, with a loud warning: pass clean single ids (copy from session_list / session_metadata). The message must be self-contained either way: the target does not inherit this conversation's context. Delivery is not retried: if the workspace restarts while a message waits or before it lands, it is lost — re-send. Not for: questions you need answered in THIS thread (use the task tool, which blocks and returns the result), or starting a new session (create_session).",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"session_id":      map[string]any{"type": "string", "description": "The target session (from session_list / session_metadata)"},
								"message":         map[string]any{"type": "string", "description": "The message text — self-contained: the target does not inherit this conversation's context"},
								"from_session_id": map[string]any{"type": "string", "description": "Optional fallback: YOUR own session ID, used only when the platform could not inject it automatically (the result tells you). Find yours via session_metadata — your title identifies you. Lets the recipient reply to you."},
							},
							"required": []string{"session_id", "message"},
						},
					},
					{
						Name:        "trigger_list",
						Description: "List YOUR automation triggers (cron schedules + webhooks) - the workspace owner's, resolved from this pod's identity. Each entry carries sourceType/sourceConfig, target workspace/workflow, enabled, consecutiveFailures, lastFiredAt/nextFireAt. Start here before create/update: live entries show the exact body shapes. Pair with trigger_fires for why a trigger is misbehaving.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						Name:        "trigger_create",
						Description: "Create an automation trigger owned by this workspace's user. sourceType is cron or webhook; sourceConfig carries the source's config (cron: expr, five-field cron syntax, + optional tz as an IANA name - both are validated, invalid schedules are rejected with 400). Without a workflow target it fires a routine - a single agent turn (prompt, optional agent/script) IN THIS WORKSPACE (the platform forces this workspace as the target - you cannot schedule work into other workspaces). To fire a DAG instead, set workflowId (camelCase!) to a workflow whose targetWorkspaceId is THIS workspace - the platform enforces that scope. `input` (object) is the static run input for workflow-mode triggers, validated against the workflow's inputSchema when `inputFrom` is `mapped`; `inputFrom` selects what a fired run's input is: `envelope` (default — the system envelope {source, received_at, headers, body}), `body` (webhook only — the posted payload becomes the run input), or `mapped` (the static `input` document). Wiring an envelope-mode trigger to a workflow whose schema requires non-envelope fields is rejected with 400 — set `input`, use `inputFrom: \"body\"`, or relax the schema. A new or rescheduled trigger does NOT fire immediately: scheduling starts at the next occurrence of the schedule. autoDisableAfter N consecutive failures disables the trigger - find failures via trigger_fires.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"trigger": map[string]any{"type": "object", "description": "The trigger body - same shape as trigger_list entries minus server fields. Minimum: name, sourceType, sourceConfig; plus prompt (routine) or workflowId (DAG, camelCase)"},
						}, "required": []string{"trigger"}},
					},
					{
						Name:        "trigger_update",
						Description: "Partially update one trigger (id + the fields to change; omitted = keep existing). sourceType is immutable after create. `inputFrom` and `input` (the trigger_create input-mapping fields) are patchable here too. Typical: flip enabled after a fire-storm, tweak a cron schedule, fix a prompt.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id":    map[string]any{"type": "string", "description": "Trigger ID (from trigger_list)"},
							"patch": map[string]any{"type": "object", "description": "Fields to change (UpdateTriggerRequest shape)"},
						}, "required": []string{"id", "patch"}},
					},
					{
						Name:        "trigger_delete",
						Description: "Delete one trigger permanently. Disarm-first alternative for temporary pauses: trigger_update {enabled:false}.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id": map[string]any{"type": "string", "description": "Trigger ID (from trigger_list)"},
						}, "required": []string{"id"}},
					},
					{
						Name:        "trigger_fires",
						Description: "The debugging gold: a trigger's fire audit - per-fire status, the input envelope (cron render / webhook body), error payloads, and the consecutive-failure trail behind consecutiveFailures/auto-disable. A `validation_error` fire means the resolved run input failed the workflow's inputSchema: its actionResult carries the typed schema_mismatch violations (locations only, never payload values) and no run was queued. Use when a trigger 'is not working': this says whether it fired, what it saw, and why it failed.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id": map[string]any{"type": "string", "description": "Trigger ID (from trigger_list)"},
						}, "required": []string{"id"}},
					},
					{
						Name:        "trigger_rotate_webhook_secret",
						Description: "Rotate a webhook trigger's HMAC signing secret. Returns {webhookSecret, webhookUrl} — hand the secret to the EXTERNAL sender; sign deliveries as X-Hub-Signature-256: sha256=<hex hmac of the raw body>. Rotating invalidates the previous secret immediately. Use after trigger_create for a webhook source (create does NOT return a secret) or whenever a secret may have leaked.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id": map[string]any{"type": "string", "description": "Trigger ID (from trigger_list)"},
						}, "required": []string{"id"}},
					},
					{
						Name:        "workflow_list",
						Description: "List YOUR workflows (DAG specs) - the workspace owner's. Entries show the full spec (nodes, edges, input schema) - the reference shape for workflow_create.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
					},
					{
						Name:        "workflow_create",
						Description: "Create a workflow (DAG spec) owned by this workspace's user. specYaml is a STRINGIFIED spec - a JSON object string or YAML text (single document; the server converts YAML to the canonical JSON spec) - passed through to the platform verbatim - learn exact shapes from workflow_list entries. Node vocabulary is exactly four (validated): script, agent, http, condition. Script node data: {language: \"python\" or \"node\", handler: source string defining a handler(input) -> dict function} - NOT a shell command. Agent node prompts support {{.path}} placeholders filled from the node input: top-level keys render exactly, dotted paths ({{.body.topic}}) walk nested objects (webhook runs put the payload under body; hyphens allowed); scalars render bare, objects/arrays as JSON; unresolvable refs stay literal. Set targetWorkspaceId (this workspace) or runs are rejected with 'workspace_id is required'. inputSchema (JSON Schema) is enforced on manual runs and on fired runs that carry input mapping (`input`/`inputFrom`). Wire triggers via trigger_create {workflowId} or fire manually with workflow_run.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"workflow": map[string]any{"type": "object", "description": "The workflow body - same shape as workflow_list entries minus server fields"},
						}, "required": []string{"workflow"}},
					},
					{
						Name:        "workflow_update",
						Description: "Partially update one workflow (id + fields to change; omitted = keep existing). Patch shape = UpdateWorkflowRequest: specYaml (the DAG as a STRINGIFIED spec - JSON object string or YAML text - re-validated, node types script/agent/http/condition), inputSchema (JSON Schema, compile-checked and enforced on workflow_run inputs), targetWorkspaceId (set this workspace or runs are rejected), defaults, status, name/slug/description. Spec re-validation fails closed with per-node details.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id":    map[string]any{"type": "string", "description": "Workflow ID (from workflow_list)"},
							"patch": map[string]any{"type": "object", "description": "Fields to change (UpdateWorkflowRequest shape)"},
						}, "required": []string{"id", "patch"}},
					},
					{
						Name:        "workflow_delete",
						Description: "Delete one workflow permanently. Triggers referencing it will fail to fire - check trigger_list first.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id": map[string]any{"type": "string", "description": "Workflow ID (from workflow_list)"},
						}, "required": []string{"id"}},
					},
					{
						Name:        "workflow_run",
						Description: "Manually fire a workflow NOW (the test loop's fire button): starts a run with your input object (must satisfy the workflow's inputSchema) and returns the run. Poll status via workflow_runs.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id":    map[string]any{"type": "string", "description": "Workflow ID (from workflow_list)"},
							"input": map[string]any{"type": "object", "description": "The run's input object (validated against the workflow's inputSchema)"},
						}, "required": []string{"id"}},
					},
					{
						Name:        "workflow_runs",
						Description: "A workflow's run history: statuses (running/succeeded/failed), error codes + payloads, started/finished times. The debugging read for a failing DAG - pair with trigger_fires when the run was trigger-fired.",
						InputSchema: map[string]any{"type": "object", "properties": map[string]any{
							"id": map[string]any{"type": "string", "description": "Workflow ID (from workflow_list)"},
						}, "required": []string{"id"}},
					},
					{
						Name:        "abort_session",
						Description: "Stop a session's current turn (IDs from session_list / session_metadata — the busy ones). The turn ends immediately; the session's history and recorded work are kept — only the in-flight generation is cut. Use for cross-session management: a runaway or wrong-direction session you started (create_session / send_message), or stopping work that is no longer needed so it stops consuming tokens. Abort stops the in-flight turn DESTRUCTIVELY: any message queued for the target (e.g. a send_message still waiting for its turn to end) may be dropped — after aborting, re-send anything that mattered. Aborting an idle session is a harmless no-op. Sending another message afterwards (send_message) starts a new turn as usual. Not for: your own current session (you cannot abort your way out of this turn — finish it), or deleting history (compact summarizes; sessions are never deleted through these tools).",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"session_id": map[string]any{"type": "string", "description": "The session whose current turn should be stopped"},
							},
							"required": []string{"session_id"},
						},
					},
					{
						Name:        "get_datetime",
						Description: "Get the current date and time — in UTC and in the USER's timezone (IANA zone name and UTC offset included). The user's zone comes live from their browser when connected (source: browser); pass an explicit `timezone` argument (IANA name like \"America/Los_Angeles\") to convert for any other zone or when no browser is connected (source: argument); without either you get the pod's zone (source: pod — usually UTC). Use before any timestamp-sensitive work: scheduling, log correlation, interpreting relative times in user requests (\"yesterday\", \"next week\", \"this afternoon\"), file timestamps, or when the user asks for the time. Do not assume the user's zone matches the pod's. When source is not browser and the user's zone matters, ask or infer from context and pass it explicitly.",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"timezone": map[string]any{"type": "string", "description": "Optional IANA timezone name (e.g. \"America/Los_Angeles\") to convert for; default = the user's browser zone when known, else the pod's zone"},
							},
						},
					},
					{
						Name:        "session_metadata",
						Description: "Read-only vitals for this workspace's sessions: per-session message count, how full the context window is (tokens used vs the model's limit), total token usage, age, model, busy flag — plus the workspace ID and agent version. Omit session_id for all sessions, or pass one to zoom in. Use when deciding whether to compact (context fill high), whether to spawn a parallel session (who is busy with what), before long work that might exhaust context, or when the user asks how big/old/costly a session is. Output is aggregate metadata only — read message CONTENT with session_read.",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"session_id": map[string]any{"type": "string", "description": "Optional: one session's metadata (from session_list); omitted = every session"},
							},
							"required": []string{},
						},
					},
					{
						Name:        "compact",
						Description: "Compact a session's history — the model summarizes the conversation and the summary replaces it in the context window, freeing context without losing the thread. Omit session_id to target the single currently-running session (usually your own — find IDs with session_metadata otherwise). Compaction preserves file changes and tool history on disk; only the in-context view shrinks. If the target is busy (your own session always is while a tool runs), the compaction is SCHEDULED and runs exactly when the current turn ends — plan around the context only being freed from your NEXT turn. Optional model overrides the summarizing model (default: the session's current model).",
						InputSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"session_id": map[string]any{"type": "string", "description": "Optional: the session to compact; omitted = the single busy session (your own when unambiguous)"},
								"model":      map[string]any{"type": "string", "description": "Optional provider/model to summarize with (default: the session's current model)"},
							},
							"required": []string{},
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
	case "rename_session":
		sessionID, _ := args["session_id"].(string)
		title, _ := args["title"].(string)
		return mcpRenameSession(ctx, password, sessionID, title)
	case "rename_workspace":
		// The workspace identity comes from pod env, never from tool
		// arguments (mirrors secrets_resync's no-identity-input rule).
		name, _ := args["name"].(string)
		return mcpRenameWorkspace(ctx, name)
	case "call_with_model":
		prompt, _ := args["prompt"].(string)
		model, _ := args["model"].(string)
		var images []string
		if raw, ok := args["images"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					images = append(images, s)
				}
			}
		}
		return mcpCallWithModel(ctx, password, prompt, model, images)
	case "create_session":
		prompt, _ := args["prompt"].(string)
		title, _ := args["title"].(string)
		return mcpCreateSession(ctx, password, prompt, title)
	case "send_message":
		sessionID, _ := args["session_id"].(string)
		message, _ := args["message"].(string)
		// Hybrid origin (#1469 ruling): lsp_injected_session is the
		// platform plugin's harness-attested injection (never
		// schema-advertised, never model-supplied by construction);
		// from_session_id is the optional self-declared fallback the
		// model may supply when injection is absent. mcpSendMessage
		// prefers injection, validates by-ID either way, and labels
		// the mode in the sentinel and the result.
		injected, _ := args["lsp_injected_session"].(string)
		declared, _ := args["from_session_id"].(string)
		return mcpSendMessage(ctx, password, sessionID, message, injected, declared)
	case "abort_session":
		sessionID, _ := args["session_id"].(string)
		return mcpAbortSession(ctx, password, sessionID)
	case "trigger_list", "trigger_create", "trigger_update", "trigger_delete", "trigger_fires",
		"trigger_rotate_webhook_secret",
		"workflow_list", "workflow_create", "workflow_update", "workflow_delete", "workflow_run", "workflow_runs":
		return mcpAutomation(ctx, name, args)
	case "get_datetime":
		timezoneArg, _ := args["timezone"].(string)
		return mcpGetDatetime(timezoneArg)
	case "session_metadata":
		sessionID, _ := args["session_id"].(string)
		return mcpSessionMetadata(ctx, password, sessionID)
	case "compact":
		sessionID, _ := args["session_id"].(string)
		model, _ := args["model"].(string)
		return mcpCompact(ctx, password, sessionID, model)
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
	body, err := seamClientWithPassword(password).SessionListRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	return string(body), nil
}

func mcpSessionRead(ctx context.Context, password, sessionID string, limit int) (string, error) {
	body, _, err := seamClientWithPassword(password).SessionMessagesRaw(ctx, sessionID, limit, "")
	if err != nil {
		return "", fmt.Errorf("failed to read session: %w", err)
	}
	// #1307: image data URLs never cross this surface either — strip
	// them to metadata + omission markers (the seam owns the wire rule).
	return string(opencode.StripImageDataURLs(body)), nil
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

// platformPluginPath is the origin-injection plugin's in-pod path: the
// opencode overlay image volume is mounted read-only at /opencode, and
// the plugin ships inside it at /plugins (design 0053 §4.2, #1469) —
// version-coupled to the pinned binary by construction.
const platformPluginPath = "/opencode/plugins/llmsafespaces-origin.js"

// platformPluginSpec is the config `plugin:` entry form: a file:// URL
// imports in place from the read-only mount (no install, no user-space
// dependence — verified against the pinned harness, #1469).
const platformPluginSpec = "file://" + platformPluginPath

// injectPlatformAgentConfig returns the pre-marshal hook that stamps
// BOTH platform entries into agent-config.json:
//
//   - the llmsafespaces MCP server entry (Basic credential because
//     /v1/mcp enforces auth, #847; an empty password yields a DISABLED
//     entry — an enabled-but-credential-less entry would just 401 on
//     every JSON-RPC call, so disabling keeps opencode from retrying a
//     provably unusable server).
//   - the origin-injection plugin (#1465): appended to the plugin
//     array, CONCAT + DEDUP — user-staged plugins are preserved (the
//     writer re-emits them via pluginRaw; the harness would dedup on
//     its own, but this file is rebuilt whole, so the merge is ours).
//
// In production the password is never empty here: the credential-setup
// init script installs it before invoking materialize (this hook's only
// pre-boot caller), and even a failed read self-heals —
// ensureBootAgentConfig unconditionally re-stamps a credentialed entry
// before opencode starts.
func injectPlatformAgentConfig(password string) func(map[string]json.RawMessage) {
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
			} else {
				mcpMap := map[string]json.RawMessage{"llmsafespaces": entryJSON}
				merged, _ := json.Marshal(mcpMap)
				cfg["mcp"] = merged
			}
		} else {
			mcpMap := map[string]json.RawMessage{"llmsafespaces": entryJSON}
			merged, _ := json.Marshal(mcpMap)
			cfg["mcp"] = merged
		}

		injectPlatformPlugin(cfg)
	}
}

// injectPlatformPlugin appends the origin-injection plugin to the
// config's plugin array, preserving user entries and deduping ours.
func injectPlatformPlugin(cfg map[string]json.RawMessage) {
	var plugins []string
	if existing, ok := cfg["plugin"]; ok {
		_ = json.Unmarshal(existing, &plugins)
	}
	for _, p := range plugins {
		if p == platformPluginSpec {
			return
		}
	}
	plugins = append(plugins, platformPluginSpec)
	merged, err := json.Marshal(plugins)
	if err != nil {
		return
	}
	cfg["plugin"] = merged
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
