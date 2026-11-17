import { describe, expect, it, vi } from "vitest";
import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { HarnessPickerDialog } from "./HarnessPickerDialog";
import { render } from "../../test/utils";

function renderPicker(over: Partial<Parameters<typeof HarnessPickerDialog>[0]> = {}) {
  const onSubmit = vi.fn();
  const props: Parameters<typeof HarnessPickerDialog>[0] = {
    open: true,
    onOpenChange: vi.fn(),
    workspaceName: "My Workspace",
    options: ["opencode", "pi"],
    submitting: false,
    onSubmit,
    ...over,
  };
  render(<HarnessPickerDialog {...props} />);
  return { props, onSubmit };
}

describe("HarnessPickerDialog — the first-session picker", () => {
  it("renders the enabled harnesses with name, blurb, and icon", () => {
    renderPicker();

    const dialog = screen.getByRole("dialog");
    expect(dialog).toBeInTheDocument();
    expect(screen.getByText("opencode")).toBeInTheDocument();
    expect(screen.getByText(/terminal-native coding agent/i)).toBeInTheDocument();
    expect(screen.getByText("pi")).toBeInTheDocument();
    expect(screen.getByText(/minimal, hackable agent runtime/i)).toBeInTheDocument();
  });

  it("preselects the platform default (opencode)", () => {
    renderPicker();

    expect(screen.getByRole("radio", { name: /opencode/i })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("radio", { name: /pi/i })).toHaveAttribute("aria-checked", "false");
  });

  it("submits the selected harness — the required selection rides the submit", async () => {
    const user = userEvent.setup();
    const { onSubmit } = renderPicker();

    // Default preselect submits as-is…
    await user.click(screen.getByTestId("harness-picker-submit"));
    expect(onSubmit).toHaveBeenCalledWith("opencode");

    // …and an explicit change submits the choice.
    await user.click(screen.getByRole("radio", { name: /pi/i }));
    await user.click(screen.getByTestId("harness-picker-submit"));
    expect(onSubmit).toHaveBeenLastCalledWith("pi");
  });

  it("keyboard: arrows move the selection and the live region announces it (#2021)", async () => {
    const user = userEvent.setup();
    renderPicker();

    const opencode = screen.getByRole("radio", { name: /opencode/i });
    await user.click(opencode);
    await user.keyboard("{ArrowDown}");

    expect(screen.getByRole("radio", { name: /pi/i })).toHaveFocus();
    expect(screen.getByRole("radio", { name: /pi/i })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByTestId("harness-picker-announcement")).toHaveTextContent(
      "Selected harness: pi",
    );

    await user.keyboard("{ArrowUp}");
    expect(screen.getByRole("radio", { name: /opencode/i })).toHaveFocus();
    expect(screen.getByRole("radio", { name: /opencode/i })).toHaveAttribute("aria-checked", "true");
  });

  it("roving tabindex: only the selected radio is tabbable", () => {
    renderPicker();

    expect(screen.getByRole("radio", { name: /opencode/i })).toHaveAttribute("tabindex", "0");
    expect(screen.getByRole("radio", { name: /pi/i })).toHaveAttribute("tabindex", "-1");
  });

  it("renders the error and disables submit while submitting", () => {
    renderPicker({ submitting: true, error: "Failed to create session" });

    expect(screen.getByText("Failed to create session")).toBeInTheDocument();
    expect(screen.getByTestId("harness-picker-submit")).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
  });

  it("cancel dismisses without submitting — no session is created", async () => {
    const user = userEvent.setup();
    const { props, onSubmit } = renderPicker();

    await user.click(screen.getByRole("button", { name: "Cancel" }));

    expect(onSubmit).not.toHaveBeenCalled();
    expect(props.onOpenChange).toHaveBeenCalledWith(false);
  });
});
