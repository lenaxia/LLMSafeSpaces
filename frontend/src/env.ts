export interface EnvConfig {
  apiBaseUrl: string;
}

let cached: EnvConfig | null = null;

export async function loadEnv(): Promise<EnvConfig> {
  if (cached) return cached;
  try {
    const res = await fetch("/env.json");
    cached = await res.json();
  } catch {
    cached = { apiBaseUrl: "/api/v1" };
  }
  return cached!;
}

export function getEnv(): EnvConfig {
  return cached ?? { apiBaseUrl: "/api/v1" };
}
