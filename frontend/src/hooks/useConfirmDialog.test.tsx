import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { useConfirmDialog } from "./useConfirmDialog";

function TestHost() {
  const { confirm, dialog } = useConfirmDialog();
  return (
    <div>
      <button onClick={() => confirm({
        title: "Delete workspace?",
        description: "This action cannot be undone.",
        confirmLabel: "Delete",
        destructive: true,
        onConfirm: () => { /* action runs in test */ },
      })}>
        Trigger
      </button>
      {dialog}
    </div>
  );
}

describe("useConfirmDialog", () => {
  it("does not show dialog until confirm() is called", () => {
    render(<TestHost />);
    expect(screen.queryByText("Delete workspace?")).not.toBeInTheDocument();
  });

  it("opens dialog when confirm() is called", async () => {
    render(<TestHost />);
    fireEvent.click(screen.getByText("Trigger"));
    await waitFor(() => {
      expect(screen.getByText("Delete workspace?")).toBeInTheDocument();
    });
  });

  it("calls onConfirm when confirm button is clicked", async () => {
    const onConfirm = vi.fn();
    function Host() {
      const { confirm, dialog } = useConfirmDialog();
      return (
        <div>
          <button onClick={() => confirm({
            title: "Test", description: "desc", confirmLabel: "Go",
            onConfirm,
          })}>Open</button>
          {dialog}
        </div>
      );
    }
    render(<Host />);
    fireEvent.click(screen.getByText("Open"));
    await waitFor(() => screen.getByText("Go"));
    fireEvent.click(screen.getByText("Go"));
    expect(onConfirm).toHaveBeenCalledOnce();
  });

  it("closes and does NOT call onConfirm when cancel is clicked", async () => {
    const onConfirm = vi.fn();
    function Host() {
      const { confirm, dialog } = useConfirmDialog();
      return (
        <div>
          <button onClick={() => confirm({
            title: "Test", description: "desc", confirmLabel: "Go",
            onConfirm,
          })}>Open</button>
          {dialog}
        </div>
      );
    }
    render(<Host />);
    fireEvent.click(screen.getByText("Open"));
    await waitFor(() => screen.getByText("Cancel"));
    fireEvent.click(screen.getByText("Cancel"));
    expect(onConfirm).not.toHaveBeenCalled();
    await waitFor(() => {
      expect(screen.queryByText("Test")).not.toBeInTheDocument();
    });
  });
});
