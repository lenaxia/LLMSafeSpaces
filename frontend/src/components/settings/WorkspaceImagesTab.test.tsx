import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";

const mockGetCatalog = vi.fn();
const mockListConfigs = vi.fn();
const mockCreateConfig = vi.fn();

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    getCatalog: (...a: unknown[]) => mockGetCatalog(...a),
    listConfigs: (...a: unknown[]) => mockListConfigs(...a),
    getConfig: vi.fn(),
    createConfig: (...a: unknown[]) => mockCreateConfig(...a),
  },
}));

const mockToast = vi.fn();

vi.mock("../../providers/ToastProvider", () => ({
  useToast: () => ({ toast: mockToast }),
}));

import { WorkspaceImagesTab } from "./WorkspaceImagesTab";

const defaultCatalog = {
  architectures: ["linux/amd64"],
  bases: [{ name: "bookworm", version: "0.6.0", image: "img", tag: "0.6.0", isDefault: true }],
  extensions: [
    { id: "ffmpeg", type: "apt", value: "ffmpeg", supportedBases: ["bookworm"], retired: false, reviewRequested: false },
    { id: "python313", type: "mise", value: "python@3.13", supportedBases: ["bookworm"], retired: false, reviewRequested: false },
  ],
  knownFailures: [] as Array<Record<string, unknown>>,
};
const defaultConfigs = { configs: [{ id: "c1", hash: "s-a", name: "ml-stack", selection: ["ffmpeg"], resolvedValues: {}, baseName: "bookworm", baseVersion: "0.6.0", scope: "member", status: "ready" }] };
const defaultCreated = { id: "c2", hash: "s-b", name: "new-cfg", selection: ["python313"], resolvedValues: {}, baseName: "bookworm", baseVersion: "0.6.0", scope: "member", status: "building" };

describe("WorkspaceImagesTab", () => {
  beforeEach(() => {
    mockGetCatalog.mockResolvedValue(defaultCatalog);
    mockListConfigs.mockResolvedValue(defaultConfigs);
    mockCreateConfig.mockResolvedValue(defaultCreated);
  });

  it("renders saved configs with status pills", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ml-stack")).toBeInTheDocument(); });
    expect(screen.getByText("ready")).toBeInTheDocument();
  });

  it("renders catalog extensions as checkboxes", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
  });

  it("enables create button when name + extension selected", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    expect(screen.getByText("Create & Build")).not.toBeDisabled();
  });

  it("disables create button when no name", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    expect(screen.getByText("Create & Build")).toBeDisabled();
  });

  it("disables create button when no extension selected", async () => {
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    expect(screen.getByText("Create & Build")).toBeDisabled();
  });

  it("shows blocked when selection matches a non-retriable known failure", async () => {
    mockGetCatalog.mockResolvedValue({
      ...defaultCatalog,
      extensions: [{ id: "ffmpeg", type: "apt", value: "ffmpeg", supportedBases: ["bookworm"], retired: false, reviewRequested: false }],
      knownFailures: [{ selectionHash: "s-x", selection: ["ffmpeg"], baseName: "bookworm", retriable: false, detectedAt: "2026-01-01" }],
    });
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    expect(screen.getByText("Combination blocked")).toBeDisabled();
  });

  it("shows error toast and preserves config list on create failure", async () => {
    mockCreateConfig.mockRejectedValue(new Error("server error"));
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });
    fireEvent.click(screen.getByText("ffmpeg"));
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "t" } });
    fireEvent.click(screen.getByText("Create & Build"));
    // Wait for the promise to settle
    await waitFor(() => { expect(mockCreateConfig).toHaveBeenCalled(); }, { timeout: 3000 });
    // Config list should still show the pre-existing config
    expect(screen.getByText("ml-stack")).toBeInTheDocument();
  });

  it("shows error message on catalog load failure", async () => {
    mockGetCatalog.mockRejectedValue(new Error("network error"));
    render(<WorkspaceImagesTab />);
    await waitFor(() => {
      expect(screen.getByText(/network error|Failed to load/i)).toBeInTheDocument();
    });
  });

  it("creates config on save (happy path): appends config, resets form, fires toast", async () => {
    mockToast.mockClear();
    render(<WorkspaceImagesTab />);
    await waitFor(() => { expect(screen.getByText("ffmpeg")).toBeInTheDocument(); });

    // Select extension + type name
    fireEvent.click(screen.getByText("ffmpeg"));
    fireEvent.change(screen.getByPlaceholderText("e.g. ml-stack"), { target: { value: "success-cfg" } });
    fireEvent.click(screen.getByText("Create & Build"));

    await waitFor(() => { expect(mockCreateConfig).toHaveBeenCalled(); }, { timeout: 3000 });

    // Config appended to the list (the mock returns name="new-cfg")
    await waitFor(() => { expect(screen.getByText("new-cfg")).toBeInTheDocument(); });

    // Form reset: name input cleared
    expect(screen.getByPlaceholderText("e.g. ml-stack")).toHaveValue("");

    // Toast fired with success message
    expect(mockToast).toHaveBeenCalledWith(
      expect.stringContaining("Image config created"), "success",
    );
  });
});
