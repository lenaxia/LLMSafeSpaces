import { useState, useRef, useEffect, useLayoutEffect, useCallback, Fragment } from "react";
import { createPortal } from "react-dom";
import { Copy, MoreHorizontal } from "lucide-react";
import { cn } from "../../lib/utils";

export interface KebabMenuItem {
  label: string;
  onClick: () => void;
  destructive?: boolean;
  disabled?: boolean;
  /**
   * Optional section header. Items sharing the same `section` value render
   * consecutively under a labelled divider (e.g. "Lifecycle"). Items are
   * rendered in array order, so the caller controls grouping order. When no
   * item declares a section the menu falls back to its legacy layout
   * (non-destructive items first, destructive items last with a divider),
   * preserving the behaviour of menus that pre-date sections.
   */
  section?: string;
}

interface Props {
  items: KebabMenuItem[];
  align?: "left" | "right";
  footer?: string[];
}

const MENU_GAP = 4;
const VIEWPORT_PAD = 8;

/**
 * Pure, viewport-aware positioning for the kebab menu. Extracted so the
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

export function KebabMenu({ items, align = "right", footer }: Props) {
  const [open, setOpen] = useState(false);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const menuRef = useRef<HTMLDivElement>(null);
  const [pos, setPos] = useState<{ top: number; left: number; maxHeight?: number }>({ top: 0, left: 0 });

  const measureAndPosition = useCallback(() => {
    const btn = buttonRef.current;
    if (!btn) return;
    const btnRect = btn.getBoundingClientRect();
    const menu = menuRef.current;
    // Before the menu has mounted (first open tick), fall back to the
    // assumed 160px width and an estimated height; once mounted we measure
    // the real offsetHeight/offsetWidth so flip/clamp use true geometry.
    const menuSize = {
      width: menu?.offsetWidth ?? 160,
      height: menu?.offsetHeight ?? 0,
    };
    setPos(
      computeMenuPosition(
        btnRect,
        menuSize,
        { width: window.innerWidth, height: window.innerHeight },
        align,
      ),
    );
  }, [align]);

  // Position synchronously after the menu mounts, before paint, so there is
  // no flash at a stale offset. Reads the rendered menu size on the first
  // committed render.
  useLayoutEffect(() => {
    if (!open) return;
    measureAndPosition();
  }, [open, measureAndPosition]);

  useEffect(() => {
    if (!open) return;
    const handleClick = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node) &&
          buttonRef.current && !buttonRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    // Re-position on scroll/resize so the menu stays anchored to its trigger
    // and clamped to the viewport as layout changes (layout-aware).
    const handleReposition = () => measureAndPosition();
    document.addEventListener("mousedown", handleClick);
    window.addEventListener("scroll", handleReposition, true);
    window.addEventListener("resize", handleReposition);
    return () => {
      document.removeEventListener("mousedown", handleClick);
      window.removeEventListener("scroll", handleReposition, true);
      window.removeEventListener("resize", handleReposition);
    };
  }, [open, measureAndPosition]);

  const close = () => setOpen(false);

  return (
    <>
      <button
        ref={buttonRef}
        onClick={(e) => { e.stopPropagation(); setOpen(!open); }}
        className="rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground transition-opacity"
        aria-label="Actions"
        aria-haspopup="true"
        aria-expanded={open}
      >
        <MoreHorizontal className="h-4 w-4" />
      </button>
      {open && createPortal(
        <div
          ref={menuRef}
          className="fixed z-[9999] w-40 rounded-md border border-border bg-popover py-1 shadow-md overflow-y-auto"
          style={{ top: pos.top, left: pos.left, maxHeight: pos.maxHeight }}
          role="menu"
        >
          <MenuBody items={items} footer={footer} onClose={close} />
        </div>,
        document.body,
      )}
    </>
  );
}

function MenuBody({ items, footer, onClose }: { items: KebabMenuItem[]; footer?: string[]; onClose: () => void }) {
  const hasSections = items.some((i) => i.section !== undefined);

  if (!hasSections) {
    // Legacy two-phase layout: non-destructive items, then footer, then a
    // divider, then destructive items. Preserves the exact behaviour of the
    // session/workspace menus that pre-date sections.
    const normal = items.filter((i) => !i.destructive);
    const destructive = items.filter((i) => i.destructive);
    return (
      <>
        {normal.map((item, i) => <ItemButton key={i} item={item} onClose={onClose} />)}
        <Footer footer={footer} />
        {destructive.length > 0 && <div className="border-t border-border mx-2 my-1" />}
        {destructive.map((item, i) => <ItemButton key={`d-${i}`} item={item} onClose={onClose} />)}
      </>
    );
  }

  // Sectioned layout: items render in array order, with a labelled divider
  // whenever the `section` value changes (the header renders only for named
  // sections; items without a section sit at the top with no header).
  let prevSection: string | undefined;
  return (
    <>
      {items.map((item, i) => {
        const showHeader = item.section !== undefined && item.section !== prevSection;
        prevSection = item.section;
        return (
          <Fragment key={i}>
            {showHeader && (
              <div className="border-t border-border mx-2 my-1">
                <div className="px-3 pt-1 pb-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground/70">
                  {item.section}
                </div>
              </div>
            )}
            <ItemButton item={item} onClose={onClose} />
          </Fragment>
        );
      })}
      <Footer footer={footer} />
    </>
  );
}

function ItemButton({ item, onClose }: { item: KebabMenuItem; onClose: () => void }) {
  return (
    <button
      role="menuitem"
      disabled={item.disabled}
      onClick={(e) => {
        e.stopPropagation();
        if (!item.disabled) {
          item.onClick();
          onClose();
        }
      }}
      className={cn(
        "flex w-full items-center px-3 py-1.5 text-left text-xs transition-colors",
        item.destructive
          ? "text-destructive hover:bg-red-500/10 dark:hover:bg-red-500/20"
          : "text-foreground hover:bg-accent",
        item.disabled && "opacity-50 cursor-not-allowed",
      )}
    >
      {item.label}
    </button>
  );
}

function Footer({ footer }: { footer?: string[] }) {
  if (!footer || footer.length === 0) return null;
  return (
    <div className="border-t border-border mx-2 my-1 pt-1">
      <div className="flex items-center justify-between px-3">
        <div>
          {footer.map((line, i) => (
            <div key={i} className="py-0.5 text-[10px] text-muted-foreground select-text">
              {line}
            </div>
          ))}
        </div>
        <button
          onClick={(e) => {
            e.stopPropagation();
            navigator.clipboard.writeText(footer.join("\n"));
          }}
          className="ml-2 rounded p-0.5 text-muted-foreground hover:text-foreground hover:bg-accent transition-colors"
          aria-label="Copy version info"
          title="Copy version info"
        >
          <Copy className="h-3 w-3" />
        </button>
      </div>
    </div>
  );
}
