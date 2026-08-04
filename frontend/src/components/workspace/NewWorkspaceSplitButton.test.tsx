import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { render } from "../../test/utils";
import { NewWorkspaceSplitButton } from "./NewWorkspaceSplitButton";

vi.mock("../../api/workspaces", () => ({
  workspacesApi: {
    create: vi.fn().mockResolvedValue({ id: "ws-new" }),
  },
}));

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    listConfigs: vi.fn().mockResolvedValue({
      configs: [
        { id: "c1", hash: "s-ready1", name: "Python+Node", status: "ready", selection: ["python-3.12", "node-22"], scope: "member" },
        { id: "c2", hash: "s-building1", name: "Building Image", status: "building", selection: ["go-1.24"], scope: "member" },
      ],
    }),
  },
}));

describe("NewWorkspaceSplitButton", () => {
  it("renders + and arrow buttons", () => {
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);
    expect(screen.getByLabelText("New workspace (default image)")).toBeInTheDocument();
    expect(screen.getByLabelText("Select workspace image")).toBeInTheDocument();
  });

  it("launches default on + click", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<NewWorkspaceSplitButton onCreated={onCreated} />);

    await user.click(screen.getByLabelText("New workspace (default image)"));
    expect(onCreated).toHaveBeenCalledWith("ws-new");
  });

  it("opens popup showing ready + building configs with pills", async () => {
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    await user.click(screen.getByLabelText("Select workspace image"));

    // Ready config with green pill
    expect(await screen.findByText("Python+Node")).toBeInTheDocument();
    expect(screen.getByText("Ready")).toBeInTheDocument();

    // Building config with yellow pill
    expect(screen.getByText("Building Image")).toBeInTheDocument();
    expect(screen.getByText("Building")).toBeInTheDocument();
  });

  it("launches workspace when ready config clicked", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<NewWorkspaceSplitButton onCreated={onCreated} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    const readyConfig = await screen.findByText("Python+Node");
    await user.click(readyConfig);

    expect(onCreated).toHaveBeenCalledWith("ws-new");
  });

  it("building configs are not clickable", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<NewWorkspaceSplitButton onCreated={onCreated} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    const buildingConfig = await screen.findByText("Building Image");
    await user.click(buildingConfig);

    // Building config click should NOT trigger creation
    expect(onCreated).not.toHaveBeenCalled();
  });
});
