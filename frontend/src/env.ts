export interface EnvConfig {
  apiBaseUrl: string;
  // Cloudflare Turnstile site key (public) — when non-empty, the
  // register page renders the widget and the SPA attaches the returned
  // token to POST /auth/register. When empty, the widget is not
  // rendered and the API middleware is expected to be disabled server
  // side too. Kept as a plain string (not optional) so callers can
  // just check `.length > 0` — matches the chart's fallback of
  // TURNSTILE_SITE_KEY="" when turnstile.enabled=false.
  turnstileSiteKey: string;
}

let cached: EnvConfig | null = null;

export async function loadEnv(): Promise<EnvConfig> {
  if (cached) return cached;
  try {
    const res = await fetch("/env.json");
    const raw = (await res.json()) as Partial<EnvConfig>;
    cached = {
      apiBaseUrl: raw.apiBaseUrl ?? "/api/v1",
      turnstileSiteKey: raw.turnstileSiteKey ?? "",
    };
  } catch {
    cached = { apiBaseUrl: "/api/v1", turnstileSiteKey: "" };
  }
  return cached;
}

export function getEnv(): EnvConfig {
  return cached ?? { apiBaseUrl: "/api/v1", turnstileSiteKey: "" };
}
