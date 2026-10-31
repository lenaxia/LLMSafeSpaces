// LLMSafeSpaces origin-injection plugin (issue #1465, extended #1627).
//
// Runs INSIDE opencode as a first-party platform plugin and stamps the
// calling session's ID onto the llmsafespaces MCP tools' outgoing
// arguments, so agentd can attribute inter-session messages and scope
// session_metadata to the calling session without the model ever
// supplying (or seeing) a session parameter.
//
// Mechanism (verified against the pinned opencode 1.18.15 bundle):
// the harness invokes every "tool.execute.before" hook with
// (input, output) where output.args IS the exact arguments object the
// tool will receive — passed by reference, before the MCP call is
// made. Mutating output.args in place therefore reaches the wire.
//
// Contract rules:
//   - UNCONDITIONAL overwrite: lsp_injected_session (the platform
//     namespace key agentd labels mode "injected" from) is always SET
//     from the harness's input.sessionID, never read — a model-supplied
//     value can never survive a working plugin.
//   - Defensive no-op on any shape mismatch: if the hook surface
//     changes in a future harness, this hook does nothing and agentd
//     REFUSES the send ("no origin: the platform could not inject
//     your session ID — degraded pod") — #1849: there is no
//     self-declared fallback anymore, so drift is loud, never a
//     silent downgrade to an unattested origin.
//   - Never throws: a broken plugin must not break the host tool call.
//
// Delivery vehicle (overlay image vs controller volume) is decided
// platform-side; this file is the single source of the plugin logic.

/** The qualified tool names the harness registers for the platform MCP
 *  server's tools (server "llmsafespaces" + "_" + tool). send_message
 *  carries the origin for attribution (#1465); session_metadata,
 *  delete_session, and the delete bulk (#1836) carry the CALLING
 *  session for the current-session default and the self-delete guard
 *  (#1627). The 21 board MANAGER
 *  tools carry the calling session's identity the same way — AUDIT
 *  ATTRIBUTION ONLY, never a gate (the #1747 §5 manager grant is
 *  repealed, ruling 2026-10-27 / tsk-copper-refectory-generates):
 *  the seam stamps it into the audit actor, and injection OVERWRITES
 *  any model-supplied copy. The session idle-subscription trio (design
 *  /workspace/reports/session-subscription-design.md §5) carries the
 *  caller as the SUBSCRIBER identity (#1849 posture — agentd refuses
 *  an unattested origin), and create_session carries it for the
 *  optional subscribe flag's caller→child watch edge. Kept in
 *  lockstep with the agentd registry by
 *  TestMCPBoard_ManagerInjectionSupply (both directions). */
const INJECTED_SESSION_TOOLS = new Set([
  "llmsafespaces_send_message",
  "llmsafespaces_session_metadata",
  "llmsafespaces_delete_session",
  "llmsafespaces_session_delete_bulk",
  "llmsafespaces_subscribe_session",
  "llmsafespaces_unsubscribe_session",
  "llmsafespaces_list_subscriptions",
  "llmsafespaces_create_session",
  "llmsafespaces_board_create_project",
  "llmsafespaces_board_update_project",
  "llmsafespaces_board_project_archive",
  "llmsafespaces_board_project_unarchive",
  "llmsafespaces_board_project_pause",
  "llmsafespaces_board_project_resume",
  "llmsafespaces_board_create_epic",
  "llmsafespaces_board_update_epic",
  "llmsafespaces_board_epic_activate",
  "llmsafespaces_board_epic_pause",
  "llmsafespaces_board_epic_archive",
  "llmsafespaces_board_epic_unarchive",
  "llmsafespaces_board_delete_epic",
  "llmsafespaces_board_create_task",
  "llmsafespaces_board_update_task",
  "llmsafespaces_board_add_dependency",
  "llmsafespaces_board_remove_dependency",
  "llmsafespaces_board_delete_task",
  "llmsafespaces_board_move_stage_bulk",
  "llmsafespaces_board_delete_tasks",
  "llmsafespaces_board_get_config",
  "llmsafespaces_board_set_config",
  "llmsafespaces_board_pause_task",
  "llmsafespaces_board_resume_task",
  "llmsafespaces_board_rearm_task",
  "llmsafespaces_board_depose_task",
  "llmsafespaces_board_import_plan",
  "llmsafespaces_board_list",
  "llmsafespaces_board_health",
  "llmsafespaces_board_validate_file",
])

/**
 * @returns {import("@opencode-ai/plugin").Hooks}
 */
export default function llmsafespacesOrigin() {
  return {
    "tool.execute.before": async (input, output) => {
      try {
        if (!input || !INJECTED_SESSION_TOOLS.has(input.tool)) return
        if (typeof input.sessionID !== "string" || input.sessionID === "") return
        if (!output || typeof output !== "object") return
        if (!output.args || typeof output.args !== "object") return
        // lsp_injected_session is the PLATFORM namespace: the sole
        // origin source (#1849 — from_session_id is gone), injected
        // unconditionally so a model-supplied copy can never survive;
        // agentd labels the mode "injected" from this key alone.
        output.args.lsp_injected_session = input.sessionID
      } catch {
        // never break the host tool call
      }
    },
  }
}
