export interface EnvConfig {
  apiBaseUrl: string;
  // Cloudflare Turnstile site key (public). Optional in the interface
  // so older test mocks (`{ apiBaseUrl: "..." }`) don't break; getEnv()
  // and loadEnv() normalize to empty string when missing. When
  // non-empty, the register page renders the widget and the SPA
  // attaches the token to POST /auth/register.
  turnstileSiteKey?: string;
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
