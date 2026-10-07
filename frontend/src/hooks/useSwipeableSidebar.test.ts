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

  const handle = document.createElement("div");
  container.appendChild(handle);

  return { container, sidebar, overlay, handle };
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

  vi.spyOn(dom.handle, "getBoundingClientRect").mockReturnValue({
    left: 16, top: 0, width: 28, height: 800, right: 44, bottom: 800, x: 16, y: 0,
  } as DOMRect);

  const { result } = renderHook(
    ({ isOpen }) => {
      const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
      const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
      const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);
      const handleRef = useRef<HTMLDivElement>(dom.handle as HTMLDivElement);
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
        handleRef,
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

  // ── Handle-zone swipe to open (#1623) ─────────────────────────────────
  //
  // The OS owns the absolute left edge (its back-gesture zone — no page
  // technique reliably suppresses it on iOS). The app's swipe-to-open
  // affordance is the INSET drag-handle strip: the gesture engages only
  // for touches that start inside the handle's rect. A swipe at the
  // absolute edge belongs to the OS and must do nothing here.

  describe("handle-zone swipe to open", () => {
    it("opens sidebar on rightward swipe from the handle zone", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [
        { clientX: 130, clientY: 200 }],
      );
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(setIsOpen).toHaveBeenCalledWith(true);
    });

    it("does not open on swipe starting at the absolute left edge (OS back-gesture zone)", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 10, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
    });

    it("does not open on swipe starting outside the handle zone", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 50, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 150, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 150, clientY: 200 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
    });

    it("does not open on very short swipe below settle threshold", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
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
    it("does not intercept vertical gestures at the absolute edge (edge-to-edge scrolling)", () => {
      const { setIsOpen, dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 10, clientY: 100 },
      ]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 15, clientY: 300 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 15, clientY: 300 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not intercept primarily vertical gestures outside the handle zone", () => {
      const { setIsOpen, dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 50, clientY: 100 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 55, clientY: 300 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 55, clientY: 300 }]);

      expect(setIsOpen).not.toHaveBeenCalled();
    });
  });

  describe("visual tracking during swipe", () => {
    it("moves sidebar transform during handle-zone swipe", () => {
      const { dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
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

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);

      expect(dom.sidebar.style.transform).toBe("");
      expect(dom.sidebar.style.transition).toBe("");
    });
  });

  describe("touchmove prevention", () => {
    it("prevents default on horizontal swipe from the handle zone", () => {
      const { dom } = setupHook(false);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
      const moveEvent = dispatchTouch(dom.container, "touchmove", [
        { clientX: 130, clientY: 200 },
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

  // ── Gesture claim (#1623: recognition at start, claim at move) ──────
  //
  // The a56430b7/#590 absolute-edge claim (preventDefault on every
  // touchstart with clientX < 30) is REMOVED by the owner ruling: the OS
  // captures its back-gesture before page JavaScript often enough that no
  // page technique wins reliably (~50% observed), and the claim's
  // documented tradeoff blocked vertical scrolling in the leftmost 30px.
  // The handle strip is inset CLEAR of the OS back-gesture zone, so it
  // needs no touchstart claim to beat the OS — and making none preserves
  // tap-through: a touchstart preventDefault suppresses synthetic
  // clicks, which broke the hamburger button under the strip (the r2
  // review's empirical finding). The gesture is claimed only at the
  // horizontal-move stage (pinned in the touchmove-prevention describe).

  describe("gesture claim (touchstart stays unclaimed; the move carries it)", () => {
    it("does not prevent default on touchstart in the handle zone (tap-through: synthetic clicks must survive)", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 30, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not prevent default on touchstart at the absolute edge (the OS back-gesture zone)", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 10, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not prevent default on touchstart outside the handle zone", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 100, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not prevent default on touchstart when sidebar is open (the drawer surface is inset; close-swipes never race the OS)", () => {
      const { dom } = setupHook(true);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 10, clientY: 200 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });

    it("does not prevent default on multi-touch touchstart", () => {
      const { dom } = setupHook(false);

      const startEvent = dispatchTouch(dom.container, "touchstart", [
        { clientX: 30, clientY: 100 },
        { clientX: 200, clientY: 100 },
      ]);

      expect(startEvent.defaultPrevented).toBe(false);
    });
  });

  describe("when disabled", () => {
    it("does not attach touch listeners when enabled is false", () => {
      const setIsOpen = vi.fn();

      renderHook(() => {
        const containerRef = useRef<HTMLDivElement>(dom.container as HTMLDivElement);
        const sidebarRef = useRef<HTMLDivElement>(dom.sidebar as HTMLDivElement);
        const overlayRef = useRef<HTMLDivElement>(dom.overlay as HTMLDivElement);
        const handleRef = useRef<HTMLDivElement>(dom.handle as HTMLDivElement);

        useSwipeableSidebar({
          containerRef,
          sidebarRef,
          overlayRef,
          handleRef,
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
        const handleRef = useRef<HTMLDivElement>(dom.handle as HTMLDivElement);

        useSwipeableSidebar({
          containerRef,
          sidebarRef,
          overlayRef,
          handleRef,
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

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);
      expect(setIsOpen).toHaveBeenCalledWith(true);

      dispatchTouch(dom.container, "touchstart", [{ clientX: 30, clientY: 200 }]);
      dispatchTouch(dom.container, "touchmove", [{ clientX: 130, clientY: 200 }]);
      dispatchTouch(dom.container, "touchend", [], [{ clientX: 130, clientY: 200 }]);
      expect(setIsOpen).toHaveBeenCalledTimes(2);
    });
  });
});
