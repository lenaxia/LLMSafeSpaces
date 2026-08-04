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
        { id: "c1", hash: "s-ready1", name: "Python+Node", status: "ready", selection: ["python-3.12", "node-22"] },
        { id: "c2", hash: "s-building1", name: "Building", status: "building", selection: ["go-1.24"] },
      ],
    }),
  },
}));

describe("NewWorkspaceSplitButton", () => {
  it("renders the + and ▼ buttons", () => {
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

  it("opens popup on ▼ click and shows ready configs", async () => {
    const user = userEvent.setup();
    render(<NewWorkspaceSplitButton onCreated={vi.fn()} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    expect(await screen.findByText("Python+Node")).toBeInTheDocument();
  });

  it("launches with config hash when a config is clicked", async () => {
    const user = userEvent.setup();
    const onCreated = vi.fn();
    render(<NewWorkspaceSplitButton onCreated={onCreated} />);

    await user.click(screen.getByLabelText("Select workspace image"));
    const configBtn = await screen.findByText("Python+Node");
    await user.click(configBtn);
    expect(onCreated).toHaveBeenCalledWith("ws-new");
  });
});
