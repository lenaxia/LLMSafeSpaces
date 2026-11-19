// useEntityLinks — React seams over the entityLinks registry
// (tsk-vibrant-cadet-pearls). The registry is module-level state
// seeded by <EntityIndexSync> from the react-query caches; these
// hooks re-render on registry changes via useSyncExternalStore.

import { useCallback, useEffect, useSyncExternalStore } from "react";
import {
  getEntityRegistryVersion,
  linkifyMarkdown,
  lookupEntity,
  requestEntityResolution,
  subscribeEntityRegistry,
  type EntityRef,
} from "../lib/entityLinks";

function useEntityRegistryVersion(): number {
  const version = useSyncExternalStore(
    subscribeEntityRegistry,
    getEntityRegistryVersion,
    getEntityRegistryVersion,
  );
  return version;
}

export interface EntityLinkifyOptions {
  /** While streaming, tier-a (registry) resolution still applies, but
   * candidates are NOT queued for lazy probing — a half-streamed id
   * would poison the miss cache. The final render queues them. */
  streaming?: boolean;
}

/** Returns a stable-ish markdown transform: resolved candidates
 * become links; unresolved pass through. Re-created only when the
 * registry version moves (rerender-stability pin: identical input +
 * version → identical output, no churn). */
export function useEntityLinkify(): (text: string, opts?: EntityLinkifyOptions) => string {
  const version = useEntityRegistryVersion();
  const linkify = useCallback(
    (text: string, opts?: EntityLinkifyOptions) => {
      if (!text) return text;
      const streaming = opts?.streaming ?? false;
      return linkifyMarkdown(text, lookupEntity, streaming ? undefined : (c) => {
        requestEntityResolution([{ raw: c.raw, kind: c.kind }]);
      });
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [version],
  );
  return linkify;
}

/** Resolves one session reference (phrase or raw ses_* id) for the
 * injected-message header. Falls back to undefined (caller renders
 * the raw phrase — never blank). Kicks a lazy probe exactly once per
 * candidate per miss-TTL; settles stable once resolved. */
export function useSessionEntityRef(rawId: string | undefined): EntityRef | undefined {
  const version = useEntityRegistryVersion();
  const ref = rawId ? lookupEntity(rawId, "session") : undefined;
  useEffect(() => {
    if (!rawId || ref) return;
    requestEntityResolution([{ raw: rawId, kind: "session" }]);
  }, [rawId, ref, version]);
  return ref;
}
