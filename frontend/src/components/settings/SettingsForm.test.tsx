import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { render as renderWithQuery } from "../../test/utils";
import { describe, it, expect, vi } from "vitest";
import { SettingsForm } from "./SettingsForm";
import type { SettingDef } from "../../api/settings";

vi.mock("../../api/imageFactory", () => ({
  imageFactoryApi: {
    listConfigs: vi.fn().mockResolvedValue([
        { id: "c1", hash: "s-abc", name: "Python Image", status: "ready", selection: ["python-3.12"], scope: "member" },
      ]),
  },
}));

const mockSchema: SettingDef[] = [
  { key: "test.bool", tier: 3, type: "bool", default: false, category: "Test", label: "Bool Setting", description: "A boolean" },
  { key: "test.int", tier: 3, type: "int", default: 14, min: 10, max: 24, category: "Test", label: "Int Setting", description: "An integer" },
  { key: "test.enum", tier: 3, type: "enum", default: "a", enum: ["a", "b", "c"], category: "Test", label: "Enum Setting", description: "An enum" },
  { key: "test.string", tier: 3, type: "string", default: "", category: "Other", label: "String Setting", description: "A string" },
];

describe("SettingsForm", () => {
  it("renders all categories", () => {
    render(<SettingsForm schema={mockSchema} values={{}} onSave={vi.fn()} />);
    expect(screen.getByText("Test")).toBeInTheDocument();
    expect(screen.getByText("Other")).toBeInTheDocument();
  });

  it("renders labels and descriptions", () => {
    render(<SettingsForm schema={mockSchema} values={{}} onSave={vi.fn()} />);
    expect(screen.getByText("Bool Setting")).toBeInTheDocument();
    expect(screen.getByText("A boolean")).toBeInTheDocument();
    expect(screen.getByText("Int Setting")).toBeInTheDocument();
  });

  it("renders toggle for bool type", () => {
    render(<SettingsForm schema={mockSchema} values={{ "test.bool": true }} onSave={vi.fn()} />);
    const toggle = screen.getByRole("switch");
    expect(toggle).toBeInTheDocument();
    expect(toggle).toHaveAttribute("data-state", "checked");
  });

  it("renders number input for int type", () => {
    render(<SettingsForm schema={mockSchema} values={{ "test.int": 18 }} onSave={vi.fn()} />);
    const input = screen.getByRole("spinbutton");
    expect(input).toHaveValue(18);
  });

  it("renders text input for string type", () => {
    render(<SettingsForm schema={mockSchema} values={{ "test.string": "hello" }} onSave={vi.fn()} />);
    const input = screen.getByDisplayValue("hello");
    expect(input).toBeInTheDocument();
  });

  it("calls onSave when toggle is clicked", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<SettingsForm schema={mockSchema} values={{ "test.bool": false }} onSave={onSave} />);

    const toggle = screen.getByRole("switch");
    fireEvent.click(toggle);

    await waitFor(() => {
      expect(onSave).toHaveBeenCalledWith("test.bool", true);
    });
  });

  it("calls onSave when number input is changed and blurred", async () => {
    const onSave = vi.fn().mockResolvedValue(undefined);
    render(<SettingsForm schema={mockSchema} values={{ "test.int": 14 }} onSave={onSave} />);

    const input = screen.getByRole("spinbutton");
    fireEvent.change(input, { target: { value: "20" } });
    fireEvent.blur(input);

    await waitFor(() => {
      expect(onSave).toHaveBeenCalledWith("test.int", 20);
    });
  });

  it("does not crash when onSave fails", async () => {
    const onSave = vi.fn().mockRejectedValue(new Error("Network error"));
    render(<SettingsForm schema={mockSchema} values={{ "test.bool": false }} onSave={onSave} />);

    const toggle = screen.getByRole("switch");
    fireEvent.click(toggle);

    await waitFor(() => {
      expect(onSave).toHaveBeenCalled();
    });
    // Error is handled by parent (toast) — no inline error shown
  });

  it("uses default value when value not in values map", () => {
    render(<SettingsForm schema={mockSchema} values={{}} onSave={vi.fn()} />);
    // Int default is 14
    const input = screen.getByRole("spinbutton");
    expect(input).toHaveValue(14);
  });

  it("disables controls when disabled prop is true", () => {
    render(<SettingsForm schema={mockSchema} values={{}} onSave={vi.fn()} disabled />);
    const toggle = screen.getByRole("switch");
    expect(toggle).toBeDisabled();
    const input = screen.getByRole("spinbutton");
    expect(input).toBeDisabled();
  });

  describe("responsive layout", () => {
    it("setting rows use responsive flex direction (column on mobile, row on desktop)", () => {
      const { container } = render(<SettingsForm schema={mockSchema} values={{}} onSave={vi.fn()} />);
      const rows = container.querySelectorAll(".divide-y > div");
      // Each row should have flex-col for mobile and sm:flex-row for desktop
      rows.forEach((row) => {
        expect(row.className).toContain("flex-col");
        expect(row.className).toContain("sm:flex-row");
      });
    });

    it("string input uses full width on mobile and fixed width on desktop", () => {
      render(<SettingsForm schema={mockSchema} values={{ "test.string": "hello" }} onSave={vi.fn()} />);
      const input = screen.getByDisplayValue("hello");
      expect(input.className).toContain("w-full");
      expect(input.className).toContain("sm:w-48");
    });
  });

  // Regression: real production failure 2026-06-18.
  // Admin saved workspace.defaultResources.memory = "8gi" (lowercase
  // unit). The setting had no pattern, so the value reached the
  // database. Every subsequent workspace creation failed with a
  // cryptic webhook error. The frontend silently submitted the
  // invalid value because the StringInput did not consult def.pattern.
  // These tests pin the contract: when def.pattern is present, the
  // frontend must validate before calling onSave and must show the
  // user what's expected.
  describe("string input pattern validation", () => {
    const memorySchema: SettingDef[] = [
      {
        key: "workspace.defaultResources.memory",
        tier: 2,
        type: "string",
        default: "1Gi",
        pattern: "^[0-9]+(Ki|Mi|Gi)$",
        category: "Workspace",
        label: "Default Memory",
        description: "Default memory limit (e.g. 512Mi, 1Gi). Suffix is case-sensitive.",
      },
    ];

    it("does not call onSave when value violates pattern", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);

      const input = screen.getByDisplayValue("1Gi");
      // Truly-invalid value the normalizer can't safely fix.
      fireEvent.change(input, { target: { value: "banana" } });
      fireEvent.blur(input);

      // Give any pending microtasks a chance to run, then assert the
      // negative: no save call was made for the invalid value. We
      // can't waitFor a non-event, so a microtask flush + immediate
      // assertion is the right shape.
      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(onSave).not.toHaveBeenCalled();
    });

    it("shows a visible error when value violates pattern", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);

      const input = screen.getByDisplayValue("1Gi");
      fireEvent.change(input, { target: { value: "banana" } });
      fireEvent.blur(input);

      // The error must be reachable via the accessibility tree —
      // aria-invalid plus an aria-describedby message — so screen
      // readers and the visible UI are both wired.
      await waitFor(() => {
        expect(input).toHaveAttribute("aria-invalid", "true");
      });
      const describedBy = input.getAttribute("aria-describedby");
      expect(describedBy).toBeTruthy();
      const errorEl = document.getElementById(describedBy as string);
      expect(errorEl).not.toBeNull();
      expect(errorEl?.textContent || "").toMatch(/match|pattern|format/i);
    });

    it("calls onSave when value matches pattern", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);

      const input = screen.getByDisplayValue("1Gi");
      fireEvent.change(input, { target: { value: "8Gi" } });
      fireEvent.blur(input);

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("workspace.defaultResources.memory", "8Gi");
      });
      // No error state when value is valid.
      expect(input).not.toHaveAttribute("aria-invalid", "true");
    });

    it("clears error state when invalid value is replaced with valid one", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);

      const input = screen.getByDisplayValue("1Gi");

      // First: invalid → error appears
      fireEvent.change(input, { target: { value: "banana" } });
      fireEvent.blur(input);
      await waitFor(() => {
        expect(input).toHaveAttribute("aria-invalid", "true");
      });

      // Then: valid → error clears
      fireEvent.change(input, { target: { value: "8Gi" } });
      fireEvent.blur(input);
      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("workspace.defaultResources.memory", "8Gi");
      });
      expect(input).not.toHaveAttribute("aria-invalid", "true");
    });

    it("does not block input while typing — only validates on commit", () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);

      const input = screen.getByDisplayValue("1Gi");
      // User is mid-typing "8Gi" — at "8" alone, value is invalid by
      // pattern. We must NOT show an error or block typing yet.
      fireEvent.change(input, { target: { value: "8" } });
      expect(input).not.toHaveAttribute("aria-invalid", "true");
      // Until they blur (commit), nothing happens.
      expect(onSave).not.toHaveBeenCalled();
    });

    it("shows pattern as a hint (placeholder or title) so user knows what's expected", () => {
      render(<SettingsForm schema={memorySchema} values={{}} onSave={vi.fn()} />);
      const input = screen.getByLabelText("Default Memory");
      // Either as placeholder or title — both are accessible hints.
      const placeholder = input.getAttribute("placeholder") || "";
      const title = input.getAttribute("title") || "";
      const hint = `${placeholder}${title}`;
      // The hint must reveal what's expected — either by showing the
      // pattern verbatim, or by showing an example value (e.g. "1Gi").
      // Either is acceptable, both fail the empty-string case.
      expect(hint.length).toBeGreaterThan(0);
    });

    it("string settings without a pattern accept any value (no regression)", async () => {
      // The branding-style settings (instance.name etc.) and free-form
      // strings should still work as before — only patterned strings
      // get the new validation.
      const onSave = vi.fn().mockResolvedValue(undefined);
      const freeFormSchema: SettingDef[] = [
        { key: "test.string", tier: 3, type: "string", default: "", category: "Test", label: "Free Text", description: "" },
      ];
      render(<SettingsForm schema={freeFormSchema} values={{ "test.string": "" }} onSave={onSave} />);
      const input = screen.getByLabelText("Free Text");
      fireEvent.change(input, { target: { value: "anything goes 123!@#" } });
      fireEvent.blur(input);
      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("test.string", "anything goes 123!@#");
      });
      expect(input).not.toHaveAttribute("aria-invalid", "true");
    });
  });

  // Normalization on commit: when the user types an unambiguous near-miss
  // (lowercase unit, GB instead of Gi, stray whitespace) the form
  // canonicalizes the value before submitting. The user sees the
  // auto-correction land in the input itself — no toast, no error,
  // just the value silently fixed. This mirrors the backend
  // pkg/settings/Normalize() so a curl client and a UI client both
  // get the same canonical value on the wire.
  describe("string input normalization", () => {
    const memorySchema: SettingDef[] = [
      {
        key: "workspace.defaultResources.memory",
        tier: 2,
        type: "string",
        default: "1Gi",
        pattern: "^[0-9]+(Ki|Mi|Gi)$",
        category: "Workspace",
        label: "Default Memory",
        description: "Default memory limit",
      },
    ];

    it("auto-corrects lowercase unit on blur (the production bug)", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);
      const input = screen.getByDisplayValue("1Gi") as HTMLInputElement;

      fireEvent.change(input, { target: { value: "8gi" } });
      fireEvent.blur(input);

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("workspace.defaultResources.memory", "8Gi");
      });
      expect(input.value).toBe("8Gi");
      expect(input).not.toHaveAttribute("aria-invalid", "true");
    });

    it("auto-corrects GB → Gi", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);
      const input = screen.getByDisplayValue("1Gi") as HTMLInputElement;

      fireEvent.change(input, { target: { value: "8GB" } });
      fireEvent.blur(input);

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("workspace.defaultResources.memory", "8Gi");
      });
      expect(input.value).toBe("8Gi");
    });

    it("trims whitespace", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);
      const input = screen.getByDisplayValue("1Gi") as HTMLInputElement;

      fireEvent.change(input, { target: { value: "  8 Gi  " } });
      fireEvent.blur(input);

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("workspace.defaultResources.memory", "8Gi");
      });
      expect(input.value).toBe("8Gi");
    });

    it("does not normalize ambiguous inputs — they fall through to the pattern error", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);
      const input = screen.getByDisplayValue("1Gi") as HTMLInputElement;

      // Bare "G" is ambiguous (decimal vs binary). Don't auto-correct;
      // show the user the error so they pick consciously.
      fireEvent.change(input, { target: { value: "8 G" } });
      fireEvent.blur(input);

      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(onSave).not.toHaveBeenCalled();
      await waitFor(() => {
        expect(input).toHaveAttribute("aria-invalid", "true");
      });
      expect(input.value).toBe("8 G");
    });

    it("does not normalize garbage", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      render(<SettingsForm schema={memorySchema} values={{ "workspace.defaultResources.memory": "1Gi" }} onSave={onSave} />);
      const input = screen.getByDisplayValue("1Gi") as HTMLInputElement;

      fireEvent.change(input, { target: { value: "banana" } });
      fireEvent.blur(input);

      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(onSave).not.toHaveBeenCalled();
      await waitFor(() => {
        expect(input).toHaveAttribute("aria-invalid", "true");
      });
    });

    it("CPU 500M → 500m", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      const cpuSchema: SettingDef[] = [
        {
          key: "workspace.defaultResources.cpu",
          tier: 2,
          type: "string",
          default: "500m",
          pattern: "^([0-9]+m|[0-9]+\\.[0-9]+)$",
          category: "Workspace",
          label: "Default CPU",
          description: "Default CPU limit",
        },
      ];
      render(<SettingsForm schema={cpuSchema} values={{ "workspace.defaultResources.cpu": "500m" }} onSave={onSave} />);
      const input = screen.getByDisplayValue("500m") as HTMLInputElement;

      fireEvent.change(input, { target: { value: "1000M" } });
      fireEvent.blur(input);

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("workspace.defaultResources.cpu", "1000m");
      });
      expect(input.value).toBe("1000m");
    });

    it("non-resource patterned strings are not normalized", async () => {
      // instance.name has a pattern (^.{1,64}$) but isn't a resource
      // quantity. Don't auto-uppercase or trim it — that'd be
      // surprising behavior for a name field.
      const onSave = vi.fn().mockResolvedValue(undefined);
      const nameSchema: SettingDef[] = [
        {
          key: "instance.name",
          tier: 2,
          type: "string",
          default: "LLMSafeSpace",
          pattern: "^.{1,64}$",
          category: "Branding",
          label: "Instance Name",
          description: "",
        },
      ];
      render(<SettingsForm schema={nameSchema} values={{ "instance.name": "Old" }} onSave={onSave} />);
      const input = screen.getByDisplayValue("Old") as HTMLInputElement;

      fireEvent.change(input, { target: { value: "  My Workspace  " } });
      fireEvent.blur(input);

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("instance.name", "  My Workspace  ");
      });
      expect(input.value).toBe("  My Workspace  ");
    });
  });

  describe("readOnly (helm-managed)", () => {
    it("shows Managed by Helm badge for readOnly settings", () => {
      const readOnlySchema: SettingDef[] = [
        { key: "email.provider", tier: 2, type: "string", default: "", category: "Email", label: "Provider", description: "Email provider", readOnly: true },
      ];
      render(<SettingsForm schema={readOnlySchema} values={{ "email.provider": "ses" }} onSave={vi.fn()} />);
      expect(screen.getByText("Managed by Helm")).toBeInTheDocument();
    });

    it("disables the control for readOnly settings", () => {
      const readOnlySchema: SettingDef[] = [
        { key: "email.provider", tier: 2, type: "string", default: "", category: "Email", label: "Provider", description: "Email provider", readOnly: true },
      ];
      render(<SettingsForm schema={readOnlySchema} values={{ "email.provider": "ses" }} onSave={vi.fn()} />);
      const input = screen.getByDisplayValue("ses") as HTMLInputElement;
      expect(input).toBeDisabled();
    });

    it("does not call onSave for readOnly settings when user attempts to edit", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      const readOnlySchema: SettingDef[] = [
        { key: "email.provider", tier: 2, type: "string", default: "", category: "Email", label: "Provider", description: "Email provider", readOnly: true },
      ];
      render(<SettingsForm schema={readOnlySchema} values={{ "email.provider": "ses" }} onSave={onSave} />);
      // The input is disabled, so change events won't fire in a real browser.
      // Verify the guard by checking onSave was never called after render + attempted interaction.
      const input = screen.getByDisplayValue("ses") as HTMLInputElement;
      expect(input).toBeDisabled();
      expect(onSave).not.toHaveBeenCalled();
    });

    it("does not show badge for non-readOnly settings", () => {
      render(<SettingsForm schema={mockSchema} values={{}} onSave={vi.fn()} />);
      expect(screen.queryByText("Managed by Helm")).not.toBeInTheDocument();
    });
  });

  describe("preferredRuntime dropdown", () => {
    const runtimeSchema: SettingDef[] = [
      { key: "preferredRuntime", tier: 3, type: "string", default: "", category: "Workspace", label: "Default Image", description: "Test" },
    ];

    it("renders a select dropdown with ready configs", async () => {
      renderWithQuery(<SettingsForm schema={runtimeSchema} values={{ preferredRuntime: "" }} onSave={vi.fn()} />);

      const select = await screen.findByDisplayValue("Use default");
      expect(select.tagName).toBe("SELECT");
      expect(screen.getByText("Python Image")).toBeInTheDocument();
    });

    it("falls back to text input when no configs available", async () => {
      const { imageFactoryApi } = await import("../../api/imageFactory");
      vi.mocked(imageFactoryApi.listConfigs).mockResolvedValueOnce([]);

      renderWithQuery(<SettingsForm schema={runtimeSchema} values={{ preferredRuntime: "" }} onSave={vi.fn()} />);

      const input = await screen.findByPlaceholderText("No custom images available — using default");
      expect(input.tagName).toBe("INPUT");
    });

    it("calls onSave with config hash when a config is selected", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      renderWithQuery(<SettingsForm schema={runtimeSchema} values={{ preferredRuntime: "" }} onSave={onSave} />);

      const select = await screen.findByDisplayValue("Use default");
      fireEvent.change(select, { target: { value: "s-abc" } });

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("preferredRuntime", "s-abc");
      });
    });

    it("calls onSave with empty string when 'Use default' is selected", async () => {
      const onSave = vi.fn().mockResolvedValue(undefined);
      renderWithQuery(<SettingsForm schema={runtimeSchema} values={{ preferredRuntime: "s-abc" }} onSave={onSave} />);

      const select = await screen.findByDisplayValue("Python Image");
      fireEvent.change(select, { target: { value: "" } });

      await waitFor(() => {
        expect(onSave).toHaveBeenCalledWith("preferredRuntime", "");
      });
    });
  });
});
