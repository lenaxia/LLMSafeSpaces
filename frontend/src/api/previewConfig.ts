import { useEffect, useState } from "react";
import { api } from "./client";

// Feature discovery for per-workspace preview origins (#1366). The value
// comes from the public /auth/config endpoint (no auth required) and is
// fetched once per app load. Consumers use it to know that legacy
// path-tunnel dev-preview links (the rate-limited API path emitted by
// pre-preview-origin agentd) can be upgraded to the policy-free preview
// origin via the bootstrap endpoint.

let cache: string | undefined;
let inflight: Promise<void> | undefined;

function load(): Promise<void> {
  if (!inflight) {
    inflight = api
      .get<{ previewOriginBaseDomain?: string }>("/auth/config")
      .then((c) => {
        cache = c.previewOriginBaseDomain || "";
      })
      .catch(() => {
        // Feature discovery is best-effort: on failure treat preview
        // origins as absent (links render exactly as emitted).
        cache = "";
      });
  }
  return inflight;
}

/** Returns the preview-origin base domain, "" when absent, undefined while unresolved. */
export function previewOriginBaseDomain(): string | undefined {
  return cache;
}

/** React hook form: re-renders the component once the value resolves. */
export function usePreviewOriginBaseDomain(): string | undefined {
  const [value, setValue] = useState<string | undefined>(cache);
  useEffect(() => {
    if (cache !== undefined) {
      setValue(cache);
      return;
    }
    let alive = true;
    void load().then(() => {
      if (alive) setValue(cache);
    });
    return () => {
      alive = false;
    };
  }, []);
  return value;
}
