import { useEffect, useRef, type RefObject } from "react";

// #1623: the absolute-edge claim (EDGE_ZONE + touchstart preventDefault,
// a56430b7/#590) is RETIRED — the OS captures its back-gesture before
// page JavaScript often enough that no page technique wins reliably
// (~50% observed in production), and the claim's documented tradeoff
// blocked vertical scrolling in the leftmost 30px. Swipe-to-open now
// engages from the INSET drag-handle strip (see SidebarDrawer): a touch
// starts the open-gesture iff it begins inside the handle's rect, and
// ONLY that zone is claimed — it starts inside the OS back-gesture
// curve, so the OS never races for it. The absolute edge belongs to
// back-navigation; the hamburger stays the always-works open path.
const MIN_DRAG_PX = 30;
const SETTLE_RATIO = 1 / 3;

interface UseSwipeableSidebarOptions {
  containerRef: RefObject<HTMLDivElement | null>;
  sidebarRef: RefObject<HTMLDivElement | null>;
  overlayRef: RefObject<HTMLDivElement | null>;
  /** The inset drag-handle strip; the open-gesture starts only inside its rect. */
  handleRef: RefObject<HTMLDivElement | null>;
  isOpen: boolean;
  setIsOpen: (value: boolean | ((prev: boolean) => boolean)) => void;
  enabled: boolean;
  sidebarWidth: number;
}

export function useSwipeableSidebar({
  containerRef,
  sidebarRef,
  overlayRef,
  handleRef,
  isOpen,
  setIsOpen,
  enabled,
  sidebarWidth,
}: UseSwipeableSidebarOptions) {
  const touchStartX = useRef(0);
  const touchStartY = useRef(0);
  const isHandleSwipe = useRef(false);
  const isSwiping = useRef(false);
  const swipeOffset = useRef(0);
  const isOpenRef = useRef(isOpen);

  useEffect(() => {
    isOpenRef.current = isOpen;
  }, [isOpen]);

  useEffect(() => {
    if (!enabled) return;

    const el = containerRef.current;
    if (!el) return;

    const onStart = (e: TouchEvent) => {
      if (e.touches.length > 1) return;
      const t = e.touches[0]!;
      touchStartX.current = t.clientX;
      touchStartY.current = t.clientY;
      isHandleSwipe.current = startedInHandle(handleRef.current, t.clientX, t.clientY);
      // Claim the gesture at touchstart ONLY for handle-zone touches: the
      // handle strip is inset from the absolute edge (outside the OS
      // back-gesture zone), so claiming here cannot race the OS — and a
      // deliberate affordance may own its touches outright (the strip is
      // not a scroll surface). The absolute edge is left to the OS.
      if (isHandleSwipe.current) {
        e.preventDefault();
      }
    };

    const onMove = (e: TouchEvent) => {
      if (e.touches.length > 1) return;
      const t = e.touches[0]!;
      const dx = t.clientX - touchStartX.current;
      const dy = Math.abs(t.clientY - touchStartY.current);

      if (dy > Math.abs(dx)) return;

      e.preventDefault();

      const side = sidebarRef.current;
      const over = overlayRef.current;
      const open = isOpenRef.current;

      if (isHandleSwipe.current && dx > 0 && !open) {
        isSwiping.current = true;
        const offset = Math.min(dx, sidebarWidth);
        swipeOffset.current = offset;
        if (side) {
          side.style.transition = "none";
          side.style.transform = `translateX(${-sidebarWidth + offset}px)`;
        }
        if (over) {
          over.style.transition = "none";
          over.style.opacity = String((offset / sidebarWidth) * 0.5);
          over.style.pointerEvents = "auto";
        }
      } else if (open && dx < 0) {
        isSwiping.current = true;
        const offset = Math.max(dx, -sidebarWidth);
        swipeOffset.current = offset;
        if (side) {
          side.style.transition = "none";
          side.style.transform = `translateX(${offset}px)`;
        }
        if (over) {
          over.style.transition = "none";
          over.style.opacity = String(((sidebarWidth + offset) / sidebarWidth) * 0.5);
        }
      }
    };

    const onEnd = (e: TouchEvent) => {
      const side = sidebarRef.current;
      const over = overlayRef.current;

      if (isSwiping.current) {
        const open = isOpenRef.current;
        const settleThreshold = sidebarWidth * SETTLE_RATIO;

        const targetOpen = open
          ? swipeOffset.current > -settleThreshold
          : swipeOffset.current > settleThreshold;

        if (side) {
          side.style.transition = "";
          side.style.transform = "";
        }
        if (over) {
          over.style.transition = "";
          over.style.opacity = "";
          over.style.pointerEvents = "";
        }

        setIsOpen(targetOpen);
        isSwiping.current = false;
        swipeOffset.current = 0;
      } else {
        const t = e.changedTouches[0];
        if (!t) return;
        const dx = t.clientX - touchStartX.current;
        const dy = Math.abs(t.clientY - touchStartY.current);
        if (dy > Math.abs(dx)) return;

        if (isHandleSwipe.current && dx > MIN_DRAG_PX) {
          setIsOpen(true);
        } else if (isOpenRef.current && dx < -MIN_DRAG_PX) {
          setIsOpen(false);
        }
      }

      isHandleSwipe.current = false;
    };

    el.addEventListener("touchstart", onStart, { passive: false });
    el.addEventListener("touchmove", onMove, { passive: false });
    el.addEventListener("touchend", onEnd, { passive: true });

    return () => {
      el.removeEventListener("touchstart", onStart);
      el.removeEventListener("touchmove", onMove);
      el.removeEventListener("touchend", onEnd);
    };
  }, [enabled, sidebarWidth, sidebarRef, overlayRef, handleRef, setIsOpen, containerRef]);
}

// startedInHandle reports whether a touch began inside the drag-handle
// strip's rect. The handle is positioned via CSS (safe-area-inset aware),
// so its live rect is the source of truth — a null handle (unmounted, e.g.
// the sidebar is open) starts no open-gesture.
function startedInHandle(handle: HTMLDivElement | null, clientX: number, clientY: number): boolean {
  if (!handle) return false;
  const rect = handle.getBoundingClientRect();
  return clientX >= rect.left && clientX <= rect.right && clientY >= rect.top && clientY <= rect.bottom;
}
