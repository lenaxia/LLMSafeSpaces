# Triggers as Routines (Epic 64 Redesign)

**Status:** Design
**Created:** 2026-08-08
**Replaces:** The `target_type` / `run_script` / `run_workflow` split from Epic 64 v1
**Supersedes:** `BuildRunScriptSpec`, synthetic workflow rows, the routine-as-separate-entity proposal

---

## Problem Statement

Epic 64 shipped a DAG workflow engine as the universal automation primitive. The user's actual workflows (5 real flows) reveal that 4 of 5 are single agent turns on a schedule — not DAGs. The current design forces every automation through the DAG model, producing:

1. **`run_script` namespace pollution** — synthetic workflow rows with `script:` prefix, FK hacks, orphan risk
2. **Session clutter** — ephemeral sessions never deleted, accumulating in the workspace sidebar
3. **No cross-run state** — workflows are stateless; the agent has no memory of the previous run
4. **Over-engineering** — 4 of 5 flows don't need topological sort, condition branching, or node retries

The root cause: the trigger→target split forces a choice between "run a DAG" and "run a script," when neither matches the common case — "prompt an agent on a schedule with memory."

## Design

### Trigger absorbs the routine

A trigger IS the automation definition. It carries schedule/event config (source) and either a routine config (inline agent turn) or a workflow reference (DAG). The switch is the presence of `workflow_id`:

```
Trigger
  ├── source: cron | webhook
  ├── source_config: { expr, tz } | { webhook_id }
  │
  ├── workspace_id: uuid (required)
  │
  ├── ─── routine mode (workflow_id IS NULL) ───────
  ├── prompt: text (text/template — {{.input}}, {{.prevResult}})
  ├── agent: string (named opencode agent profile, optional)
  ├── memory:
  │     mode: none | last_result
  │     max_runs: int (how many previous results to inject, default 1)
  ├── capture: errors_only | full (default: errors_only)
  ├── preserve_session: never | always | on_failure (default: never)
  ├── notify: jsonb (notification config — deferred to v2)
  ├── script_path: string (optional — script runs before agent)
  ├── script_args: string[]
  ├── script_env: map[string]string
  │
  ├── ─── workflow mode (workflow_id IS NOT NULL) ──
  └── workflow_id: uuid (references workflows table)
```

When `workflow_id` is set, the trigger fires a DAG run (existing engine, unchanged). When it's NULL, the trigger fires a routine — a single agent turn (optionally preceded by a script) in the target workspace.

### Migration: `target_type` and `target_config` → routine fields

The existing `target_type` (`run_workflow` | `run_script`) and `target_config` (JSON blob) are replaced by explicit columns:

| Old field | New field(s) | Migration |
|---|---|---|
| `target_type: 'run_workflow'` + `target_config: {workflowId}` | `workflow_id` column | Backfill from `target_config->>'workflowId'` |
| `target_type: 'run_script'` + `target_config: {workspaceId, path, args, env, prompt}` | `workspace_id`, `script_path`, `script_args`, `script_env`, `prompt` | Backfill from target_config JSON |
| `target_type` (column) | Dropped | Remove CHECK constraint + column |
| `target_config` (column) | Dropped | — |

### `trigger_fires` gains `result` column

Routine executions store their captured result directly in `trigger_fires.result` (jsonb). This is the memory source for `memory: last_result` — the scheduler queries the last successful fire's result and injects it into the prompt template as `{{.prevResult}}`.

For workflow runs, `trigger_fires.action_result` already carries `{workflow_run_id}`. No change needed for the workflow path.

### Session lifecycle

Routine execution manages sessions explicitly:

| `preserve_session` | Behavior |
|---|---|
| `never` (default) | Session created, agent responds, full transcript captured to `trigger_fires.result`, session DELETED. Zero sidebar footprint. |
| `on_failure` | Session deleted on success; preserved on error (so the user can investigate). |
| `always` | Session persists. Shows in sidebar with origin indicator. Linked from the trigger's fire history. |

The capture policy controls what goes into PG:

| `capture` | Behavior |
|---|---|
| `errors_only` (default) | On success: nothing stored (fire status = `succeeded`, result = null). On failure: full error + transcript stored. |
| `full` | Always stores the agent response (text parts) in `trigger_fires.result`. |

### Memory: `last_result` mode

When `memory.mode = last_result`, the scheduler queries the most recent `trigger_fires` row for this trigger with `status = 'succeeded'` and a non-null `result`. It injects the result as `{{.prevResult}}` in the prompt template before rendering.

This enables Flow 3 (hourly action items) without the agentd MCP server. The agent sees the previous hour's result directly in the prompt. It can compare, roll forward, or start fresh.

`max_runs` controls how far back to look (default 1 = just the last result). This covers the common case. A future `session_chain` mode (continue the same session) covers the advanced case — deferred.

### Routine execution path

```
trigger fires (cron or webhook)
  → activate workspace (existing EnsureActive)
  → if workflow_id set:
       create workflow run (existing DAG engine — unchanged)
       record fire with action_result: {workflow_run_id}
       return
  → resolve memory (query last successful fire's result)
  → render prompt template ({{.input}}, {{.prevResult}})
  → if script_path set:
       exec script via agentd (existing script node execution)
       inject script output as {{.scriptResult}} in prompt
  → create ephemeral opencode session
  → POST /session/:id/message with rendered prompt
  → capture response (text parts + tool parts)
  → apply capture policy (store or discard)
  → apply preserve_session policy (delete or keep)
  → apply notification (deferred)
  → record fire with result
```

This runs in the scheduler (same goroutine that handles cron ticks) or the webhook receiver. No new component.

### Session origin tracking

Preserved sessions need an origin indicator in the sidebar. Since opencode sessions have no metadata field (validated: `POST /session {}` returns `{id, slug, projectID, directory}` — no metadata), we use a PG-side mapping table:

```sql
CREATE TABLE session_origins (
    session_id   text PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    origin       text NOT NULL DEFAULT 'manual',  -- manual | routine | workflow | api
    trigger_id   uuid REFERENCES triggers(id) ON DELETE SET NULL,
    fire_id      uuid,
    title        text,
    created_at   timestamptz DEFAULT now()
);
```

The session list API (currently proxied from opencode) enriches each session with its origin from this table. Sessions not in the table have `origin: manual` (the default — interactive sessions created by the user). The sidebar renders an icon based on origin.

This is the bridge layer — minimal, additive, doesn't touch opencode's session model. When unified sessions arrive (if ever), this table becomes the source of truth and opencode.db becomes a cache.

## Scope

### In scope

- **Trigger schema migration:** add routine fields, drop `target_type` + `target_config`, backfill existing data
- **Routine executor:** the session lifecycle + prompt rendering + result capture path (~150-200 lines)
- **`trigger_fires.result` column:** stores captured results for memory + observability
- **`session_origins` table:** origin tracking for the sidebar
- **Session deletion:** agentd already has `DELETE /session/:id` (validated in the contract). The routine executor calls it after capturing the result.
- **Memory (last_result):** scheduler queries previous fire result, injects into prompt template
- **Frontend trigger form:** workspace picker, prompt editor, memory config, capture/preserve toggles, optional script fields
- **SDK updates:** trigger create/update API changes (target_type → routine fields / workflow_id)
- **MCP tools update:** `trigger_create` tool signature changes

### Out of scope (deferred)

- **Unified sessions** (opencode external session store) — the session_origins bridge is the interim
- **`session_chain` memory mode** — continue same session across runs. Needs session context management (when to reset, context window limits). `last_result` covers the common case.
- **agentd built-in MCP server** — `session_read`, `session_list` tools for in-workspace agents. `last_result` memory mode reduces the need. Build when a flow explicitly requires multi-session rummaging.
- **Notification layer** (slack/email/push) — `notify` field reserved, implementation deferred
- **Event/calendar/email trigger sources** — the user's flows need these but they're independent additions to the source layer
- **Lambda-style execution** — ephemeral pods, not tied to workspaces
- **Configurable concurrency > 1** — single-in-flight still applies to workflow runs. Routine runs are independent (no shared workflow state) so single-in-flight doesn't restrict them.

## Data Model

### Migration 000020: triggers absorb routine

```sql
BEGIN;

-- Add routine fields to triggers
ALTER TABLE triggers ADD COLUMN workspace_id uuid REFERENCES workspaces(id) ON DELETE SET NULL;
ALTER TABLE triggers ADD COLUMN prompt text DEFAULT '';
ALTER TABLE triggers ADD COLUMN agent text DEFAULT '';
ALTER TABLE triggers ADD COLUMN script_path text DEFAULT '';
ALTER TABLE triggers ADD COLUMN script_args text[] DEFAULT '{}';
ALTER TABLE triggers ADD COLUMN script_env jsonb DEFAULT '{}'::jsonb;
ALTER TABLE triggers ADD COLUMN memory_mode text NOT NULL DEFAULT 'none';
ALTER TABLE triggers ADD COLUMN memory_max_runs int NOT NULL DEFAULT 1;
ALTER TABLE triggers ADD COLUMN capture_mode text NOT NULL DEFAULT 'errors_only';
ALTER TABLE triggers ADD COLUMN preserve_session text NOT NULL DEFAULT 'never';
ALTER TABLE triggers ADD COLUMN workflow_id uuid REFERENCES workflows(id) ON DELETE SET NULL;

-- Backfill from existing target_type + target_config
UPDATE triggers
SET workflow_id = (target_config->>'workflowId')::uuid
WHERE target_type = 'run_workflow'
  AND target_config->>'workflowId' IS NOT NULL;

UPDATE triggers
SET workspace_id = (target_config->>'workspaceId')::uuid,
    script_path = COALESCE(target_config->>'path', ''),
    prompt = COALESCE(target_config->>'prompt', ''),
    script_args = ARRAY(
        SELECT json_array_elements_text(target_config->'args')
    ),
    script_env = COALESCE(target_config->'env', '{}'::jsonb)
WHERE target_type = 'run_script';

-- Add CHECK constraints for new fields
ALTER TABLE triggers ADD CONSTRAINT triggers_memory_mode_chk
    CHECK (memory_mode IN ('none', 'last_result'));
ALTER TABLE triggers ADD CONSTRAINT triggers_capture_mode_chk
    CHECK (capture_mode IN ('errors_only', 'full'));
ALTER TABLE triggers ADD CONSTRAINT triggers_preserve_session_chk
    CHECK (preserve_session IN ('never', 'always', 'on_failure'));

-- Drop old columns + constraints
ALTER TABLE triggers DROP CONSTRAINT IF EXISTS triggers_target_type_check;
ALTER TABLE triggers DROP COLUMN IF EXISTS target_type;
ALTER TABLE triggers DROP COLUMN IF EXISTS target_config;

COMMIT;
```

### Migration 000021: trigger_fires result + session_origins

```sql
BEGIN;

-- trigger_fires: add result column for routine memory + observability
ALTER TABLE trigger_fires ADD COLUMN result jsonb;
ALTER TABLE trigger_fires ADD COLUMN result_captured_at timestamptz;

-- trigger_fires: relax action_type CHECK (routines don't have a target_type)
ALTER TABLE trigger_fires DROP CONSTRAINT IF EXISTS trigger_fires_action_type_check;
DO $$ BEGIN
    ALTER TABLE trigger_fires ADD CONSTRAINT trigger_fires_action_type_chk
        CHECK (action_type IN ('run_workflow', 'routine', 'webhook'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Session origin tracking (bridge layer until unified sessions)
CREATE TABLE IF NOT EXISTS session_origins (
    session_id   text PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    origin       text NOT NULL DEFAULT 'manual',
    trigger_id   uuid REFERENCES triggers(id) ON DELETE SET NULL,
    fire_id      uuid,
    title        text,
    created_at   timestamptz DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_session_origins_workspace
    ON session_origins (workspace_id, created_at DESC);

COMMIT;
```

### Routine-specific config fields on triggers

| Column | Type | Default | Purpose |
|---|---|---|---|
| `workspace_id` | uuid FK | NULL | Target workspace for routine execution |
| `prompt` | text | `''` | Prompt template (text/template) |
| `agent` | text | `''` | Named opencode agent profile |
| `script_path` | text | `''` | Optional script to run before agent |
| `script_args` | text[] | `'{}'` | Script arguments |
| `script_env` | jsonb | `'{}'` | Script environment |
| `memory_mode` | text | `'none'` | none or last_result |
| `memory_max_runs` | int | `1` | How many previous results to inject |
| `capture_mode` | text | `'errors_only'` | errors_only or full |
| `preserve_session` | text | `'never'` | never, always, or on_failure |
| `workflow_id` | uuid FK | NULL | If set, fires a DAG instead |

### Existing instance_settings

No changes. The existing `triggers.maxPerUser`, `triggers.cronMinIntervalSec`, `triggers.webhookRateLimitPerSec` apply to all triggers regardless of routine vs workflow mode.

## Execution: Routine path (new)

In the scheduler (`engine.go`), when a trigger fires:

```go
func (s *Scheduler) fireTrigger(...) {
    if trigger.WorkflowID != nil && *trigger.WorkflowID != "" {
        s.fireWorkflowTarget(...)  // existing DAG path — unchanged
        return
    }
    s.fireRoutineTarget(...)  // new routine path
}
```

`fireRoutineTarget`:
1. Activate workspace (reuse `EnsureActive` — needs the scheduler to hold an `WorkspaceActivator`)
2. Render prompt: `text/template` with `{{.input}}` (envelope), `{{.prevResult}}` (memory), `{{.scriptResult}}` (if script ran)
3. If `script_path` is set: dispatch to agentd's script execution endpoint
4. Create opencode session via agentd
5. Send prompt via agentd's agent node execution
6. Capture response (text + tool parts)
7. Apply capture policy
8. Apply preserve_session policy (delete session if `never` or `on_failure`+success)
9. Record fire with result

The scheduler needs new dependencies: `WorkspaceActivator` (already exists in the engine), `AgentdExecutor` (already exists). The routine path reuses existing infrastructure.

## Execution: Workflow path (unchanged)

When `workflow_id` is set, the existing `fireWorkflowTarget` creates a workflow run. The DAG engine picks it up. No changes.

## How each user flow maps

| Flow | Trigger config |
|---|---|
| 1. Outlook sync | cron + `prompt` + `capture: errors_only` + `preserve_session: never` + `memory: none` |
| 2. Daily health report | cron + `prompt` + `capture: full` + `preserve_session: never` + `memory: none` |
| 3. Hourly action items | cron + `prompt` + `capture: full` + `preserve_session: never` + `memory: last_result` |
| 4. Monthly team meeting | cron + `prompt` + `preserve_session: always` + `memory: none` |
| 5. Meeting summary pipeline | webhook + `workflow_id: "wf-meeting-pipeline"` (DAG) |

## What gets deleted

- `BuildRunScriptSpec` function (engine.go) — no longer needed
- `GetOrCreateScriptWorkflow` store method — no longer needed
- `TriggerTargetRunWorkflow` / `TriggerTargetRunScript` constants — replaced by null/non-null `workflow_id`
- `RunScriptTargetConfig` type — fields absorbed into the trigger
- `RunWorkflowTargetConfig` type — `workflow_id` is now a direct column, `input_template` moves to a `workflow_input_template` column or stays in a reduced `source_config`
- `target_type` / `target_config` columns — replaced by explicit fields
- The `run_script` code paths in scheduler + webhook receiver — replaced by `fireRoutineTarget`

## SDK changes

All 4 SDKs (Go, TypeScript, Python, Java) reference `target_type` in their trigger create/update methods. The API signature changes:

**Before:**
```
POST /me/triggers
{ name, sourceType, targetType: "run_workflow", sourceConfig, targetConfig: {workflowId} }
```

**After:**
```
POST /me/triggers
{ name, sourceType, sourceConfig, workspaceId, prompt, memoryMode, captureMode, preserveSession }
// OR
{ name, sourceType, sourceConfig, workflowId }
```

The SDKs need updated methods. This is a breaking API change — version-bumped.

## Adversarial review

### Weaknesses

1. **The scheduler now executes routines directly** — it holds the workspace activation + agentd call. Today the scheduler only fires triggers and creates run rows. With routines, it's doing the actual execution (session lifecycle, prompt rendering, result capture). This makes the scheduler goroutine longer-running and harder to cancel. **Mitigation:** time-box routine execution with the same timeout mechanism as workflow runs. The scheduler already uses goroutines per fire.

2. **Session deletion failure is silent.** If `DELETE /session/:id` fails (opencode hiccup), the session leaks into the sidebar. No retry, no alert. **Mitigation:** log the failure; a janitor cleans orphaned workflow sessions on a TTL (same pattern as `jwt_session_janitor`).

3. **`last_result` memory is lossy for workflows.** It only injects the previous routine's text output, not the full session context. For flows that need deep context ("what did the agent conclude 3 runs ago?"), `last_result` is insufficient. **Mitigation:** this is why `session_chain` mode exists (deferred). `last_result` covers the common case.

4. **Migration is one-way.** Dropping `target_type` and `target_config` means downgrade loses the trigger type info. **Mitigation:** the down migration recreates the columns from the backfilled data.

### False alarms

- **"The trigger entity is too large."** It has ~15 fields. The workflows table has 14. Triggers are the automation definition — they should carry the full config. Splitting routine fields into a separate table adds a join without reducing complexity.

- **"Single-in-flight is still too restrictive."** It only applies to workflow runs (where the partial unique index is per-workflow). Routine runs don't create workflow runs, so they're not subject to the index. Two routine triggers can fire concurrently against the same workspace (Edge Case 16 applies — concurrent run + interactive session is the user's responsibility).

- **"The scheduler can't handle routine execution."** It already handles workspace activation (via the engine's activator), HTTP calls to agentd, and fire-row persistence. Routine execution is the same call pattern with a different payload.

## Next steps

1. Migration 000020 + 000021 (schema changes + backfill)
2. Types update (drop target_type/target_config, add routine fields)
3. Store update (CRUD with new fields)
4. Handler update (create/update with new fields, validation)
5. Scheduler: `fireRoutineTarget` implementation
6. Webhook receiver: routine path
7. Delete `BuildRunScriptSpec`, `GetOrCreateScriptWorkflow`, old target_type code paths
8. Frontend trigger form rebuild (routine fields)
9. Session_origins table + session list enrichment
10. SDK updates (4 SDKs)
11. MCP tools update
12. Tests (TDD per step)
