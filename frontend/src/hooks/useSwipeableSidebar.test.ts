import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { renderHook } from "@testing-library/react";
import { useRef, useState } from "react";
import { useSwipeableSidebar } from "./useSwipeableSidebar";

if (typeof Touch === "undefined") {
  const PolyfillTouch = class {
    clientX: number;
    clientY: number;
    identifier: number;
    target: EventTarget;
    constructor(init: TouchInit) {
      this.clientX = init.clientX ?? 0;
      this.clientY = init.clientY ?? 0;
      this.identifier = init.identifier;
      this.target = init.target;
    }
  };
  (globalThis as unknown as Record<string, unknown>)["Touch"] = PolyfillTouch;
}

function createDOM() {
  const container = document.createElement("div");
  container.style.width = "400px";
  container.style.height = "800px";
  document.body.appendChild(container);

  const sidebar = document.createElement("div");
  sidebar.style.width = "256px";
  container.appendChild(sidebar);

  const overlay = document.createElement("div");
  container.appendChild(overlay);

  return { container, sidebar, overlay };
}

function dispatchTouch(
  target: Element,
  type: string,
  touches: { clientX: number; clientY: number }[],
  changedTouches?: { clientX: number; clientY: number }[],
) {
  const touchList = touches.map(
    (t) => new Touch({ identifier: 0, target, clientX: t.clientX, clientY: t.clientY }),
  );
  const changed = (changedTouches ?? touches).map(
    (t) => new Touch({ identifier: 0, target, clientX: t.clientX, clientY: t.clientY }),
  );
  const event = new TouchEvent(type, {
    touches: touchList,
    changedTouches: changed,
    bubbles: true,
    cancelable: true,
  });
  target.dispatchEvent(event);
  return event;
}

function setupHook(initialOpen = false) {
  const dom = createDOM();
  const setIsOpen = vi.fn();
  const isOpenRef = { current: initialOpen };

  const { result } = renderHook(
    ({ isOpen }) => {
      const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
      const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
      const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);
      const [open, setOpen] = useState(isOpen);
      isOpenRef.current = open;

      const wrappedSetOpen = (v: boolean | ((prev: boolean) => boolean)) => {
        const next = typeof v === "function" ? v(open) : v;
        isOpenRef.current = next;
        setIsOpen(next);
        setOpen(next);
      };

      useSwipeableSidebar({
        containerRef,
        sidebarRef,
        overlayRef,
        isOpen: open,
        setIsOpen: wrappedSetOpen,
        enabled: true,
        sidebarWidth: 256,
      });

      return { containerRef, sidebarRef, overlayRef, open, setOpen: wrappedSetOpen };
    },
    { initialProps: { isOpen: initialOpen } },
  );

  return { dom, setIsOpen, isOpenRef, result };
}

describe("useSwipeableSidebar", () => {
  let dom: ReturnType<typeof createDOM>;

  beforeEach(() => {
    dom = createDOM();
  });

  afterEach(() => {
    dom.container.remove();
    vi.restoreAllMocks();
  });

  // ── Full-edge swipe to open (#1629) ──────────────────────────────────
  //
  // `overscroll-behavior-x: none` on html/body (the stylesheet pin lives
  // in styles/index.test.ts) removes the browser's horizontal swipe-back
  // navigation at the CSS level — the mechanism the #1626 ruling missed.
  // With no OS race left to lose, the FULL left edge is the app's again:
  // the original a56430b7/#590 EDGE_ZONE contract is restored — a touch
  // starting in the leftmost 30px is claimed at touchstart (preventDefault)
  // and a rightward drag opens the drawer. The hamburger stays the
  // always-works path; swipe-to-close is unchanged.

  describe("full-edge swipe to open", () => {
    it("opens sidebar on rightward swipe from the left edge", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [
        { clientX: 130, clientY: 200 }],
      );
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(setIsOpen).toHaveBeenCalledWith(true);
    });

    it("opens at the inner boundary of the edge zone (29px)", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 29, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(setIsOpen).toHaveBeenCalledWith(true);
    });

    it("does not open on swipe starting at 30px (outside the edge zone)", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 150, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 150, clientY: 200 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
    });

    it("does not open on swipe starting outside the edge zone", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 50, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 150, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 150, clientY: 200 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
    });

    it("does not open on very short swipe below settle threshold", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 40, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 40, clientY: 200 }]);

      expect(setIsOpen).toHaveBeenCalledWith(false);
    });
  });

  describe("swipe to close", () => {
    it("closes sidebar on leftward swipe when open", () => {
      const { setIsOpen, dom } = setupHook(true);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 200, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 100, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 100, clientY: 200 }]);

      expect(setIsOpen).toHaveBeenCalledWith(false);
    });

    it("does not close on very short leftward swipe", () => {
      const { setIsOpen, dom } = setupHook(true);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 200, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 195, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 195, clientY: 200 }]);

      expect(setIsOpen).toHaveBeenCalledWith(true);
    });
  });

  describe("vertical scroll passthrough", () => {
    it("does not intercept primarily vertical gestures outside the edge zone", () => {
      const { setIsOpen, dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 50, clientY: 100 },
      ]);
      const moveEvent = dispatchTouch(dom.container, "touchmove", [
        { clientX: 55, clientY: 300 },
      ]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 55, clientY: 300 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
      expect(startEvent.defaultPrevented).toBe(false);
      expect(moveEvent.defaultPrevented).toBe(false);
    });

    it("never claims the MOVE of a primarily vertical drag, even from the edge", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 100 }]);
      const moveEvent = dispatchTouch(dom.container, "touchmove", [
        { clientX: 15, clientY: 300 },
      ]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 15, clientY: 300 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
      expect(moveEvent.defaultPrevented).toBe(false);
    });
  });

  describe("visual tracking during swipe", () => {
    it("moves sidebar transform during edge swipe", () => {
      const { dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);

      const transform = dom.sidebar.style.transform;
      expect(transform).toContain("translateX");
    });

    it("moves sidebar transform during close swipe", () => {
      const { dom } = setupHook(true);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 200, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 100, clientY: 200 }]);

      const transform = dom.sidebar.style.transform;
      expect(transform).toContain("translateX");
    });

    it("clears inline styles after transition completes", () => {
      const { dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(dom.sidebar.style.transform).toBe("");
      expect(dom.sidebar.style.transition).toBe("");
    });
  });

  describe("touchmove prevention", () => {
    it("prevents default on horizontal swipe from the edge", () => {
      const { dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      const moveEvent = dispatchTouch(dom.container, "touchmove", [
        { clientX: 130, clientY: 200 },
      ]);

      expect(moveEvent.defaultPrevented).toBe(true);
    });

    it("prevents default on horizontal swipe outside the edge zone too (blanket horizontal claim — no scoping)", () => {
      const { dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 100, clientY: 200 }]);
      const moveEvent = dispatchTouch(dom.container, "touchmove", [
        { clientX: 200, clientY: 200 },
      ]);

      expect(moveEvent.defaultPrevented).toBe(true);
    });

    it("does not prevent default on vertical gesture", () => {
      const { dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 50, clientY: 100 }]);
      const moveEvent = dispatchTouch(dom.container, "touchmove", [
        { clientX: 52, clientY: 300 },
      ]);

      expect(moveEvent.defaultPrevented).toBe(false);
    });
  });

  // ── Gesture claim (#1629: recognition AND claim at touchstart) ──────
  //
  // The #1626 handle design claimed only at the horizontal-move stage
  // because it ceded the absolute edge to the OS. The CSS mechanism
  // (overscroll-behavior-x: none) removes that race at the platform
  // level, so the a56430b7/#590 touchstart claim is restored: an edge
  // touchstart is preventDefaulted (non-passive listener) at the
  // earliest moment. The leftmost 30px of a plain surface is the
  // gesture zone, not a tap surface — EXCEPT interactive controls: the
  // hamburger button's left half lives inside the band (header px-3),
  // and the #1626 r2 review proved empirically that a touchstart
  // preventDefault over a tappable control suppresses its synthetic
  // click. The claim therefore skips touches that land on interactive
  // elements — "the hamburger stays the always-works path" is part of
  // the #1629 ruling. Their swipes are not gesture origins either
  // (isEdgeSwipe stays false), which matches the original behavior for
  // every non-edge start.

  describe("gesture claim (touchstart preventDefault at the edge)", () => {
    it("prevents default on touchstart at the left edge (claims the gesture early)", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 10, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(true);
    });

    it("claims at the inner boundary of the edge zone (29px)", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 29, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(true);
    });

    it("does not prevent default on touchstart at 30px (outside the edge zone)", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 30, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not prevent default on touchstart outside the edge zone", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 100, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("prevents default on touchstart at the edge while the sidebar is open (no back-nav over the drawer)", () => {
      const { dom } = setupHook(true);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 10, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(true);
    });

    it("does not prevent default on multi-touch touchstart", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 10, clientY: 100 },
        { clientX: 200, clientY: 100 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not claim a touchstart that lands on a button inside the edge band (the hamburger's left half)", () => {
      const dom = createDOM();
      const button = document.createElement("button");
      dom.container.appendChild(button);
      const setIsOpen = vi.fn();

      renderHook(() => {
        const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
        const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
        const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);
        useSwipeableSidebar({
          containerRef, sidebarRef, overlayRef,
          isOpen: false, setIsOpen, enabled: true, sidebarWidth: 256,
        });
      });

      const startEvent = dispatchTouch(button, "touchstart", [{ clientX: 15, clientY: 200 }]);
      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not claim a touchstart that lands on a link inside the edge band", () => {
      const dom = createDOM();
      const link = document.createElement("a");
      dom.container.appendChild(link);
      const setIsOpen = vi.fn();

      renderHook(() => {
        const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
        const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
        const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);
        useSwipeableSidebar({
          containerRef, sidebarRef, overlayRef,
          isOpen: false, setIsOpen, enabled: true, sidebarWidth: 256,
        });
      });

      const startEvent = dispatchTouch(link, "touchstart", [{ clientX: 15, clientY: 200 }]);
      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("a swipe starting on an interactive element never becomes an open-gesture (buttons are taps, not swipe origins)", () => {
      const dom = createDOM();
      const button = document.createElement("button");
      dom.container.appendChild(button);
      const setIsOpen = vi.fn();

      renderHook(() => {
        const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
        const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
        const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);
        useSwipeableSidebar({
          containerRef, sidebarRef, overlayRef,
          isOpen: false, setIsOpen, enabled: true, sidebarWidth: 256,
        });
      });

      dispatchTouch(button, "touchstart", [{ clientX: 15, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 140, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 140, clientY: 200 }]);
      expect(setIsOpen).not.toHaveBeenCalled();
    });
  });

  describe("when disabled", () => {
    it("does not attach touch listeners when enabled is false", () => {
      const setIsOpen = vi.fn();

      renderHook(() => {
        const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
        const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
        const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);

        useSwipeableSidebar({
          containerRef,
          sidebarRef,
          overlayRef,
          isOpen: false,
          setIsOpen,
          enabled: false,
          sidebarWidth: 256,
        });
      });

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
    });
  });

  describe("cleanup", () => {
    it("removes event listeners on unmount", () => {
      const setIsOpen = vi.fn();

      const { unmount } = renderHook(() => {
        const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
        const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
        const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);

        useSwipeableSidebar({
          containerRef,
          sidebarRef,
          overlayRef,
          isOpen: false,
          setIsOpen,
          enabled: true,
          sidebarWidth: 256,
        });
      });

      unmount();

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
    });

    it("listeners persist after first swipe — gesture works more than once", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);
      expect(setIsOpen).toHaveBeenCalledWith(true);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);
      expect(setIsOpen).toHaveBeenCalledTimes(2);
    });
  });
});
