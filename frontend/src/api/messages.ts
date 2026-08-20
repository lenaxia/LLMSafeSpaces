import { api, getRaw } from "./client";
import type { Message, SendMessageRequest } from "./types";

// ContractMessage is the wire shape from the API's GetHistory endpoint
// when the Adapter is wired (US-65.4). Mirrors pkg/session.Message.
interface ContractMessage {
  id: string;
  type: string;
  createdAt?: string;
  parts?: Array<{
    type: string;
    id?: string;
    text?: string;
    reasoning?: string;
    tool?: {
      name?: string;
      callId?: string;
      input?: unknown;
      output?: unknown;
      state?: {
        status?: string;
        error?: string;
        startedAt?: string;
      };
    };
    fileChange?: {
      path?: string;
      patch?: string;
      status?: string;
    };
  }>;
  model?: { id?: string; provider?: string };
}

export function transformHistory(raw: ContractMessage[]): Message[] {
  return raw
    .filter((m) => m.type === "user" || m.type === "assistant")
    .map((m) => ({
      id: m.id,
      role: m.type as "user" | "assistant",
      parts: (m.parts ?? []).filter((p) => {
        if (p.type === "tool") return true;
        if (p.type === "text" && p.text) return true;
        if (p.type === "reasoning" && p.reasoning) return true;
        if (p.type === "file_change") return true;
        return false;
      }).map((p) => {
        if (p.type === "tool" && p.tool) {
          const toolName = p.tool.name ?? "";
          return {
            type: "tool_use" as const,
            id: p.id,
            text: toolName,
            toolCallId: p.tool.callId,
            toolState: p.tool.state?.status ?? "",
            input: p.tool.input,
            toolOutput: typeof p.tool.output === "string" ? p.tool.output : undefined,
            toolStartedAt: p.tool.state?.startedAt,
          };
        }
        if (p.type === "reasoning") {
          return { type: "reasoning", text: p.reasoning ?? "" };
        }
        return p;
      }),
      createdAt: m.createdAt || undefined,
      modelID: m.model?.id ?? undefined,
    }))
    .filter((m) => m.parts.length > 0);
}

export interface HistoryPage {
  messages: Message[];
  nextCursor?: string;
}

const PAGE_LIMIT = 50;

export const messagesApi = {
  getHistory: async (workspaceId: string, sessionId: string): Promise<Message[]> => {
    const raw = await api.get<ContractMessage[]>(
      `/workspaces/${workspaceId}/sessions/${sessionId}/message`,
    );
    return transformHistory(raw);
  },
  getHistoryPage: async (
    workspaceId: string,
    sessionId: string,
    opts?: { before?: string },
  ): Promise<HistoryPage> => {
    const params = new URLSearchParams();
    params.set("limit", String(PAGE_LIMIT));
    if (opts?.before) params.set("before", opts.before);
    const { data, headers } = await getRaw<ContractMessage[]>(
      `/workspaces/${workspaceId}/sessions/${sessionId}/message?${params.toString()}`,
    );
    return {
      messages: transformHistory(data),
      nextCursor: headers.get("X-Next-Cursor") ?? undefined,
    };
  },
  sendAsync: (workspaceId: string, sessionId: string, req: SendMessageRequest) =>
    api.post<void>(`/workspaces/${workspaceId}/sessions/${sessionId}/prompt`, req),
  queueMessage: (workspaceId: string, sessionId: string, text: string) =>
    api.post<{ messageID: string }>(`/workspaces/${workspaceId}/sessions/${sessionId}/queue`, { text }),
  getQueue: async (workspaceId: string, sessionId: string) => {
    const res = await api.get<{ messages: Array<{
      id: string; text: string; session_id: string; workspace_id: string; enqueued_at: string; retry_count: number;
    }> }>(`/workspaces/${workspaceId}/sessions/${sessionId}/queue`);
    return res;
  },
  deleteQueueMessage: (workspaceId: string, sessionId: string, messageId: string) =>
    api.delete<void>(`/workspaces/${workspaceId}/sessions/${sessionId}/queue/${messageId}`),
};
