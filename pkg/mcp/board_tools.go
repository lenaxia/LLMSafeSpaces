// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

package mcp

// board_tools.go — the Task Board's EXTERNAL platform MCP projection
// (design 0067 §4.3b, issue #1700): customer-facing board_* tools that
// ride the owner's REST projection (/api/v1/me/*, ruling R7) through
// the same handlers the REST column pins — identical guard outcomes by
// construction (the API's AuthMiddleware scopes every call to the
// authenticated owner; foreign rows render the same plain 404 a miss
// renders, never disclosed).
//
// Deliberate asymmetry (§4.3b, mirrored in the parity matrix):
// NO heartbeat, NO report_throttle, NO claim tool exists on this
// surface — claiming is engine-side (claim-then-spawn, R9) and the
// in-flight worker actions live on the agent registry only (§4.3c).
// External actors hold NO claim tokens: every board_* tool is a
// manager action under the owner's authority.
//
// Naming: design 0067 §4.3b names board_list_projects, board_get,
// board_update_item, board_move_stage, board_start_story, the
// epic/project lifecycle pairs, the dependency pair, and board_read
// (cancel was eliminated — board_delete_story/board_delete_epic replace
// it). The design's board_create_* wildcard expands to
// board_create_project/epic/task (REST vocabulary); board_read is the
// tasks-listing reader (search/health join when the engine seams land,
// #1699 follow-ups).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/lenaxia/llmsafespaces/pkg/board"
)

// --- Tool definitions ---

// --- Stage vocabularies (single source, r2; #1747 r5: five stages) ---
//
// Cancel is ELIMINATED — delete replaces it, so the stage machine is
// FIVE stages and the eliminated stage's literal is just an invalid
// filter value (the tool tier rejects it like any other misspelling).

// boardStages is the WIRE stage set — the READ filter's vocabulary
// (board_read's stage parameter). 000050: pending_input is a FLAG, not
// a stage — the filter literal survives as the flag filter (flagged
// rows wherever they sit), and `failed` joins the storable set as the
// failure terminus. board.StageInVocabulary is the engine's single
// source for this same set.
var boardStages = []string{
	"backlog", "in_progress", "in_review", "done", "failed", "pending_input",
}

// boardMoveTargets is the stage machine's legal manager move_stage
// TARGET set (pkg/board stage.go's enumerated map, manager-surface
// column): in_progress→{in_review, done, pending_input, failed},
// in_review→{in_progress, done, pending_input, failed}, the pending_input
// human exits →{in_progress, backlog} (000050: those exits CLEAR the
// flag — the row keeps its real stage throughout), and failed is the
// failure terminus (attempts exhaustion + the wedge verdict land there).
// Entering the pipeline FROM backlog is claim-only (task_start/dispatch
// — no manager write exists), so the schema must not advertise it.
// #1747: the terminal cancel stage is ELIMINATED (delete replaces cancel).
var boardMoveTargets = []string{
	"in_progress", "in_review", "done", "pending_input", "failed", "backlog",
}

var boardListProjectsTool = mcp.NewTool("board_list_projects",
	mcp.WithDescription("List the authenticated user's board projects (ruling R7 — user-level only). "+
		"Each project is a swimlane targeting one workspace; parallel work = sibling projects."),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardGetTool = mcp.NewTool("board_get",
	mcp.WithDescription("Get one board item by short id. The prefix selects the kind: prj- a project, "+
		"epc- an epic (with derived progress), tsk- a story (with blocked-by derivation; "+
		"the REST wire term is 'task'). "+
		"Ids are word phrases — tsk-crimson-finch-blooms (#1831, session-ID playbook); a near-miss "+
		"returns 404 with did-you-mean suggestions ranked over WORDS. Short-id addressing is the "+
		"surface's contract (design 0067 §4.3a)."),
	mcp.WithString("item_id", mcp.Required(),
		mcp.Description("Short id (phrase form, #1831): prj-amber-lantern-ambles, epc-meek-gondola-waits, or tsk-crimson-finch-blooms (legacy random forms like tsk-ab12cd34 still resolve, deprecated)")),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardReadTool = mcp.NewTool("board_read",
	mcp.WithDescription("Read a board. Default (with project_id): the project's stories (the wire/API "+
		"term is 'task'), optionally filtered by one of the wire stages (backlog, in_progress, "+
		"in_review, done, failed + the "+
		"pending_input flag filter — 000050: flagged rows wherever they sit; a story carries "+
		"pending_input=true as a badge on its REAL stage). kind=epics lists the project's epics with "+
		"derived progress. q= searches your board (short id exact or title match, board-wide when "+
		"project_id is omitted) with did-you-mean suggestions on zero-hit near-misses. health=true "+
		"renders the board health snapshot instead. Story rows carry assignee_session / "+
		"implementer_session as RESOLVABLE session refs (owner ruling 2026-10-29): the 3-word "+
		"phrase when the session index knows the session, else the raw harness id (*_deprecated "+
		"flagged) — both forms address the session surfaces for inspection."),
	mcp.WithString("project_id", mcp.Description("Project short id, phrase form (prj-amber-lantern-ambles; legacy prj-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("stage", mcp.Description("Optional stage filter (wire set: backlog, in_progress, in_review, done, failed, or pending_input = the flag filter — rows awaiting input on any real stage)")),
	mcp.WithBoolean("failed", mcp.Description("#1813 failed-state filter: true lists only stories whose last attempt failed (crash loop, exhaustion — last_attempt_failed overlay), false only clean rows")),
	mcp.WithString("kind", mcp.Description("Projection selector: epics lists the project's epics (requires project_id)")),
	mcp.WithString("q", mcp.Description("Search query (short id exact or title substring)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Search filter: stories carrying ALL of these tags")),
	mcp.WithString("epic_id", mcp.Description("Search filter: narrow to one epic (epc-…, phrase or legacy form)")),
	mcp.WithBoolean("health", mcp.Description("Render the board health snapshot (leases, cooldowns, reclaims, pending origins)")),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardHealthTool = mcp.NewTool("board_health",
	mcp.WithDescription("The board health snapshot (design 0067 §4.8): active leases with expiry, recent "+
		"reclaims, story and project-slot cooldowns, pending_input counts by origin, per-project worker "+
		"slots in use. The same projection board_read?health=true renders."),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardRecordVerdictTool = mcp.NewTool("board_record_verdict",
	mcp.WithDescription("Record a structured review verdict on a story (the REST wire term is 'task') and "+
		"apply its transition through the guarded stage machine (#1832 item 5 — the forge-agnostic "+
		"verdict core; forge adapters map "+
		"INTO it, never re-implement it). approve → done STILL passes the Done guard (ALL criteria met + "+
		"evidence_url); a refused transition is a RECORDED outcome (applied=false, transition=\"refused:"+
		"<code>\"), not an error. fail → in_progress rides the bounce: fresh claim minted, attempts "+
		"UNCHANGED (a bounce is an iteration inside the verdict chain, never a failure-ledger entry)."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id (tsk-XXXXXXXX)")),
	mcp.WithString("verdict", mcp.Required(), mcp.Description("approve or fail")),
	mcp.WithString("reviewer", mcp.Required(), mcp.Description("Reviewer identity (the record's author)")),
	mcp.WithString("evidence_url", mcp.Description("Artifact link for the approve transition (required by the Done guard)")),
	mcp.WithArray("findings",
		mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"severity":       map[string]any{"type": "string", "enum": []string{"blocker", "major", "minor"}},
				"pointer":        map[string]any{"type": "string"},
				"criterionIndex": map[string]any{"type": "integer"},
			},
			"required": []string{"severity", "pointer"},
		}),
		mcp.Description("Findings [{severity, pointer, criterionIndex?}] — required on fail (what to fix); pointer is file:line or criterion text")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardStoryVerdictsTool = mcp.NewTool("board_story_verdicts",
	mcp.WithDescription("A story's ordered verdict chain (#1832 item 5): every recorded verdict with its "+
		"findings, reviewer, and whether its transition applied — the record the next claimant's charter "+
		"envelope embeds after a fail-bounce."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id (tsk-XXXXXXXX)")),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardStoryEventsTool = mcp.NewTool("board_story_events",
	mcp.WithDescription("A story's FULL audit timeline (#1832 item 4): every board_events row scoped to "+
		"the story — stage transitions, claims (who/when), review verdicts when present, dependency-graph "+
		"changes, orphan/unblock events — chronologically, behind the same owner perimeter as board_get "+
		"(foreign rows are plain misses). Optional limit keeps the newest N rows (default 200, max 500)."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id (tsk-XXXXXXXX)")),
	mcp.WithInteger("limit", mcp.Description("Keep the newest N rows (default 200, max 500)")),
	mcp.WithReadOnlyHintAnnotation(true),
)

var boardCreateProjectTool = mcp.NewTool("board_create_project",
	mcp.WithDescription("Create a board project: a swimlane with ONE target workspace the owning user "+
		"can access (rulings R7/R8) and a worker-slot budget (max concurrent in-flight claims, default 2)."),
	mcp.WithString("name", mcp.Required(), mcp.Description("Project name")),
	mcp.WithString("target_workspace_id", mcp.Required(),
		mcp.Description("The single target workspace (ruling R8 — parallel work = sibling projects)")),
	mcp.WithInteger("worker_slots", mcp.Description("Max concurrent in-flight claims (default 2)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags (on update, omission CLEARS — send the full set to keep)")),
	mcp.WithString("request_id",
		mcp.Description("Idempotency key: reuse ONE id per logical operation across retries (clientMessageID discipline)")),
)

var boardCreateEpicTool = mcp.NewTool("board_create_epic",
	mcp.WithDescription("Create a DRAFT epic in a project (ruling R2's default). Activate it to start "+
		"feeding work to fresh workers; the outcome field is the human's steering wheel. "+
		"TERMINAL-TARGET GUARD (#1948): an ARCHIVED project refuses the create — the epic would "+
		"sit silently unfed outside the claimable set; unarchive the project first."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id, phrase form (prj-amber-lantern-ambles; legacy prj-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("title", mcp.Required(), mcp.Description("Epic title")),
	mcp.WithString("outcome", mcp.Description("What this epic exists to achieve (the steering wheel)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardCreateStoryTool = mcp.NewTool("board_create_story",
	mcp.WithDescription("Create one story (the REST wire term is 'task') in an epic's backlog. TERMINAL-TARGET GUARD (#1948): creating into a "+
		"completed or archived epic (or an archived project) is REFUSED loud — the row would sit silently "+
		"unfed outside the claimable set; reopen (epic_activate) or unarchive first. Paused epics still "+
		"accept creates (pause is a scheduling lever; resume re-feeds), as do draft ones (activate to feed). "+
		"ATOMIC-CREATE CONTRACT (#1849): a non-empty "+
		"description (the worker's charter) AND at least one acceptance criterion are REQUIRED — a create "+
		"missing either, or whose title/description/criterion is ENTIRELY a placeholder-class token "+
		"(placeholder, todo, tbd, asdf, qwerty…), is rejected outright; re-compose (prose that merely "+
		"mentions a token is fine). The description (markdown) seeds the worker charter; criteria define "+
		"Done (ALL met + evidence_url — ruling R2). Tags may carry "+
		"the explicit pr:N anchor (owner ruling OR4 — pattern ^pr:[1-9][0-9]*$, the only PR-drift form "+
		"the reconciler consumes; malformed anchors are inert tags, never guessed) and the delegated-review "+
		"anchor review-of:<subject-short-id> (#1832 — a review story whose subject's done transition settles it: "+
		"done when its criteria are ALL met, drift_review flag (pending_input) when unmet — the park "+
		"is a FLAG on the row's real stage (000050), never a costume column). depends_on adds "+
		"prerequisite edges in the same write (#1747)."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id, phrase form (epc-meek-gondola-waits; legacy epc-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("title", mcp.Required(), mcp.Description("Story title")),
	mcp.WithString("description", mcp.Required(), mcp.Description("REQUIRED at create (#1849): markdown; seeds the fresh worker's charter (R9)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("Tags (may include one pr:N anchor; the full set — project/epic updates rewrite tags wholesale)")),
	mcp.WithArray("acceptance_criteria",
		mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
				"met":  map[string]any{"type": "boolean"},
			},
			"required": []string{"text"},
		}),
		mcp.Required(),
		mcp.Description("REQUIRED at create (#1849): checkable criteria [{text, met}] — Done = ALL met + evidence")),
	mcp.WithArray("depends_on", mcp.WithStringItems(),
		mcp.Description("Prerequisite story short ids — edges added in the same transaction (#1747)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardUpdateItemTool = mcp.NewTool("board_update_item",
	mcp.WithDescription("Patch a board item by short id; only fields applying to the addressed kind are "+
		"accepted (a field for another kind is an error, not a silent drop). Omitted scalar fields keep "+
		"existing values. TAGS semantics differ by kind (engine rules, REST-identical): on a story "+
		"(the REST wire term is 'task'), omitted "+
		"tags KEEP the existing set and an explicit [] CLEARS it; on a project or epic, tags are written "+
		"unconditionally — OMITTING THEM CLEARS the set (send the full set to keep it). Acceptance criteria "+
		"REPLACE when present. set_criteria_met is {index, met} — the "+
		"reviewer's Done preparation can check off AND uncheck (#1837 P2b; the bare integer remains the check-off form); "+
		"refused alongside acceptance_criteria. The assignee is engine-set, never a patch field."),
	mcp.WithString("item_id", mcp.Required(),
		mcp.Description("Short id, phrase form (#1831): prj-… (name/target_workspace_id/worker_slots/tags), "+
			"epc-… (title/outcome/tags), tsk-… (title/description/tags/"+
			"acceptance_criteria/set_criteria_met); legacy prj-XXXXXXXX still resolves, deprecated")),
	mcp.WithString("name", mcp.Description("Project name (prj- only; rename cascades the display inheritance)")),
	mcp.WithString("target_workspace_id", mcp.Description("Project target workspace (prj- only; must be accessible to the owner)")),
	mcp.WithInteger("worker_slots", mcp.Description("Project worker-slot budget (prj- only)")),
	mcp.WithString("title", mcp.Description("Epic/story title (epc-/tsk- only)")),
	mcp.WithString("outcome", mcp.Description("Epic outcome (epc- only)")),
	mcp.WithString("description", mcp.Description("Story description markdown (tsk- only)")),
	mcp.WithArray("tags", mcp.WithStringItems(), mcp.Description("tsk-: omitted keeps, [] clears; prj-/epc-: written unconditionally — omitted CLEARS (send the full set to keep)")),
	mcp.WithArray("acceptance_criteria",
		mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
				"met":  map[string]any{"type": "boolean"},
			},
			"required": []string{"text"},
		}),
		mcp.Description("Story criteria [{text, met}] (tsk- only; REPLACE when present)")),
	mcp.WithObject("set_criteria_met",
		mcp.Properties(map[string]any{
			"index": map[string]any{"type": "integer", "minimum": 0},
			"met":   map[string]any{"type": "boolean", "default": true},
		}),
		// NOTE: the object is OPTIONAL at the tool level (mcp-go has no
		// inner-required PropertyOption — index's requirement rides this
		// description, matching openapi's inner required:[index]).
		mcp.Description("OPTIONAL {index, met} — index is required inside the object; check off OR uncheck the criterion at the 0-based index (tsk- only; #1837 P2b). The legacy bare-integer form N still decodes as {index:N, met:true}. Refused alongside acceptance_criteria (send one or the other); out-of-bounds indices are loud errors")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardMoveStageTool = mcp.NewTool("board_move_stage",
	mcp.WithDescription("Move a story (the REST wire term is 'task') to another stage (server-arbitrated; a drag that loses a race "+
		"returns the authoritative guard error). Legal manager moves: in_progress→{in_review, done, "+
		"pending_input, failed}, in_review→{in_progress (the review bounce), done, pending_input, failed}, "+
		"and the pending_input human exits →{in_progress, backlog} — 000050: pending_input is a FLAG "+
		"not a stage; the move literal SETS the flag (the row keeps its real stage) and the exits "+
		"CLEAR it. failed is the failure terminus (attempts exhaustion + the lease wedge verdict). "+
		"Entering the pipeline from backlog is claim-only (board_start_story — no manager "+
		"write exists); removal is board_delete, never a stage move (#1747). done requires ALL acceptance "+
		"criteria met + evidence_url (ruling R2). Forward moves refuse on a paused story (409 task_paused)."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id, phrase form (tsk-crimson-finch-blooms; legacy tsk-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("to_stage", mcp.Required(), mcp.Enum(boardMoveTargets...), mcp.Description("Target stage")),
	mcp.WithString("evidence_url", mcp.Description("Artifact link for THIS transition (required for done)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardStartStoryTool = mcp.NewTool("board_start_story",
	mcp.WithDescription("Manual one-off dispatch (task_start): claim + fresh-charter worker spawn into "+
		"the project's target workspace (R9). Explicitly allowed in draft epics; paused/completed/archived "+
		"epics and archived projects refuse it. No caller-supplied worker identity exists on any surface."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id, phrase form (tsk-crimson-finch-blooms; legacy tsk-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

// #1836: the bulk claim/complete pair — per-item result arrays, partial
// failures included; the batch cap is 50. board_start_stories is the
// plural claim form; board_move_stage_bulk is the batch stage move
// (one shared target stage — per-item stages would be N single calls).
var boardStartStoriesTool = mcp.NewTool("board_start_stories",
	mcp.WithDescription("Bulk claim (#1836): dispatch up to 50 stories (the REST wire term is 'task') in one call — each item is one full "+
		"task_start (claim + fresh-charter worker spawn; draft∪active epics; every guard identical to the "+
		"single form). Response is a per-item result array [{id, status: done|failed, escalated?, error?}] — "+
		"partial failures are reported per item, never all-or-nothing; re-submitting the batch (same "+
		"request_id) is safe: an item already claimed (in_progress) returns done, not conflict. NO claim "+
		"tokens render on the bulk projection (#1864 ruling) — the spawned workers receive theirs in their "+
		"charters. Prefer this over N board_start_story calls whenever the dispatch set is known up front."),
	mcp.WithArray("story_ids", mcp.Required(), mcp.WithStringItems(),
		mcp.Description("Story short ids (phrase form preferred; legacy forms resolve) — 1 to 50 items")),
	mcp.WithString("request_id", mcp.Description("Idempotency key: ONE per logical batch, reused across retries of the same batch — "+
		"reusing a batch's key for a DIFFERENT batch replays the first batch's recorded outcomes (stale done items)")),
)

var boardMoveStageBulkTool = mcp.NewTool("board_move_stage_bulk",
	mcp.WithDescription("Bulk stage move (#1836): one shared target stage + evidence across up to 50 stories "+
		"(the REST wire term is 'task') in one call — each item is one full server-arbitrated board_move_stage (the Done guard, pause guards, "+
		"claim-token rule, auto-review on →in_review, and the →done settle cascade all apply per item). "+
		"Response is a per-item result array [{id, status: done|failed, error?}] — partial failures per item, "+
		"never all-or-nothing; re-submitting the batch (same request_id) is safe: an item already at the "+
		"target stage returns done, not conflict. The canonical use: completing review sets (in_review → done)."),
	mcp.WithArray("story_ids", mcp.Required(), mcp.WithStringItems(),
		mcp.Description("Story short ids (phrase form preferred; legacy forms resolve) — 1 to 50 items")),
	mcp.WithString("to_stage", mcp.Required(), mcp.Enum(boardMoveTargets...),
		mcp.Description("The ONE target stage every item moves to (per-item from-stage legality stays server-arbitrated)")),
	mcp.WithString("evidence_url", mcp.Description("Artifact link applied to every item's transition (required when to_stage=done)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key: ONE per logical batch, reused across retries of the same batch — "+
		"reusing a batch's key for a DIFFERENT batch replays the first batch's recorded outcomes (stale done items)")),
)
var boardDeleteStoriesTool = mcp.NewTool("board_delete_stories",
	mcp.WithDescription("Bulk delete (#1836): the story-delete batch (wire: delete_task) — up to 50 destructive removals in one call, "+
		"ONE shared reconnect flag. Each item is one full board_delete_story (its own snapshot-bearing audit event, "+
		"orphan events, post-commit worker abort for an in-flight row). Response is a per-item result array "+
		"[{id, status: done|failed, orphaned_dependents?, reconnected?, error?}] — partial failures per item, never "+
		"all-or-nothing; one malformed id sinks only its own item. Idempotent retry for a destructive variant: "+
		"re-submitting the batch is safe, and a miss, a foreign row, and an already-deleted row all render the SAME "+
		"not_found item (absence is the done state — rendering it done would fabricate outcomes for foreign probes "+
		"and leak foreign existence; never a conflict). Canonical use: cleanup sweeps after re-planning."),
	mcp.WithArray("story_ids", mcp.Required(), mcp.WithStringItems(),
		mcp.Description("Story short ids (phrase form preferred; legacy forms resolve) — 1 to 50 items")),
	mcp.WithBoolean("reconnect", mcp.Description("The ONE shared reconnect flag: orphans of every deleted item inherit that item's direct upstream edges (deduplicated, cycle-guarded) — default off")),
	mcp.WithString("request_id", mcp.Description("Idempotency key: ONE per logical batch, reused across retries of the same batch — "+
		"reusing a batch's key for a DIFFERENT batch replays the first batch's recorded outcomes (stale done items)")),
)

var boardRearmStoryTool = mcp.NewTool("board_rearm_story",
	mcp.WithDescription("Re-pool an attempts-exhausted story (the REST wire term is 'task'; #1832 WS3, 000050-shaped): a escalated "+
		"row (stage=failed, pending_origin=escalated — attempts at the cap) "+
		"returns to backlog with attempts reset to 0 — a fresh-judgment ledger (the pre-reset count rides the rearm audit event). "+
		"The re-pooled row is claimable immediately. Only failed/escalated rows; other stages and origins keep their "+
		"enumerated transitions."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id (tsk-…, phrase or legacy form)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardDeposeStoryTool = mcp.NewTool("board_depose_story",
	mcp.WithDescription("Depose a LAPSED claim on a story (the REST wire term is 'task'; S5, the DEPOSED directive — 2026-10-30 "+
		"incident remediation): the orchestrator records the salvage pointer — branch, PR number, last known state — on the row, "+
		"requeues an in_progress lapsed claim exactly as the reclaim would (failure overlay + backoff), delivers the DEPOSED "+
		"directive to the stale session, and the NEXT claim's charter embeds the pointer automatically (inherit, don't restart). "+
		"A backlog row takes the pointer alone (the reclaim already ran). GUARDS: a live healthy claim refuses (409 claim_live — "+
		"unexpired lease, or life evidence on the expired one: movement/mid-turn busy/post-sentinel reaction/post-nudge heartbeat); "+
		"done/failed/in_review rows refuse; every deposition is audit-logged (action 'depose')"),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id (tsk-…, phrase or legacy form)")),
	mcp.WithString("branch", mcp.Description("The pushed branch name the deposed attempt left behind")),
	mcp.WithString("pr", mcp.Description("The open pull request's number or URL")),
	mcp.WithString("head", mcp.Description("The last known commit SHA on the branch")),
	mcp.WithString("state", mcp.Description("Last known state, free text: what was done, what gates ran, what remains (at least one of branch/pr/head/state/note required)")),
	mcp.WithString("note", mcp.Description("Guidance for the inheriting worker (verify-first, what to build on)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardEpicActivateTool = mcp.NewTool("board_epic_activate",
	mcp.WithDescription("Activate an epic: start feeding ready work to fresh workers. Re-opens a completed "+
		"epic; unarchive always restores active. #1747: refuses cross-epic dependencies into inactive epics "+
		"unless acknowledged, reports the initial ready set, and cascades resume directives to held workers."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id, phrase form (epc-meek-gondola-waits; legacy epc-XXXXXXXX still resolves, deprecated)")),
	mcp.WithBoolean("acknowledge_inactive_deps",
		mcp.Description("Acknowledge cross-epic deps entering inactive epics (they stay blocking until those epics activate)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardEpicPauseTool = mcp.NewTool("board_epic_pause",
	mcp.WithDescription("Pause an epic: stop NEW claims without interrupting in-flight leases — and "+
		"cascade HALT directives to the epic's in-flight workers (#1747 pause-must-pause; they land at "+
		"the current turn's boundary)."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id, phrase form (epc-meek-gondola-waits; legacy epc-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardEpicArchiveTool = mcp.NewTool("board_epic_archive",
	mcp.WithDescription("Archive an epic (reversible storage; it leaves the candidate set)."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id, phrase form (epc-meek-gondola-waits; legacy epc-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardEpicUnarchiveTool = mcp.NewTool("board_epic_unarchive",
	mcp.WithDescription("Unarchive an epic — always restores active (the resumable default)."),
	mcp.WithString("epic_id", mcp.Required(), mcp.Description("Epic short id, phrase form (epc-meek-gondola-waits; legacy epc-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardProjectArchiveTool = mcp.NewTool("board_project_archive",
	mcp.WithDescription("Archive a project (reversible): stops ALL claims into it."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id, phrase form (prj-amber-lantern-ambles; legacy prj-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardProjectUnarchiveTool = mcp.NewTool("board_project_unarchive",
	mcp.WithDescription("Unarchive a project: restores active."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id, phrase form (prj-amber-lantern-ambles; legacy prj-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardProjectPauseTool = mcp.NewTool("board_project_pause",
	mcp.WithDescription("Pause a project (#1747 — the scheduling lever: 'pause this project so another "+
		"moves forward'). Blocks claiming for every story under it (query scoping), cascades HALT directives "+
		"to its in-flight workers; never interrupts leases. resume_project clears it."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id, phrase form (prj-amber-lantern-ambles; legacy prj-XXXXXXXX still resolves, deprecated)")),
)

var boardProjectResumeTool = mcp.NewTool("board_project_resume",
	mcp.WithDescription("Resume a paused project: clears the flag, cascades RESUME directives to held "+
		"workers; lapsed leases re-enter the FIFO pool."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id, phrase form (prj-amber-lantern-ambles; legacy prj-XXXXXXXX still resolves, deprecated)")),
)

var boardPauseStoryTool = mcp.NewTool("board_pause_story",
	mcp.WithDescription("Pause a story (the REST wire term is 'task'; #1747): parks it — claim candidacy off, forward stage-writes "+
		"refused (409 task_paused), HALT directive to the in-flight worker (lands at the turn boundary; "+
		"the lease keeps running — LAPSE accounting, never frozen). Dependencies stay blocking."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id, phrase form (tsk-crimson-finch-blooms; legacy tsk-XXXXXXXX still resolves, deprecated)")),
)

var boardResumeStoryTool = mcp.NewTool("board_resume_story",
	mcp.WithDescription("Resume a paused story: clears the flag, delivers the RESUME directive — the held "+
		"session continues if its lease survived; a lapsed one re-enters the FIFO pool (fresh worker)."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Story short id, phrase form (tsk-crimson-finch-blooms; legacy tsk-XXXXXXXX still resolves, deprecated)")),
)

var boardDeleteTool = mcp.NewTool("board_delete",
	mcp.WithDescription("Destructive delete of a story or epic by short id (the wire/API term for stories is 'task'; #1747: cancel is ELIMINATED — "+
		"delete replaces it). The audit log keeps every removal: each deleted row's event embeds a full "+
		"final-state snapshot. delete_task: idle rows go straight; an in-flight row gets a clean worker "+
		"abort first; dependent edges cascade away — orphaned dependents are listed in the response and "+
		"each gets an unblocked_by_delete audit event. NO auto-reconnect: reconnect=true (story only) lets "+
		"orphans inherit the deleted node's direct upstream edges (deduplicated, cycle-guarded). "+
		"delete_epic: cascades to its stories and requires force=true while any claim is in-flight."),
	mcp.WithString("item_id", mcp.Required(), mcp.Description("Story or epic short id (tsk-/epc-)")),
	mcp.WithBoolean("force", mcp.Description("epic only: abort in-flight workers and delete anyway")),
	mcp.WithBoolean("reconnect", mcp.Description("story only (tsk-): orphans inherit the deleted node's upstream edges (default off)")),
)

var boardImportPlanTool = mcp.NewTool("board_import_plan",
	mcp.WithDescription("Materialize an entire plan atomically (#1747): one epic + its stories (the REST wire term is 'task'; the plan payload key is 'tasks') + dependency "+
		"edges from a single payload, all-or-nothing. TERMINAL-TARGET GUARD (#1948): an ARCHIVED project "+
		"refuses the import whole — the fresh epic and every node would sit silently unfed outside the "+
		"claimable set; unarchive first (the minted epic itself is always draft, never terminal). "+
		"dry_run=true validates WITHOUT writing — the render "+
		"(graph, warnings, initial ready set) is the user's plan-approval moment. Stories address each other "+
		"by payload key; edges live within the workspace."),
	mcp.WithString("project_id", mcp.Required(), mcp.Description("Project short id, phrase form (prj-amber-lantern-ambles; legacy prj-XXXXXXXX still resolves, deprecated)")),
	mcp.WithString("epic_title", mcp.Required(), mcp.Description("The created epic's title")),
	mcp.WithString("outcome", mcp.Description("The epic's outcome (steering wheel)")),
	mcp.WithBoolean("dry_run", mcp.Description("Validate + render only — nothing is written")),
	mcp.WithArray("tasks",
		mcp.Items(map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":                 map[string]any{"type": "string"},
				"title":               map[string]any{"type": "string"},
				"description":         map[string]any{"type": "string"},
				"tags":                map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"acceptance_criteria": map[string]any{"type": "array"},
				"depends_on":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"key", "title", "description", "acceptance_criteria"},
		}),
		mcp.Description("Plan nodes (#1849: a non-empty description and >=1 acceptance criterion per node are REQUIRED — the dry-run refuses seedless nodes): [{key, title, description, tags, acceptance_criteria, depends_on:[keys]}]")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardAddDependencyTool = mcp.NewTool("board_add_dependency",
	mcp.WithDescription("Add a structural dependency edge (rulings R3/R4 as amended by #1747 — the ONLY "+
		"blocking mechanism): N-ary task→task within a WORKSPACE (cross-epic allowed, never cross-"+
		"workspace); cycles are rejected with did-you-mean suggestions (409); duplicate edges are "+
		"idempotent no-ops; edges per node are capped (blast-radius bound). Unblocks when ALL targets are Done."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Dependent story short id (tsk-…, phrase or legacy form)")),
	mcp.WithString("depends_on", mcp.Required(), mcp.Description("Prerequisite story short id (tsk-…, phrase or legacy form)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

var boardRemoveDependencyTool = mcp.NewTool("board_remove_dependency",
	mcp.WithDescription("Remove a dependency edge — one of the pending_input resolutions (removing an "+
		"abandoned prerequisite's edge unblocks the row; the human re-queue stays a move-stage write)."),
	mcp.WithString("story_id", mcp.Required(), mcp.Description("Dependent story short id (tsk-…, phrase or legacy form)")),
	mcp.WithString("depends_on", mcp.Required(), mcp.Description("Prerequisite story short id (tsk-…, phrase or legacy form)")),
	mcp.WithString("request_id", mcp.Description("Idempotency key (one per logical operation)")),
)

// --- the story rename's one-release compatibility window ---
//
// Owner ruling 2026-10-30 phase 2 ("the machinery follows the prose"):
// every task-specific board_* tool name and its story-reference INPUT
// KEYS say story (board_create_story, story_id, story_ids). The
// pre-rename spellings — tool names board_*_task(s) and input keys
// task_id / task_ids — resolve as DEPRECATED ALIASES through ONE
// release window (v0.45.x), every such answer carrying the notice
// below; the removal ships in v0.46.0 with the CHANGELOG breaking
// entry. The REST wire beneath (paths, DTO fields, the plan payload's
// "tasks" key, the board_events action vocabulary) keeps its task
// spelling deliberately — that is the platform's wire term, not an
// MCP name (documented in the rename PR).

// boardStoryAliasWindow names the releases the deprecated spellings
// live: accepted through v0.45.x, removed in v0.46.0.
const boardStoryAliasWindow = "accepted through v0.45.x, removed in v0.46.0"

// boardStoryAliasNotice is appended to every answer served under a
// deprecated alias TOOL NAME.
func boardStoryAliasNotice(alias, canonical string) string {
	return fmt.Sprintf("DEPRECATED: %s is the pre-rename name of %s (the Sprint Board story rename, phase 2 — owner ruling 2026-10-30). "+
		"It behaves identically (%s); call %s — with story_id/story_ids input keys — instead.",
		alias, canonical, boardStoryAliasWindow, canonical)
}

// boardStoryLegacyKeyNotice is appended when a call carried the
// pre-rename INPUT-KEY spellings (task_id/task_ids).
const boardStoryLegacyKeyNotice = "DEPRECATED: the task_id/task_ids argument keys are the pre-rename spellings of " +
	"story_id/story_ids (the Sprint Board story rename, phase 2 — owner ruling 2026-10-30). " +
	"They resolve identically (" + boardStoryAliasWindow + "); send story_id/story_ids instead."

// storyRefArg reads the story reference under the canonical story_id
// key, falling back to the one-release legacy task_id spelling.
func storyRefArg(args map[string]any) string {
	if v := strArg(args, "story_id"); v != "" {
		return v
	}
	return strArg(args, "task_id")
}

// storyRefIDsArg reads the bulk story-reference array under the
// canonical story_ids key, falling back to legacy task_ids.
func storyRefIDsArg(args map[string]any) ([]any, bool) {
	if v, ok := args["story_ids"].([]any); ok {
		return v, true
	}
	v, ok := args["task_ids"].([]any)
	return v, ok
}

// boardStoryLegacyKeysPresent reports whether the call used the
// pre-rename input-key spellings.
func boardStoryLegacyKeysPresent(args map[string]any) bool {
	if _, ok := args["task_id"]; ok {
		return true
	}
	_, ok := args["task_ids"]
	return ok
}

// boardStoryToolAliases maps every deprecated alias tool name to its
// canonical rename (registration order = tools/list order).
var boardStoryToolAliases = map[string]string{
	"board_create_task":   "board_create_story",
	"board_task_verdicts": "board_story_verdicts",
	"board_task_events":   "board_story_events",
	"board_start_tasks":   "board_start_stories",
	"board_start_task":    "board_start_story",
	"board_delete_tasks":  "board_delete_stories",
	"board_rearm_task":    "board_rearm_story",
	"board_pause_task":    "board_pause_story",
	"board_resume_task":   "board_resume_story",
	// The S5 depose tool shipped (v0.44.x, the integration branch)
	// under its task name — same one-release window as the fleet.
	"board_depose_task": "board_depose_story",
}

// storyDeprecationMiddleware is the notice carrier for the alias
// window: a call served under a deprecated alias TOOL NAME gets the
// alias notice, and a call (under any board tool) carrying the legacy
// INPUT-KEY spellings gets the key notice — both appended as a second
// content block on the result, error results included (the #1816 §2
// discipline: a deprecated form that fails still says it is going
// away). mcp-go resolves the tool BEFORE middleware runs, so the
// aliases are REGISTERED tools whose handlers are the canonical ones;
// this middleware adds only the notices.
func storyDeprecationMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := next(ctx, req)
		if err != nil || res == nil {
			return res, err
		}
		var notice string
		if canonical, ok := boardStoryToolAliases[req.Params.Name]; ok {
			notice = boardStoryAliasNotice(req.Params.Name, canonical)
		}
		if notice == "" && strings.HasPrefix(req.Params.Name, "board_") &&
			boardStoryLegacyKeysPresent(req.GetArguments()) {
			notice = boardStoryLegacyKeyNotice
		}
		if notice == "" {
			return res, nil
		}
		res.Content = append(res.Content, mcp.NewTextContent(notice))
		return res, nil
	}
}

// sortedStoryAliases returns the alias→canonical pairs in
// deterministic (sorted by alias) order so registration — and thus
// tools/list ordering — is stable.
func sortedStoryAliases() []struct{ alias, canonical string } {
	names := make([]string, 0, len(boardStoryToolAliases))
	for a := range boardStoryToolAliases {
		names = append(names, a)
	}
	sort.Strings(names)
	out := make([]struct{ alias, canonical string }, 0, len(names))
	for _, a := range names {
		out = append(out, struct{ alias, canonical string }{a, boardStoryToolAliases[a]})
	}
	return out
}

// --- Tool handlers ---

func (h *handlers) boardListProjects(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := h.client.BoardListProjects(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to list board projects: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardGet(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id := strArg(args, "item_id")
	if id == "" {
		return mcp.NewToolResultError("item_id is required"), nil
	}
	kind, err := boardItemKind(id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var resp json.RawMessage
	switch kind {
	case "project":
		resp, err = h.client.BoardGetProject(ctx, id)
	case "epic":
		resp, err = h.client.BoardGetEpic(ctx, id)
	case "task":
		resp, err = h.client.BoardGetStory(ctx, id)
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to get %s %s: %v", kind, id, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardStoryEvents(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id := storyRefArg(args)
	if id == "" {
		return mcp.NewToolResultError("story_id is required"), nil
	}
	limit := intArg(args, "limit")
	resp, err := h.client.BoardStoryEvents(ctx, id, limit)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to read task events for %s: %v", id, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardRead(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	projectID := strArg(args, "project_id")
	stage := strArg(args, "stage")
	kind := strArg(args, "kind")
	q := strArg(args, "q")
	if b, ok := boolArg(args, "health"); ok && b {
		resp, err := h.client.BoardHealth(ctx)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read board health: %v", err)), nil
		}
		return mcp.NewToolResultText(string(resp)), nil
	}
	if q != "" {
		resp, err := h.client.BoardSearch(ctx, BoardSearchQuery{
			Q: q, Project: projectID, Epic: strArg(args, "epic_id"), Stage: stage,
			Tags: stringSliceArg(args, "tags"),
		})
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to search board: %v", err)), nil
		}
		return mcp.NewToolResultText(string(resp)), nil
	}
	if projectID == "" {
		return mcp.NewToolResultError("project_id is required (or pass q to search, health for the snapshot)"), nil
	}
	if kind == "epics" {
		resp, err := h.client.BoardListEpics(ctx, projectID)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("failed to read board epics: %v", err)), nil
		}
		return mcp.NewToolResultText(string(resp)), nil
	}
	if stage != "" && !boardStageSet()[stage] {
		return mcp.NewToolResultError("stage filter must be one of " + strings.Join(boardStages, ", ")), nil
	}
	// #1813: tri-state — explicit true/false both thread (absent = no
	// filter). The tool description promises "false only clean rows".
	failed := ""
	if b, ok := boolArg(args, "failed"); ok {
		failed = strconv.FormatBool(b)
	}
	resp, err := h.client.BoardListTasks(ctx, projectID, stage, failed)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to read board tasks: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardHealth(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := h.client.BoardHealth(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to read board health: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardCreateProject(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	name := strArg(args, "name")
	targetWS := strArg(args, "target_workspace_id")
	if name == "" || targetWS == "" {
		return mcp.NewToolResultError("name and target_workspace_id are required"), nil
	}
	resp, err := h.client.BoardCreateProject(ctx, BoardProjectBody{
		Name: name, TargetWorkspaceID: targetWS,
		WorkerSlots: intArg(args, "worker_slots"), Tags: stringSliceArg(args, "tags"),
		RequestID: strArg(args, "request_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create board project: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardCreateEpic(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	projectID := strArg(args, "project_id")
	title := strArg(args, "title")
	if projectID == "" || title == "" {
		return mcp.NewToolResultError("project_id and title are required"), nil
	}
	resp, err := h.client.BoardCreateEpic(ctx, projectID, BoardEpicBody{
		Title: title, Outcome: strArg(args, "outcome"),
		Tags: stringSliceArg(args, "tags"), RequestID: strArg(args, "request_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create board epic: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardCreateStory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	epicID := strArg(args, "epic_id")
	title := strArg(args, "title")
	if epicID == "" || title == "" {
		return mcp.NewToolResultError("epic_id and title are required"), nil
	}
	// #1837 P2b: create never had criteria check-off — refuse the field
	// loudly (the uniform no-silent-drop posture), never ignore it.
	if _, has := args["set_criteria_met"]; has && args["set_criteria_met"] != nil {
		return mcp.NewToolResultError("set_criteria_met applies to task UPDATES only — create accepts criteria with explicit met flags"), nil
	}
	criteria, err := boardCriteriaArg(args, "acceptance_criteria")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp, err := h.client.BoardCreateStory(ctx, epicID, BoardStoryBody{
		Title: title, Description: strArg(args, "description"),
		Tags: stringSlicePtrArg(args, "tags"), AcceptanceCriteria: criteria,
		DependsOn: stringSliceArg(args, "depends_on"), RequestID: strArg(args, "request_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to create board task: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardUpdateItem(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id := strArg(args, "item_id")
	if id == "" {
		return mcp.NewToolResultError("item_id is required"), nil
	}
	kind, err := boardItemKind(id)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	// Kind-scoped fields: only the addressed kind's PATCH fields are
	// accepted — a field for another kind is an explicit error, never a
	// silent drop.
	allowed := map[string]bool{"item_id": true, "request_id": true}
	switch kind {
	case "project":
		allowed["name"] = true
		allowed["target_workspace_id"] = true
		allowed["worker_slots"] = true
		allowed["tags"] = true
	case "epic":
		allowed["title"] = true
		allowed["outcome"] = true
		allowed["tags"] = true
	case "task":
		allowed["title"] = true
		allowed["description"] = true
		allowed["tags"] = true
		allowed["acceptance_criteria"] = true
		allowed["set_criteria_met"] = true
	}
	var extraneous []string
	for k := range args {
		if !allowed[k] {
			extraneous = append(extraneous, k)
		}
	}
	if len(extraneous) > 0 {
		sort.Strings(extraneous)
		return mcp.NewToolResultError(fmt.Sprintf(
			"field(s) %v do not apply to a %s update (this tool patches one kind at a time; the prj-/epc-/tsk- prefix selects it)", extraneous, kind)), nil
	}
	requestID := strArg(args, "request_id")
	var resp json.RawMessage
	switch kind {
	case "project":
		resp, err = h.client.BoardUpdateProject(ctx, id, BoardProjectBody{
			Name: strArg(args, "name"), TargetWorkspaceID: strArg(args, "target_workspace_id"),
			WorkerSlots: intArg(args, "worker_slots"), Tags: stringSliceArg(args, "tags"),
			RequestID: requestID,
		})
	case "epic":
		resp, err = h.client.BoardUpdateEpic(ctx, id, BoardEpicBody{
			Title: strArg(args, "title"), Outcome: strArg(args, "outcome"),
			Tags:      stringSliceArg(args, "tags"),
			RequestID: requestID,
		})
	case "task":
		criteria, cerr := boardCriteriaArg(args, "acceptance_criteria")
		if cerr != nil {
			return mcp.NewToolResultError(cerr.Error()), nil
		}
		check, cerr := criteriaCheckArg(args, "set_criteria_met")
		if cerr != nil {
			return mcp.NewToolResultError(cerr.Error()), nil
		}
		resp, err = h.client.BoardUpdateStory(ctx, id, BoardStoryBody{
			Title: strArg(args, "title"), Description: strArg(args, "description"),
			Tags:               stringSlicePtrArg(args, "tags"),
			AcceptanceCriteria: criteria, SetCriteriaMet: check,
			RequestID: requestID,
		})
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to update board %s %s: %v", kind, id, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardMoveStage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	toStage := strArg(args, "to_stage")
	if taskID == "" || toStage == "" {
		return mcp.NewToolResultError("story_id and to_stage are required"), nil
	}
	if !boardMoveTargetSet()[toStage] {
		return mcp.NewToolResultError("to_stage must be a legal manager move target: " + strings.Join(boardMoveTargets, ", ") +
			" (removal is board_delete — never a stage move; entering the pipeline from backlog is claim-only — board_start_story)"), nil
	}
	resp, err := h.client.BoardMoveStage(ctx, taskID, toStage, strArg(args, "evidence_url"), strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to move stage: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

// verdictFindingsArg decodes the findings array from MCP arguments
// (#1832 item 5). A malformed entry is a tool error BEFORE any client
// call — the adapter mirrors the server's strict-bind posture.
func verdictFindingsArg(args map[string]any) ([]BoardVerdictFinding, error) {
	raw, ok := args["findings"]
	if !ok || raw == nil {
		return nil, nil
	}
	decodeFinding := func(e any) (BoardVerdictFinding, bool) {
		f := BoardVerdictFinding{}
		m, ok := e.(map[string]any)
		if !ok {
			return f, false
		}
		f.Severity, _ = m["severity"].(string)
		f.Pointer, _ = m["pointer"].(string)
		if v, ok := m["criterionIndex"]; ok && v != nil {
			if f64, ok := v.(float64); ok {
				i := int(f64)
				f.CriterionIndex = &i
			}
		}
		return f, true
	}
	var out []BoardVerdictFinding
	switch arr := raw.(type) {
	case []any: // the wire shape (JSON round-trip)
		out = make([]BoardVerdictFinding, 0, len(arr))
		for _, e := range arr {
			f, ok := decodeFinding(e)
			if !ok {
				return nil, fmt.Errorf("findings entries must be {severity, pointer, criterionIndex?} objects")
			}
			out = append(out, f)
		}
	case []map[string]any: // the in-process test harness shape
		out = make([]BoardVerdictFinding, 0, len(arr))
		for _, m := range arr {
			f, ok := decodeFinding(map[string]any(m))
			if !ok {
				return nil, fmt.Errorf("findings entries must be {severity, pointer, criterionIndex?} objects")
			}
			out = append(out, f)
		}
	default:
		return nil, fmt.Errorf("findings must be an array of {severity, pointer, criterionIndex?} objects")
	}
	return out, nil
}

func (h *handlers) boardRecordVerdict(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	verdict := strArg(args, "verdict")
	reviewer := strArg(args, "reviewer")
	if taskID == "" || verdict == "" || reviewer == "" {
		return mcp.NewToolResultError("story_id, verdict, and reviewer are required"), nil
	}
	if verdict != "approve" && verdict != "fail" {
		return mcp.NewToolResultError("verdict must be approve or fail"), nil
	}
	findings, err := verdictFindingsArg(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp, err := h.client.BoardRecordVerdict(ctx, taskID, BoardVerdictBody{
		Verdict: verdict, Findings: findings, Reviewer: reviewer,
		EvidenceURL: strArg(args, "evidence_url"), RequestID: strArg(args, "request_id"),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to record verdict: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardStoryVerdicts(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	if taskID == "" {
		return mcp.NewToolResultError("story_id is required"), nil
	}
	resp, err := h.client.BoardStoryVerdicts(ctx, taskID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to read verdict chain: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardStartStory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	if taskID == "" {
		return mcp.NewToolResultError("story_id is required"), nil
	}
	resp, err := h.client.BoardStartStory(ctx, taskID, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to start task: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

// bulkIDsArg reads + validates the #1836 batch arrays: present,
// non-empty, under the cap. Front-running a doomed wire call at the
// tier that can is the #1904 discipline.
func bulkIDsArg(args map[string]any) ([]string, error) {
	raw, ok := storyRefIDsArg(args)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("story_ids is required (1 to %d story ids)", board.BulkMaxItems)
	}
	if len(raw) > board.BulkMaxItems {
		return nil, fmt.Errorf("bulk batch capped at %d items (got %d) — split the batch", board.BulkMaxItems, len(raw))
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("story_ids entries must be non-empty story id strings")
		}
		out = append(out, s)
	}
	return out, nil
}

func (h *handlers) boardStartStories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	ids, err := bulkIDsArg(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	resp, err := h.client.BoardStartStories(ctx, ids, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to bulk start tasks: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardMoveStageBulk(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	ids, err := bulkIDsArg(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	toStage := strArg(args, "to_stage")
	if toStage == "" {
		return mcp.NewToolResultError("to_stage is required"), nil
	}
	if !boardMoveTargetSet()[toStage] {
		return mcp.NewToolResultError("to_stage must be a legal manager move target: " + strings.Join(boardMoveTargets, ", ") +
			" (removal is board_delete — never a stage move; entering the pipeline from backlog is claim-only — board_start_story)"), nil
	}
	resp, err := h.client.BoardMoveStageBulk(ctx, ids, toStage, strArg(args, "evidence_url"), strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to bulk move stages: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardDeleteStories(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	ids, err := bulkIDsArg(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	reconnect, _ := args["reconnect"].(bool)
	resp, err := h.client.BoardDeleteStoriesBulk(ctx, ids, reconnect, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to bulk delete tasks: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardRearmStory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	if taskID == "" {
		return mcp.NewToolResultError("story_id is required"), nil
	}
	resp, err := h.client.BoardRearmStory(ctx, taskID, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to re-arm task: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardDeposeStory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	if taskID == "" {
		return mcp.NewToolResultError("story_id is required"), nil
	}
	ptr := board.SalvagePointer{
		Branch: strArg(args, "branch"),
		PR:     strArg(args, "pr"),
		Head:   strArg(args, "head"),
		State:  strArg(args, "state"),
		Note:   strArg(args, "note"),
	}
	if ptr.Empty() {
		return mcp.NewToolResultError("the salvage pointer is empty — provide at least one of branch, pr, head, state, note"), nil
	}
	resp, err := h.client.BoardDeposeStory(ctx, taskID, ptr, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to depose story: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardEpicLifecycle(ctx context.Context, req mcp.CallToolRequest,
	call func(ctx context.Context, epicID, requestID string) (json.RawMessage, error), verb string) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	epicID := strArg(args, "epic_id")
	if epicID == "" {
		return mcp.NewToolResultError("epic_id is required"), nil
	}
	resp, err := call(ctx, epicID, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to %s epic: %v", verb, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardEpicActivate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	epicID := strArg(args, "epic_id")
	if epicID == "" {
		return mcp.NewToolResultError("epic_id is required"), nil
	}
	ack, _ := args["acknowledge_inactive_deps"].(bool)
	resp, err := h.client.BoardEpicActivate(ctx, epicID, ack, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to activate epic: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardEpicPause(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardEpicLifecycle(ctx, req, h.client.BoardEpicPause, "pause")
}

func (h *handlers) boardEpicArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardEpicLifecycle(ctx, req, h.client.BoardEpicArchive, "archive")
}

func (h *handlers) boardEpicUnarchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardEpicLifecycle(ctx, req, h.client.BoardEpicUnarchive, "unarchive")
}

func (h *handlers) boardProjectLifecycle(ctx context.Context, req mcp.CallToolRequest,
	call func(ctx context.Context, projectID, requestID string) (json.RawMessage, error), verb string) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	projectID := strArg(args, "project_id")
	if projectID == "" {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	resp, err := call(ctx, projectID, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to %s project: %v", verb, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardProjectArchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardProjectLifecycle(ctx, req, h.client.BoardArchiveProject, "archive")
}

func (h *handlers) boardProjectUnarchive(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardProjectLifecycle(ctx, req, h.client.BoardUnarchiveProject, "unarchive")
}

func (h *handlers) boardProjectPause(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardProjectLifecycle(ctx, req, h.client.BoardProjectPause, "pause")
}

func (h *handlers) boardProjectResume(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return h.boardProjectLifecycle(ctx, req, h.client.BoardProjectResume, "resume")
}

func (h *handlers) boardPauseStory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	if taskID == "" {
		return mcp.NewToolResultError("story_id is required"), nil
	}
	resp, err := h.client.BoardPauseStory(ctx, taskID, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to pause task: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardResumeStory(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	if taskID == "" {
		return mcp.NewToolResultError("story_id is required"), nil
	}
	resp, err := h.client.BoardResumeStory(ctx, taskID, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to resume task: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardDelete(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	id := strArg(args, "item_id")
	if id == "" {
		return mcp.NewToolResultError("item_id is required"), nil
	}
	force, _ := args["force"].(bool)
	reconnect, _ := args["reconnect"].(bool)
	var resp json.RawMessage
	var err error
	kind, kerr := boardItemKind(id)
	if kerr != nil {
		return mcp.NewToolResultError(kerr.Error()), nil
	}
	switch kind {
	case "epic":
		resp, err = h.client.BoardDeleteEpic(ctx, id, force)
	case "task":
		resp, err = h.client.BoardDeleteStory(ctx, id, reconnect)
	case "project":
		return mcp.NewToolResultError("projects are not deleted — archive them (reversible storage); delete is for epics and tasks"), nil
	}
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to delete %s %s: %v", kind, id, err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardImportPlan(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	projectID := strArg(args, "project_id")
	epicTitle := strArg(args, "epic_title")
	if projectID == "" || epicTitle == "" {
		return mcp.NewToolResultError("project_id and epic_title are required"), nil
	}
	body := BoardPlanBody{EpicTitle: epicTitle, Outcome: strArg(args, "outcome"), RequestID: strArg(args, "request_id")}
	body.DryRun, _ = args["dry_run"].(bool)
	if raw, ok := args["tasks"].([]any); ok {
		for _, e := range raw {
			m, ok := e.(map[string]any)
			if !ok {
				return mcp.NewToolResultError("tasks entries must be objects"), nil
			}
			t := BoardPlanStoryBody{}
			t.Key, _ = m["key"].(string)
			t.Title, _ = m["title"].(string)
			t.Description, _ = m["description"].(string)
			if arr, ok := m["tags"].([]any); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok {
						t.Tags = append(t.Tags, s)
					}
				}
			}
			if arr, ok := m["depends_on"].([]any); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok {
						t.DependsOn = append(t.DependsOn, s)
					}
				}
			}
			if arr, ok := m["acceptance_criteria"].([]any); ok {
				for _, v := range arr {
					if cm, ok := v.(map[string]any); ok {
						c := BoardCriterion{}
						c.Text, _ = cm["text"].(string)
						if b, ok := cm["met"].(bool); ok {
							c.Met = b
						}
						t.AcceptanceCriteria = append(t.AcceptanceCriteria, c)
					}
				}
			}
			if t.Key == "" || t.Title == "" {
				return mcp.NewToolResultError("every plan task needs key and title"), nil
			}
			// #1849: the seed contract rides plan nodes — teach it at the
			// same tier the schema does (the service refuses seedless
			// nodes at dry-run; this front-runs the burned round-trip).
			if t.Description == "" || len(t.AcceptanceCriteria) == 0 {
				return mcp.NewToolResultError("every plan task needs a description (the worker's charter) and at least one acceptance criterion (#1849) — the dry-run refuses seedless nodes"), nil
			}
			body.Tasks = append(body.Tasks, t)
		}
	}
	resp, err := h.client.BoardImportPlan(ctx, projectID, body)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to import plan: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardAddDependency(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	dependsOn := strArg(args, "depends_on")
	if taskID == "" || dependsOn == "" {
		return mcp.NewToolResultError("story_id and depends_on are required"), nil
	}
	resp, err := h.client.BoardAddDependency(ctx, taskID, dependsOn, strArg(args, "request_id"))
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to add dependency: %v", err)), nil
	}
	return mcp.NewToolResultText(string(resp)), nil
}

func (h *handlers) boardRemoveDependency(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()
	taskID := storyRefArg(args)
	dependsOn := strArg(args, "depends_on")
	if taskID == "" || dependsOn == "" {
		return mcp.NewToolResultError("story_id and depends_on are required"), nil
	}
	if err := h.client.BoardRemoveDependency(ctx, taskID, dependsOn, strArg(args, "request_id")); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("failed to remove dependency: %v", err)), nil
	}
	return mcp.NewToolResultText(`{"removed":true}`), nil
}

// boardItemKind resolves the prj-/epc-/tsk- prefix to its REST resource.
// Bare UUIDs are refused: the unified tools (board_get, board_update_item,
// board_delete) need the kind to select the route, and short ids are this
// surface's addressing contract (§4.3a).
func boardItemKind(id string) (string, error) {
	switch {
	case strings.HasPrefix(id, "prj-"):
		return "project", nil
	case strings.HasPrefix(id, "epc-"):
		return "epic", nil
	case strings.HasPrefix(id, "tsk-"):
		return "task", nil
	default:
		return "", fmt.Errorf("item_id must be a short id carrying its prj-/epc-/tsk- prefix (got %q) — the prefix selects the item kind", id)
	}
}

// boardStageSet is the WIRE stage set as a set (the board_read filter's
// vocabulary — built from boardStages, the single source).
func boardStageSet() map[string]bool {
	out := make(map[string]bool, len(boardStages))
	for _, s := range boardStages {
		out[s] = true
	}
	return out
}

// boardMoveTargetSet is the legal manager move_stage target set (built
// from boardMoveTargets, the single source).
func boardMoveTargetSet() map[string]bool {
	out := make(map[string]bool, len(boardMoveTargets))
	for _, s := range boardMoveTargets {
		out[s] = true
	}
	return out
}

// stringSlicePtrArg is the presence-aware slice decoder (r2): a PRESENT
// key yields a pointer to the (possibly empty) slice, an ABSENT key
// yields nil. The REST binding carries exactly this distinction for
// task tags — "tags":[] clears, omission keeps — so the wire must see
// it too.
func stringSlicePtrArg(args map[string]any, key string) *[]string {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, _ := v.(string)
		out = append(out, s)
	}
	return &out
}

// intArg pulls an integer argument. JSON decoding yields float64;
// direct in-process callers may hand over native ints — both are
// accepted (the run_resolve/workflow handlers' string-arg pattern,
// numeric edition).
func intArg(args map[string]any, key string) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	if v, ok := args[key].(int); ok {
		return v
	}
	if v, ok := args[key].(int64); ok {
		return int(v)
	}
	return 0
}

// criteriaCheckArg pulls the optional set_criteria_met argument
// (#1837 P2b): the object form {index, met} or the legacy integer
// (check-off). A malformed payload is an error BEFORE any client call.
func criteriaCheckArg(args map[string]any, key string) (*board.CriteriaCheck, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	switch v := raw.(type) {
	case float64:
		if v != float64(int(v)) {
			return nil, fmt.Errorf("%s: index must be an integer", key)
		}
		return &board.CriteriaCheck{Index: int(v), Met: true}, nil
	case int:
		return &board.CriteriaCheck{Index: v, Met: true}, nil
	case int64:
		return &board.CriteriaCheck{Index: int(v), Met: true}, nil
	case map[string]any:
		idxRaw, hasIdx := v["index"]
		if !hasIdx || idxRaw == nil {
			return nil, fmt.Errorf("%s: {index, met} requires index", key)
		}
		f64, ok := idxRaw.(float64)
		if !ok || f64 != float64(int(f64)) {
			return nil, fmt.Errorf("%s: index must be an integer", key)
		}
		met := true
		if m, has := v["met"]; has && m != nil {
			b, ok := m.(bool)
			if !ok {
				return nil, fmt.Errorf("%s: met must be a boolean", key)
			}
			met = b
		}
		return &board.CriteriaCheck{Index: int(f64), Met: met}, nil
	default:
		return nil, fmt.Errorf("%s: expected {index, met} or a legacy integer index", key)
	}
}

// boardCriteriaArg decodes the acceptance_criteria array ([{text, met}],
// ruling R2) from MCP arguments.
func boardCriteriaArg(args map[string]any, key string) ([]BoardCriterion, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of {text, met} objects", key)
	}
	out := make([]BoardCriterion, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s entries must be {text, met} objects", key)
		}
		text, _ := m["text"].(string)
		if text == "" {
			return nil, fmt.Errorf("each acceptance criterion requires non-empty text")
		}
		met, _ := m["met"].(bool)
		out = append(out, BoardCriterion{Text: text, Met: met})
	}
	return out, nil
}

// AddBoardTools registers the external board_* tools (design 0067 §4.3b)
// on an existing MCPServer. Called by NewServer after the Epic 64 tools.
func AddBoardTools(srv *server.MCPServer, h *handlers) {
	srv.Use(storyDeprecationMiddleware)
	srv.AddTools(
		server.ServerTool{Tool: boardListProjectsTool, Handler: h.boardListProjects},
		server.ServerTool{Tool: boardGetTool, Handler: h.boardGet},
		server.ServerTool{Tool: boardReadTool, Handler: h.boardRead},
		server.ServerTool{Tool: boardStoryEventsTool, Handler: h.boardStoryEvents},
		server.ServerTool{Tool: boardHealthTool, Handler: h.boardHealth},
		server.ServerTool{Tool: boardCreateProjectTool, Handler: h.boardCreateProject},
		server.ServerTool{Tool: boardCreateEpicTool, Handler: h.boardCreateEpic},
		server.ServerTool{Tool: boardCreateStoryTool, Handler: h.boardCreateStory},
		server.ServerTool{Tool: boardUpdateItemTool, Handler: h.boardUpdateItem},
		server.ServerTool{Tool: boardMoveStageTool, Handler: h.boardMoveStage},
		server.ServerTool{Tool: boardRecordVerdictTool, Handler: h.boardRecordVerdict},
		server.ServerTool{Tool: boardStoryVerdictsTool, Handler: h.boardStoryVerdicts},
		server.ServerTool{Tool: boardStartStoryTool, Handler: h.boardStartStory},
		server.ServerTool{Tool: boardStartStoriesTool, Handler: h.boardStartStories},
		server.ServerTool{Tool: boardMoveStageBulkTool, Handler: h.boardMoveStageBulk},
		server.ServerTool{Tool: boardDeleteStoriesTool, Handler: h.boardDeleteStories},
		server.ServerTool{Tool: boardRearmStoryTool, Handler: h.boardRearmStory},
		server.ServerTool{Tool: boardDeposeStoryTool, Handler: h.boardDeposeStory},
		server.ServerTool{Tool: boardEpicActivateTool, Handler: h.boardEpicActivate},
		server.ServerTool{Tool: boardEpicPauseTool, Handler: h.boardEpicPause},
		server.ServerTool{Tool: boardEpicArchiveTool, Handler: h.boardEpicArchive},
		server.ServerTool{Tool: boardEpicUnarchiveTool, Handler: h.boardEpicUnarchive},
		server.ServerTool{Tool: boardProjectArchiveTool, Handler: h.boardProjectArchive},
		server.ServerTool{Tool: boardProjectUnarchiveTool, Handler: h.boardProjectUnarchive},
		server.ServerTool{Tool: boardProjectPauseTool, Handler: h.boardProjectPause},
		server.ServerTool{Tool: boardProjectResumeTool, Handler: h.boardProjectResume},
		server.ServerTool{Tool: boardPauseStoryTool, Handler: h.boardPauseStory},
		server.ServerTool{Tool: boardResumeStoryTool, Handler: h.boardResumeStory},
		server.ServerTool{Tool: boardDeleteTool, Handler: h.boardDelete},
		server.ServerTool{Tool: boardImportPlanTool, Handler: h.boardImportPlan},
		server.ServerTool{Tool: boardAddDependencyTool, Handler: h.boardAddDependency},
		server.ServerTool{Tool: boardRemoveDependencyTool, Handler: h.boardRemoveDependency},
	)
	// The one-release deprecated alias names (story rename phase 2):
	// mcp-go resolves tools by name BEFORE middleware runs, so an alias
	// must be a REGISTERED tool — same input schema as its canonical
	// twin, same handler, a description that names the rename and the
	// removal release. storyDeprecationMiddleware (installed above)
	// appends the per-call deprecation notice.
	boardStoryAliasRegistrations(srv, h)
}

// boardStoryAliasRegistrations registers every deprecated alias name
// against its canonical tool + handler (the alias table lives in
// boardStoryToolAliases; this wires the Go side).
func boardStoryAliasRegistrations(srv *server.MCPServer, h *handlers) {
	type aliasReg struct {
		tool    mcp.Tool
		handler server.ToolHandlerFunc
	}
	canonical := map[string]aliasReg{
		"board_create_story":   {boardCreateStoryTool, h.boardCreateStory},
		"board_story_verdicts": {boardStoryVerdictsTool, h.boardStoryVerdicts},
		"board_story_events":   {boardStoryEventsTool, h.boardStoryEvents},
		"board_start_stories":  {boardStartStoriesTool, h.boardStartStories},
		"board_start_story":    {boardStartStoryTool, h.boardStartStory},
		"board_delete_stories": {boardDeleteStoriesTool, h.boardDeleteStories},
		"board_rearm_story":    {boardRearmStoryTool, h.boardRearmStory},
		"board_pause_story":    {boardPauseStoryTool, h.boardPauseStory},
		"board_resume_story":   {boardResumeStoryTool, h.boardResumeStory},
		"board_depose_story":   {boardDeposeStoryTool, h.boardDeposeStory},
	}
	for _, pair := range sortedStoryAliases() {
		reg, ok := canonical[pair.canonical]
		if !ok {
			continue
		}
		aliasTool := reg.tool
		aliasTool.Name = pair.alias
		aliasTool.Description = "DEPRECATED ALIAS of " + pair.canonical + " (the Sprint Board story rename, phase 2 — owner ruling 2026-10-30). " +
			"Behaves identically (" + boardStoryAliasWindow + "); call " + pair.canonical + " instead."
		srv.AddTool(aliasTool, reg.handler)
	}
}
