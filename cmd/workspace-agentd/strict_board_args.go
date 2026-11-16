// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// strict_board_args.go — #2030c: strict argument checking for the
// board-facing tools, rollout-guarded behind AGENTD_STRICT_BOARD_ARGS=1
// (default OFF = byte-identical behavior: the catalog and the dispatch
// path are untouched when the flag is unset — the
// AGENTD_EVENTBUS_ADDR additive-rollout posture).
//
// Why (issue #2030, live-instance comment, candidate M2): the origin
// plugin stamps every INJECTED_SESSION_TOOLS call with
// lsp_injected_session and the stamped args PERSIST into session
// history; the model replays them and pattern-continues — benign extra
// fields most times (live-verified 2026-11-15: 272 model emissions on
// the legacy *_task alias names alone, which the plugin does NOT
// stamp), value-replacement into a semantic param under the #2030
// bleed condition (13 all-time events, e.g. board_import_plan's
// projectId). Schemas with additionalProperties accept the extra
// fields SILENTLY — the corruption stays invisible until it hits a
// semantic param. Strict mode turns both classes into loud, teaching
// errors the observed self-correction path already routes around
// (agents re-emit without the extra field).
//
// The identity split (the subtlety that keeps valid calls landing):
//
//   - lsp_injected_session arriving on a tool IN the plugin's stamp
//     set is the PLATFORM's own stamp (the plugin overwrites it
//     unconditionally — a model-supplied copy can never survive, per
//     the plugin's own #1849 note) → admitted silently, exactly as
//     boardIdentityInlineKeys admits it today. The stamp-set check
//     uses the ORIGINAL wire name (pre-alias): an alias-named call
//     (board_update_task) is never stamped by the plugin, so a stamp
//     key on one is ALWAYS a model emission → the teaching error.
//
//   - sessionId/session_id are identity keys on every tool where the
//     tool's own schema does NOT declare them (boardManagerSession's
//     declared-identity fallback is the degraded-pod path; a pod
//     running strict mode has the plugin) → the teaching error. Where
//     the schema declares them (board_heartbeat's RC-B
//     self-identification, session tools) they are ordinary params.
//
// Evidence gate (this story's own charter): the rollout default is
// decided on the post-#2030a-hygiene residual emission rate; until
// #2059 deploys and the rate is re-measured, the default is OFF.

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
)

// boardStrictArgsFlag is the rollout flag's env name (opt-in: the
// restrictive direction defaults to today's tolerant behavior).
const boardStrictArgsFlag = "AGENTD_STRICT_BOARD_ARGS"

// boardStrictArgsEnabled reports whether strict board-argument
// checking is on for this agentd process.
func boardStrictArgsEnabled() bool {
	return os.Getenv(boardStrictArgsFlag) == "1"
}

// boardStampSetTools mirrors the origin plugin's
// INJECTED_SESSION_TOOLS (runtimes/opencode/plugins/
// llmsafespaces-origin.js) in its bare-name form (the plugin set
// carries the "llmsafespaces_" server prefix; the MCP layer here sees
// bare names). A call to one of these tools arrives WITH the
// lsp_injected_session stamp; every other tool arrives without.
// Kept in lockstep by TestStrictBoardArgs_PluginSetParity, which
// parses the plugin file and refuses drift in either direction.
var boardStampSetTools = map[string]bool{
	"send_message":            true,
	"session_metadata":        true,
	"delete_session":          true,
	"session_delete_bulk":     true,
	"subscribe_session":       true,
	"unsubscribe_session":     true,
	"list_subscriptions":      true,
	"create_session":          true,
	"board_create_project":    true,
	"board_update_project":    true,
	"board_project_archive":   true,
	"board_project_unarchive": true,
	"board_project_pause":     true,
	"board_project_resume":    true,
	"board_create_epic":       true,
	"board_update_epic":       true,
	"board_epic_activate":     true,
	"board_epic_pause":        true,
	"board_epic_archive":      true,
	"board_epic_unarchive":    true,
	"board_delete_epic":       true,
	"board_create_story":      true,
	"board_update_story":      true,
	"board_add_dependency":    true,
	"board_remove_dependency": true,
	"board_delete_story":      true,
	"board_move_stage_bulk":   true,
	"board_delete_stories":    true,
	"board_get_config":        true,
	"board_set_config":        true,
	"board_pause_story":       true,
	"board_resume_story":      true,
	"board_rearm_story":       true,
	"board_depose_story":      true,
	"board_import_plan":       true,
	"board_list":              true,
	"board_health":            true,
	"board_validate_file":     true,
}

// boardIdentityStampErrorText is the teaching error for an emitted
// identity key — precise about WHAT the key is (platform-injected,
// never the caller's to emit) and HOW to recover (re-emit without it;
// the observed self-correction path). Pinned by
// TestStrictBoardArgs_IdentityTeachingError.
const boardIdentityStampErrorText = "%s failed: %q is a platform-injected identity stamp — identity stamps are platform-injected, never emit them; re-emit the call without this field (the platform stamps caller identity itself)"

// boardUnknownArgErrorText is the loud unknown-field rejection: names
// the field, teaches recovery (re-emit without it — the self-correction
// path the live instance already observed), and never leaves the
// caller guessing the vocabulary (the admitted keys ride along).
const boardUnknownArgErrorText = "%s failed: strict argument checking rejected unknown field %q — this tool accepts %s; re-emit the call without the extra field (unknown fields are not silently accepted in strict mode)"

// strictBoardArgsCheck validates one tools/call argument set for a
// board-facing tool. originalName is the name as the caller spelled it
// (pre-alias — the plugin stamps by that spelling); canonicalName is
// the post-alias name whose schema owns the admitted vocabulary. It
// returns nil whenever strict mode is off, the tool is not a board
// tool, or the tool is unknown (the dispatcher owns that error), and
// otherwise nil unless an argument key is an emitted identity key or
// an unknown field. Deterministic refusal order: identity keys first
// (sorted — the precise diagnosis outranks the generic one), then
// unknown fields (sorted — never map order).
func strictBoardArgsCheck(originalName, canonicalName string, args map[string]any) error {
	if !boardStrictArgsEnabled() || !strings.HasPrefix(canonicalName, "board_") {
		return nil
	}
	if mcpToolByName(canonicalName) == nil {
		return nil // unknown tool: the dispatcher's own error names it
	}
	if len(args) == 0 {
		return nil
	}
	admitted := boardToolAdmittedArgs(canonicalName)
	for _, k := range slices.Sorted(maps.Keys(args)) {
		switch k {
		case "lsp_injected_session":
			// The platform's own stamp on stamp-set tools (the plugin
			// overwrites model copies unconditionally); a model
			// emission everywhere else — the alias names are never
			// stamped, so the 272-event legacy-name class lands here.
			if !boardStampSetTools[originalName] {
				return fmt.Errorf(boardIdentityStampErrorText, canonicalName, k)
			}
		case "sessionId", "session_id":
			// Ordinary params where the tool's schema declares them;
			// identity-stamp emissions where it does not.
			if !admitted[k] {
				return fmt.Errorf(boardIdentityStampErrorText, canonicalName, k)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(args)) {
		// The platform's own stamp on stamp-set tools: admitted (the
		// identity pass above did not reject it) — not an unknown field.
		if k == "lsp_injected_session" && boardStampSetTools[originalName] {
			continue
		}
		if !admitted[k] {
			return fmt.Errorf(boardUnknownArgErrorText, canonicalName, k, strings.Join(slices.Sorted(maps.Keys(admitted)), ", "))
		}
	}
	return nil
}

// boardWrapperKeys are the nested-body wrapper keys the dispatch layer
// tolerates (boardArgs unwraps project/epic/task/plan on manager
// tools; passThroughBody unwraps task on worker tools). Admitted at
// the TOP LEVEL only — validity-preserving (a nested call is valid
// today, so strict mode must not reject it); the inner payload rides
// the wire verbatim where the platform validates it, exactly today's
// contract.
var boardWrapperKeys = []string{"project", "epic", "task", "plan"}

// boardToolAdmittedArgs derives one board tool's admitted top-level
// argument vocabulary from the CATALOG (the single source of truth —
// the same literals tools/list serves): the schema's declared
// properties, the file-crud spec's inline + file keys, the
// storyId/storyIds ↔ taskId/taskIds rename pairs (the alias shim
// translates the canonical spelling onto the wire's task spelling
// BEFORE this check runs, so both spellings are valid), the nested
// wrapper keys, and requestId on every non-file-bearing tool (the
// platform DTO accepts it on every manager route; the file-bearing
// tools already refuse it inline through the design-0070 resolver —
// strict mode keeps that contract, with the unknown-field message).
func boardToolAdmittedArgs(name string) map[string]bool {
	admitted := map[string]bool{}
	tool := mcpToolByName(name)
	if tool == nil {
		return admitted
	}
	for k := range boardSchemaProperties(tool.InputSchema) {
		admitted[k] = true
	}
	spec := boardToolFileSpec(name)
	if spec.fileKey != "" {
		admitted[spec.fileKey] = true
		for _, k := range spec.inline {
			admitted[k] = true
		}
	} else {
		admitted["requestId"] = true
	}
	// The rename pairs: whichever spelling the schema advertises, the
	// shim guarantees the other one lands identically on the wire.
	if admitted["storyId"] || admitted["taskId"] {
		admitted["storyId"], admitted["taskId"] = true, true
	}
	if admitted["storyIds"] || admitted["taskIds"] {
		admitted["storyIds"], admitted["taskIds"] = true, true
	}
	for _, k := range boardWrapperKeys {
		admitted[k] = true
	}
	return admitted
}

// boardSchemaProperties decodes one catalog InputSchema (either
// representation the catalog uses — map or ordered raw JSON) into its
// properties map; nil when the shape carries none.
func boardSchemaProperties(schema any) map[string]any {
	switch s := schema.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		return props
	case json.RawMessage:
		var decoded struct {
			Properties map[string]any `json:"properties"`
		}
		if json.Unmarshal(s, &decoded) == nil {
			return decoded.Properties
		}
	}
	return nil
}

// strictBoardToolSchemas returns the catalog with
// additionalProperties:false stamped onto every board tool's
// InputSchema when strict mode is on — the schema-side half of the
// contract (the model is TAUGHT the closed vocabulary up front; the
// dispatch-side half enforces it). Map schemas are shallow-copied
// (never mutating the shared literals); ordered raw-JSON schemas are
// byte-spliced right after the opening brace so the deliberate key
// order — #1849's long-string-last discipline — survives byte-for-byte
// after the new (short) leading key. With strict mode off the catalog
// is returned untouched (byte-identical advertisement).
func strictBoardToolSchemas(tools []mcpTool) []mcpTool {
	if !boardStrictArgsEnabled() {
		return tools
	}
	out := make([]mcpTool, len(tools))
	copy(out, tools)
	for i, tool := range out {
		if !strings.HasPrefix(tool.Name, "board_") {
			continue
		}
		switch s := tool.InputSchema.(type) {
		case map[string]any:
			copied := make(map[string]any, len(s)+1)
			for k, v := range s {
				copied[k] = v
			}
			copied["additionalProperties"] = false
			out[i].InputSchema = copied
		case json.RawMessage:
			out[i].InputSchema = spliceAdditionalPropertiesFalse(s)
		}
	}
	return out
}

// spliceAdditionalPropertiesFalse inserts the
// "additionalProperties":false pair as the FIRST key of a raw object
// schema, preserving every following byte (order pins in the tests
// assert the suffix survives exactly — #1849's long-string-last
// discipline must not be disturbed). A schema that already carries
// the key, or is not a JSON object, is returned untouched.
func spliceAdditionalPropertiesFalse(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "{") || strings.Contains(trimmed, "\"additionalProperties\"") {
		return raw
	}
	spliced := `{"additionalProperties":false,` + trimmed[1:]
	// Re-validate: the splice must still decode as one JSON object.
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(spliced), &probe) != nil {
		return raw
	}
	return json.RawMessage(spliced)
}
