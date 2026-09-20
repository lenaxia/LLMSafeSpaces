// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { workspacesApi } from "../../api/workspaces";

/**
 * The composer's slash-command registry (#1496 part 1). Every command
 * maps to an action users actually perform mid-chat, chosen by walking
 * the existing API surface — no command here invents backend behavior:
 *
 *   /compact — typed action `action.compact` via POST /sessions/:id/actions
 *              (single-flight pod-side; 501 + capability detail when the
 *              state-authority flag is off — surfaced verbatim)
 *   /model   — opens the composer options drawer (model picker lives there)
 *   /rename  — PUT /sessions/:id/title (args = the new title; reuses the
 *              sidebar's rename adapter + cache invalidation)
 *   /new     — new session (ensure/create + navigate; callback wired by
 *              the page, same mutation the sidebar's "+ New chat" uses)
 *   /abort   — stops the current turn (the composer's existing abort path)
 *   /help    — local overlay listing the commands (no backend)
 *
 * Excluded by evidence: /share — no session-share endpoint exists in
 * the router; adding API surface is out of lane (#1496).
 */

export interface SlashCommandContext {
  workspaceId?: string;
  sessionId?: string;
  /** Opens the model/persona options drawer. */
  openOptionsDrawer: () => void;
  /** Opens the command-help overlay. */
  openHelp: () => void;
  /** New-session navigation (page-owned). */
  onNewSession?: () => void;
  /** Abort the current turn (composer's existing path). */
  onAbort?: () => void;
  /** Surface a command result/error inline above the composer. */
  notify: (notice: { kind: "info" | "error"; text: string }) => void;
  /** Invalidate the sessions list so a rename reflects immediately. */
  invalidateSessions: () => void;
}

export interface SlashCommand {
  id: string;
  label: string;
  hint: string;
  /** Extra requirement beyond the composer itself (shown when unmet). */
  requires?: (ctx: SlashCommandContext) => string | null;
  run: (args: string, ctx: SlashCommandContext) => void | Promise<void>;
}

export const SLASH_COMMANDS: SlashCommand[] = [
  {
    id: "help",
    label: "/help",
    hint: "List composer commands",
    run: (_args, ctx) => {
      ctx.openHelp();
    },
  },
  {
    id: "compact",
    label: "/compact",
    hint: "Compact this session's history (frees context)",
    requires: (ctx) => (!ctx.workspaceId || !ctx.sessionId ? "needs an active session" : null),
    run: async (_args, ctx) => {
      try {
        await workspacesApi.sessionAction(ctx.workspaceId!, ctx.sessionId!, { type: "action.compact" });
        ctx.notify({ kind: "info", text: "Compaction scheduled — it runs when the current turn ends." });
      } catch (err) {
        ctx.notify({ kind: "error", text: `Compact failed: ${errorText(err)}` });
      }
    },
  },
  {
    id: "model",
    label: "/model",
    hint: "Open the model picker",
    run: async (_args, ctx) => {
      ctx.openOptionsDrawer();
      ctx.notify({ kind: "info", text: "Model picker opened." });
    },
  },
  {
    id: "rename",
    label: "/rename",
    hint: "Rename this session — /rename <new title>",
    requires: (ctx) => (!ctx.workspaceId || !ctx.sessionId ? "needs an active session" : null),
    run: async (args, ctx) => {
      const title = args.trim();
      if (!title) {
        ctx.notify({ kind: "error", text: "Usage: /rename <new title>" });
        return;
      }
      try {
        await workspacesApi.renameSession(ctx.workspaceId!, ctx.sessionId!, title);
        ctx.invalidateSessions();
        ctx.notify({ kind: "info", text: `Session renamed to “${title}”.` });
      } catch (err) {
        ctx.notify({ kind: "error", text: `Rename failed: ${errorText(err)}` });
      }
    },
  },
  {
    id: "new",
    label: "/new",
    hint: "Start a new session in this workspace",
    requires: (ctx) => (!ctx.onNewSession ? "unavailable here" : null),
    run: async (_args, ctx) => {
      ctx.onNewSession?.();
    },
  },
  {
    id: "abort",
    label: "/abort",
    hint: "Stop the current turn",
    requires: (ctx) => (!ctx.onAbort ? "nothing to stop" : null),
    run: async (_args, ctx) => {
      ctx.onAbort?.();
      ctx.notify({ kind: "info", text: "Stopping the current turn…" });
    },
  },
];

export function errorText(err: unknown): string {
  if (err instanceof Error) return err.message;
  return String(err);
}
