import { ApiClientError } from "../api/client";

/**
 * Outcome of consuming a single-use email token (verification or password
 * reset). The backend distinguishes expiry from consumption via the 410
 * body's error string ("token expired" / "token already used"); 404 means
 * the token never existed (or is the wrong kind for this endpoint).
 */
export type TokenFailure = "expired" | "used" | "not-found" | "gone" | "error";

export function classifyTokenError(err: unknown): TokenFailure {
  if (err instanceof ApiClientError) {
    if (err.status === 404) return "not-found";
    if (err.status === 410) {
      const msg = (err.body?.error ?? "").toLowerCase();
      if (msg.includes("expire")) return "expired";
      if (msg.includes("already used")) return "used";
      // Unknown 410 — the link is spent somehow, but don't claim to know why.
      return "gone";
    }
  }
  return "error";
}

/**
 * Remove a query parameter from the current URL without navigation. Used to
 * strip single-use ?token= values once a terminal result is shown, so a
 * refresh cannot re-POST (and misreport) an already-consumed token.
 */
export function stripQueryParam(name: string): void {
  const params = new URLSearchParams(window.location.search);
  if (!params.has(name)) return;
  params.delete(name);
  const clean = params.toString();
  window.history.replaceState({}, "", clean ? `?${clean}` : window.location.pathname);
}
