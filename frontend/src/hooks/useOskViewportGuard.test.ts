import { afterEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { useOskViewportGuard } from "./useOskViewportGuard";

// #1648 criterion 4 — dvh does NOT shrink for the on-screen keyboard
// (Chrome 108+ default is interactive-widget=resizes-visual: only the
// visual viewport resizes; iOS Safari overlays). The guard pins the
// shells to the live visual-viewport height while a keyboard is detected
// and clears the override the moment heights reconverge. jsdom has no
// visualViewport, so these tests install a controllable one.

type Listener = () => void;

function installVisualViewport(initial: { height: number; offsetTop?: number }) {
  const listeners = new Set<Listener>();
  const state = { height: initial.height, offsetTop: initial.offsetTop ?? 0 };
  const vv = {
    get height() { return state.height; },
    get offsetTop() { return state.offsetTop; },
    get width() { return 390; },
    get scale() { return 1; },
    addEventListener: (_: string, fn: Listener) => listeners.add(fn),
    removeEventListener: (_: string, fn: Listener) => listeners.delete(fn),
  };
  Object.defineProperty(window, "visualViewport", {
    configurable: true,
    value: vv,
    writable: true,
  });
  return {
    set: (next: Partial<typeof state>) => { Object.assign(state, next); },
    fire: () => { listeners.forEach((fn) => fn()); },
  };
}

function setLayoutViewportHeight(px: number) {
  Object.defineProperty(window, "innerHeight", { configurable: true, value: px, writable: true });
}

afterEach(() => {
  delete (window as { visualViewport?: unknown }).visualViewport;
  delete document.documentElement.dataset.oskOpen;
  document.documentElement.style.removeProperty("--shell-visual-height");
  vi.restoreAllMocks();
});

describe("useOskViewportGuard (#1648 keyboard lane)", () => {
  it("pins the shell height while the visual viewport shrinks (keyboard open)", () => {
    setLayoutViewportHeight(844);
    const vv = installVisualViewport({ height: 844 });
    renderHook(() => useOskViewportGuard());

    act(() => { vv.set({ height: 400 }); vv.fire(); });

    expect(document.documentElement.dataset.oskOpen).toBe("true");
    expect(document.documentElement.style.getPropertyValue("--shell-visual-height")).toBe("400px");
  });

  it("clears the override when heights reconverge (keyboard closed)", () => {
    setLayoutViewportHeight(844);
    const vv = installVisualViewport({ height: 844 });
    renderHook(() => useOskViewportGuard());
    act(() => { vv.set({ height: 400 }); vv.fire(); });
    expect(document.documentElement.dataset.oskOpen).toBe("true");

    act(() => { vv.set({ height: 844 }); vv.fire(); });

    expect(document.documentElement.dataset.oskOpen).toBeUndefined();
    expect(document.documentElement.style.getPropertyValue("--shell-visual-height")).toBe("");
  });

  it("does not activate for sub-threshold differences (URL-bar jitter, not keyboard)", () => {
    setLayoutViewportHeight(844);
    const vv = installVisualViewport({ height: 844 });
    renderHook(() => useOskViewportGuard());

    act(() => { vv.set({ height: 830 }); vv.fire(); }); // 14px < 24px threshold

    expect(document.documentElement.dataset.oskOpen).toBeUndefined();
  });

  it("subtracts a panned offsetTop so the bottom row stays in the visible region", () => {
    setLayoutViewportHeight(844);
    const vv = installVisualViewport({ height: 844 });
    renderHook(() => useOskViewportGuard());

    // Keyboard open AND the visual viewport panned down 200px: the visible
    // span measured from the shell's top is innerHeight - offsetTop (644),
    // smaller than vv.height (700) — the min() must pick the span so the
    // composer bottom stays inside the visible region.
    act(() => { vv.set({ height: 700, offsetTop: 200 }); vv.fire(); });

    expect(document.documentElement.style.getPropertyValue("--shell-visual-height")).toBe("644px");
  });

  it("is a safe no-op where visualViewport is unavailable", () => {
    renderHook(() => useOskViewportGuard());
    expect(document.documentElement.dataset.oskOpen).toBeUndefined();
  });

  it("cleans up the override and listeners on unmount", () => {
    setLayoutViewportHeight(844);
    const vv = installVisualViewport({ height: 844 });
    const { unmount } = renderHook(() => useOskViewportGuard());
    act(() => { vv.set({ height: 400 }); vv.fire(); });
    expect(document.documentElement.dataset.oskOpen).toBe("true");

    unmount();

    expect(document.documentElement.dataset.oskOpen).toBeUndefined();
    expect(document.documentElement.style.getPropertyValue("--shell-visual-height")).toBe("");
    // No further reactions after unmount.
    act(() => { vv.set({ height: 300 }); vv.fire(); });
    expect(document.documentElement.dataset.oskOpen).toBeUndefined();
  });
});
