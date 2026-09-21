// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { api } from "./client";

// #1499: the user-prompt library adapter (named distinctly from
// prompts.ts — the platform/org prompt-POLICY surface, a different
// concept that owns the bare "prompts" noun here). The composer
// #-recall lane (#1496) consumes the same list contract via its own
// adapter.
export interface UserPrompt {
  id: string;
  name: string;
  content: string;
  createdAt: string;
  updatedAt: string;
}

export interface CreatePromptRequest {
  name: string;
  content: string;
}

export interface UpdatePromptRequest {
  name?: string;
  content?: string;
}

export const userPromptsApi = {
  list: () => api.get<{ prompts: UserPrompt[] }>("/me/prompts"),
  create: (req: CreatePromptRequest) => api.post<{ prompt: UserPrompt }>("/me/prompts", req),
  update: (id: string, req: UpdatePromptRequest) => api.put<{ prompt: UserPrompt }>(`/me/prompts/${id}`, req),
  delete: (id: string) => api.delete<void>(`/me/prompts/${id}`),
};
