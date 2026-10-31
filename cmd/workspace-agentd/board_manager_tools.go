// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_manager_tools.go — the manager board tools' dispatch: session
// identity RESOLUTION for audit attribution (never a gate — the
// manager-grant RBAC concept is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates), verbatim body passthrough with the
// resolver stamps, and one wire call per tool over the pod-identity
// seam. The platform validates; agentd re-models nothing (the
// mcpAutomation discipline).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/lenaxia/llmsafespaces/pkg/agent/opencode"
	"go.uber.org/zap"
)

// boardManagerSession resolves the calling session's identity for a
// manager tool call — ATTRIBUTION ONLY, never a refusal (the #1747 §5
// manager gate is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates: a workspace pod is single-tenant,
// every session shares one credential, and an identity-grant layer over
// a shared credential is theater; if a task impersonates another
// session that is the user's prompting issue). The harness-attested
// injected identity wins over any declared copy (the send_message
// pattern); the DECLARED fallback (degraded pod, plugin absent) may be
// typed as a 3-word phrase or alias — it resolves through the same
// §4.4 precedence as every other caller-typed session ref before it
// feeds the platform wire (#1816: every id-taking surface accepts
// phrases). An unresolvable declared ref degrades to empty
// attribution, loudly noted — the call still proceeds.
func boardManagerSession(ctx context.Context, password, name string, body map[string]any) string {
	injected, _ := body["lsp_injected_session"].(string)
	// sessionId is canonical; session_id is its one-release legacy
	// alias, gated on sessionId's ABSENCE — narrower than argStr's
	// wrong-type case: a present-but-mistyped sessionId degrades to
	// empty attribution rather than consulting the alias.
	declared, _ := body["sessionId"].(string)
	if _, present := body["sessionId"]; !present {
		declared, _ = body["session_id"].(string)
	}
	sid := strings.TrimSpace(injected)
	if sid == "" {
		sid = strings.TrimSpace(declared)
		if sid != "" {
			resolved, rerr := resolveSessionRef(ctx, password, sid)
			if rerr != nil {
				logBoardAttributionLoss(name, sid, rerr)
				return ""
			}
			sid = resolved
		}
	}
	return sid
}

// logBoardAttributionLoss notes a declared session ref that could not
// be resolved: the manager call proceeds (attribution-only identity),
// but the audit actor lands unattributed — the note keeps that
// visible instead of silently degrading.
func logBoardAttributionLoss(name, ref string, err error) {
	log.Warn("board manager dispatch: declared session ref did not resolve — proceeding with empty audit attribution",
		zap.String("tool", name),
		zap.String("ref", ref),
		zap.Error(err))
}

// boardWorkerSelfCertificationPin is the ONE surviving registry
// refusal (ruling 2026-10-27 / tsk-copper-refectory-generates): the
// manager-grant gate is gone — every manager tool opens to any
// in-workspace session, spawned workers included — EXCEPT the
// reviewer-grant widening this pin refuses: a spawned board worker
// self-certifying its own work through ANY manager shape that writes
// met flags — the board_update_story inline setCriteriaMet check-off,
// a board_update_story patchFile acceptanceCriteria replace (met flags
// ride the replace; Service.UpdateTask rewrites the set wholesale
// with no persona guard), and the CREATE shapes (board_create_story
// bodyFile, board_import_plan planFile task items) whose pre-met
// criteria would author done-ready rows with zero reviewer
// involvement (PR review rounds r2–r4: the pin must watch every
// shape the dispatch surface executes — wrapper keys, sibling
// fields, sibling tools). #1946's grant stays reviewer-persona-only.
// Non-worker pre-met creates stay legal (the platform surface
// deliberately supports importing completed work). The update-side
// replace arm is PRESENCE-based — a worker's met:false-only rewording
// refuses too (fail-closed over-breadth, accepted for a pin).
//
// Runs INSIDE dispatchBoardManagerTool on the post-file-merge args —
// one parse shared between pin and wire body (no TOCTOU window
// between a pin read and the dispatch read), with the caller's
// already-resolved identity (no double resolve).
func boardWorkerSelfCertificationPin(name, sid string, args map[string]any) error {
	if sid == "" || !boardWorkerSessions.isWorker(sid) {
		return nil
	}
	switch name {
	case "board_update_story":
		if _, ok := args["setCriteriaMet"]; ok {
			return fmt.Errorf("%s failed: task-worker self-certification stays refused — criteria-met recording is the reviewer-persona claim's scoped grant (issue #1946); a spawned board worker may not check off its own work through the manager shape (ruling 2026-10-27, tsk-copper-refectory-generates)", name)
		}
		if _, ok := args["acceptanceCriteria"]; ok {
			return fmt.Errorf("%s failed: task-worker self-certification stays refused — a patchFile acceptanceCriteria replace rewrites met flags with no persona guard (issue #1946); a spawned board worker may not re-write its own criteria through the manager shape (ruling 2026-10-27, tsk-copper-refectory-generates)", name)
		}
	case "board_create_story":
		if anyCriterionMetTrue(args["acceptanceCriteria"]) {
			return fmt.Errorf("%s failed: task-worker self-certification stays refused — a pre-met criterion authors a done-ready row with no reviewer involvement (issue #1946); a spawned board worker may not create its own work pre-certified (ruling 2026-10-27, tsk-copper-refectory-generates)", name)
		}
	case "board_import_plan":
		tasks, _ := args["tasks"].([]any)
		for _, t := range tasks {
			task, ok := t.(map[string]any)
			if !ok {
				continue
			}
			if anyCriterionMetTrue(task["acceptanceCriteria"]) {
				return fmt.Errorf("%s failed: task-worker self-certification stays refused — a pre-met criterion authors a done-ready row with no reviewer involvement (issue #1946); a spawned board worker may not import its own work pre-certified (ruling 2026-10-27, tsk-copper-refectory-generates)", name)
			}
		}
	}
	return nil
}

// anyCriterionMetTrue reports whether a criteria array carries any
// explicit met:true entry (the pre-met create shapes; absent/false
// flags are ordinary backlog criteria and stay legal).
func anyCriterionMetTrue(v any) bool {
	criteria, ok := v.([]any)
	if !ok {
		return false
	}
	for _, c := range criteria {
		if m, ok := c.(map[string]any); ok {
			if met, _ := m["met"].(bool); met {
				return true
			}
		}
	}
	return false
}

// boardArgs unwraps the documented flat argument form, tolerating the
// nested {"project"|"epic"|"task"|"plan": {...}} wrapper
// (passThroughBody's convention, boardToolArgs' generalized form).
func boardArgs(body map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		if nested, ok := body[k].(map[string]any); ok {
			return nested
		}
	}
	return body
}

// boardRefArg extracts a validated path reference argument.
func boardRefArg(args map[string]any, key string) string {
	ref, _ := args[key].(string)
	return strings.TrimSpace(ref)
}

// boardBoolArg reads an optional boolean argument.
func boardBoolArg(args map[string]any, key string) bool {
	v, _ := args[key].(bool)
	return v
}

// boardWireBody marshals the caller's argument set for verbatim wire
// submission (minus the routing/identity fields agentd owns).
func boardWireBody(args map[string]any) []byte {
	clean := make(map[string]any, len(args))
	for k, v := range args {
		// Strip BOTH identity spellings (camelCase canonical + the
		// one-release snake alias) — neither the injected stamp nor a
		// caller-echoed identity key ever rides the platform wire.
		if k == "lsp_injected_session" || k == "sessionId" || k == "session_id" {
			continue
		}
		clean[k] = v
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// --- the file-crud cutover: payload files are the only payload path ------

// boardFileSpec is one manager tool's payload-file contract: fileKey
// names the payload file param ("" = the tool admits NO file param —
// its arguments are small scalars); inline are the keys allowed INLINE
// (routing keys + control-plane scalars + identity plumbing) —
// everything else inline is refused (the cutover's core guarantee).
// The PAYLOAD field vocabulary is not restated here: the compiled
// schemas in board_payload_schema.go (pinned to the platform DTOs by
// the parity test) are its single source of truth.
type boardFileSpec struct {
	fileKey string
	inline  []string
}

// boardIdentityInlineKeys: identity plumbing consumed by
// boardManagerSession/boardWireBody before the wire — tolerated
// inline on every tool, never part of the payload question.
var boardIdentityInlineKeys = []string{"lsp_injected_session", "sessionId", "session_id"}

// boardToolFileSpec returns the tool's file contract. requestId rides
// the file with the payload (it is part of the platform DTO on every
// manager route); setCriteriaMet and dryRun are control-plane
// (index-addressed / carve-out-gating) and stay INLINE ONLY — a file
// spelling of them would be silently unread by the routing/control
// reads, the exact silent-drop class this resolver refuses.
func boardToolFileSpec(name string) boardFileSpec {
	switch name {
	case "board_create_story":
		return boardFileSpec{fileKey: "bodyFile", inline: []string{"epicId"}}
	case "board_create_epic":
		return boardFileSpec{fileKey: "bodyFile", inline: []string{"projectId"}}
	case "board_update_story":
		return boardFileSpec{fileKey: "patchFile", inline: []string{"taskId", "shortId", "claimToken", "setCriteriaMet"}}
	case "board_update_epic":
		return boardFileSpec{fileKey: "patchFile", inline: []string{"epicId"}}
	case "board_import_plan":
		return boardFileSpec{fileKey: "planFile", inline: []string{"projectId", "dryRun"}}
	case "board_set_config":
		// The config override map rides its file (design 0070); the
		// tier selector + project addressing are routing keys, inline
		// only (a file spelling would be silently unread by the wire
		// call's routing reads — the placement rule).
		return boardFileSpec{fileKey: "configFile", inline: []string{"tier", "projectId"}}
	}
	return boardFileSpec{}
}

// admittedFileKeys prose names a tool's admitted file params for the
// stray-key refusal (never a bare "no" — the caller learns the shape).
func (s boardFileSpec) admittedFileKeys() string {
	if s.fileKey == "" {
		return "no file params"
	}
	return s.fileKey
}

// resolveBoardToolFileArgs enforces one manager tool's payload-file
// contract on args and merges the file's fields in for the verbatim
// wire body:
//
//   - stray file keys refused on EVERY tool (a *File/*_file spelling
//     the tool does not admit is a caller mistake — refused loudly,
//     never silently dropped onto the wire);
//   - on the five payload-bearing tools the payload file is REQUIRED
//     (absence → the write→validate→submit workflow hint) and inline
//     PAYLOAD keys are refused (routing + control-plane + identity
//     keys are the inline vocabulary);
//   - the payload file's JSON is strict-decoded (single document,
//     duplicate keys refused, object top level), then runs THE SAME
//     validator as board_validate_file — any severity=error finding
//     refuses the wire call locally, embedding the identical report
//     (nested file keys, routing/control keys in the file, unknown
//     fields, shape and cap violations are the schema's findings).
//
// The consumed (anchored) path returns for the cleanup hook; "" when
// no file was consumed.
func resolveBoardToolFileArgs(name string, args map[string]any) (string, error) {
	spec := boardToolFileSpec(name)
	// Deterministic refusal order: sorted keys, never map order.
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if isBoardFileParamKey(k) && k != spec.fileKey {
			return "", fmt.Errorf("%s is not admitted here (this tool accepts %s) — refusing rather than silently dropping it", k, spec.admittedFileKeys())
		}
	}
	if spec.fileKey == "" {
		return "", nil // small-scalar tool: no payload file support
	}
	// The cutover's core guarantee: payload structure NEVER rides
	// inline — only routing, control-plane, and identity keys may.
	for _, k := range slices.Sorted(maps.Keys(args)) {
		if k == spec.fileKey || slices.Contains(spec.inline, k) || slices.Contains(boardIdentityInlineKeys, k) {
			continue
		}
		return "", fmt.Errorf("%s no longer rides inline — payload fields ride in %s: write the JSON payload to a workspace file (write tool), check it with board_validate_file, then pass its path", k, spec.fileKey)
	}
	raw, present := args[spec.fileKey]
	// The bare criteria check-off is the one inline write that is NOT
	// payload structure (#1946/#1837 P2b: {index, met} is small,
	// index-addressed control-plane) — board_update_story admits it
	// without a patch file; every other call needs the file.
	controlPlaneOnly := name == "board_update_story" && args["setCriteriaMet"] != nil
	if !present || raw == nil {
		if controlPlaneOnly {
			return "", nil
		}
		return "", fmt.Errorf("%s is required — write the JSON payload to a workspace file (write tool), check it with board_validate_file, then pass its path; payload fields no longer ride inline", spec.fileKey)
	}
	path, ok := raw.(string)
	if !ok || strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s must be a non-empty workspace file path", spec.fileKey)
	}
	delete(args, spec.fileKey) // consumed: no file spelling ever reaches the wire
	fields, consumed, err := resolveBoardPayloadFile(spec.fileKey, path)
	if err != nil {
		return "", err
	}
	// THE CRUD-TIME GATE (second amendment): the same validator
	// board_validate_file runs, on every payload file, before the wire
	// call. Error findings refuse locally — the refusal embeds the
	// identical report the tool would have returned; warnings do not
	// block. Validate-first is UX, not correctness.
	report := validateBoardPayload(boardOpForTool(name), fields)
	if report.hasErrors() {
		return "", fmt.Errorf("%s failed local validation — fix the payload file and re-submit (board_validate_file with op %q returns the same findings): %s",
			spec.fileKey, report.Op, boardValidateReportJSON(report))
	}
	for k, v := range fields {
		args[k] = v
	}
	return consumed, nil
}

// boardInjectCleanupWarning folds a failed payload-file deletion into
// the SUCCEEDED call's result as a "cleanup_warning" field — a delete
// failure warns, it never fails the call (the CRUD already landed).
func boardInjectCleanupWarning(res *opencode.AutomationResponse, path string, rmErr error) {
	warning := fmt.Sprintf("the call succeeded but its payload file could not be deleted: %v — remove %s yourself; the platform write has landed", rmErr, path)
	var obj map[string]any
	if err := json.Unmarshal([]byte(res.Body), &obj); err == nil && obj != nil {
		obj["cleanup_warning"] = warning
		if b, mErr := json.Marshal(obj); mErr == nil {
			res.Body = string(b)
			return
		}
	}
	// Non-object (or empty) platform body: wrap, never drop the warning.
	out, _ := json.Marshal(map[string]any{"status": res.Status, "body": res.Body, "cleanup_warning": warning})
	res.Body = string(out)
}

// dispatchBoardManagerTool routes one manager tool to its seam call.
// The manager set is open to ANY in-workspace session (the #1747 §5
// manager grant is repealed, ruling 2026-10-27 /
// tsk-copper-refectory-generates); the resolved identity is audit
// attribution only.
func dispatchBoardManagerTool(ctx context.Context, client *opencode.Client, password, name string, body map[string]any, saToken, workspaceID string) (*opencode.AutomationResponse, error) {
	sessionID := boardManagerSession(ctx, password, name, body)
	args := boardArgs(body, "project", "epic", "task", "plan")

	// The file-crud cutover (design 0070): the payload file is the ONLY
	// payload path — stray file keys refused on every tool, inline
	// payload keys refused, the file strict-decoded and validated by
	// the same validator board_validate_file runs (error findings
	// refuse locally), its fields merged for the verbatim wire body,
	// and the consumed path remembered for the §2.4 cleanup hook.
	consumedPath, err := resolveBoardToolFileArgs(name, args)
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", name, err)
	}

	// The #1946 pin, on the post-merge args: every met-flag-writing
	// shape (update check-off/replace, create/import pre-met) refuses
	// for spawned workers — see boardWorkerSelfCertificationPin.
	if err := boardWorkerSelfCertificationPin(name, sessionID, args); err != nil {
		return nil, err
	}

	wire := boardWireBody(args)

	res, err := boardManagerWireCall(ctx, client, name, saToken, workspaceID, sessionID, args, wire)
	if err != nil {
		return nil, err
	}
	// §2.4 cleanup: a successful call deletes its consumed payload
	// file — a retry referencing it gets the file-not-found hint
	// (pointing at board_list to confirm the landing). import_plan's
	// dryRun carve-out: a dry run mutates nothing; the file must
	// survive for the real run. Delete failure warns (folded into the
	// result body), never fails the succeeded call.
	if consumedPath != "" && (name != "board_import_plan" || !boardBoolArg(args, "dryRun")) {
		if rmErr := os.Remove(consumedPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			boardInjectCleanupWarning(res, consumedPath, rmErr)
		}
	}
	return res, nil
}

// boardManagerWireCall drives one manager tool's seam call — the wire
// contract is verbatim passthrough (boardWireBody) with agentd-owned
// routing fields; the platform validates, agentd re-models nothing.
func boardManagerWireCall(ctx context.Context, client *opencode.Client, name, saToken, workspaceID, sessionID string, args map[string]any, wire []byte) (*opencode.AutomationResponse, error) {
	switch name {
	case "board_list":
		return client.BoardManagerListTasks(ctx, saToken, workspaceID, sessionID,
			boardRefArg(args, "projectId"), boardRefArg(args, "stage"), boardRefArg(args, "failed"))
	case "board_health":
		return client.BoardManagerHealth(ctx, saToken, workspaceID, sessionID)
	case "board_get_config":
		return client.BoardGetConfig(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"))
	case "board_set_config":
		// The tier selector rides the wire body with the config keys
		// (both stamped verbatim; the platform validates the tier and
		// the key vocabulary — agentd re-models nothing).
		return client.BoardSetConfig(ctx, saToken, workspaceID, sessionID, wire)
	case "board_create_project":
		return client.BoardCreateProject(ctx, saToken, workspaceID, sessionID, wire)
	case "board_update_project":
		return client.BoardUpdateProject(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), wire)
	case "board_project_archive":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "archive")
	case "board_project_unarchive":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "unarchive")
	case "board_project_pause":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "pause")
	case "board_project_resume":
		return client.BoardManagerProjectAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), "resume")
	case "board_create_epic":
		return client.BoardCreateEpic(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), wire)
	case "board_update_epic":
		return client.BoardUpdateEpic(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), wire)
	case "board_epic_activate":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "activate", wire)
	case "board_epic_pause":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "pause", wire)
	case "board_epic_archive":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "archive", wire)
	case "board_epic_unarchive":
		return client.BoardManagerEpicAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), "unarchive", wire)
	case "board_delete_epic":
		return client.BoardDeleteEpic(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), boardBoolArg(args, "force"))
	case "board_create_story":
		return client.BoardCreateTask(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "epicId"), wire)
	case "board_update_story":
		return client.BoardUpdateTask(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), wire)
	case "board_add_dependency":
		return client.BoardAddDependency(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), wire)
	case "board_remove_dependency":
		return client.BoardRemoveDependency(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), boardRefArg(args, "dependsOn"))
	case "board_delete_story":
		return client.BoardDeleteTask(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), boardBoolArg(args, "reconnect"))
	case "board_move_stage":
		// #1832 WS2: the manager-grade move. The task ref rides the
		// path (taskId preferred, shortId tolerated — the worker
		// surface's pick order); {toStage, evidenceUrl, requestId}
		// ride the body.
		ref := boardRefArg(args, "taskId")
		if ref == "" {
			ref = boardRefArg(args, "shortId")
		}
		return client.BoardManagerMoveStage(ctx, saToken, workspaceID, sessionID, ref, wire)
	case "board_move_stage_bulk":
		// #1836: the bulk complete — {taskIds, toStage, evidenceUrl,
		// requestId} ride the body verbatim (the platform validates;
		// agentd re-models nothing).
		return client.BoardManagerMoveStageBulk(ctx, saToken, workspaceID, sessionID, wire)
	case "board_delete_stories":
		// #1836 sibling: the bulk delete — {taskIds, reconnect,
		// requestId} ride the body verbatim (the platform validates;
		// agentd re-models nothing).
		return client.BoardManagerDeleteTasksBulk(ctx, saToken, workspaceID, sessionID, wire)
	case "board_rearm_story":
		// #1832 WS3: the attempts-exhaustion re-pool.
		return client.BoardRearmTask(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), wire)
	case "board_pause_story":
		return client.BoardManagerTaskAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), "pause")
	case "board_resume_story":
		return client.BoardManagerTaskAction(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "taskId"), "resume")
	case "board_import_plan":
		return client.BoardImportPlan(ctx, saToken, workspaceID, sessionID, boardRefArg(args, "projectId"), wire)
	default:
		return nil, fmt.Errorf("unknown board manager tool: %s", name)
	}
}
