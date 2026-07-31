import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { RecoveryCodesDisplay } from "./RecoveryCodesDisplay";

describe("RecoveryCodesDisplay", () => {
  it("renders all codes", () => {
    render(
      <RecoveryCodesDisplay codes={["CODE1", "CODE2", "CODE3"]} onContinue={vi.fn()} />,
    );
    expect(screen.getByText("CODE1")).toBeInTheDocument();
    expect(screen.getByText("CODE2")).toBeInTheDocument();
    expect(screen.getByText("CODE3")).toBeInTheDocument();
  });

  it("disables continue until acknowledged", () => {
    render(
      <RecoveryCodesDisplay codes={["C1"]} onContinue={vi.fn()} />,
    );
    const continueBtn = screen.getByText("Continue");
    expect(continueBtn).toBeDisabled();

    const checkbox = screen.getByRole("checkbox");
    fireEvent.click(checkbox);
    expect(continueBtn).not.toBeDisabled();
  });

  it("calls onContinue when acknowledged + clicked", () => {
    const onContinue = vi.fn();
    render(
      <RecoveryCodesDisplay codes={["C1"]} onContinue={onContinue} />,
    );
    fireEvent.click(screen.getByRole("checkbox"));
    fireEvent.click(screen.getByText("Continue"));
    expect(onContinue).toHaveBeenCalledOnce();
  });

  it("copy button calls clipboard", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText } });
    render(
      <RecoveryCodesDisplay codes={["C1"]} onContinue={vi.fn()} />,
    );
    fireEvent.click(screen.getByText("Copy codes"));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("C1"));
    await waitFor(() => expect(screen.getByText("Copied!")).toBeInTheDocument());
  });

  it("copy handles missing clipboard gracefully", () => {
    const originalClipboard = navigator.clipboard;
    Object.defineProperty(navigator, "clipboard", { value: undefined, writable: true });
    render(
      <RecoveryCodesDisplay codes={["C1"]} onContinue={vi.fn()} />,
    );
    expect(() => fireEvent.click(screen.getByText("Copy codes"))).not.toThrow();
    Object.defineProperty(navigator, "clipboard", { value: originalClipboard, writable: true });
  });
});
