import { api } from "./client";
import type { QuestionRequest, PermissionRequest } from "./types";

export const inputApi = {
  listQuestions: (workspaceId: string) =>
    api.get<QuestionRequest[]>(`/workspaces/${workspaceId}/question`),

  questionReply: (workspaceId: string, requestId: string, answers: string[][]) =>
    api.post<boolean>(`/workspaces/${workspaceId}/question/${requestId}/reply`, { answers }),

  questionReject: (workspaceId: string, requestId: string) =>
    api.post<boolean>(`/workspaces/${workspaceId}/question/${requestId}/reject`, {}),

  listPermissions: (workspaceId: string) =>
    api.get<PermissionRequest[]>(`/workspaces/${workspaceId}/permission`),

  permissionReply: (workspaceId: string, requestId: string, reply: "once" | "always" | "reject", message?: string) =>
    api.post<boolean>(`/workspaces/${workspaceId}/permission/${requestId}/reply`, { reply, ...(message ? { message } : {}) }),

  dismissInboxRecord: (workspaceId: string, sessionId: string, requestId: string) =>
    api.delete<void>(`/workspaces/${workspaceId}/sessions/${sessionId}/inbox/${requestId}`),
};
