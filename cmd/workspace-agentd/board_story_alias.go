// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// board_story_alias.go — the Sprint Board story rename's one-release
// compatibility shim (owner ruling 2026-10-30 phase 2: "the machinery
// follows the prose"). The registry's task-prefixed board tools say
// story (board_create_story, storyId/storyIds input keys); the
// pre-rename spellings resolve as DEPRECATED ALIASES through ONE
// release window (v0.45.x) — every such answer carries the notice —
// and the removal ships in v0.46.0 with the CHANGELOG breaking entry.
//
// The aliases are CALL-TIME only, deliberately NOT registered in
// tools/list (mcpToolCatalog stays exactly the parity matrix's agent
// column): the in-workspace agent should learn the canonical names
// from the catalog, while a caller that pinned an old name still
// lands. The REST wire beneath (paths, DTO keys, the plan payload's
// "tasks" key, board_events action vocabulary) keeps its task
// spelling — that is the platform's wire term, not an MCP name
// (decision recorded in the rename PR + CHANGELOG).

import "strings"

// boardStoryAliasWindow names the releases: accepted through v0.45.x,
// removed in v0.46.0 (kept in lockstep with pkg/mcp's copy — the
// surfaces ship together).
const boardStoryAliasWindow = "accepted through v0.45.x, removed in v0.46.0"

// boardStoryToolAliases maps every pre-rename tool name to its
// canonical rename.
var boardStoryToolAliases = map[string]string{
	"board_create_task":  "board_create_story",
	"board_update_task":  "board_update_story",
	"board_delete_task":  "board_delete_story",
	"board_delete_tasks": "board_delete_stories",
	"board_rearm_task":   "board_rearm_story",
	"board_pause_task":   "board_pause_story",
	"board_resume_task":  "board_resume_story",
}

// boardStoryAliasNotice renders the deprecated-tool-name notice.
func boardStoryAliasNotice(alias, canonical string) string {
	return "DEPRECATED: " + alias + " is the pre-rename name of " + canonical +
		" (the Sprint Board story rename, phase 2 — owner ruling 2026-10-30). It behaves identically (" +
		boardStoryAliasWindow + "); call " + canonical + " — with storyId/storyIds input keys — instead."
}

// boardStoryLegacyKeyNotice is the deprecated-input-key notice.
const boardStoryLegacyKeyNotice = "DEPRECATED: the taskId/taskIds argument keys are the pre-rename spellings of " +
	"storyId/storyIds (the Sprint Board story rename, phase 2 — owner ruling 2026-10-30). " +
	"They resolve identically (" + boardStoryAliasWindow + "); send storyId/storyIds instead."

// boardStoryDeprecationNote picks the notice to carry for a call: the
// alias-name notice when the tool was called by its pre-rename name,
// else the legacy-key notice when the arguments used the task-form
// spellings, else none.
func boardStoryDeprecationNote(aliasNotice string, legacyKeys bool) string {
	if aliasNotice != "" {
		return aliasNotice
	}
	if legacyKeys {
		return boardStoryLegacyKeyNotice
	}
	return ""
}

// boardStoryToolCanonical resolves a called tool name through the
// alias window: (canonical name, notice). A non-alias name returns
// itself and an empty notice.
func boardStoryToolCanonical(name string) (string, string) {
	canonical, ok := boardStoryToolAliases[name]
	if !ok {
		return name, ""
	}
	return canonical, boardStoryAliasNotice(name, canonical)
}

// boardStoryToolName reports whether the name is a board story tool
// (canonical or alias) — the input-key normalization's scope.
func boardStoryToolName(name string) bool {
	if _, ok := boardStoryToolAliases[name]; ok {
		return true
	}
	if !strings.HasPrefix(name, "board_") {
		return false
	}
	return strings.Contains(name, "story") || strings.Contains(name, "move_stage") ||
		strings.Contains(name, "record_verdict") || strings.Contains(name, "dependency") ||
		name == "board_read"
}

// normalizeBoardStoryArgs translates the canonical story-form INPUT
// KEYS (storyId/storyIds) onto the wire's task spelling (taskId/
// taskIds) in a SHALLOW COPY of the flat argument map and of the
// tolerated nested {"task": {...}} wrapper — the platform seam keeps
// its task-term body, so the wire stays byte-compatible with the
// pre-rename shape. Returns the (possibly same) map and whether the
// caller used a LEGACY key spelling (which drives the notice).
func normalizeBoardStoryArgs(name string, args map[string]any) (map[string]any, bool) {
	if !boardStoryToolName(name) || args == nil {
		return args, false
	}
	legacy := false
	out := normalizeBoardStoryArgsMap(args, &legacy)
	if nested, ok := args["task"].(map[string]any); ok {
		out["task"] = normalizeBoardStoryArgsMap(nested, &legacy)
	}
	return out, legacy
}

// normalizeBoardStoryArgsMap is normalizeBoardStoryArgs on one map:
// storyId→taskId and storyIds→taskIds when the caller used the
// canonical spelling; a present legacy spelling marks the window.
func normalizeBoardStoryArgsMap(args map[string]any, legacy *bool) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = v
	}
	if v, ok := args["storyId"]; ok {
		delete(out, "storyId")
		out["taskId"] = v
	}
	if v, ok := args["storyIds"]; ok {
		delete(out, "storyIds")
		out["taskIds"] = v
	}
	if _, ok := args["taskId"]; ok {
		*legacy = true
	}
	if _, ok := args["taskIds"]; ok {
		*legacy = true
	}
	return out
}

// firstNonEmpty returns the first non-empty string (the notice picker
// — a call carrying both a deprecated session ref AND a deprecated
// story spelling surfaces one notice, the session one first: it names
// the caller's most immediate correction).
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
