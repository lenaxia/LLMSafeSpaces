// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { api } from "./client";

/**
 * User prompt library — the @-recall surface (#1496 part 2). The list
 * endpoint is the sibling lane's contract (manager + CRUD backend);
 * this adapter codes to the contract published on issue #1499:
 *   GET /me/prompts → 200 {"prompts":[{id,name,content,createdAt,updatedAt}]}
 * (named envelope per house convention; updatedAt DESC; no pagination
 * in v1). Until that backend lands, tests mock the network at this
 * seam — one file if the contract ever moves again.
 */

export interface UserPrompt {
  id: string;
  name: string;
  content: string;
  createdAt?: string;
  updatedAt?: string;
}

export interface UserPromptList {
  prompts: UserPrompt[];
}

export const promptLibraryApi = {
  list: () => api.get<UserPromptList>("/me/prompts").then((r) => r.prompts ?? []),
};
