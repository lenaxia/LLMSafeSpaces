import { api } from "./client";

interface CreateSessionResponse {
  sessionId: string;
  workspaceId: string;
  workspacePhase: string;
  resumed: boolean;
}

export const sessionsApi = {
  create: (workspaceId: string, title?: string) =>
    api.post<CreateSessionResponse>(`/workspaces/${workspaceId}/sessions/new`, title ? { title } : {}),

  // #1821 piece 4: trigger an on-demand busy-snapshot flight for the
  // workspace (202 + the flight lands on the user-event stream). The
  // busy lease's authoritative recheck surface — the D10 marker on the
  // resulting flight decides keep-vs-clear, so an unreachable authority
  // reads unknown, never idle.
  requestStatusSnapshot: (workspaceId: string) =>
    api.post<void>(`/workspaces/${workspaceId}/sessions/status-snapshot`, {}),
};
