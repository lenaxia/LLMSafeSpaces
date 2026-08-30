import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { RefObject } from "react";

const MENU_GAP = 4;
const VIEWPORT_PAD = 8;

/**
 * Pure, viewport-aware positioning for portal menus. Extracted so the
 * geometry (flip-above-when-no-room-below, horizontal clamp, maxHeight cap
 * for menus taller than the viewport) is unit-testable without jsdom layout.
 *
 * Returns the absolute (fixed-position) top/left plus an optional maxHeight
 * the caller must apply (with overflow-y-auto) when the menu is taller than
 * the available room on the chosen side.
 */
export function computeMenuPosition(
  btnRect: { top: number; bottom: number; left: number; right: number },
  menuSize: { width: number; height: number },
  viewport: { width: number; height: number },
  align: "left" | "right",
): { top: number; left: number; maxHeight?: number } {
  const mw = menuSize.width;
  const mh = menuSize.height;
  const vw = viewport.width;
  const vh = viewport.height;

  const spaceBelow = vh - btnRect.bottom - MENU_GAP;
  const spaceAbove = btnRect.top - MENU_GAP;

  let top: number;
  let maxHeight: number | undefined;

  if (mh <= spaceBelow) {
    // Plenty of room below — default placement.
    top = btnRect.bottom + MENU_GAP;
  } else if (mh <= spaceAbove) {
    // Not enough room below, but enough above — flip.
    top = btnRect.top - mh - MENU_GAP;
  } else {
    // Taller than both sides: pick the side with more room, clamp the top,
    // and cap the menu height so it scrolls inside the viewport instead of
    // overflowing.
    if (spaceBelow >= spaceAbove) {
      top = btnRect.bottom + MENU_GAP;
      maxHeight = vh - VIEWPORT_PAD - top;
    } else {
      top = VIEWPORT_PAD;
      maxHeight = btnRect.top - MENU_GAP - VIEWPORT_PAD;
    }
  }
  if (top < VIEWPORT_PAD) top = VIEWPORT_PAD;

  let left = align === "right" ? btnRect.right - mw : btnRect.left;
  if (left + mw > vw - VIEWPORT_PAD) left = vw - VIEWPORT_PAD - mw;
  if (left < VIEWPORT_PAD) left = VIEWPORT_PAD;

  return { top, left, ...(maxHeight !== undefined ? { maxHeight } : {}) };
}

export interface MenuPosition {
  top: number;
  left: number;
  maxHeight?: number;
}

/**
 * Viewport-aware positioning for a body-portaled floating element (menu,
 * toast, notice) anchored to a trigger.
 *
 * Returns a `triggerRef` for the anchor element, a `menuRef` for the floating
 * element, and the `pos` to apply as inline style (`position: fixed` via the
 * caller's className). While `active`, the position is computed before paint,
 * re-measured after paint, and kept anchored on scroll/resize — the element
 * flips above the trigger, clamps horizontally, and caps its height so it
 * never overflows the viewport.
 *
 * `anchorRef` lets several floating elements (e.g. a dropdown and its toast)
 * share one anchor: pass the first hook's `triggerRef` instead of binding a
 * second ref to the same button (a `ref` prop accepts only one ref).
 */
export function useMenuPosition<T extends HTMLElement = HTMLButtonElement, M extends HTMLElement = HTMLDivElement>(
  active: boolean,
  align: "left" | "right",
  fallbackWidth = 160,
  anchorRef?: RefObject<T | null>,
): { triggerRef: RefObject<T | null>; menuRef: RefObject<M | null>; pos: MenuPosition } {
  const ownAnchorRef = useRef<T>(null);
  const triggerRef = anchorRef ?? ownAnchorRef;
  const menuRef = useRef<M>(null);
  const [pos, setPos] = useState<MenuPosition>({ top: 0, left: 0 });

  const measureAndPosition = useCallback(() => {
    const btn = triggerRef.current;
    if (!btn) return;
    const btnRect = btn.getBoundingClientRect();
    const menu = menuRef.current;
    // scrollHeight (not offsetHeight) for height: once a maxHeight cap is
    // applied, offsetHeight returns the *capped* height and a re-measure
    // would wrongly conclude the menu fits and drop the cap, re-expanding it
    // past the viewport. scrollHeight always reports the full natural content
    // height regardless of the cap, so the cap decision is stable across
    // re-measures. offsetWidth is safe — width is never capped; fall back to
    // fallbackWidth when the floating element is not mounted yet.
    const menuSize = {
      width: menu?.offsetWidth ?? fallbackWidth,
      height: menu?.scrollHeight ?? 0,
    };
    setPos(
      computeMenuPosition(
        btnRect,
        menuSize,
        { width: window.innerWidth, height: window.innerHeight },
        align,
      ),
    );
  }, [align, fallbackWidth, triggerRef]);

  // Position synchronously after the floating element mounts, before paint,
  // so there is no flash at a stale offset.
  useLayoutEffect(() => {
    if (!active) return;
    measureAndPosition();
  }, [active, measureAndPosition]);

  useEffect(() => {
    if (!active) return;
    // Re-measure after paint. The pre-paint useLayoutEffect above reads the
    // element's size synchronously after commit; in most cases that is
    // correct, but a freshly portal-mounted element can report a stale
    // height (e.g. before fonts/images settle), which would skip the
    // flip/maxHeight cap and let the element overflow. This post-paint
    // remeasure self-corrects that case (a one-frame correction is better
    // than a permanent overflow). It's a no-op when the first measurement
    // was right.
    measureAndPosition();
    // Re-position on scroll/resize so the element stays anchored to its
    // trigger and clamped to the viewport as layout changes.
    const handleReposition = () => measureAndPosition();
    window.addEventListener("scroll", handleReposition, true);
    window.addEventListener("resize", handleReposition);
    return () => {
      window.removeEventListener("scroll", handleReposition, true);
      window.removeEventListener("resize", handleReposition);
    };
  }, [active, measureAndPosition]);

  return { triggerRef, menuRef, pos };
}
