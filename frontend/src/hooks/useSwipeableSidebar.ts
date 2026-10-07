import { useEffect, useRef, type RefObject } from "react";

// #1629: the full-edge gesture. `overscroll-behavior-x: none` on
// html/body (frontend/src/styles/index.css) disables the browser's
// horizontal swipe-back navigation at the CSS level — the standard PWA
// mechanism the #1626 diagnosis missed (MDN: contain/none "disables
// native browser navigation, including ... horizontal swipe navigation";
// Chrome 63+, Safari 16+, Firefox 59+). With no OS race left to lose, the
// original a56430b7/#590 EDGE_ZONE contract is restored: a touch in the
// leftmost 30px is CLAIMED at touchstart (preventDefault on a
// non-passive listener) and a rightward drag opens the drawer. Two
// documented tradeoffs: vertical scrolling from the leftmost 30px is
// blocked (that band is the gesture zone, not a scroll surface), and
// touches that land on interactive controls are never claimed — the
// hamburger's left half sits inside the band, and the #1626 r2 review
// proved a touchstart preventDefault over a tappable control suppresses
// its synthetic click. The hamburger stays the always-works path. The
// #1626 inset drag-handle strip and its claim-at-move scoping are
// deleted. Swipe-to-close is unchanged.
const EDGE_ZONE = 30;
const SETTLE_RATIO = 1 / 3;
const INTERACTIVE_SELECTOR = "button, a, input, textarea, select, [role='button'], [role='tab'], [role='option'], [role='switch']";

interface UseSwipeableSidebarOptions {
  containerRef: RefObject<HTMLDivElement | null>;
  sidebarRef: RefObject<HTMLDivElement | null>;
  overlayRef: RefObject<HTMLDivElement | null>;
  isOpen: boolean;
  setIsOpen: (value: boolean | ((prev: boolean) => boolean)) => void;
  enabled: boolean;
  sidebarWidth: number;
}

export function useSwipeableSidebar({
  containerRef,
  sidebarRef,
  overlayRef,
  isOpen,
  setIsOpen,
  enabled,
  sidebarWidth,
}: UseSwipeableSidebarOptions) {
  const touchStartX = useRef(0);
  const touchStartY = useRef(0);
  const isEdgeSwipe = useRef(false);
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
      // Skip interactive controls (the hamburger's left half lives inside
      // the band): a preventDefault here would suppress the control's
      // synthetic click — the #1626 r2 empirical finding — and controls
      // are tap targets, not swipe origins.
      const onInteractiveControl = t.target instanceof Element && t.target.closest(INTERACTIVE_SELECTOR) !== null;
      isEdgeSwipe.current = t.clientX < EDGE_ZONE && !onInteractiveControl;
      // Claim edge touches at the earliest moment. overscroll-behavior-x
      // already tells the browser the horizontal overscroll belongs to
      // the page (no back-nav gesture engages); preventDefault here keeps
      // the touch ours — no scroll, no history navigation, no synthetic
      // click from inside the gesture band.
      if (isEdgeSwipe.current) {
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

      if (isEdgeSwipe.current && dx > 0 && !open) {
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

        if (isEdgeSwipe.current && dx > EDGE_ZONE) {
          setIsOpen(true);
        } else if (isOpenRef.current && dx < -EDGE_ZONE) {
          setIsOpen(false);
        }
      }

      isEdgeSwipe.current = false;
    };

    el.addEventListener("touchstart", onStart, { passive: false });
    el.addEventListener("touchmove", onMove, { passive: false });
    el.addEventListener("touchend", onEnd, { passive: true });

    return () => {
      el.removeEventListener("touchstart", onStart);
      el.removeEventListener("touchmove", onMove);
      el.removeEventListener("touchend", onEnd);
    };
  }, [enabled, sidebarWidth, sidebarRef, overlayRef, setIsOpen, containerRef]);
}
