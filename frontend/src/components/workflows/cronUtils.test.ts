import { describe, expect, it } from "vitest";
import { toCronConfig, cronToFriendly, describeCron } from "./cronUtils";

describe("toCronConfig", () => {
  it("passes through a valid cron source config", () => {
    expect(toCronConfig({ expr: "0 2 * * *", tz: "UTC" })).toEqual({ expr: "0 2 * * *", tz: "UTC" });
  });

  it("defaults tz to UTC when absent", () => {
    expect(toCronConfig({ expr: "0 2 * * *" })).toEqual({ expr: "0 2 * * *", tz: "UTC" });
  });

  it("drops a non-string tz", () => {
    expect(toCronConfig({ expr: "0 2 * * *", tz: 42 })).toEqual({ expr: "0 2 * * *", tz: "UTC" });
  });

  it("returns an empty config for an empty webhook source config", () => {
    expect(toCronConfig({})).toEqual({ expr: "", tz: "UTC" });
  });

  it("returns an empty config for null/undefined/non-object values", () => {
    expect(toCronConfig(null)).toEqual({ expr: "", tz: "UTC" });
    expect(toCronConfig(undefined)).toEqual({ expr: "", tz: "UTC" });
    expect(toCronConfig("0 2 * * *")).toEqual({ expr: "", tz: "UTC" });
    expect(toCronConfig([])).toEqual({ expr: "", tz: "UTC" });
  });

  it("ignores a non-string expr", () => {
    expect(toCronConfig({ expr: 5, tz: "UTC" })).toEqual({ expr: "", tz: "UTC" });
  });

  it("composes with cronToFriendly/describeCron for display", () => {
    expect(describeCron(cronToFriendly(toCronConfig({ expr: "0 9 * * 1-5", tz: "UTC" })))).toBe(
      "Weekdays at 9:00 AM UTC",
    );
  });
});
