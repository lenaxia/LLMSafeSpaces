// Copyright (C) 2026 Michael Kao
// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQuery } from "@tanstack/react-query";
import { promptLibraryApi } from "../api/promptLibrary";

/**
 * The user's prompt library for @-recall (#1496). Read-only v1; the
 * manager lane owns mutations. Failures degrade to an empty list —
 * recall is an input affordance, never a blocking surface — but the
 * error flag is exposed so the composer can hint why the list is empty.
 */
export function usePromptLibrary() {
  const query = useQuery({
    queryKey: ["prompt-library"],
    queryFn: promptLibraryApi.list,
    staleTime: 30_000,
    retry: 1,
  });
  return { prompts: query.data ?? [], isError: query.isError, isLoading: query.isLoading };
}
