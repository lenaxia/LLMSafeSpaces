import { describe, expect, it, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import { render } from "../../test/utils";
import { PlatformVersionsTab } from "./PlatformVersionsTab";

vi.mock("../../api/platformInfo", () => ({
  platformInfoApi: {
    get: vi.fn(),
  },
}));

import { platformInfoApi } from "../../api/platformInfo";

const mockGet = platformInfoApi.get as ReturnType<typeof vi.fn>;

describe("PlatformVersionsTab", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("renders a spinner while loading", () => {
    mockGet.mockReturnValue(new Promise(() => {}));
    render(<PlatformVersionsTab />);
    expect(document.querySelector(".animate-spin")).toBeInTheDocument();
  });

  it("renders all component versions on success", async () => {
    mockGet.mockResolvedValue({
      api: "0.4.5",
      controller: "0.4.5",
      frontend: "0.4.5",
      relayRouter: "0.4.5",
      baseRuntime: "0.4.5",
    });
    render(<PlatformVersionsTab />);
    await waitFor(() => {
      expect(screen.getAllByText("0.4.5").length).toBe(5);
    });
    // Every component label is rendered.
    expect(screen.getByText(/api/i)).toBeInTheDocument();
    expect(screen.getByText(/controller/i)).toBeInTheDocument();
    expect(screen.getByText(/frontend/i)).toBeInTheDocument();
    expect(screen.getByText(/relay router/i)).toBeInTheDocument();
    expect(screen.getByText(/base runtime/i)).toBeInTheDocument();
  });

  it("shows an error message on fetch failure", async () => {
    mockGet.mockRejectedValue(new Error("network"));
    render(<PlatformVersionsTab />);
    await waitFor(() => {
      expect(screen.getByText(/failed to load/i)).toBeInTheDocument();
    });
  });

  it("renders unknown for a missing version", async () => {
    mockGet.mockResolvedValue({
      api: "0.4.5",
      controller: "",
      frontend: "",
      relayRouter: "",
      baseRuntime: "",
    });
    render(<PlatformVersionsTab />);
    await waitFor(() => {
      // At least one "unknown" placeholder for the empty fields.
      expect(screen.getAllByText(/unknown/i).length).toBeGreaterThanOrEqual(1);
    });
  });
});
