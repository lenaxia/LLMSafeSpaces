package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/lenaxia/llmsafespaces/pkg/utilities"
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
	Name        string `json:"name"`
	Description string `json:"description"`
	// InputSchema is `any` (not map[string]any): JSON object key order on
	// the wire matters for emission reliability — glm-class models latch
	// into duplication loops when a long freeform string (message, content)
	// is emitted with structural keys still owed AFTER it (#1849 factorial:
	// platform tools put long strings last and never corrupt; send_message
	// put `message` first and corrupted 5/5). Map marshaling is alphabetical
	// and cannot express "string field last", so order-sensitive schemas are
	// supplied as pre-ordered json.RawMessage.
	InputSchema any `json:"inputSchema"`
	// Annotations: the MCP spec's behavioral hints (readOnlyHint,
	// destructiveHint, idempotentHint, openWorldHint) — clients MUST
	// treat them as untrusted disclosure, which is exactly their use
	// here: telling the harness which tools mutate or destroy state
	// so it can gate/surface them without parsing descriptions.
	Annotations *mcpToolAnnotations `json:"annotations,omitempty"`
	// OutputSchema: when a tool's result is a stable JSON object, the
	// schema declares it (MCP 2026-07-28: results then carry a
	// structuredContent twin validated against this schema, alongside
	// the TextContent form for compatibility).
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

// mcpToolAnnotations mirrors the spec's ToolAnnotations. Only hints
// that are TRUE are set; omission means "not claimed" (the client
// treats every hint as untrusted anyway).
type mcpToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

func boolPtr(b bool) *bool { return &b }

// annot builds tool annotations. Every tool in this catalog operates
// on closed workspace state (no open-world interaction), so
// openWorldHint:false rides along whenever any hint is declared.
func annot(readOnly, destructive, idempotent bool) *mcpToolAnnotations {
	return &mcpToolAnnotations{
		ReadOnlyHint:    &readOnly,
		DestructiveHint: &destructive,
		IdempotentHint:  &idempotent,
		OpenWorldHint:   boolPtr(false),
	}
}

// maxMCPBodyBytes bounds a /v1/mcp request body (#1561's socialized
// cap): every tool on this wire is metadata-scale — arguments are
// text/ids, not file payloads (uploads own their own leg).
const maxMCPBodyBytes = 1 << 20

// decodeOneDocument decodes exactly ONE JSON document, tolerating a
// whitespace-only remainder (every curl caller appends a newline); any
// other trailing data is an error (#1561: Decode's one-value
// semantics silently skipped it) naming the offset AFTER the first
// document — the scan's start, exact for every shape. The trailing
// error wraps its underlying cause (%w) so a cap trip in the scan
// (http.MaxBytesError) still classifies as 413 at the handler. It is
// the shared inbound-JSON boundary for agentd's HTTP wires (#1565:
// /v1/mcp, /v1/workflow/node/execute, /v1/user-timezone). The
// outbound twin (client.go's decodeStrict) keeps its own error
// contract.
func decodeOneDocument(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(v); err != nil {
		return err
	}
	end := dec.InputOffset()
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		if err != nil {
			return fmt.Errorf("trailing data after offset %d (one JSON document per request): %w", end, err)
		}
		return fmt.Errorf("trailing data after offset %d (one JSON document per request)", end)
	}
	return nil
}

// mcpToolCatalog is the tools/list registry — every agent-facing
// tool this workspace serves. Extracted from mcpHandler (funlen);
// the descriptions are the agent's only documentation — the
// ToolDescriptionGuidance test pins them.
// boardManagerTools returns the #1747 Task Board MANAGER catalog
// entries (extracted from mcpToolCatalog — funlen): the full manager
// set at project/epic/task level, open to ANY in-workspace session
// (the #1747 §5 manager grant is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates).
func boardManagerTools() []mcpTool {
	return []mcpTool{
		{
			// #1837 P2a: the orchestrator's overview reads. board_read
			// stays per-task (the worker view); board_list/board_health
			// are the manager's project/fleet projections.
			Name:        "board_list",
			Annotations: annot(true, false, false),
			Description: "List EVERY task of one board project in a single call (#1837 P2a) — the orchestrator's overview the per-task board_read cannot give. Each row carries the stage, claim (assignee session), lease expiry, paused, blocked, attempts, and cooldown columns (live claim TOKENS are deliberately absent from the bulk projection — board_read keeps the token for the claimant's own flow); the response also derives the project's ready HINT (backlog + unblocked + unpaused + not cooling — the task-level claim predicates; epic-quota and project gates resolve at claim time; rendered only on fully unfiltered lists). The assignee_session column is a RESOLVABLE session ref (owner ruling 2026-10-29): the 3-word phrase when the session index knows the worker, else the raw harness id (assignee_session_deprecated) — both forms address session_read / session_metadata when you need to inspect the session behind a claim. Workspace-scoped (a foreign workspace's project is a plain 404); open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"projectId": map[string]any{"type": "string", "description": "Project short id (prj-… or UUID)"},
				"stage":     map[string]any{"type": "string", "enum": []string{"backlog", "in_progress", "in_review", "done", "failed", "pending_input"}, "description": "Optional stage filter (wire set; pending_input = the flag filter — rows awaiting input on any real stage, 000050)"},
				"failed":    map[string]any{"type": "string", "enum": []string{"true", "false"}, "description": "Optional #1813 failed-attempt filter (true = only last-attempt-failed rows)"},
			}, "required": []string{"projectId"}},
		},
		{
			Name:        "board_health",
			Annotations: annot(true, false, false),
			Description: "The WORKSPACE's board health snapshot (#1837 P2a): active leases with expiry and assignee, task and project-slot cooldowns, pending_input counts by origin, recent reclaims, and per-project worker slots in use — one call over every board this workspace's projects carry. Use it to answer 'what is in flight, what is stuck, what cools down' before scheduling decisions. Workspace-scoped; open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "board_get_config",
			Annotations: annot(true, false, false),
			Description: "Read the Task Board's configuration — the effective merged view with SOURCE annotations (per key: code_default | user_default | project_override) and per-stage persona provenance (project_override | user_default | default_reviewer | none_explicit | none, the persona included). Personas are ROLE REFERENCES ONLY (owner ruling 2026-10-29): each stagePersonas entry renders as {stage: role-ref} — the bare id/slug of the referenced platform agent role (the spawned worker's model/tools/system-prompt come from it; the platform seeds a built-in reviewer role carrying the default reviewer's verdict-channel charter as its system prompt). No projectId → the user tier (board-wide defaults: reviewColumn, autoAssignReviewer, defaultWorkerSlots, stagePersonas); a projectId → that project's view (its raw override map plus the merged row). The same truth the board UI's config panels render. READ: open to any in-workspace session (the #1747 §5 manager-grant repeal); no claim token, no gate.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"projectId": map[string]any{"type": "string", "description": "Optional project short id (prj-… or UUID) — omit for the user tier's board-wide defaults"},
			}},
		},
		{
			Name:        "board_set_config",
			Description: "REPLACE-write one tier of the Task Board's configuration. tier=user sets the board-wide defaults ({reviewColumn?, autoAssignReviewer?, defaultWorkerSlots?, stagePersonas?}); tier=project sets ONE project's override map (projectId required; defaultWorkerSlots refuses — a user-level key). stagePersonas values are ROLE REFERENCES ONLY (owner ruling 2026-10-29): a bare role id/slug string (\"in_review\": \"reviewer\" — canonical) or {roleId} — the platform validates each reference against the available agent-role set (a mismatch refuses); name/instructions shapes reject (the board defines no behavior of its own — roles do). Absent keys revert to inheritance, exactly the REST contract: an explicit stagePersonas map OWNS every stage it does not list ({} = the no-personas opt-out, suppressing even the built-in auto-reviewer); a map without an in_review entry suppresses the default reviewer — add its persona back if that is not intended. PAYLOAD IS FILE-ONLY (design 0070 cutover): write the config JSON with the write tool, check it with board_validate_file (op set_config), then submit configFile — the file is DELETED after a successful set. The response is board_get_config's source-annotated view of the written tier. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: json.RawMessage(`{"properties":{"tier":{"description":"Which tier this set writes","enum":["user","project"],"type":"string"},"projectId":{"description":"REQUIRED for tier=project (refused for tier=user) — the project short id whose override map is written","type":"string"},"configFile":{"description":"REQUIRED — workspace file holding the config override JSON {reviewColumn?, autoAssignReviewer?, defaultWorkerSlots?, stagePersonas?} (relative paths resolve against the workspace root; tier/projectId ride the tool call, not the file)","type":"string"}},"required":["tier","configFile"],"type":"object"}`),
		},
		{
			// Task Board MANAGER tools (#1747, design 0067 §4.3c as
			// amended): the collaboration-to-execution loop — take a
			// user-approved design, create the epics/tasks, activate
			// the epic, and the engine's claim-then-spawn executes. The
			// #1747 §5 manager grant is REPEALED (ruling 2026-10-27 /
			// tsk-copper-refectory-generates): every manager tool opens
			// to ANY in-workspace session — a workspace pod is
			// single-tenant, all sessions share one credential, and the
			// claim-token fence is the real protection. The calling
			// session's identity rides the wire as audit attribution
			// only. claim_next is engine-internal forever; task_start
			// is owner-surface only; cancel is ELIMINATED (delete
			// replaces it); ordering is dependency-only FIFO — no
			// priority fields exist.
			Name:        "board_create_project",
			Description: "Create a Task Board project in THIS workspace: a swimlane with a worker-slot budget (max concurrent in-flight workers, default 2). The project targets your own workspace by construction — you manage THIS workspace's board only (ruling #1747). Use when the user wants a new board/campaign for structured multi-agent work. Not for: repeated scheduled jobs (create a workflow instead — boards are one-shot sprints). Open to any in-workspace session, spawned workers included (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"name":        map[string]any{"type": "string", "description": "Project name"},
				"workerSlots": map[string]any{"type": "integer", "description": "Max concurrent in-flight claims (default 2)"},
				"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags"},
			}, "required": []string{"name"}},
		},
		{
			Name:        "board_update_project",
			Description: "Patch a board project (name, worker slots, tags). A pod cannot retarget a project's workspace — it is bound to this workspace structurally (ruling #1747). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"projectId":   map[string]any{"type": "string", "description": "Project short id (prj-… or UUID)"},
				"name":        map[string]any{"type": "string", "description": "New name (rename cascades display inheritance)"},
				"workerSlots": map[string]any{"type": "integer", "description": "Worker-slot budget"},
				"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Tags (REPLACE — send the full set)"},
			}, "required": []string{"projectId"}},
		},
		{
			Name:        "board_project_archive",
			Description: "Archive a board project (reversible storage): stops ALL claims into it. Not deletion — archive_project keeps every row; unarchive restores. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"projectId": map[string]any{"type": "string", "description": "Project short id (prj-… or UUID)"},
			}, "required": []string{"projectId"}},
		},
		{
			Name:        "board_project_unarchive",
			Description: "Unarchive a board project: restores it to active (claims resume). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"projectId": map[string]any{"type": "string", "description": "Project short id (prj-… or UUID)"},
			}, "required": []string{"projectId"}},
		},
		{
			Name:        "board_project_pause",
			Description: "Pause a board project — the scheduling lever (\"pause this project so another moves forward\"): claim candidacy turns off for every task under it, in-flight workers receive HALT directives (they finish their current thought, then stop — never a mid-generation cut), and leases keep running. Use when the user wants work in this project to yield resources to another project. resume_project clears it. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"projectId": map[string]any{"type": "string", "description": "Project short id (prj-… or UUID)"},
			}, "required": []string{"projectId"}},
		},
		{
			Name:        "board_project_resume",
			Description: "Resume a paused board project: clears the pause and delivers RESUME directives to held workers; a worker whose lease lapsed while paused re-enters the FIFO pool (a fresh worker may take the task). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"projectId": map[string]any{"type": "string", "description": "Project short id (prj-… or UUID)"},
			}, "required": []string{"projectId"}},
		},
		{
			Name:        "board_create_epic",
			Description: "Create a DRAFT epic in a board project (the default state). The epic holds tasks and gates their claiming; activate it to start feeding work to fresh workers. The outcome field is the human's steering wheel — what this epic exists to achieve. PAYLOAD IS FILE-ONLY (design 0070 cutover): write the whole payload JSON {title, outcome, tags} with the write tool, check it with board_validate_file (op create_epic), then submit bodyFile — the file is DELETED after a successful create (re-write to retry; a retried create that finds no file should check board_list to confirm it landed). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: json.RawMessage(`{"properties":{"projectId":{"description":"Project short id (prj-… or UUID)","type":"string"},"bodyFile":{"description":"REQUIRED — workspace file holding the whole epic payload JSON {title, outcome, tags, requestId?} (relative paths resolve against the workspace root)","type":"string"}},"required":["projectId","bodyFile"],"type":"object"}`),
		},
		{
			Name:        "board_update_epic",
			Description: "Patch an epic (title, outcome, tags — replace-if-present semantics). The outcome is the steering wheel for every worker under the epic; a title rename cascades to its tasks' display. PAYLOAD IS FILE-ONLY (design 0070 cutover): write the patch JSON {title?, outcome?, tags?} with the write tool (board_read first for read-before-update), check it with board_validate_file (op update_epic_patch), then submit patchFile — the file is DELETED after a successful update. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: json.RawMessage(`{"properties":{"epicId":{"description":"Epic short id (epc-… or UUID)","type":"string"},"patchFile":{"description":"REQUIRED — workspace file holding the patch JSON {title?, outcome?, tags?, requestId?} (relative paths resolve against the workspace root)","type":"string"}},"required":["epicId","patchFile"],"type":"object"}`),
		},
		{
			Name:        "board_epic_activate",
			Description: "Activate an epic — the publish/start lever: ready tasks start feeding to fresh workers via the engine's claim-then-spawn. Refuses while cross-epic dependencies enter INACTIVE epics unless acknowledged (pass acknowledgeInactiveDeps after the user accepts the blockage); reports the initial ready set in the response. Held workers under the epic receive RESUME directives. Not for: assigning a specific task NOW (there is no such agent tool — the engine claims in FIFO order; ruling #1747). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"epicId":                  map[string]any{"type": "string", "description": "Epic short id (epc-… or UUID)"},
				"acknowledgeInactiveDeps": map[string]any{"type": "boolean", "description": "Acknowledge deps entering inactive epics (they stay blocking until those epics activate)"},
			}, "required": []string{"epicId"}},
		},
		{
			Name:        "board_epic_pause",
			Description: "Pause an epic: stops NEW claims for its tasks and cascades HALT directives to the epic's in-flight workers (they finish the current thought, then stop). In-flight leases are never interrupted mid-generation; forward stage-writes refuse while paused. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"epicId": map[string]any{"type": "string", "description": "Epic short id (epc-… or UUID)"},
			}, "required": []string{"epicId"}},
		},
		{
			Name:        "board_epic_archive",
			Description: "Archive an epic (reversible storage): it leaves the claimable set. Not deletion — unarchive restores active. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"epicId": map[string]any{"type": "string", "description": "Epic short id (epc-… or UUID)"},
			}, "required": []string{"epicId"}},
		},
		{
			Name:        "board_epic_unarchive",
			Description: "Unarchive an epic — always restores it to active (the resumable default). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"epicId": map[string]any{"type": "string", "description": "Epic short id (epc-… or UUID)"},
			}, "required": []string{"epicId"}},
		},
		{
			Name:        "board_delete_epic",
			Annotations: annot(false, true, false),
			Description: "Destructively delete an epic AND its tasks (#1747: cancel is eliminated — delete replaces it). The audit log keeps every removal: each deleted row's event embeds its full final-state snapshot. REFUSES while any claim under the epic is in-flight unless force=true (the delete commits, then in-flight workers are aborted — post-commit) — confirm with the user before forcing. Tasks with dependents outside the epic: those dependents lose their edges (listed in the response, each gets an unblocked_by_delete audit event — visible, never a refusal). Not for reversible shelving — archive instead. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"epicId": map[string]any{"type": "string", "description": "Epic short id (epc-… or UUID)"},
				"force":  map[string]any{"type": "boolean", "description": "Abort in-flight workers and delete anyway (confirm with the user first)"},
			}, "required": []string{"epicId"}},
		},
		{
			Name:        "board_create_task",
			Description: "Create a backlog task in an epic. The description (markdown) seeds the worker charter; acceptance criteria define Done (ALL met + evidence). ATOMIC-CREATE CONTRACT (#1849): a non-empty description AND at least one acceptance criterion are REQUIRED — a create missing either, or whose title/description/criterion is ENTIRELY a placeholder-class token (placeholder, todo, tbd, asdf, qwerty…), is rejected outright; re-compose (prose that merely mentions such a token is fine). dependsOn (short ids of existing tasks) adds prerequisite edges in the same write; ordering is dependency-only FIFO by creation — no priority field exists. PAYLOAD IS FILE-ONLY (design 0070 cutover): write the whole payload JSON {title, description, tags, acceptanceCriteria, dependsOn} with the write tool, check it with board_validate_file (op create_task), then submit bodyFile — the file is DELETED after a successful create (re-write to retry; a retried create that finds no file should check board_list to confirm it landed). This is the FLAT single-task document; for an epic plus its tasks, or SEVERAL tasks at once, use board_import_plan's planFile instead. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: json.RawMessage(`{"properties":{"epicId":{"description":"Epic short id (epc-… or UUID)","type":"string"},"bodyFile":{"description":"REQUIRED — workspace file holding the whole task payload JSON {title, description, tags, acceptanceCriteria, dependsOn, requestId?} (relative paths resolve against the workspace root)","type":"string"}},"required":["epicId","bodyFile"],"type":"object"}`),
		},
		{
			Name:        "board_update_task",
			Description: "Patch a task (title, description, tags, criteria — replace-if-present semantics; omitted fields keep existing values). setCriteriaMet is {index, met} — check off OR uncheck the criterion at the 0-based index (the reviewer's Done preparation can walk flags BOTH ways, #1837 P2b; the bare-integer form still decodes as {index, met:true}); REFUSED alongside an acceptanceCriteria replace (send one or the other); out-of-bounds indices are loud errors. THE PATCH PAYLOAD IS FILE-ONLY (design 0070 cutover): write the patch JSON {title?, description?, tags?, acceptanceCriteria?} with the write tool (board_read first — criteria REPLACE the full set), check it with board_validate_file (op update_task_patch), then submit patchFile — the file is DELETED after a successful update. patchFile is REQUIRED on the manager route; the ONE exception is the bare criteria check-off {taskId, setCriteriaMet}. #1946 scoped grant: a caller sending its charter claimToken rides the worker route and may record setCriteriaMet on its OWN claimed row ONLY (any other field with a claimToken refuses loudly); task workers self-certifying their own work stay refused (ruling 2026-10-27, tsk-copper-refectory-generates — the one surviving registry refusal).",
			InputSchema: json.RawMessage(`{"properties":{"taskId":{"description":"Task short id (phrase form tsk-crimson-finch-blooms, legacy tsk-XXXXXXXX forms still resolve, or UUID)","type":"string"},"claimToken":{"description":"The claim token from your charter envelope — present ONLY for the #1946 reviewer-persona grant: the call routes to the worker surface and may record setCriteriaMet on your OWN claimed row (any other field refuses). Omit it for the manager patch.","type":"string"},"setCriteriaMet":{"description":"{index, met} — check off OR uncheck the criterion (the legacy bare integer = check-off); refused alongside acceptanceCriteria","properties":{"index":{"description":"0-based criterion index","type":"integer"},"met":{"description":"The flag to write (default true — the legacy bare-integer form)","type":"boolean"}},"required":["index"],"type":"object"},"patchFile":{"description":"REQUIRED (manager patch) — workspace file holding the patch JSON {title?, description?, tags?, acceptanceCriteria?, requestId?} (relative paths resolve against the workspace root)","type":"string"}},"required":["taskId"],"type":"object"}`),
		},
		{
			Name:        "board_add_dependency",
			Annotations: annot(false, false, true),
			Description: "Add a structural dependency edge — the ONLY blocking mechanism: task→task within this workspace (cross-epic allowed, never cross-workspace). Cycles are rejected (409 with did-you-mean suggestions); duplicate edges are idempotent no-ops; edges per node are capped (16 default). Dependencies are SEMANTIC prerequisites — never use edges to express preference (ordering is FIFO by creation; ruling #1747). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: json.RawMessage(`{"properties":{"taskId":{"description":"Dependent task short id (tsk-… phrase/legacy form, or UUID)","type":"string"},"dependsOn":{"description":"Prerequisite task short id (tsk-… phrase/legacy form, or UUID)","type":"string"}},"required":["taskId","dependsOn"],"type":"object"}`),
		},
		{
			Name:        "board_remove_dependency",
			Annotations: annot(false, false, true),
			Description: "Remove one dependency edge. Removing an abandoned prerequisite's edge unblocks the dependent row (one of the pending_input resolutions). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: json.RawMessage(`{"properties":{"taskId":{"description":"Dependent task short id (tsk-… phrase/legacy form, or UUID)","type":"string"},"dependsOn":{"description":"Prerequisite task short id (tsk-… phrase/legacy form, or UUID)","type":"string"}},"required":["taskId","dependsOn"],"type":"object"}`),
		},
		{
			Name:        "board_delete_task",
			Annotations: annot(false, true, false),
			Description: "Destructively delete a task (#1747: cancel is eliminated — delete replaces it). An idle row goes straight; an in-flight row's worker turn is cut AFTER the delete commits (post-commit abort — the row's removal is the authority). The audit event embeds the task's full final-state snapshot. The task's edges cascade away: orphaned dependents are LISTED in the response and each gets an unblocked_by_delete audit event — they simply become ready; NO auto-reconnect ever happens (a wrong reconnect is a silent stale-chain stall). reconnect=true lets orphans inherit the deleted node's direct upstream edges (deduplicated, cycle-guarded) — only when the user's intent says so. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"taskId":    map[string]any{"type": "string", "description": "Task short id (phrase form tsk-crimson-finch-blooms, legacy tsk-XXXXXXXX forms still resolve, or UUID)"},
				"reconnect": map[string]any{"type": "boolean", "description": "Orphans inherit the deleted node's direct upstream edges (default off — the graph never rewrites itself implicitly)"},
			}, "required": []string{"taskId"}},
		},
		{
			Name:        "board_pause_task",
			Description: "Pause one task — the scheduling lever: claim candidacy off, forward stage-writes refused (409 task_paused), and a HALT directive lands on the in-flight worker (it finishes its current thought, then stops — never a mid-generation cut). The lease keeps running (LAPSE accounting): a long pause naturally reclaims; resume brings the row back. Dependencies stay blocking while paused. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"taskId": map[string]any{"type": "string", "description": "Task short id (phrase form tsk-crimson-finch-blooms, legacy tsk-XXXXXXXX forms still resolve, or UUID)"},
			}, "required": []string{"taskId"}},
		},
		{
			Name:        "board_resume_task",
			Description: "Resume a paused task: clears the flag and delivers the RESUME directive — the held session continues if its lease survived; a lapsed one re-enters the FIFO pool (a fresh worker claims it). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"taskId": map[string]any{"type": "string", "description": "Task short id (phrase form tsk-crimson-finch-blooms, legacy tsk-XXXXXXXX forms still resolve, or UUID)"},
			}, "required": []string{"taskId"}},
		},
		{
			Name:        "board_rearm_task",
			Description: "Re-pool an attempts-exhausted task (#1832 WS3): an escalated row (stage=failed, pending_origin=escalated — attempts at the cap, the engine withheld the grant) returns to backlog with attempts reset to 0 (a fresh-judgment ledger; the pre-reset count rides the rearm audit event) and is claimable immediately. Only failed/escalated rows; other stages and origins keep their enumerated transitions. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"taskId":    map[string]any{"type": "string", "description": "Task short id (phrase form tsk-crimson-finch-blooms, legacy tsk-XXXXXXXX forms still resolve, or UUID)"},
				"requestId": map[string]any{"type": "string", "description": "Optional idempotency key: reuse ONE id per logical operation across retries"},
			}, "required": []string{"taskId"}},
		},
		{
			// #1836: the bulk complete — the review-settlement sweep in
			// one call (the #1832 WS2 manager move's batch twin). The
			// bulk CLAIM variant deliberately does not exist on this
			// surface: task_start is owner-surface only (round-3 #2).
			Name:        "board_move_stage_bulk",
			Description: "Bulk stage move (#1836): one shared target stage + evidenceUrl across up to 50 tasks in ONE call — each item is one full server-arbitrated board_move_stage (Done guard — ALL criteria met + evidence for done; pause guards; claim-token rule; auto-review dispatch on toStage=in_review; the done settle cascade). Response is a per-item result array [{id, status: done|failed, error?}] — partial failures are reported per item, never all-or-nothing; one malformed id sinks only its own item. Re-submitting the batch (same requestId) is safe: an item already at the target stage returns done, not conflict. The canonical use: completing review sets (in_review → done) after a verification sweep. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"taskIds":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Task short ids (phrase form preferred; legacy forms and UUIDs resolve) — 1 to 50 items"},
				"toStage":     map[string]any{"type": "string", "enum": []string{"in_progress", "in_review", "done", "pending_input", "failed", "backlog"}, "description": "The ONE target stage every item moves to (per-item from-stage legality stays server-arbitrated; pending_input = set the flag, failed = the failure terminus — 000050)"},
				"evidenceUrl": map[string]any{"type": "string", "description": "Artifact link applied to every item's transition (required when toStage=done)"},
				"requestId":   map[string]any{"type": "string", "description": "Idempotency key: ONE per logical batch, reused across retries of the same batch — reusing a batch's key for a DIFFERENT batch replays the first batch's recorded outcomes (stale done items)"},
			}, "required": []string{"taskIds", "toStage"}},
		},
		{
			// #1836 sibling: the bulk delete — board_delete_task's batch
			// twin (delete IS a manager action; only claim/create are
			// owner-only, so unlike the bulk claim this form lives here).
			Name:        "board_delete_tasks",
			Description: "Bulk delete (#1836): the delete_task(s) batch — up to 50 destructive removals in ONE call, ONE shared reconnect flag. Each item is one full board_delete_task (its own snapshot-bearing audit event, orphan events + is_blocked rederive, post-commit worker abort for an in-flight row). Response is a per-item result array [{id, status: done|failed, orphaned_dependents?, reconnected?, error?}] — partial failures are reported per item, never all-or-nothing; one malformed id sinks only its own item. Idempotent retry for a destructive variant: re-submitting the batch is safe, and a miss, a foreign row, and an already-deleted row all render the SAME not_found item (absence is the done state — rendering it done would fabricate outcomes for foreign probes and leak foreign existence; never a conflict). Canonical use: cleanup sweeps after re-planning. Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"taskIds":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Task short ids (phrase form preferred; legacy forms and UUIDs resolve) — 1 to 50 items"},
				"reconnect": map[string]any{"type": "boolean", "description": "The ONE shared reconnect flag: orphans of every deleted item inherit that item's direct upstream edges (deduplicated, cycle-guarded) — default off"},
				"requestId": map[string]any{"type": "string", "description": "Idempotency key: ONE per logical batch, reused across retries of the same batch — reusing a batch's key for a DIFFERENT batch replays the first batch's recorded outcomes (stale done items)"},
			}, "required": []string{"taskIds"}},
		},
		{
			Name:        "board_import_plan",
			Description: "Materialize a plan atomically (#1747): ONE unified plan document (design 0070 §1a), any granularity — a NEW epic with all its tasks ({epic: {title, outcome, tags}, tasks: [1..N]}), or N tasks attached to an EXISTING epic ({epicId: \"epc-…\", tasks: [1..N]}); a single task is a one-element plan or board_create_task's flat bodyFile. EXACTLY ONE of epic|epicId (both/neither is refused); tasks address each other by payload key in dependsOn; nodes-per-epic and edges-per-node caps apply; all-or-nothing in every mode. ALWAYS dry-run first (dryRun=true): the response renders the graph, warnings, and the initial ready set — show it to the user as the plan-approval moment; write only after approval. THE PLAN IS FILE-ONLY (design 0070 cutover): write the plan JSON with the write tool, check it with board_validate_file (op import_plan — local shape checks; the dryRun covers GRAPH semantics: cycles, caps), then submit planFile — the file is DELETED after a successful non-dry run (a dry run keeps it for the real submission). Open to any in-workspace session (the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates).",
			InputSchema: json.RawMessage(`{"properties":{"projectId":{"description":"Project short id (prj-… or UUID)","type":"string"},"dryRun":{"description":"Validate + render only — nothing is written (the plan-approval moment; the plan file survives)","type":"boolean"},"planFile":{"description":"REQUIRED — workspace file holding the unified plan JSON {epic:{title,outcome,tags} | epicId, tasks:[{key,title,description,tags,acceptanceCriteria,dependsOn}], requestId?} (relative paths resolve against the workspace root)","type":"string"}},"required":["projectId","planFile"],"type":"object"}`),
		},
		{
			// design 0070 §2.5: the local, advisory pre-submit check —
			// the same validator the CRUD dispatch gate runs, as a tool
			// (write → validate → submit; validate-first is UX).
			Name:        "board_validate_file",
			Annotations: annot(true, false, false),
			Description: "Validate one board payload FILE locally before submitting it — zero wire calls, purely advisory: the platform stays the authority, and for import_plan the existing dryRun checks GRAPH semantics (cycles, caps) while this checks the file's SHAPE. The canonical workflow for board_create_task/board_create_epic/board_update_task/board_update_epic/board_import_plan/board_set_config: write the JSON payload with the write tool → board_validate_file → submit the tool call with bodyFile/patchFile/planFile/configFile (the file auto-deletes on success). Checks, in order: file discipline (regular file, 256 KiB cap, UTF-8); strict JSON (single document, duplicate keys refused, object top level); placement (file params and routing/control keys do not belong INSIDE the file); the op's compiled schema (admitted fields derived from the platform request DTOs — unknown fields, required fields, types, shapes, caps, with JSON instance paths like tasks[2].acceptanceCriteria[0].text); #1849 atomic-create (non-empty description AND >=1 criterion); the placeholder-class token scan on titles/descriptions/criteria; plan-key uniqueness; exactly-one-of epic|epicId for import_plan. Findings carry {path, rule, severity error|warning, problem, fix, expected, actual}; warnings never block (e.g. acceptanceCriteria in an update patch REPLACES the full criteria set). The SAME findings refuse the CRUD call at submit time if you skip this step. This tool never deletes the file.",
			InputSchema: json.RawMessage(`{"properties":{"op":{"description":"The payload kind the file will be submitted as","enum":["create_task","create_epic","update_task_patch","update_epic_patch","import_plan","set_config"],"type":"string"},"file":{"description":"Workspace file path holding the JSON payload (relative paths resolve against the workspace root)","type":"string"}},"required":["op","file"],"type":"object"}`),
		},
	}
}

// mcpToolByName returns the catalog entry for a tool name (nil when
// unknown) — used by tools/call to decide whether the result carries
// a structuredContent twin.
func mcpToolByName(name string) *mcpTool {
	cat := mcpToolCatalog()
	for i := range cat {
		if cat[i].Name == name {
			return &cat[i]
		}
	}
	return nil
}

//nolint:funlen // the catalog is one flat literal — one entry per tool, ordered; decomposition would scatter the tool surface (same posture as app.New)
func mcpToolCatalog() []mcpTool {
	tools := append(boardManagerTools(), []mcpTool{
		{
			Name:        "session_list",
			Annotations: annot(true, false, false),
			Description: "List past agent sessions (conversations) from this workspace — NON-ARCHIVED sessions by default (archived history is opt-in: pass includeArchived:true to add it back). This workspace's history is often the fastest source of context: what was already built, tried, decided, or broken. Use when: starting a task in a workspace you did not create from scratch; the user references earlier work (\"continue\", \"like last time\", \"that bug from before\"); you are about to rebuild something that may already exist; you are resuming after a suspend/resume or a fresh chat in the same workspace; the user asks what was done previously. Not for: the current conversation (you already have it in context). Pair with session_read to pull the details of a specific session. Archive status is NOT included here — use session_metadata (its entries carry archived) when you need to know which sessions are archived.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"includeArchived": map[string]any{"type": "boolean", "description": "true = archived sessions are included (the pre-#1735 behavior). Default false = archived sessions are EXCLUDED — they are read-only history, not ambient noise; the filter only applies while the archived set is fresh (~15s), so during a platform outage it degrades to the unfiltered list (every row then carries archived_set_stale:true — known-unfiltered, never a failed tool), never a silent gap"},
				},
			},
		},
		{
			Name:        "session_read",
			Annotations: annot(true, false, false),
			Description: "Read the message history of a past session in this workspace (IDs from session_list). Use when you need the specifics of prior work: file paths touched, approaches tried and abandoned, decisions and their reasons, commands that worked or failed. Prefer a limit first and read more only if needed — summarize for yourself rather than dumping full histories into your reply. Not for: current-conversation content or live program state (these are past transcripts, not a running log).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "The session to read — its 3-word phrase (from session_list / session_metadata; case-insensitive), an unambiguous title/slug alias, or (deprecated, one release) the full/hashed id"},
					"limit":     map[string]any{"type": "integer", "description": "Max messages to return (default 20)"},
				},
				"required": []string{"sessionId"},
			},
		},
		{
			Name:        "dev_preview_url",
			Description: "Returns the preview URL for a web app running in this workspace, which you can offer to the user as an open-preview link (the chat UI renders it as a button; otherwise relay it as a markdown link). Offer it when: the user is working on frontend/UI changes (components, pages, styles) — offer to spin up the dev preview unprompted: start the dev server and share the link so they can follow the work as it lands, rather than waiting to be asked; you have started or verified a web server on a localhost port; you finish building a UI the user will want to inspect; the user asks to see or try the app. Do not use when: nothing is listening on that port and you have not offered to start it — the link does not start the app, it only points at it; you have already shared the URL for that port (it is deterministic — one link suffices); the port is below 1024 or in 4096-4098 (refused). Requirements: the user must have Dev Preview enabled (Workspace Settings → Dev Preview) — the tool now FAILS LOUD with the recovery hint when the space has it disabled (no URL is minted; a note is added when the controller has not reported the state). No API call is made.",
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
			Name:        "feature_status",
			Annotations: annot(true, false, false),
			Description: "Inspect this space's feature flags: which features are active, their SOURCE (space-set via the workspace CRD vs operator/chart-set), and whether any are agent-controllable (one today: dev_preview_headers — the rest are reported as fact, not assumed). Read-only, no API call. Use when: diagnosing why a feature (e.g. dev preview) is not working before assuming breakage; checking whether the inference relay plane is active; understanding the deployment shape (sidecar vs single-container, upload staging). Output: a machine-readable JSON array of {feature, active, source, source_detail, controllable}. Note: instance-level operator settings (rate limits, workflow/triggers knobs, the dev-preview kill-switch) live in the API's settings store and are intentionally NOT readable from inside the space — their absence here is the D3 posture, not an omission.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
		devPreviewHeadersTool,
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
					"sessionId": map[string]any{"type": "string", "description": "The session to rename (from session_list, or your own session)"},
					"title":     map[string]any{"type": "string", "description": "The new title (max 200 chars)"},
				},
				"required": []string{"sessionId", "title"},
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
			Name:        "list_models",
			Annotations: annot(true, false, false),
			Description: "List the models usable in this workspace — one entry per model of every CONNECTED provider, as provider/model references ready for call_with_model's model argument (bare model names are NOT valid; the qualified provider/model form is — the entries themselves are the examples). Each entry carries contextWindow/maxOutput and a tri-state vision field: vision:true / vision:false when the catalog knows, ABSENT when unknown (never guess from absence). Use it BEFORE call_with_model when the valid names are unknown, AFTER any call_with_model failure naming an unknown provider or model — the failures quote this tool by name — and to find a vision-capable model for image calls. Read-only catalog query (the workspace's provider list); no LLM call is made. Not for: switching the session's own model (the user does that).",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
				"required":   []string{},
			},
		},
		{
			Name:        "call_with_model",
			Description: "Make ONE single-shot LLM call with a different model than the session's, returning its text response — the exchange lands in THIS conversation as the tool call + result (like every tool), in line with your turn. A clean carrier session runs the call and is deleted afterward, leaving history clean; the carrier is required, not a quirk: a session running a turn (yours, by definition, while a tool executes) BLOCKS incoming messages, so the call must run on an idle session. Use when a capability would serve one sub-task better than switching the whole session's model: a VISION model to interpret images (pass their workspace paths in images — bytes ride the call as attachments), a long-context model to digest a huge file, a fast/cheap model to draft or classify, a second opinion. The model does not inherit your conversation — only your prompt (and images) crosses over — though it does receive the workspace agent's standard setup, so include every bit of context the call needs explicitly. Prefer switching the session model (or asking the user to) when the capability is needed for the ongoing conversation rather than one call.",
			// Ordered schema (#1849 emission findings): discrete keys
			// first (model), bulk array mid (images), longest freeform
			// string LAST (prompt) — alphabetical map marshaling put
			// images before model, a bulk-before-discrete emission shape.
			InputSchema: json.RawMessage(`{"type":"object","properties":{"model":{"type":"string","description":"Target model as provider/model — see list_models for this workspace's valid names (no example is given here on purpose: providers vary per workspace and a wrong prefix is refused with the valid alternatives); a bare name is rejected with a did-you-mean hint"},"images":{"type":"array","items":{"type":"string"},"description":"Optional workspace file paths of images to show the model (png/jpg/jpeg/gif/webp, 5 MiB each, 8 MiB combined). Vision-incapable models are refused up front"},"prompt":{"type":"string","description":"The complete prompt for the one-shot call — the target model has no tools and no conversation context"}},"required":["prompt","model"]}`),
		},
		{
			Name:        "create_session",
			Description: "Create a NEW top-level agent session in this workspace — a peer of yours — and hand it a starting prompt, fire-and-forget: returns the session_id immediately while the first turn runs in the background. The result does NOT come back to you — create_session never returns the session's output to this thread. Use it ONLY when: (1) the task is independent and you will NOT depend on its result (true fire-and-forget — launch it and move on), or (2) the task will require HUMAN input or is intended for human consumption — the session appears in the workspace's session list, where a person can open it, answer its questions, and steer it. NOT for fully autonomous work whose outcome you need (implementing a user story, an investigation, any question you need answered): use the task tool instead — it blocks and returns the result to this thread. The new session is a full agent sharing this workspace's files and tools — coordinate via files, not assumptions, and make the prompt self-contained (it does not inherit this conversation's context). Include the returned session_id in the child's prompt when you can: a session that knows its own ID can attribute its send_message traffic without looking it up. subscribe (optional, default false): after creation, watch the new session (subscribe_session semantics) — when its turn completes (or ends in error) a notification lands in YOUR thread as your next turn, and the return carries \"subscribed\": true; the flag is ignored with a loud warning when the caller is itself a subagent session (subagents return to their parent, never take wake-up turns) or when no caller identity could be injected (degraded pod — the session is still created). Notification semantics are subscribe_session's: advisory, at-least-once, suppressed when the child already sent you its final message. Track it later with session_list / session_read / session_metadata. Delivery is not retried: if the agent restarts mid-turn the prompt is lost — re-send by reading the session and continuing it.",
			// Ordered schema (#1849 emission findings): discrete keys
			// first (title, subscribe), prompt (the long freeform
			// string) LAST — map marshaling would emit prompt first
			// with title still owed after it, the exact
			// resume-after-string seam where glm-class emission dies
			// (send_message corrupted 5/5 in that shape;
			// write/bash-style long-string-last never does).
			InputSchema: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string","description":"Optional title (max 200 chars); omitted = auto-generated"},"subscribe":{"type":"boolean","description":"Optional (default false): also subscribe the CALLER to the new session (subscribe_session semantics) — a notification lands in your thread when its turn completes; the result carries \"subscribed\": true. Ignored with a loud warning for subagent callers (parent set) or when no caller identity is injected"},"prompt":{"type":"string","description":"The first prompt for the new session — self-contained: same workspace files, none of this conversation's context. Its output will NOT return to you."}},"required":["prompt"]}`),
		},
		{
			Name:        "send_message",
			Description: "Send a text message to another session in this workspace (IDs from session_list / session_metadata), fire-and-forget: the message is delivered and the target session's reply — if any — stays in THAT session; nothing returns to you. Read the target later with session_read if you need its response. Every delivered message carries your origin as a return address (visible to the recipient as \"message from session …\") so they know who to reply to — reply via send_message to that origin. The origin is metadata, not authentication: claiming another session's ID grants nothing (all sessions share one credential) — it exists so the receiving thread knows where to direct its response. The platform stamps your session ID automatically — you never supply it. Use to steer or follow up on sessions you created (create_session), to hand work to an idle session, or to answer a question another session's agent asked you in its transcript. Busy targets queue the message server-side and deliver it the moment their current turn ends (status says delivering_after_current_turn). Sending to your OWN current session schedules the message as your next turn after this one completes — a self follow-up, not mid-turn injection — and the result carries a loud warning field when destination==your own session (self-addressed sends are a common misfire; intentional self-notes may carry on). If the destination value you emit accidentally pastes duplicated JSON syntax after a valid target id (a known emission slip when a similar prior call is visible in context), the server recovers the leading id and delivers there, with a loud warning: pass clean single ids, copied whole from session_list / session_metadata. The message must be the complete intended content. If you notice your drafted text was replaced by a placeholder token or truncated, do NOT send — re-compose. Placeholder/TODO-class payloads (a known emission-corruption symptom) are rejected outright by the platform. The message must be self-contained either way: the target does not inherit this conversation's context. Delivery is not retried: if the workspace restarts while a message waits or before it lands, it is lost — re-send. Not for: questions you need answered in THIS thread (use the task tool, which blocks and returns the result), or starting a new session (create_session).",
			// Ordered schema (#1849 emission findings): destination FIRST,
			// message LAST. The 5 live send_message corruptions all opened
			// with the long prose key and died resuming structural emission
			// after it (Unterminated string / Expected '}'); platform tools
			// (write: filePath→content, edit, bash) always emit long strings
			// last and never corrupt across thousands of calls. The former
			// session_id param is renamed destination (#1849 casing pass:
			// one camelCase catalog) — chosen so ALPHABETICAL order is
			// also correct (destination < message): even if this schema
			// ever regressed to a Go map, the bulk-last shape survives.
			// The snake_case spelling still resolves (one-release legacy
			// alias). from_session_id is GONE: origin is platform-injected
			// only (the plugin's lsp_injected_session stamp) — the agent
			// is never responsible for supplying its own return address.
			InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"destination": {"type": "string", "description": "The target session — its 3-word phrase (from session_list / session_metadata; case-insensitive), an unambiguous title/slug alias, or (deprecated, one release) the full/hashed id"},
				"message": {"type": "string", "description": "The message text — self-contained: the target does not inherit this conversation's context"}
			},
			"required": ["destination", "message"]
		}`),
			Annotations: annot(false, false, false),
			OutputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"status": {"type": "string"},
					"session_id": {"type": "string"},
					"origin": {"type": "string"},
					"origin_mode": {"type": "string"},
					"warning": {"type": "string"},
					"session_id_deprecated": {"type": "string"},
					"origin_deprecated": {"type": "string"},
					"deprecation": {"type": "string"}
				},
				"required": ["status", "session_id", "origin", "origin_mode"]
			}`),
		},
		{
			// Session idle-subscriptions v1 (design
			// /workspace/reports/session-subscription-design.md §5): the
			// push primitive — register interest in ANOTHER session's
			// turn boundary; the notifier delivers the wake-up.
			Name:        "subscribe_session",
			Annotations: annot(false, false, true),
			Description: "Watch another session in this workspace and be woken when its turn completes — the push primitive polling cannot replace: when the watched session goes idle (or ends its turn in error), a short notification message lands in YOUR thread as your next turn, so you learn a delegated worker finished without polling session_list / board_list / session_metadata. Use when you handed work to a session (create_session, send_message) and need to act on its completion. The watched session is addressed by sessionId (its 3-word phrase from session_list / session_metadata; case-insensitive); events optionally narrows what wakes you — accepted values \"idle\" and \"error\", default both (a worker that died mid-turn must still wake you). YOU are the subscriber — the platform injects your session identity; you never supply it. Re-subscribing is idempotent and updates the event set. Refused loudly, never guessed: watching yourself (an infinite wake loop), watch cycles (A watching B while B watches A — each settle would trigger the next, forever), subtask sessions on EITHER side (the task tool's subtasks return their result synchronously to their parent — use THAT for work whose result you need in this thread, not this; and subtasks never take wake-up turns), an unknown session (with a did-you-mean), more than 32 watchers on one session, or an unsupported event value. NOT a message channel: the notification carries no content — to talk to the watched session use send_message. Semantics stated plainly: a notification is SUPPRESSED when the watched session already sent you its final message after its last mutating work (you are already informed); a settle that flips back to busy within the ~5s debounce window notifies nothing (the next turn started — you wake when it ends); the notification is advisory and at-least-once — duplicates are possible after a restart, and a lost wake-up is the failure that matters, so verify critical outcomes via board_list / board_read or session_metadata. Manage watches with unsubscribe_session and list_subscriptions.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "The session to WATCH — its 3-word phrase (from session_list / session_metadata; case-insensitive), an unambiguous title/slug alias, or (deprecated, one release) the full/hashed id"},
					"events":    map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": []string{"idle", "error"}}, "description": "Optional: which turn-completions wake you — \"idle\" (turn completed), \"error\" (turn ended in error). Default: both. Anything else is a loud refusal"},
					"topics":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Optional (design 0072): the event-bus topic alias of `events` — session.<id>.idle, session.<id>.error, or session.<id>.* for both. The <id> must be this same session (any address form); other topics (lease.*, task.*) are a loud refusal — arbitrary pattern subscription is a future tool. Unions with `events`"},
				},
				"required": []string{"sessionId"},
			},
			OutputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"status": {"type": "string"},
					"session_id": {"type": "string"},
					"events": {"type": "array", "items": {"type": "string"}},
					"session_id_deprecated": {"type": "string"},
					"deprecation": {"type": "string"}
				},
				"required": ["status", "session_id", "events"]
			}`),
		},
		{
			Name:        "unsubscribe_session",
			Annotations: annot(false, false, true),
			Description: "Stop watching a session (the reverse of subscribe_session): removes YOUR watch edge on it — no further turn-completion notifications for it land in your thread. Takes the same sessionId address form subscribe_session takes. Idempotent by design: an edge that does not exist returns {\"status\":\"not_subscribed\"}, never an error — unsubscribing twice, or unsubscribing from a session you never watched, is a clean no-op. You are identified by the platform-injected session identity; you can only remove your own edge.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "The session to stop watching — same address forms subscribe_session accepts"},
				},
				"required": []string{"sessionId"},
			},
		},
		{
			Name:        "list_subscriptions",
			Annotations: annot(true, false, false),
			Description: "List subscription (watch) edges — the only introspection path for the subscription registry. With no arguments: YOUR outbound watches (the sessions whose turn-completions will wake you, with their event sets). With sessionId: that session's outbound watches instead. Each row names the watched session by its 3-word phrase. Pair with subscribe_session / unsubscribe_session to manage the set.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "Optional: list THIS session's outbound watches instead of your own (its 3-word phrase or another accepted address form); omitted = your own"},
				},
			},
		},
		{
			Name:        "trigger_list",
			Annotations: annot(true, false, false),
			Description: "List YOUR automation triggers (cron schedules + webhooks) - the workspace owner's, resolved from this pod's identity. Each entry carries sourceType/sourceConfig, target workspace/workflow, enabled, consecutiveFailures, lastFiredAt/nextFireAt. Start here before create/update: live entries show the exact body shapes. Pair with trigger_fires for why a trigger is misbehaving.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			Name:        "trigger_create",
			Description: "Create an automation trigger owned by this workspace's user. sourceType is cron or webhook; sourceConfig carries the source's config (cron: expr, five-field cron syntax, + optional tz as an IANA name - both are validated, invalid schedules are rejected with 400). Without a workflow target it fires a routine - a single agent turn (prompt, optional agent/script) IN THIS WORKSPACE by default (the platform forces this workspace as the target). To run the routine in ANOTHER workspace instead, set targetWorkspaceId (camelCase) on the routine - legal only when the workspace's USER owns the target or administers its org: the platform checks those rights (403 without them, 404 if the target does not exist) and the routine then runs IN the target workspace with that workspace's identity. targetWorkspaceId is create-only - trigger_update rejects it; delete and recreate to move a routine. To fire a DAG instead, set workflowId (camelCase!) to a workflow whose targetWorkspaceId is THIS workspace - the platform enforces that scope. `input` (object) is the static run input for workflow-mode triggers, validated against the workflow's inputSchema when `inputFrom` is `mapped`; `inputFrom` selects what a fired run's input is: `envelope` (default — the system envelope {source, received_at, headers, body}), `body` (webhook only — the posted payload becomes the run input), or `mapped` (the static `input` document). Wiring an envelope-mode trigger to a workflow whose schema requires non-envelope fields is rejected with 400 — set `input`, use `inputFrom: \"body\"`, or relax the schema. #1812 INLINE GATE SCRIPT: `script` on a routine is INLINE SOURCE — an object {language: \"python\"|\"node\", handler: <source defining handler(input) -> {\"proceed\": <bool>, \"vars\": {<str>: <str>}}>}, NOT a path or command. Before every fire's LLM call the handler runs in the routine's workspace with the fire envelope as input: proceed=false EXITS the fire with NO LLM call (a cheap deterministic gate — poll checks, state diffs, threshold logic run as code; the model is only invoked when the gate opens); proceed=true fires the prompt with vars substituted jinja-style ({{key}} — literal token replacement; unmatched tokens stay literal; {{.input}}/{{.prevResult}}/{{.scriptResult}} are platform tokens, never vars) and ALL vars appended as trailing [trigger script vars] metadata; ANY script failure — runtime error, unparseable output, or a return violating the schema (proceed must be a bool, vars values must be strings, keys must not start with \".\") — fails CLOSED: failed fire, the named error in trigger_fires, no LLM call. Unhandled exceptions count toward auto-disable; a handler wanting a different failure posture on error — quiet (proceed=false) or fail-open (proceed=true) — must catch its own exceptions and return that verdict itself. A `script` trigger with NO prompt never calls the LLM at all (gate + side effects). Worked gate-script patterns (external polling, conditional requests, state dedup across fires, failure postures, self-contained action prompts) live at /sandbox-runtime/docs/gate-script-patterns.md. `script` is rejected (400) alongside workflowId — a DAG carries its own script nodes. A new or rescheduled trigger does NOT fire immediately: scheduling starts at the next occurrence of the schedule. autoDisableAfter N consecutive failures disables the trigger - find failures via trigger_fires.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"trigger": map[string]any{"type": "object", "description": "The trigger body - same shape as trigger_list entries minus server fields. Minimum: name, sourceType, sourceConfig; plus prompt (routine) or workflowId (DAG, camelCase). Optional script: {language: \"python\"|\"node\", handler: <inline source returning {\"proceed\": bool, \"vars\": {str: str}}> - gates the fire before any LLM call (proceed=false exits silently; true substitutes {{key}} vars into the prompt and appends all vars as trailing metadata; failures fail closed — catch your own exceptions to choose the posture; worked patterns live at /sandbox-runtime/docs/gate-script-patterns.md). Optional targetWorkspaceId (routine only) runs the routine in another workspace owned/org-administered by the user - see the tool description for the rights check"},
			}, "required": []string{"trigger"}},
		},
		{
			Name:        "trigger_update",
			Description: "Partially update one trigger (id + the fields to change; omitted = keep existing). sourceType is immutable after create. `inputFrom` and `input` (the trigger_create input-mapping fields) are patchable here too; so is `script` (the #1812 inline gate script): an object {language, handler} replaces it, JSON null clears it back to no-script, omitted keeps it. Typical: flip enabled after a fire-storm, tweak a cron schedule, fix a prompt, or tighten a gate threshold.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"id":    map[string]any{"type": "string", "description": "Trigger ID (from trigger_list)"},
				"patch": map[string]any{"type": "object", "description": "Fields to change (UpdateTriggerRequest shape)"},
			}, "required": []string{"id", "patch"}},
		},
		{
			Name:        "trigger_delete",
			Annotations: annot(false, true, false),
			Description: "Delete one trigger permanently. Disarm-first alternative for temporary pauses: trigger_update {enabled:false}.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Trigger ID (from trigger_list)"},
			}, "required": []string{"id"}},
		},
		{
			Name:        "trigger_fires",
			Annotations: annot(true, false, false),
			Description: "The debugging gold: a trigger's fire audit - per-fire status, the input envelope (cron render / webhook body), error payloads, and the consecutive-failure trail behind consecutiveFailures/auto-disable. A `validation_error` fire means the resolved run input failed the workflow's inputSchema: its actionResult carries the typed schema_mismatch violations (locations only, never payload values) and no run was queued. A `skipped` fire on a script-gated trigger (#1812) means the gate script returned proceed=false - the LLM leg never ran, by the script's own decision; its actionResult carries {gate: {proceed: false, vars}} showing what the script computed. An open gate records {gate: {proceed: true, vars}} BEFORE the agent leg, so a delivered/failed fire explains which vars shaped the prompt. A gate-script error (runtime failure or a return violating the {proceed: bool, vars: {str: str}} schema) is a `failed` fire with the named error in actionResult.gate.error - fail-closed, no LLM call (a handler can catch its own exceptions and return a verdict to choose its posture instead — the trigger_create gate-script contract). Use when a trigger 'is not working': this says whether it fired, what it saw, and why it failed.",
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
			Annotations: annot(true, false, false),
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
			Annotations: annot(false, true, false),
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
			Annotations: annot(true, false, false),
			Description: "A workflow's run history: statuses (running/succeeded/failed), error codes + payloads, started/finished times. The debugging read for a failing DAG - pair with trigger_fires when the run was trigger-fired.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "Workflow ID (from workflow_list)"},
			}, "required": []string{"id"}},
		},
		{
			// Task Board worker tools (#1701, design 0067 §4.3c): the
			// in-workspace surface for a FRESH worker (R9) driving its
			// claimed task. Exactly four tools — claiming is engine-side
			// (claim-then-spawn), manager actions live on the owner's
			// surfaces, and the claim token (charter envelope) is the
			// in-flight guard on every stage/lease write.
			Name:        "board_read",
			Annotations: annot(true, false, false),
			Description: "Read ONE Task Board task — your claimed task's full worker view: the charter (description + acceptance criteria), stage, blocked-by dependencies with their stages, attempts, and your lease (claim state, expiry). Address it by shortId (a word-phrase id like tsk-crimson-finch-blooms from your charter envelope, #1831; legacy random tsk-XXXXXXXX ids still resolve, deprecated) or taskId (UUID); resolution is case-insensitive, and a near-miss id returns 404 with did-you-mean suggestions (verify the id and retry once). No claim token is needed — a read is not an in-flight write. The row's assignee_session and implementer_session are RESOLVABLE session refs (owner ruling 2026-10-29): the 3-word phrase when the session index knows the session, else the raw harness id (*_deprecated flagged) — pass either form to session_read / session_metadata to inspect the worker or reviewer behind the row. Use it to re-ground yourself on the task's current truth: after a 409 claim_token (your claim lapsed or the task moved — re-read before doing anything else), before reporting throttle or moving stage (stage and criteria may have changed), or whenever your charter's snapshot might be stale. The board's list/health overview projections live on the manager tools board_list/board_health (#1837).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"shortId": map[string]any{"type": "string", "description": "The task's short id (word-phrase, tsk-crimson-finch-blooms, from your charter envelope); case-insensitive"},
				"taskId":  map[string]any{"type": "string", "description": "The task's UUID (alternative to shortId; taskId wins when both are given)"},
			}, "required": []string{}},
		},
		{
			Name:        "board_heartbeat",
			Description: "The lease-nudge RESPONSE channel: call it ONCE, immediately, when a BOARD LEASE NUDGE message tells you your claim has gone quiet — responding first clears the wedge clock before you run any long command. There is NO periodic heartbeat discipline: a heartbeat on a task carrying no live nudge does NOT extend the lease (liveness is platform-observed — a session streaming or tool-calling for an hour never trips; a silent one is nudged, not reclaimed). Pass observedProgressAt (RFC 3339, your latest progress watermark — a future-stamped value beyond 2m of the server clock rejects; within it the server clamps). Requires the claim token from your charter envelope. A 409 with code claim_token means the claim is no longer yours (lease lapsed and was reclaimed, the task reached a terminal stage, or already moved) — STOP working the task and re-read the task with board_read; retrying the heartbeat cannot help.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"claimToken":         map[string]any{"type": "string", "description": "The claim token from your charter envelope (the in-flight guard)"},
				"observedProgressAt": map[string]any{"type": "string", "description": "RFC 3339 timestamp of your latest progress (last message/tool activity) — the response's stamp; the server clamps future stamps"},
				"shortId":            map[string]any{"type": "string", "description": "The task's short id (word-phrase form) — alternative to taskId"},
				"taskId":             map[string]any{"type": "string", "description": "The task's UUID — alternative to shortId"},
			}, "required": []string{"claimToken"}},
		},
		{
			Name:        "board_report_throttle",
			Description: "Report a provider throttle (HTTP 429 / rate limit) hit while working your claimed task: the task returns to backlog with a cooldown and you are released — without attempt penalty (the attempt is rolled back; the task is re-served after the backoff, to a fresh worker). Also sets the project's slot cooldown, pausing NEW claims into the project while in-flight leases run out. Requires the claim token from your charter envelope; retryAfterSeconds is optional (the platform's default backoff applies when omitted). This concerns a LIVE claim only: a pending_input or already-lapsed row is refused (409 claim_token) — it is NOT a self-exit from pending_input. Call it as soon as you identify the failure as provider throttling, then stop; do not keep retrying the throttled operation.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"claimToken":        map[string]any{"type": "string", "description": "The claim token from your charter envelope (the in-flight guard)"},
				"retryAfterSeconds": map[string]any{"type": "integer", "description": "Optional backoff from the provider's Retry-After; omitted = the platform's default throttle backoff"},
				"shortId":           map[string]any{"type": "string", "description": "The task's short id (word-phrase form) — alternative to taskId"},
				"taskId":            map[string]any{"type": "string", "description": "The task's UUID — alternative to shortId"},
			}, "required": []string{"claimToken"}},
		},
		{
			Name:        "board_move_stage",
			Description: "Move a task's stage — the worker's forward writes when you hold a claim token, or the manager-grade move when you don't. WORKER (claimToken present, your claimed task): in_progress → in_review when your work is done (attach evidenceUrl — the artifact link for this transition: PR, build, doc); a review bounce (in_review → in_progress) re-arms a fresh claim and worker; → done requires ALL acceptance criteria met + evidenceUrl and is the reviewer's move, not yours. MANAGER (no claimToken — any in-workspace session, spawned workers included; the manager-grant gate is repealed, ruling 2026-10-27, tsk-copper-refectory-generates): the full legal manager set including the pending_input exits (→ in_progress re-claims, → backlog re-queues — the ONE attempts reset) and failed (the failure terminus 000050: attempts exhaustion and the lease wedge verdict land there; re-arm via board_rearm_task) — the pending_input freeze-out class (#1832) is gone; 000050: pending_input is a FLAG not a stage — the literal SETS the awaiting-input flag (row keeps its real stage) and the exits CLEAR it. A 409 with code claim_token means the claim is no longer yours — STOP and re-read the task with board_read. The task's stage machine arbitrates every move server-side; on refusal the response carries the authoritative row.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"claimToken":  map[string]any{"type": "string", "description": "The claim token from your charter envelope (the in-flight guard) — OMIT it only for the manager-grade move (any in-workspace session)"},
				"toStage":     map[string]any{"type": "string", "description": "Target stage: in_review (work done), in_progress (re-work / the pending_input re-claim exit), done, pending_input (SET the awaiting-input flag — the row keeps its real stage, 000050), backlog (the pending_input re-queue exit clears the flag), failed (the failure terminus)"},
				"evidenceUrl": map[string]any{"type": "string", "description": "The artifact link for this transition (PR, build, doc) — required for → done, good practice for → in_review"},
				"requestId":   map[string]any{"type": "string", "description": "Optional idempotency key (UUID): reuse it on retry of the SAME logical move — a replay returns the original outcome, never a duplicate"},
				"shortId":     map[string]any{"type": "string", "description": "The task's short id (word-phrase form) — alternative to taskId"},
				"taskId":      map[string]any{"type": "string", "description": "The task's UUID — alternative to shortId"},
			}, "required": []string{"toStage"}},
		},
		{
			Name:        "board_record_verdict",
			Description: "Record your structured review verdict on a task (#1947 — the reviewer persona's ONE authorized write; you hold the in_review claim, so the verdict is yours to record). verdict=approve requires the task's evidence URL and ALL acceptance criteria met (the Done guard applies regardless — a refusal keeps your verdict in the chain with the reason); verdict=fail requires findings [{severity blocker|major|minor, pointer, criterionIndex}] and bounces the task to a fresh implementer with your findings embedded in its charter. Requires the claim token from your charter envelope. A 409 with code claim_token means the in_review claim is no longer yours — STOP and re-read the task with board_read.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"claimToken":  map[string]any{"type": "string", "description": "The claim token from your charter envelope (the in-flight guard — required)"},
				"verdict":     map[string]any{"type": "string", "description": "approve or fail"},
				"reviewer":    map[string]any{"type": "string", "description": "Your reviewer identity (session name or phrase)"},
				"evidenceUrl": map[string]any{"type": "string", "description": "The artifact link the verdict certifies (required for approve)"},
				"findings":    map[string]any{"type": "array", "description": "fail findings: [{severity, pointer, criterionIndex?}]", "items": map[string]any{"type": "object"}},
				"requestId":   map[string]any{"type": "string", "description": "Optional idempotency key (UUID): reuse it on retry of the SAME logical verdict"},
				"shortId":     map[string]any{"type": "string", "description": "The task's short id (word-phrase form) — alternative to taskId"},
				"taskId":      map[string]any{"type": "string", "description": "The task's UUID — alternative to shortId"},
			}, "required": []string{"claimToken", "verdict", "reviewer"}},
		},
		{
			Name:        "abort_session",
			Annotations: annot(false, true, false),
			Description: "Stop a session's current turn (IDs from session_list / session_metadata — the busy ones). The turn ends immediately; the session's history and recorded work are kept — only the in-flight generation is cut. Use for cross-session management: a runaway or wrong-direction session you started (create_session / send_message), or stopping work that is no longer needed so it stops consuming tokens. Abort stops the in-flight turn DESTRUCTIVELY: any message queued for the target (e.g. a send_message still waiting for its turn to end) may be dropped — after aborting, re-send anything that mattered. Aborting an idle session is a harmless no-op. Sending another message afterwards (send_message) starts a new turn as usual. Not for: your own current session (you cannot abort your way out of this turn — finish it), or deleting history (compact summarizes; deleting is delete_session's job — abort never removes anything).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "The session whose current turn should be stopped"},
				},
				"required": []string{"sessionId"},
			},
		},
		{
			Name:        "get_datetime",
			Annotations: annot(true, false, false),
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
			Annotations: annot(true, false, false),
			Description: "Read-only vitals for sessions in this workspace: per-session message count, how full the context window is (tokens used vs the model's limit), total token usage, age, model, busy flag, archive status — plus the workspace ID and agent version. SCOPE (breaking change): an explicit sessionId returns that one session (an archived session still resolves — the listing filter below never blocks single-ID lookups); with NO sessionId the default is YOUR CURRENT SESSION ONLY (the platform injects your session ID automatically); pass allSessions:true to list EVERY NON-ARCHIVED session's metadata (the old every-session behavior is includeArchived:true). Use when deciding whether to compact (context fill high), whether to spawn a parallel session (who is busy with what), before long work that might exhaust context, or when the user asks how big/old/costly a session is. Sessions with archived:true are read-only for the user (chat sends rejected; history viewable) — pair with session_archive/unarchive. A top-level archived_set_stale:true means the archive annotations could not prove freshness (~15s platform cache degraded) — treat archive flags in that result as possibly stale. Output is aggregate metadata only — read message CONTENT with session_read.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId":       map[string]any{"type": "string", "description": "One session's metadata (from session_list). Omitted = your current session only (the default); use allSessions:true for every session. An archived session still resolves by ID"},
					"allSessions":     map[string]any{"type": "boolean", "description": "true = metadata for every NON-ARCHIVED session in the workspace. Default false = current session only"},
					"includeArchived": map[string]any{"type": "boolean", "description": "true = archived sessions appear in the allSessions listing too (the pre-#1735 behavior). Default false = they are excluded from the aggregate; single-session sessionId lookups are never filtered"},
				},
				"required": []string{},
			},
		},
		{
			Name:        "session_archive",
			Description: "Archive (archived:true) or unarchive (archived:false) a session in this workspace — a PLATFORM-level marker, reversible, never destructive. What archiving does: the session becomes READ-ONLY for the user (their chat sends to it are rejected with a clear 'archived' error; its history stays fully viewable in the UI's Archived group; in-pod agent traffic is NOT blocked). What it does NOT do: delete anything, touch the session's history or files, or stop a running turn. Use when the user asks to tidy up, shelve, or retire a finished session ('archive that old chat'), or to move YOUR OWN finished work out of the way (a completed peer session you created). Unarchiving (archived:false) restores chat instantly. The success reply carries archived_visibility: verified (a post-write read of the archived set confirmed the transition — this pod's listings reflect it immediately) or unverified (the confirmation could not be read or disagreed; the state still holds, but listings may lag up to ~15s — do NOT re-archive on unverified, check with session_metadata instead). NOT for deleting — delete_session is the destructive tool; archive first when in doubt. The sessionId comes from session_list / session_metadata.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "The session to archive or unarchive (from session_list / session_metadata)"},
					"archived":  map[string]any{"type": "boolean", "description": "REQUIRED DIRECTION: true = ARCHIVE (make read-only, move to the Archived group); false = UNARCHIVE (restore chat). There is no default — pass it explicitly."},
				},
				"required": []string{"sessionId", "archived"},
			},
		},
		{
			Name:        "delete_session",
			Annotations: annot(false, true, false),
			Description: "PERMANENTLY delete a session from this workspace — HARD DELETE, no undo: the session's transcript disappears from BOTH the platform's index AND the agent's store in this workspace. This is not archive (session_archive is the reversible read-only marker); this is destruction. Use ONLY when the user explicitly asks to delete a session and its history ('delete that chat', 'get rid of it'), or to clean up an empty/accidental session YOU created (a leftover carrier or probe). Always prefer archiving when the user's intent is ambiguous — deletion cannot be undone. Refuses to delete YOUR OWN current session (identified by the platform's injection, or — on pods without the plugin — as the single busy session; its turn — this tool call — is still running; finish the turn and let the user or a peer delete it). The sessionId comes from session_list / session_metadata.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "The session to hard-delete (from session_list / session_metadata) — its history is destroyed on both sides"},
				},
				"required": []string{"sessionId"},
			},
		},
		{
			Name:        "compact",
			Description: "Compact a session's history — the model summarizes the conversation and the summary replaces it in the context window, freeing context without losing the thread. Omit sessionId to target the single currently-running session (usually your own — find IDs with session_metadata otherwise). Compaction preserves file changes and tool history on disk; only the in-context view shrinks. If the target is busy (your own session always is while a tool runs), the compaction is SCHEDULED and runs exactly when the current turn ends — plan around the context only being freed from your NEXT turn. Optional model overrides the summarizing model (default: the session's current model).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionId": map[string]any{"type": "string", "description": "Optional: the session to compact; omitted = the single busy session (your own when unambiguous)"},
					"model":     map[string]any{"type": "string", "description": "Optional provider/model to summarize with (default: the session's current model)"},
				},
				"required": []string{},
			},
		},
	}...)
	appendSessionBulkTools(&tools)
	return tools
}

// appendSessionBulkTools splices the #1836 session bulk pair —
// session_archive's and delete_session's batch twins (the
// cleanup-sweep round-trip economy) — in after their singles. A helper
// only for mcpToolCatalog's funlen budget; the entries are ordinary
// catalog rows.
func appendSessionBulkTools(tools *[]mcpTool) {
	*tools = append(*tools, []mcpTool{
		{
			// #1836 session sibling: the archive batch — ONE round-trip for
			// the N-session sweep (archiving ~48 sessions one call at a
			// time burned ~48 context-resending round-trips). Every item is
			// one full single-tool archive: resolve (phrase/alias/raw),
			// platform write, #1879 post-write visibility.
			Name:        "session_archive_bulk",
			Description: "Bulk archive/unarchive (#1836): up to 50 sessions in ONE call — session_archive's batch twin, built for the cleanup sweep (tidying a finished lane's sessions after a multi-agent run). ONE shared archived direction for the whole batch (true = archive every item, false = unarchive every item). Response is a per-item result array [{id, status: done|failed, archived_visibility?, error?}] — partial failures are reported per item, never all-or-nothing; one malformed id sinks only its own item; each id takes the 3-word phrase, an unambiguous title/slug alias, or the raw id (same forms session_archive takes) and echoes verbatim in its row. Idempotent retry: re-submitting the batch (or its failed subset) is safe — an already-archived session returns done again, NOT a conflict. Done rows carry archived_visibility verified (the shared post-write read confirmed every transition and this pod's listings reflect it immediately) or unverified (the confirmation could not be read or disagreed; the state still holds, listings may lag up to ~15s — check session_metadata, do NOT re-archive on unverified). NOT for deleting — session_delete_bulk is the destructive batch twin; archive first when in doubt. Over 50 items refuses the whole call.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionIds": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Session refs — 3-word phrase (from session_list / session_metadata; case-insensitive), unambiguous title/slug alias, or raw id — 1 to 50 items"},
					"archived":   map[string]any{"type": "boolean", "description": "REQUIRED DIRECTION: true = ARCHIVE every item (read-only, Archived group); false = UNARCHIVE every item (restore chat). There is no default — pass it explicitly."},
				},
				"required": []string{"sessionIds", "archived"},
			},
		},
		{
			// #1836 session sibling: the destructive delete batch — the
			// carrier/probe cleanup sweep in one call. The self-delete
			// guard and the absence≡miss≡foreign rendering are per item.
			Name:        "session_delete_bulk",
			Annotations: annot(false, true, false),
			Description: "Bulk hard-delete (#1836): up to 50 sessions in ONE call — delete_session's batch twin, for carrier/probe cleanup after a multi-agent run. HARD DELETE, no undo: every destroyed transcript disappears from BOTH the platform's index and the agent's store. Response is a per-item result array [{id, status: done|failed, error?}] — partial failures per item, never all-or-nothing; one malformed id sinks only its own item; each id takes the same forms delete_session takes and echoes verbatim. YOUR OWN current session is refused per item (self_delete_refused — finish your turn; the rest of the batch still processes). Idempotent retry for a destructive batch: re-submitting is safe; a session a prior submit already deleted, a mistyped id, and a session outside this workspace all render the SAME not_found item (absence is the done state — rendering it done would fabricate outcomes and leak existence; never a conflict). Use ONLY when the user explicitly asks to delete multiple sessions' history, or to clean up empty/accidental sessions YOU created; prefer session_archive_bulk when intent is ambiguous. Over 50 items refuses the whole call.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sessionIds": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Session refs to hard-delete — 3-word phrase (from session_list / session_metadata; case-insensitive), unambiguous title/slug alias, or raw id — 1 to 50 items"},
				},
				"required": []string{"sessionIds"},
			},
		},
	}...)
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

		// #1561: the wire's transport bounds — one JSON document, at
		// most maxMCPBodyBytes. #1530 integration: the body is read
		// ONCE (bounded by the same cap) so the tools/call
		// duplicate-key scan below can see the RAW bytes beneath the
		// decoder's silent duplicate-key collapse — the read is
		// equivalent to the streaming decode (MaxBytesReader trips at
		// the same byte count, including inside what would have been
		// the trailing scan), and decodeOneDocument keeps its strict
		// one-document contract on the in-memory copy.
		r.Body = http.MaxBytesReader(w, r.Body, maxMCPBodyBytes)
		raw, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			var mbe *http.MaxBytesError
			if errors.As(readErr, &mbe) {
				writeMCPErrorStatus(w, nil, -32700,
					fmt.Sprintf("Parse error: request body exceeds the %d-byte cap", maxMCPBodyBytes),
					http.StatusRequestEntityTooLarge)
				return
			}
			writeMCPErrorStatus(w, nil, -32700, fmt.Sprintf("Parse error: %v", readErr), http.StatusBadRequest)
			return
		}
		var req mcpRequest
		if err := decodeOneDocument(bytes.NewReader(raw), &req); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeMCPErrorStatus(w, nil, -32700,
					fmt.Sprintf("Parse error: request body exceeds the %d-byte cap", maxMCPBodyBytes),
					http.StatusRequestEntityTooLarge)
				return
			}
			// #1561: diagnostics ride the error message — a silent
			// "Parse error" masked WHAT failed for every caller.
			writeMCPErrorStatus(w, nil, -32700, fmt.Sprintf("Parse error: %v", err), http.StatusBadRequest)
			return
		}
		// #1530 duplicate-key refusal (tools/call only — the argument
		// surface where the exposure lives): the strict decode above
		// collapses duplicated object keys SILENTLY (last wins), the
		// KEY twin of the value-fragment emission slip the send_message
		// recovery guard handles. A duplicated key misbinds with zero
		// trace (wrong id, wrong target, no error) — refusing loudly
		// beats guessing which copy was intended.
		if req.Method == "tools/call" {
			// Fail CLOSED on scanner error. Unreachable by construction
			// while decodeOneDocument stays strict (it rejects trailing
			// data the scanner would error on) — kept as defense in
			// depth: if the two parsers ever diverge in what they
			// accept, a tools/call body refuses rather than dispatches
			// past the gate (r2's verified escape class).
			dups, dupTotal, scanErr := utilities.FindDuplicateKeys(raw)
			if scanErr != nil {
				writeMCPErrorStatus(w, req.ID, -32700,
					"Parse error: malformed tools/call body (unscannable JSON): "+scanErr.Error(),
					http.StatusBadRequest)
				return
			}
			if dupTotal > 0 {
				// Bounded by construction: the scanner reports at
				// most 16 truncated paths; the join cannot explode
				// (r3's uncapped build joined 89,999 full-depth
				// paths into a 3.93GB string).
				msg := "Invalid params: duplicated object key(s) " + strings.Join(dups, ", ")
				if dupTotal > len(dups) {
					msg += fmt.Sprintf(" …and %d more", dupTotal-len(dups))
				}
				msg += " — JSON allows one value per key and the server cannot guess which copy was intended; re-emit the call with each key exactly once"
				writeMCPErrorStatus(w, req.ID, -32602, msg, http.StatusBadRequest)
				return
			}
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
			writeMCPResult(w, req.ID, map[string]any{"tools": mcpToolCatalog()})

		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			// #1561: strict at this wire, with the spec's own
			// additive key allowlisted. An unknown key here is a
			// MISPLACED key (the #1530 probe's fragment closed
			// arguments early and demoted `message` to this level —
			// valid JSON, silently dropped by default Unmarshal,
			// degrading into a schema-level error that masked the
			// corruption) → reject loud, name the field (the
			// control-socket precedent: a rejection, not an
			// ignored unknown field). `_meta` is the exception:
			// the MCP spec's forward-compat mechanism for tools/call
			// lives INSIDE params (e.g. progressToken) — tolerated
			// uninterpreted, matching the envelope's additive pole.
			// Tool-ARGUMENT keys stay free-form (the tool schemas
			// own that layer); the REQUEST-object level stays
			// additive-tolerant (pinned in the tests).
			var rawParams map[string]json.RawMessage
			if err := json.Unmarshal(req.Params, &rawParams); err != nil {
				writeMCPError(w, req.ID, -32602, fmt.Sprintf("Invalid params: %v", err))
				return
			}
			for k := range rawParams {
				if k != "name" && k != "arguments" && k != "_meta" {
					writeMCPError(w, req.ID, -32602,
						fmt.Sprintf("Invalid params: unknown field %q (tools/call takes name/arguments/_meta; misplaced key?)", k))
					return
				}
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeMCPError(w, req.ID, -32602, fmt.Sprintf("Invalid params: %v", err))
				return
			}
			result, err := callMCPTool(r.Context(), password, params.Name, params.Arguments)
			if err != nil {
				res := map[string]any{
					"content": []map[string]any{
						{"type": "text", "text": fmt.Sprintf("Error: %v", err)},
					},
					"isError": true,
				}
				// #1816 §2 parity: the notice rides the error envelope
				// too — a deprecated-form ref that FAILS still tells
				// the caller the address form is going away.
				if note := sessionArgsDeprecationNote(params.Arguments); note != "" {
					res["content"] = append(res["content"].([]map[string]any),
						map[string]any{"type": "text", "text": note})
				}
				writeMCPResult(w, req.ID, res)
				return
			}
			content := []map[string]any{
				{"type": "text", "text": result},
			}
			// #1816 §2 parity with the REST Deprecation/Sunset
			// headers: a caller addressing a session by a deprecated
			// form (full/hashed id) gets the one-release notice as a
			// SECOND content block on the tools/call result — the
			// envelope-level carrier covers every session tool
			// uniformly, including array-shaped outputs (session_read)
			// that have no object field to carry it (#1806's round
			// left those shape-constrained).
			if note := sessionArgsDeprecationNote(params.Arguments); note != "" {
				content = append(content, map[string]any{"type": "text", "text": note})
			}
			res := map[string]any{
				"content": content,
			}
			// MCP 2026-07-28 structuredContent: tools that declare an
			// outputSchema AND returned a parseable JSON object get the
			// structured twin alongside the text form (spec: both, for
			// compatibility). Parse failures degrade to text-only — the
			// text form is always the source of truth here.
			if t := mcpToolByName(params.Name); t != nil && len(t.OutputSchema) > 0 {
				var structured map[string]any
				if err := json.Unmarshal([]byte(result), &structured); err == nil && structured != nil {
					res["structuredContent"] = structured
				}
			}
			writeMCPResult(w, req.ID, res)

		default:
			writeMCPError(w, req.ID, -32601, "Method not found: "+req.Method)
		}
	}
}

// argStr reads a string argument under the catalog's camelCase key,
// falling back to the legacy snake_case spelling (#1849 casing pass:
// one camelCase catalog; the snake spelling resolves for one release
// so prior-call precedent in a model's context never breaks a call).
// Presence semantics (matching argBool): an explicitly-present camel
// key — even the empty string — is authoritative and never falls
// through; the legacy key resolves when the camel key is ABSENT or
// present with a wrong type (a mistyped value is not a value — the
// pinned contract's wrong-type case).
func argStr(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	if v, ok := args[camelToSnake(key)].(string); ok {
		return v
	}
	return ""
}

// camelToSnake converts a camelCase key to its legacy snake_case
// spelling (destination→destination, sessionId→session_id,
// includeArchived→include_archived, allSessions→all_sessions).
func camelToSnake(k string) string {
	out := make([]byte, 0, len(k)+4)
	for i := 0; i < len(k); i++ {
		c := k[i]
		if c >= 'A' && c <= 'Z' {
			out = append(out, '_', c-'A'+'a')
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}

// argBool reads a boolean argument under camelCase with the legacy
// snake_case fallback (same contract as argStr).
func argBool(args map[string]any, key string) (bool, bool) {
	if v, ok := args[key].(bool); ok {
		return v, true
	}
	if v, ok := args[camelToSnake(key)].(bool); ok {
		return v, true
	}
	return false, false
}

// argSlice reads an array argument (camelCase key with the snake_case
// legacy spelling tolerated) without coercing its entries — the bulk
// tools (#1836) isolate malformed entries per item, so coercion here
// would erase the per-item validation the contract pins.
func argSlice(args map[string]any, key string) ([]any, bool) {
	if v, ok := args[key].([]any); ok {
		return v, true
	}
	if v, ok := args[camelToSnake(key)].([]any); ok {
		return v, true
	}
	return nil, false
}

func callMCPTool(ctx context.Context, password, name string, args map[string]any) (string, error) {
	switch name {
	case "session_list":
		// #1735: archived sessions are opt-in on the listing — the
		// filter is advisory (platform fact), defaulting to excluded.
		includeArchived := false
		if v, ok := argBool(args, "includeArchived"); ok {
			includeArchived = v
		}
		return mcpSessionList(ctx, password, includeArchived)
	case "session_read":
		sessionID := argStr(args, "sessionId")
		if sessionID == "" {
			return "", fmt.Errorf("sessionId is required")
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
		sessionID := argStr(args, "sessionId")
		title, _ := args["title"].(string)
		return mcpRenameSession(ctx, password, sessionID, title)
	case "rename_workspace":
		// The workspace identity comes from pod env, never from tool
		// arguments (mirrors secrets_resync's no-identity-input rule).
		name, _ := args["name"].(string)
		return mcpRenameWorkspace(ctx, name)
	case "list_models":
		return mcpListModels(ctx, password)
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
		// Idle-subscriptions v1 (§5): the optional caller→child watch.
		// lsp_injected_session identifies the caller (subscribe needs
		// an origin; creation itself never did — a degraded pod still
		// creates, the flag degrades to a loud warning).
		subscribe, _ := argBool(args, "subscribe")
		injected, _ := args["lsp_injected_session"].(string)
		return mcpCreateSession(ctx, password, prompt, title, subscribe, injected)
	case "send_message":
		// destination is canonical; session_id is its one-release
		// legacy alias, gated STRICTLY on destination's ABSENCE (the
		// rename is not a case fold, so argStr's wrong-type case
		// cannot reach the alias here): an explicitly-present
		// destination — empty string or mistyped — is authoritative
		// and refuses loudly, never falling through (the
		// silent-precedence class the round-1 review killed for file
		// params).
		sessionID := argStr(args, "destination")
		if _, present := args["destination"]; !present {
			sessionID, _ = args["session_id"].(string)
		}
		message, _ := args["message"].(string)
		// Origin (#1465/#1469, #1849 tightening): lsp_injected_session is
		// the platform plugin's harness-attested injection — the ONLY
		// source. The former from_session_id self-declared fallback is
		// GONE from the schema and the seam: the agent is never
		// responsible for supplying its own return address (it was also
		// an emission-corruption surface). Injection OVERWRITES any
		// model-supplied copy (unconditional, last-write-wins at the
		// plugin); a degraded pod (plugin absent) refuses delivery with
		// a report-the-platform error rather than accepting an
		// unattested origin.
		injected, _ := args["lsp_injected_session"].(string)
		return mcpSendMessage(ctx, password, sessionID, message, injected)
	case "subscribe_session":
		// Idle-subscriptions v1 (§5) + the design 0072 topics alias.
		// Caller identity is the plugin's lsp_injected_session stamp
		// ONLY — the #1849 send_message posture (refuse unattested
		// origin); the resolveCallerSession degraded-pod fallback
		// belongs to the delete tools, not here.
		target := argStr(args, "sessionId")
		var events []string
		if raw, present := args["events"]; present && raw != nil {
			items, ok := raw.([]any)
			if !ok {
				return "", fmt.Errorf("events must be an array of strings (accepted values: %q, %q)",
					subscriptionEventIdle, subscriptionEventError)
			}
			for _, v := range items {
				s, ok := v.(string)
				if !ok {
					return "", fmt.Errorf("events entries must be strings (accepted values: %q, %q)",
						subscriptionEventIdle, subscriptionEventError)
				}
				events = append(events, s)
			}
		}
		var topics []string
		if raw, present := args["topics"]; present && raw != nil {
			items, ok := raw.([]any)
			if !ok {
				return "", fmt.Errorf("topics must be an array of topic strings (session.<id>.idle|error|*)")
			}
			for _, v := range items {
				s, ok := v.(string)
				if !ok {
					return "", fmt.Errorf("topics entries must be strings (session.<id>.idle|error|*)")
				}
				topics = append(topics, s)
			}
		}
		injected, _ := args["lsp_injected_session"].(string)
		return mcpSubscribeSession(ctx, password, target, events, topics, injected)
	case "unsubscribe_session":
		target := argStr(args, "sessionId")
		injected, _ := args["lsp_injected_session"].(string)
		return mcpUnsubscribeSession(ctx, password, target, injected)
	case "list_subscriptions":
		target := argStr(args, "sessionId")
		injected, _ := args["lsp_injected_session"].(string)
		return mcpListSubscriptions(ctx, password, target, injected)
	case "abort_session":
		sessionID := argStr(args, "sessionId")
		return mcpAbortSession(ctx, password, sessionID)
	case "board_validate_file":
		// design 0070 §2.5: the local, advisory pre-submit check —
		// zero wire calls (deliberately NOT part of the mcpAutomation
		// group below); the same validator the CRUD gate runs.
		op, _ := args["op"].(string)
		file, _ := args["file"].(string)
		if strings.TrimSpace(file) == "" {
			return "", fmt.Errorf("file (a workspace file path holding the JSON payload) is required")
		}
		return validateBoardPayloadFile(strings.TrimSpace(op), strings.TrimSpace(file))
	case "trigger_list", "trigger_create", "trigger_update", "trigger_delete", "trigger_fires",
		"trigger_rotate_webhook_secret",
		"workflow_list", "workflow_create", "workflow_update", "workflow_delete", "workflow_run", "workflow_runs",
		"board_read", "board_heartbeat", "board_report_throttle", "board_move_stage",
		"board_list", "board_health",
		"board_get_config", "board_set_config",
		"board_create_project", "board_update_project",
		"board_project_archive", "board_project_unarchive", "board_project_pause", "board_project_resume",
		"board_create_epic", "board_update_epic",
		"board_epic_activate", "board_epic_pause", "board_epic_archive", "board_epic_unarchive",
		"board_delete_epic",
		"board_create_task", "board_update_task",
		"board_add_dependency", "board_remove_dependency",
		"board_delete_task", "board_pause_task", "board_resume_task", "board_rearm_task",
		"board_move_stage_bulk",
		"board_delete_tasks",
		"board_import_plan",
		"board_record_verdict": // #1947: the spawned reviewer persona's verdict channel
		// #1701: the board worker tools ride the same automation seam
		// (pod SA token, verbatim passthrough, platform-validated).
		// #1747: the manager set joins them — open to any in-workspace
		// session (the manager grant gate is repealed, ruling
		// 2026-10-27 / tsk-copper-refectory-generates); the resolved
		// session identity rides the wire as audit attribution.
		return mcpAutomation(ctx, password, name, args)
	case "get_datetime":
		timezoneArg, _ := args["timezone"].(string)
		return mcpGetDatetime(timezoneArg)
	case "session_metadata":
		sessionID := argStr(args, "sessionId")
		// lsp_injected_session is the platform plugin's harness-attested
		// current-session stamp (the send_message pattern): schema-
		// invisible, unconditional overwrite at the plugin, and the
		// DEFAULT scope source (#1627 ruling 5). all_sessions is the
		// explicit opt-in to the OLD every-session behavior.
		injected, _ := args["lsp_injected_session"].(string)
		allSessions := false
		if v, ok := argBool(args, "allSessions"); ok {
			allSessions = v
		}
		// #1735: the aggregate defaults to non-archived sessions;
		// include_archived is the explicit opt-in. Single-ID scopes
		// are never filtered (handler-side rule).
		includeArchived := false
		if v, ok := argBool(args, "includeArchived"); ok {
			includeArchived = v
		}
		return mcpSessionMetadata(ctx, password, sessionID, injected, allSessions, includeArchived)
	case "session_archive":
		sessionID := argStr(args, "sessionId")
		// The schema says "REQUIRED DIRECTION … no default — pass it
		// explicitly"; the dispatcher is the enforcement (the hand-rolled
		// JSON-RPC layer has no schema validator). An omitted or non-bool
		// `archived` REFUSES rather than guessing archive — a silent
		// default here would archive on a malformed call (review r1
		// finding 1).
		archived, ok := args["archived"].(bool)
		if !ok {
			return "", fmt.Errorf("archived (boolean) is required: true = archive, false = unarchive — no default")
		}
		return mcpSessionArchive(ctx, password, sessionID, archived)
	case "session_archive_bulk":
		// #1836: the batch twin. The direction is batch-level (ONE
		// shared boolean — per-item direction would be N single
		// calls), so its REQUIRED DIRECTION discipline refuses the
		// whole call, spending zero wire round-trips.
		refs, ok := argSlice(args, "sessionIds")
		if !ok {
			return "", fmt.Errorf("sessionIds (array of session refs) is required")
		}
		archived, aok := args["archived"].(bool)
		if !aok {
			return "", fmt.Errorf("archived (boolean) is required: true = archive, false = unarchive — no default")
		}
		return mcpSessionArchiveBulk(ctx, password, refs, archived)
	case "delete_session":
		sessionID := argStr(args, "sessionId")
		injected, _ := args["lsp_injected_session"].(string)
		return mcpDeleteSession(ctx, password, sessionID, injected)
	case "session_delete_bulk":
		refs, ok := argSlice(args, "sessionIds")
		if !ok {
			return "", fmt.Errorf("sessionIds (array of session refs) is required")
		}
		injected, _ := args["lsp_injected_session"].(string)
		return mcpDeleteSessionBulk(ctx, password, refs, injected)
	case "feature_status":
		return mcpFeatureStatus()
	case "dev_preview_headers":
		// Design 0062: agent-owned literals over the shared default
		// store. Tool-argument keys stay free-form at this layer (the
		// wire convention); the tool's own schema owns the surface.
		return mcpDevPreviewHeaders(args)
	case "compact":
		sessionID := argStr(args, "sessionId")
		model, _ := args["model"].(string)
		return mcpCompact(ctx, password, sessionID, model)
	case "dev_preview_url":
		// #1580/#1617: the space's dev-preview state — the API's live
		// push first, the controller's boot-env projection
		// (WORKSPACE_DEV_PREVIEW_ENABLED) second. Explicit false = the
		// feature is DISABLED for this space: fail LOUD with the
		// recovery hint — the caller must distinguish disabled from
		// broken, not learn it as a late 503 from the minted URL.
		// Unreported (no push, no env — an older controller during
		// upgrade skew): keep minting, with the unreported state called
		// out — never hard-fail a transition.
		if active, reported, _ := devPreviewState(); reported && !active {
			return "", fmt.Errorf("dev preview is DISABLED for this space — no URL is minted. Enable it in Workspace Settings → Dev Preview (the space's networkAccess.devPreview); until then nothing at this port can be previewed. This is a configuration state, not a failure: nothing is broken")
		}
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

// mcpSessionList serves the raw seam inventory. #1735: archived
// sessions are opt-in — the DEFAULT drops entries whose id is in the
// platform's archived set; include_archived:true keeps the verbatim
// pass-through (the raw shape is that tool's output contract, so
// surviving entries are re-served byte-exact, never rewritten).
func mcpSessionList(ctx context.Context, password string, includeArchived bool) (string, error) {
	body, err := seamClientWithPassword(password).SessionListRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list sessions: %w", err)
	}
	// #1735: archived sessions are opt-in — the DEFAULT drops
	// entries whose id is in the platform's archived set;
	// include_archived:true keeps every row (surviving entries are
	// re-served byte-exact, never rewritten).
	filtered := string(body)
	if !includeArchived {
		// #1879: the degrade must never be silent. The filter returns
		// the freshness verdict of the ONE cache capture that built the
		// output — when that capture was degraded the listing is
		// deliberately unfiltered and every row is stamped so the
		// caller knows archived entries may be present (a separate
		// second read for the stamp decision would race a concurrent
		// cache refresh and ship an unstamped unfiltered list — review
		// r1 finding). include_archived:true listings are unfiltered BY
		// REQUEST and take no stamp.
		var filterFresh bool
		filtered, filterFresh = filterArchivedFromSessionList(ctx, body)
		if !filterFresh {
			filtered = stampArchivedSetStale([]byte(filtered))
		}
	}
	// #1756 §1: the listing's identifier IS the phrase — each row's
	// id value flips to the replica-known phrase. Rows without a
	// known phrase (replica cold) keep the harness id value with a
	// per-row deprecation marker so callers can re-list later; the
	// raw bytes pass through UNCHANGED only when the replica is empty
	// (whole-list degraded window).
	return emitShortOnlySessionList([]byte(filtered)), nil
}

// emitShortOnlySessionList rewrites each row's id value to the
// replica-known phrase (#1756 §1: full ids leave tool output). Rows
// without a known phrase keep the harness id and gain an explicit
// "id_deprecated": true marker. No known mapping anywhere → the
// original bytes (whole-list degraded window; the boot/on-miss pulls
// converge it).
func emitShortOnlySessionList(raw []byte) string {
	store := currentSessionShortIDs()
	if store == nil || store.size() == 0 {
		return string(raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return string(raw) // never let the rewrite break the listing
	}
	rewritten := false
	for _, row := range rows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		if short := store.shortIDFor(id); short != "" {
			row["id"] = short
			delete(row, "shortId")
			rewritten = true
		} else if _, has := row["shortId"]; !has {
			row["id_deprecated"] = true
			rewritten = true
		}
	}
	if !rewritten {
		return string(raw)
	}
	out, err := json.Marshal(rows)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func mcpSessionRead(ctx context.Context, password, sessionID string, limit int) (string, error) {
	// Design 0066: the ref resolves first — short ids (case-
	// insensitive) and unambiguous aliases accepted; legacy ids pass
	// through byte-identical.
	resolved, rerr := resolveSessionRef(ctx, password, sessionID)
	if rerr != nil {
		return "", rerr
	}
	sessionID = resolved
	client := seamClientWithPassword(password)
	body, _, err := client.SessionMessagesRaw(ctx, sessionID, limit, "")
	if err != nil {
		if isSessionNotFoundErr(err) {
			// #1663: a not-found read gets the near-miss clause — the
			// mistyped-id caller learns the live id in one round trip.
			return "", fmt.Errorf("failed to read session: %w%s", err, sessionSuggestClause(ctx, client, sessionID))
		}
		return "", fmt.Errorf("failed to read session: %w", err)
	}
	// #1307: image data URLs never cross this surface either — strip
	// them to metadata + omission markers (the seam owns the wire rule).
	// #1756 §1: full ids leave tool output — rewrite the body's
	// session-id values to the replica-known phrases (best-effort,
	// same never-break-the-payload rule as the list rewrite).
	return emitShortOnlyRawBody(opencode.StripImageDataURLs(body)), nil
}

// emitShortOnlyRawBody rewrites session-id VALUES inside a raw seam
// payload to the replica-known phrase (#1756 §1: full ids leave tool
// output). Rows whose id the replica does not know keep the harness
// id — a raw body carries no per-row marker surface, and breaking
// the payload shape is worse than a degraded id.
func emitShortOnlyRawBody(raw []byte) string {
	store := currentSessionShortIDs()
	if store == nil || store.size() == 0 {
		return string(raw)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return string(raw)
	}
	rewritten := false
	var walk func(v any) any
	walk = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			for k, val := range t {
				if k == "sessionID" || k == "session_id" || k == "sessionId" {
					if id, ok := val.(string); ok && id != "" {
						if short := store.shortIDFor(id); short != "" {
							t[k] = short
							rewritten = true
							continue
						}
					}
				}
				t[k] = walk(val)
			}
		case []any:
			for i, val := range t {
				t[i] = walk(val)
			}
		}
		return v
	}
	doc = walk(doc)
	if !rewritten {
		return string(raw)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

func mcpDevPreviewURL(port int, path string) (string, error) {
	workspaceID := os.Getenv("WORKSPACE_ID")
	apiURL, err := mcpPublicAPIOrigin()
	if err != nil {
		return "", err
	}
	// #1580: the controller-skew case — no live push and no boot-env
	// projection under an older controller. Mint, but say the state is
	// unreported so the caller can tell skew from silence.
	skewNote := ""
	if _, reported, _ := devPreviewState(); !reported {
		skewNote = "\nNOTE: the controller did not report this space's dev-preview state (upgrade skew?) — verify Workspace Settings → Dev Preview."
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
			port, base, port, url, workspaceID, port, port) + skewNote, nil
	}

	url := fmt.Sprintf("%s/api/v1/workspaces/%s/dev-preview/%d%s", apiURL, workspaceID, port, path)
	return fmt.Sprintf(
		"LSP_DEV_PREVIEW_V1 port=%d mode=path\n[Open dev preview :%d](%s)\nOpens the dev preview tunnel (workspace %s, port %d). Requires dev preview enabled (Workspace Settings → Dev Preview → Enable); otherwise the URL returns 503. The app must be listening on localhost:%d in the workspace.",
		port, port, url, workspaceID, port, port) + skewNote, nil
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
	writeMCPErrorStatus(w, id, code, msg, http.StatusOK)
}

// writeMCPErrorStatus carries the HTTP status for transport-class
// errors: #1561 ruled the parse boundary rides HTTP 400/413 (the
// issue's twice-stated criterion), while application-level JSON-RPC
// errors (e.g. -32602) keep the endpoint's 200 convention.
func writeMCPErrorStatus(w http.ResponseWriter, id any, code int, msg string, status int) {
	w.WriteHeader(status)
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
