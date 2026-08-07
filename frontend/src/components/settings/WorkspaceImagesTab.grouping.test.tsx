import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

const mockGetCatalog = vi.fn();
const mockListConfigs = vi.fn();
const mockCreateConfig = vi.fn();
const mockToast = vi.fn();

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    getCatalog: (...a: unknown[]) => mockGetCatalog(...a),
    listConfigs: (...a: unknown[]) => mockListConfigs(...a),
    getConfig: vi.fn(),
    createConfig: (...a: unknown[]) => mockCreateConfig(...a),
  },
}));
vi.mock("../../providers/ToastProvider", () => ({
  useToast: () => ({ toast: mockToast }),
}));

import { WorkspaceImagesTab } from "./WorkspaceImagesTab";

describe("WorkspaceImagesTab grouping", () => {
  beforeEach(() => {
    mockGetCatalog.mockResolvedValue({
      architectures: ["linux/amd64"],
      bases: [{ name: "bookworm", version: "0.8.0", image: "img", tag: "0.8.0", isDefault: true }],
      extensions: [
        { id: "python-3.13", type: "mise", value: "python@3.13", supportedBases: ["bookworm"], retired: false, reviewRequested: false },
        { id: "playwright-deps", type: "apt", value: "libnss3", supportedBases: ["bookworm"], retired: false, reviewRequested: false },
      ],
      knownFailures: [],
    });
    mockListConfigs.mockResolvedValue([]);
  });

  it("groups extensions by type with section headers", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("python-3.13")).toBeInTheDocument(); });
    expect(screen.getByText(/Language Packs/i)).toBeInTheDocument();
    expect(screen.getByText(/System Packages/i)).toBeInTheDocument();
  });

  it("does not render empty groups", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("python-3.13")).toBeInTheDocument(); });
    expect(screen.queryByText(/Files/i)).not.toBeInTheDocument();
  });
});
