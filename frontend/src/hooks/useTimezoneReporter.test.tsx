import { renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi, beforeEach } from "vitest";
import { useTimezoneReporter } from "./useTimezoneReporter";
import { settingsApi } from "../api/settings";

vi.mock("../providers/AuthProvider", () => ({
  useAuth: () => ({ user: { id: "u-1" } }),
}));

const putSpy = vi.spyOn(settingsApi, "setUserSetting").mockResolvedValue({} as never);

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
