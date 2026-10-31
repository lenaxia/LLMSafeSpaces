import { describe, expect, it } from "vitest";
import { screen } from "@testing-library/react";
import { render } from "../../test/utils";
import { BusyIndicator } from "./BusyIndicator";

describe("BusyIndicator", () => {
  it("renders with accessible label", () => {
    render(<BusyIndicator />);
    expect(screen.getByLabelText("Agent working")).toBeInTheDocument();
  });

  it("renders medium size by default", () => {
    render(<BusyIndicator />);
    const el = screen.getByLabelText("Agent working");
    expect(el.getAttribute("class")).toContain("h-3.5");
  });

  it("renders small size", () => {
    render(<BusyIndicator size="sm" />);
    const el = screen.getByLabelText("Agent working");
    expect(el.getAttribute("class")).toContain("h-3");
  });

  it("has data-busy attribute", () => {
    render(<BusyIndicator />);
    expect(screen.getByLabelText("Agent working").hasAttribute("data-busy")).toBe(true);
  });

  it("has animation and blue color classes", () => {
    render(<BusyIndicator />);
    const el = screen.getByLabelText("Agent working");
    expect(el.getAttribute("class")).toContain("animate-spin");
    expect(el.getAttribute("class")).toContain("text-blue-500");
  });

  it("renders the distinct quiet marker when busy=false (RC-C S2)", () => {
    render(<BusyIndicator busy={false} />);
    const el = screen.getByLabelText("Agent claimed — quiet");
    expect(el.getAttribute("data-busy")).toBe("false");
    expect(el.getAttribute("class")).not.toContain("animate-spin");
    expect(el.getAttribute("class")).toContain("text-muted-foreground");
  });

  it("defaults to busy (back-compat: the session-activity surfaces pass no prop)", () => {
    render(<BusyIndicator />);
    expect(screen.getByLabelText("Agent working")).toBeInTheDocument();
  });
});
