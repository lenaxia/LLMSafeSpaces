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
//     changes in a future harness, this hook does nothing and the
//     origin falls back to the model-supplied from_session_id
//     (mode "self-declared" — visibly labeled). Silent mode downgrade
//     on drift is the detected, self-reporting failure; silent
//     misattribution is impossible.
//   - Never throws: a broken plugin must not break the host tool call.
//
// Delivery vehicle (overlay image vs controller volume) is decided
// platform-side; this file is the single source of the plugin logic.

/** The qualified tool names the harness registers for the platform MCP
 *  server's tools (server "llmsafespaces" + "_" + tool). send_message
 *  carries the origin for attribution (#1465); session_metadata and
 *  delete_session carry the CALLING session for the current-session
 *  default and the self-delete guard (#1627). The 21 board MANAGER
 *  tools (#1747 §5) carry the calling session's identity the same way:
 *  the seam stamps it into the audit actor, and injection OVERWRITES
 *  any model-supplied copy. Kept in lockstep with the agentd registry
 *  by TestMCPBoard_ManagerInjectionSupply (both directions). */
const INJECTED_SESSION_TOOLS = new Set([
  "llmsafespaces_send_message",
  "llmsafespaces_session_metadata",
  "llmsafespaces_delete_session",
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
  "llmsafespaces_board_pause_task",
  "llmsafespaces_board_resume_task",
  "llmsafespaces_board_import_plan",
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
        // lsp_injected_session is the PLATFORM namespace: distinct from
        // the schema-advertised from_session_id fallback so agentd can
        // label the origin mode (injected vs self-declared) without
        // trusting a model-supplied copy of this key.
        output.args.lsp_injected_session = input.sessionID
      } catch {
        // never break the host tool call
      }
    },
  }
}
