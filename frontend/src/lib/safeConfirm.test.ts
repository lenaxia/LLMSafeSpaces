import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { safeConfirm } from "./safeConfirm";

describe("safeConfirm", () => {
  let originalConfirm: typeof window.confirm;

  beforeEach(() => {
    originalConfirm = window.confirm;
  });

  afterEach(() => {
    window.confirm = originalConfirm;
  });

  it("returns true when window.confirm returns true", () => {
    window.confirm = vi.fn(() => true);
    expect(safeConfirm("Delete?")).toBe(true);
  });

  it("returns false when window.confirm returns false", () => {
    window.confirm = vi.fn(() => false);
    expect(safeConfirm("Delete?")).toBe(false);
  });

  it("returns false (fail CLOSED) when window.confirm throws", () => {
    window.confirm = vi.fn(() => {
      throw new Error("confirm() blocked by permissions policy");
    });
    expect(safeConfirm("Delete?")).toBe(false);
  });

  it("passes the message through to window.confirm", () => {
    const spy = vi.fn(() => true);
    window.confirm = spy;
    safeConfirm("Delete this workspace?");
    expect(spy).toHaveBeenCalledWith("Delete this workspace?");
  });
});
