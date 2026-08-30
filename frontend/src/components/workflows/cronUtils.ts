export type CronFrequency = "every-n-minutes" | "every-n-hours" | "daily" | "weekdays" | "custom";

export interface CronConfig {
  expr: string;
  tz: string;
}

export interface FriendlyCron {
  frequency: CronFrequency;
  interval?: number;
  hour?: number;
  minute?: number;
  tz: string;
  expr?: string;
}

export function friendlyToCron(f: FriendlyCron): CronConfig {
  const tz = f.tz || "UTC";
  switch (f.frequency) {
    case "every-n-minutes": {
      const n = f.interval && f.interval > 0 ? f.interval : 5;
      return { expr: `*/${n} * * * *`, tz };
    }
    case "every-n-hours": {
      const n = f.interval && f.interval > 0 ? f.interval : 1;
      return { expr: `0 */${n} * * *`, tz };
    }
    case "daily": {
      const h = f.hour ?? 9;
      const m = f.minute ?? 0;
      return { expr: `${m} ${h} * * *`, tz };
    }
    case "weekdays": {
      const h = f.hour ?? 9;
      const m = f.minute ?? 0;
      return { expr: `${m} ${h} * * 1-5`, tz };
    }
    case "custom":
      return { expr: f.expr || "0 * * * *", tz };
  }
  return { expr: "0 * * * *", tz };
}

/**
 * Narrows a trigger's sourceConfig (unknown JSON on the wire — the API
 * returns `cron: {expr, tz?}` for cron sources and `{}` for webhooks) to a
 * CronConfig. Invalid or missing values fall back to an empty config, which
 * the display helpers render as "0 * * * *" / "Custom schedule".
 */
export function toCronConfig(value: unknown): CronConfig {
  if (!value || typeof value !== "object" || Array.isArray(value)) return { expr: "", tz: "UTC" };
  const candidate = value as { expr?: unknown; tz?: unknown };
  const expr = typeof candidate.expr === "string" ? candidate.expr : "";
  const tz = typeof candidate.tz === "string" ? candidate.tz : "UTC";
  return { expr, tz };
}

export function cronToFriendly(c: CronConfig): FriendlyCron {  const expr = c.expr?.trim() || "0 * * * *";
  const parts = expr.split(/\s+/);
  if (parts.length !== 5) return { frequency: "custom", tz: c.tz || "UTC", expr };
  const min = parts[0] || "";
  const hour = parts[1] || "";
  const dom = parts[2] || "";
  const mon = parts[3] || "";
  const dow = parts[4] || "";

  if (min.startsWith("*/")) {
    const n = parseInt(min.slice(2));
    if (!isNaN(n) && hour === "*" && dom === "*" && mon === "*" && dow === "*") {
      return { frequency: "every-n-minutes", interval: n, tz: c.tz || "UTC" };
    }
  }
  if (hour.startsWith("*/") && min === "0") {
    const n = parseInt(hour.slice(2));
    if (!isNaN(n) && dom === "*" && mon === "*" && dow === "*") {
      return { frequency: "every-n-hours", interval: n, tz: c.tz || "UTC" };
    }
  }
  const h = parseInt(hour);
  const m = parseInt(min);
  if (!isNaN(h) && !isNaN(m) && dom === "*" && mon === "*") {
    if (dow === "1-5") {
      return { frequency: "weekdays", hour: h, minute: m, tz: c.tz || "UTC" };
    }
    if (dow === "*") {
      return { frequency: "daily", hour: h, minute: m, tz: c.tz || "UTC" };
    }
  }
  return { frequency: "custom", tz: c.tz || "UTC", expr };
}

export function describeCron(f: FriendlyCron): string {
  switch (f.frequency) {
    case "every-n-minutes":
      return `Every ${f.interval || 5} minute${(f.interval || 5) > 1 ? "s" : ""}`;
    case "every-n-hours":
      return `Every ${f.interval || 1} hour${(f.interval || 1) > 1 ? "s" : ""}`;
    case "daily": {
      const time = formatTime(f.hour ?? 9, f.minute ?? 0);
      return `Daily at ${time} ${f.tz || "UTC"}`;
    }
    case "weekdays": {
      const time = formatTime(f.hour ?? 9, f.minute ?? 0);
      return `Weekdays at ${time} ${f.tz || "UTC"}`;
    }
    case "custom":
      return f.expr || "Custom schedule";
  }
  return "Unknown";
}

function formatTime(hour: number, minute: number): string {
  const h12 = hour === 0 ? 12 : hour > 12 ? hour - 12 : hour;
  const ampm = hour >= 12 ? "PM" : "AM";
  return `${h12}:${minute.toString().padStart(2, "0")} ${ampm}`;
}

const COMMON_TIMEZONES = [
  "UTC", "America/New_York", "America/Chicago", "America/Denver", "America/Los_Angeles",
  "America/Toronto", "Europe/London", "Europe/Paris", "Europe/Berlin", "Asia/Tokyo",
  "Asia/Shanghai", "Asia/Kolkata", "Australia/Sydney", "Pacific/Auckland",
];
export { COMMON_TIMEZONES };
