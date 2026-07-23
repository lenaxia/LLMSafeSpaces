import { useEffect, useRef, type RefObject } from "react";

const EDGE_ZONE = 30;
const SETTLE_RATIO = 1 / 3;

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
      isEdgeSwipe.current = t.clientX < EDGE_ZONE;
      // Claim the gesture at touchstart for edge touches. Mobile browsers
      // commit to the back-nav gesture during touchstart / first-touchmove;
      // by the time touchmove fires, the OS may have already latched.
      // Calling preventDefault() here (requires a non-passive listener)
      // tells the browser "I own this touch" before it can engage back-nav.
      // This is the fix for the ~50% of edge swipes that triggered browser
      // back instead of opening the sidebar. The tradeoff: vertical
      // scrolling from the leftmost EDGE_ZONE pixels is blocked — acceptable
      // since that zone is the gesture zone, not a scroll surface.
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
