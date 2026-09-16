describe("useTimezoneReporter zone change", () => {
  it("re-reports when the browser zone changes (60s poll)", async () => {
    const zones = ["America/New_York", "Europe/Berlin"];
    let idx = 0;
    const getZone = (): string => zones[Math.min(idx, zones.length - 1)] ?? "UTC";
    const calls: string[] = [];
    putSpy.mockImplementation((_k: string, v: unknown) => {
      calls.push(v as string);
      return Promise.resolve({} as { key: string; value: unknown });
    });

    // Fake ONLY the interval functions — React's scheduler (MessageChannel)
    // must stay real or the mount effect never flushes.
    vi.useFakeTimers({ toFake: ["setInterval", "clearInterval"] });
    renderHook(() => useTimezoneReporter(getZone));
    await vi.waitFor(() => expect(calls).toEqual(["America/New_York"])); // initial report on mount

    idx = 1; // the OS zone changed
    await vi.advanceTimersByTimeAsync(61_000);
    vi.useRealTimers();

    // the poll tick picks up the new zone and re-reports
    expect(calls).toEqual(["America/New_York", "Europe/Berlin"]);
  });
});

import { renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi, beforeEach } from "vitest";
import { useTimezoneReporter } from "./useTimezoneReporter";
import { settingsApi } from "../api/settings";

vi.mock("../providers/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u-1" } }),
}));

const putSpy = vi.spyOn(settingsApi, "setUserSetting").mockResolvedValue({} as never);

// Timer isolation: a full vi.useFakeTimers() in one test (the retry leg)
// leaves global clock state that breaks later mounts — restore after each.
afterEach(() => {
  vi.useRealTimers();
});

describe("useTimezoneReporter", () => {
  beforeEach(() => {
    putSpy.mockClear();
  });

  it("reports the browser's IANA zone once per session", async () => {
    renderHook(() => useTimezoneReporter());
    await waitFor(() => expect(putSpy).toHaveBeenCalledTimes(1));
    expect(putSpy).toHaveBeenCalledWith("timezone", expect.any(String));
  });
});

describe("useTimezoneReporter retry", () => {
  it("re-reports after a failed PUT (reported ref is cleared)", async () => {
    const failSpy = vi
      .spyOn(settingsApi, "setUserSetting")
      .mockRejectedValueOnce(new Error("offline"))
      .mockResolvedValue({} as never);

    vi.useFakeTimers();
    const { rerender } = renderHook(() => useTimezoneReporter());
    // First report fails async; the interval (60s) is faked forward.
    await vi.advanceTimersByTimeAsync(61_000);
    rerender();
    await vi.advanceTimersByTimeAsync(61_000);
    vi.useRealTimers();

    await waitFor(() => expect(failSpy.mock.calls.length).toBeGreaterThanOrEqual(2));
  });
});
