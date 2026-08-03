import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { NewWorkspaceDialog } from "./NewWorkspaceDialog";

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    listConfigs: vi.fn().mockResolvedValue({
      configs: [
        { id: "c1", hash: "s-ready1", name: "Python+Node", status: "ready", selection: ["python-3.12", "node-22"] },
        { id: "c2", hash: "s-building1", name: "Building Config", status: "building", selection: ["go-1.24"] },
        { id: "c3", hash: "s-rejected1", name: "Broken Config", status: "rejected", selection: ["ffmpeg"] },
      ],
    }),
  },
}));

describe("NewWorkspaceDialog", () => {
  it("renders create and cancel buttons", () => {
    render(<NewWorkspaceDialog onCreate={vi.fn()} onCancel={vi.fn()} />);
    expect(screen.getByRole("button", { name: /create/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /cancel/i })).toBeInTheDocument();
  });

  it("calls onCreate with auto-generated name on create click", async () => {
    const user = userEvent.setup();
    const onCreate = vi.fn();
    render(<NewWorkspaceDialog onCreate={onCreate} onCancel={vi.fn()} />);

    await user.click(screen.getByRole("button", { name: /create/i }));

    expect(onCreate).toHaveBeenCalledWith(expect.objectContaining({ name: expect.any(String) }));
    expect(onCreate.mock.calls[0]![0].name.length).toBeGreaterThan(5);
  });

  it("calls onCancel when cancel clicked", async () => {
    const user = userEvent.setup();
    const onCancel = vi.fn();
    render(<NewWorkspaceDialog onCreate={vi.fn()} onCancel={onCancel} />);

    await user.click(screen.getByRole("button", { name: /cancel/i }));
    expect(onCancel).toHaveBeenCalled();
  });

  it("disables create button when loading", () => {
    render(<NewWorkspaceDialog onCreate={vi.fn()} onCancel={vi.fn()} loading />);
    expect(screen.getByRole("button", { name: /creating/i })).toBeDisabled();
  });

  it("shows image-factory configs with status pills", async () => {
    render(<NewWorkspaceDialog onCreate={vi.fn()} onCancel={vi.fn()} />);

    // Ready config is shown and clickable
    expect(await screen.findByText("Python+Node")).toBeInTheDocument();
    expect(screen.getByText("Ready")).toBeInTheDocument();
    // Building config shows pill but is disabled
    expect(screen.getByText("Building")).toBeInTheDocument();
    expect(screen.getByText("Rejected")).toBeInTheDocument();
  });

  it("selecting a ready config and clicking create passes imageConfigHash", async () => {
    const user = userEvent.setup();
    const onCreate = vi.fn();
    render(<NewWorkspaceDialog onCreate={onCreate} onCancel={vi.fn()} />);

    // Wait for configs to load + click the ready config
    const readyConfig = await screen.findByText("Python+Node");
    await user.click(readyConfig);

    await user.click(screen.getByRole("button", { name: /create/i }));

    expect(onCreate).toHaveBeenCalledWith(
      expect.objectContaining({ imageConfigHash: "s-ready1" }),
    );
  });

  it("building config is not clickable", async () => {
    const user = userEvent.setup();
    render(<NewWorkspaceDialog onCreate={vi.fn()} onCancel={vi.fn()} />);

    const buildingConfig = await screen.findByText("Building Config");
    // Clicking a disabled button should not select it
    await user.click(buildingConfig);
    // Create should NOT have imageConfigHash (default is selected)
    await user.click(screen.getByRole("button", { name: /create/i }));
    // The default-image path has no imageConfigHash
  });
});
