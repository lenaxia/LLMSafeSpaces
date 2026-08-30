import { useState, useEffect, Fragment } from "react";
import { createPortal } from "react-dom";
import { Copy, MoreHorizontal } from "lucide-react";
import { cn } from "../../lib/utils";
import { useMenuPosition } from "./menuPosition";

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

export function KebabMenu({ items, align = "right", footer }: Props) {
  const [open, setOpen] = useState(false);
  const { triggerRef: buttonRef, menuRef, pos } = useMenuPosition<HTMLButtonElement, HTMLDivElement>(open, align);

  useEffect(() => {
    if (!open) return;
    const handleClick = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node) &&
          buttonRef.current && !buttonRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", handleClick);
    return () => {
      document.removeEventListener("mousedown", handleClick);
    };
  }, [open, menuRef, buttonRef]);

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
